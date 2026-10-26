package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/spec"
	"github.com/marcioapm/lux/internal/store"
)

// RegistryConfig is [registry]: the operator's registry, from which luxd
// mints pull credentials for Runs whose tenant has a grant. Host "" is off.
type RegistryConfig struct {
	Host   string
	Grants []RegistryGrant
	// Source is the provider that mints the tokens (registry_ecr.go).
	Source RegistryTokenSource
}

// RegistryGrant lets Tenants (ids, or "*" for every tenant) pull Owner's
// repositories (<host>/<owner>/...).
type RegistryGrant struct {
	Owner   string
	Tenants []string
}

// A RegistryTokenSource mints a pull-only credential for one owner's
// repositories on the registry's host. The token Mint returns for owner
// must be able to pull <host>/<owner>/... and nothing else: no other
// owner's repositories, no push, no other action. luxd attaches it to the
// auth.json key <host>/<owner> only, caches one per owner, and decides
// which owners a Run gets; a source only mints. Mint may block (a network
// call) and must return when ctx ends. An error Mint returns must not
// contain the token, its password or any other credential material
// (keys, signed assertions, response bodies carrying them): luxd logs it.
type RegistryTokenSource interface {
	Mint(ctx context.Context, owner string) (RegistryToken, error)
}

// RegistryToken is an auth.json credential valid until Expires.
type RegistryToken struct {
	User, Password string
	Expires        time.Time
}

const (
	// registryReuse: a cached token is handed out while it has at least
	// this long left, which covers a slow pull started just before expiry.
	registryReuse = 30 * time.Minute
	// registryRefresh: below this much validity a token is still handed
	// out, and a new one is minted in the background, so delivery keeps
	// hitting a warm cache.
	registryRefresh = 40 * time.Minute
	// registryRefreshSpread: each owner refreshes up to this much later
	// than registryRefresh (registryRefreshFor), still well above
	// registryReuse.
	registryRefreshSpread = 4 * time.Minute
	// registryRetry: after a failed mint, an owner is not tried again for
	// this long, so every delivery pass does not start a failing mint.
	registryRetry       = 30 * time.Second
	registryMintTimeout = 10 * time.Second
	// registryHold bounds how long an assignment waits for its owners'
	// tokens in total, from when a delivery pass first held it, however
	// many owners it has. After it, it goes without the late ones.
	registryHold = 10 * time.Second
)

// registryMinter holds one pull token per owner, in memory only. Mints run
// in their own goroutines, never a caller's: one in flight per owner,
// owners in parallel. Callers only read the cache and start mints. Mints
// and hold timers belong to the minter's life, which close ends.
type registryMinter struct {
	cfg  RegistryConfig
	now  func() time.Time
	hold time.Duration
	// ready is called with each host waiting on a mint once it ends, and
	// with an assignment's host when its hold ends.
	ready func(hostID string)
	// afterFunc runs f after d unless stopped (time.AfterFunc; tests
	// replace it to drive hold expiry by hand).
	afterFunc func(d time.Duration, f func()) (stop func() bool)
	life      context.Context
	end       context.CancelFunc
	wg        sync.WaitGroup
	mu        sync.Mutex
	closed    bool
	owners    map[string]*ownerToken
	held      map[int64]*heldAssign
	// nextSweep: when lookup next drops holds long over.
	nextSweep time.Time
}

// heldAssign is an assignment (by message id) waiting on its owners'
// mints since since; stop cancels the pass scheduled for its bound.
type heldAssign struct {
	since time.Time
	stop  func() bool
}

type ownerToken struct {
	user, pass string
	expires    time.Time
	failedAt   time.Time
	lastErr    error
	// minting: a mint is in flight; waiters are the hosts holding an
	// assignment for it.
	minting bool
	waiters map[string]bool
}

func newRegistryMinter(cfg RegistryConfig, now func() time.Time) *registryMinter {
	if cfg.Host == "" {
		return nil
	}
	if cfg.Source == nil {
		panic("registry: a host with no token source")
	}
	life, end := context.WithCancel(context.Background())
	return &registryMinter{cfg: cfg, now: now, hold: registryHold, ready: func(string) {},
		afterFunc: func(d time.Duration, f func()) func() bool { return time.AfterFunc(d, f).Stop },
		life:      life, end: end, owners: map[string]*ownerToken{}, held: map[int64]*heldAssign{}}
}

// close cancels every mint in flight, stops every hold timer and waits for
// what already started; lookups after it hold and mint nothing.
func (m *registryMinter) close() {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.closed = true
	for _, h := range m.held {
		h.stop()
	}
	m.mu.Unlock()
	m.end()
	m.wg.Wait()
}

// notify is ready(hostID) unless the minter is closed.
func (m *registryMinter) notify(hostID string) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.wg.Add(1)
	m.mu.Unlock()
	defer m.wg.Done()
	m.ready(hostID)
}

// ownersFor are the owners tenantID may pull, in configuration order.
func (m *registryMinter) ownersFor(tenantID string) []string {
	if m == nil {
		return nil
	}
	var out []string
	for _, g := range m.cfg.Grants {
		if slices.Contains(g.Tenants, "*") || slices.Contains(g.Tenants, tenantID) {
			out = append(out, g.Owner)
		}
	}
	return out
}

var (
	errRegistryBackoff = errors.New("an earlier attempt failed; retrying shortly")
	errRegistrySlow    = errors.New("still minting when the assignment's wait ended")
)

// registryRefreshFor is registryRefresh less up to registryRefreshSpread,
// fixed per owner, so owners first minted together do not refresh together.
func registryRefreshFor(owner string) time.Duration {
	h := fnv.New32a()
	h.Write([]byte(owner))
	return registryRefresh - time.Duration(h.Sum32()%uint32(registryRefreshSpread/time.Second))*time.Second
}

// lookup is, for assignment msgID going to hostID, the auth.json entries of
// owners whose tokens are ready. A cold owner gets a mint started. pending:
// some owner is minting and msgID was first held less than m.hold ago;
// hostID is notified (m.ready) as each such mint ends, and when the hold
// does. An owner with no token that is not pending is in errs. Never waits
// on the token source. A closed minter (luxd shutting down) holds every
// assignment that needs it, and mints nothing.
func (m *registryMinter) lookup(ctx context.Context, hostID string, msgID int64, owners []string) (creds []proto.RegistryCredential, pending bool, errs map[string]error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, true, nil
	}
	now := m.now()
	h := m.held[msgID]
	since := now
	if h != nil {
		since = h.since
	}
	waiting := now.Sub(since) < m.hold
	slow := false
	for _, owner := range owners {
		t := m.owners[owner]
		if t == nil {
			t = &ownerToken{}
			m.owners[owner] = t
		}
		left := t.expires.Sub(now)
		canMint := !t.minting && now.Sub(t.failedAt) >= registryRetry
		if t.pass != "" && left >= registryReuse {
			if left < registryRefreshFor(owner) && canMint {
				m.startMint(ctx, owner, t)
			}
			creds = append(creds, proto.RegistryCredential{Registry: m.cfg.Host + "/" + owner, User: t.user, Password: t.pass})
			continue
		}
		if canMint {
			m.startMint(ctx, owner, t)
		}
		var err error
		switch {
		case t.minting && waiting:
			t.waiters[hostID] = true
			pending = true
			continue
		case t.minting:
			err, slow = errRegistrySlow, true
		default:
			err = fmt.Errorf("%w: %v", errRegistryBackoff, t.lastErr)
		}
		if errs == nil {
			errs = map[string]error{}
		}
		errs[owner] = err
	}
	switch {
	case pending && h == nil:
		// The pass after the wait sends it, even if no mint ends first.
		m.held[msgID] = &heldAssign{since: since, stop: m.afterFunc(m.hold, func() { m.notify(hostID) })}
	case !pending && !slow && h != nil:
		h.stop()
		delete(m.held, msgID)
	}
	// Past its bound with a mint still running, an assignment keeps its
	// entry, so a pass that fails to send it does not start a new wait.
	// One acked elsewhere (or whose host left) is never looked up again:
	// it goes once no mint it waited on can still be running. Swept at
	// most once per hold, not per lookup.
	if !now.Before(m.nextSweep) {
		for id, a := range m.held {
			if now.Sub(a.since) > m.hold+registryMintTimeout {
				a.stop()
				delete(m.held, id)
			}
		}
		m.nextSweep = now.Add(m.hold)
	}
	return creds, pending, errs
}

// startMint mints owner's token in its own goroutine, on a context that
// keeps ctx's values but not its cancellation: a caller going away (a
// runner disconnecting mid-mint) must neither abort the mint nor put the
// owner in backoff for every other host. The minter's close cancels it.
// Call with m.mu held, the minter open.
func (m *registryMinter) startMint(ctx context.Context, owner string, t *ownerToken) {
	t.minting, t.waiters = true, map[string]bool{}
	mctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), registryMintTimeout)
	stop := context.AfterFunc(m.life, cancel)
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer stop()
		defer cancel()
		user, pass, expires, err := m.mint(mctx, owner)
		m.mu.Lock()
		if err != nil {
			t.failedAt, t.lastErr = m.now(), err
		} else {
			t.user, t.pass, t.expires, t.failedAt, t.lastErr = user, pass, expires, time.Time{}, nil
		}
		t.minting = false
		waiters := t.waiters
		t.waiters = nil
		closed := m.closed
		m.mu.Unlock()
		if closed {
			return
		}
		for h := range waiters {
			m.ready(h)
		}
	}()
}

// errRegistryNoToken: a source answered without a password or an expiry.
// Cached, such a token would read as none and be minted again every pass.
var errRegistryNoToken = errors.New("the token source returned no password or no expiry")

// redactedError is a source's error with the password it returned beside
// it cut from the text luxd logs; errors.Is still sees the cause.
type redactedError struct {
	msg string
	err error
}

func (e *redactedError) Error() string { return e.msg }
func (e *redactedError) Unwrap() error { return e.err }

// registryRedactMin is the shortest password cut out of an error's text in
// place. A shorter one also matches ordinary words, so cutting it would
// mangle the message: the whole text is withheld instead.
const registryRedactMin = 8

// errRegistryWithheld is the text logged for a source error that quotes a
// password too short to cut out cleanly.
const errRegistryWithheld = "the token source's error was withheld: it quotes the credential it returned"

// redactPassword is err with every occurrence of password cut from its
// text (the whole text withheld when password is shorter than
// registryRedactMin), or err itself when the text does not contain it.
func redactPassword(err error, password string) error {
	msg := err.Error()
	if password == "" || !strings.Contains(msg, password) {
		return err
	}
	// A password inside the marker itself ("redacted", "[redacte", ...)
	// would survive the cut, so that text is withheld too.
	cut := strings.ReplaceAll(msg, password, "[redacted]")
	if len(password) < registryRedactMin || strings.Contains(cut, password) {
		return &redactedError{msg: errRegistryWithheld, err: err}
	}
	return &redactedError{msg: cut, err: err}
}

func (m *registryMinter) mint(ctx context.Context, owner string) (user, pass string, expires time.Time, err error) {
	tok, err := m.cfg.Source.Mint(ctx, owner)
	if err != nil {
		return "", "", time.Time{}, redactPassword(err, tok.Password)
	}
	if tok.Password == "" || tok.Expires.IsZero() {
		return "", "", time.Time{}, errRegistryNoToken
	}
	return tok.User, tok.Password, tok.Expires, nil
}

// registryOwners is which of granted a Run with image (and the pinned
// Containerfile of an earlier build, if any) pulls from on host: the owner
// segment (<host>/<owner>/...) of its image.ref, image.build.cache and
// every FROM. Every pull the runner makes is one of these, named
// literally: it refuses a FROM naming its image by a variable, so one
// gives no owner. None when an explicit registryAuth names host, which
// the runner prefers to any minted entry.
func registryOwners(image spec.Image, resolved, host string, granted []string) []string {
	host = strings.ToLower(host)
	for _, a := range image.RegistryAuth {
		if strings.ToLower(a.Registry) == host {
			return nil
		}
	}
	named := map[string]bool{}
	name := func(ref string) {
		path, ok := strings.CutPrefix(strings.ToLower(ref), host+"/")
		if owner, _, nested := strings.Cut(path, "/"); ok && nested && !strings.Contains(ref, "$") {
			named[owner] = true
		}
	}
	name(image.Ref)
	if b := image.Build; b != nil {
		name(b.Cache)
		// COPY --from and RUN --mount=from= may only name earlier stages
		// (the runner refuses an image there), so FROM lines are every
		// base pulled.
		for _, cf := range []string{b.Containerfile, resolved} {
			for line := range strings.SplitSeq(cf, "\n") {
				if f, ok := spec.ParseFrom(line); ok {
					name(f.Image)
				}
			}
		}
	}
	var out []string
	for _, owner := range granted {
		if named[owner] {
			out = append(out, owner)
		}
	}
	return out
}

// registryPlanLimit: the plan reads at most this many assignments, the
// delivery query's batch. One past it is not sendable, so it and its Run's
// later messages wait for a pass that plans it.
const registryPlanLimit = 500

// registryPlan is what a delivery pass attaches. With restrict (the host
// advertised CapRegistryMinted when the plan was read), the pass sends
// only the assignments in sendable, each with creds[id], and only while
// the host still advertises it. Every other assignment (waiting on a
// mint, or committed or made undelivered again after the plan was read)
// waits for a later pass, and so do its Run's later messages.
type registryPlan struct {
	restrict bool
	sendable []int64
	creds    map[int64][]proto.RegistryCredential
}

// planRegistryCredentials reads, before anything is marked delivered, the
// host's capability and its undelivered assignments, and looks up their
// tokens. A failure here returns before the batch is touched, so it is
// retried whole on the next pass.
func (s *Server) planRegistryCredentials(ctx context.Context, hostID string, undelivered bool) (registryPlan, error) {
	var plan registryPlan
	if s.registry == nil {
		return plan, nil
	}
	type pendingAssign struct {
		id                        int64
		epoch                     int
		runID, tenantID, resolved string
		image                     spec.Image
	}
	if s.registryPlanFails != nil {
		if err := s.registryPlanFails(); err != nil {
			return registryPlan{}, err
		}
	}
	var assigns []pendingAssign
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		// One row with a NULL id when the host is capable but has no
		// assignment; none when it is not capable.
		rows, err := tx.Query(ctx, `SELECT m.id, coalesce(m.epoch, 0), coalesce(m.run_id, ''), coalesce(m.payload->>'tenantId', ''),
				coalesce(m.payload->'spec'->'image', '{}'), coalesce(m.payload->'imageResolved'->>'containerfile', '')
			FROM hosts h
			LEFT JOIN LATERAL (SELECT * FROM host_messages
				WHERE host_id = h.id AND type = $3 AND acked_at IS NULL AND (NOT $4 OR delivered_at IS NULL)
				ORDER BY id LIMIT $5) m ON true
			WHERE h.id = $1 AND $2 = ANY(h.capabilities)
			ORDER BY m.id`, hostID, proto.CapRegistryMinted, proto.MsgAssign, undelivered, registryPlanLimit)
		if err != nil {
			return err
		}
		found, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (pendingAssign, error) {
			var a pendingAssign
			var id *int64
			var image []byte
			if err := r.Scan(&id, &a.epoch, &a.runID, &a.tenantID, &image, &a.resolved); err != nil || id == nil {
				return a, err
			}
			a.id = *id
			// An image the spec type cannot read names nothing: no token.
			_ = json.Unmarshal(image, &a.image)
			return a, nil
		})
		plan.restrict = len(found) > 0
		for _, a := range found {
			if a.id != 0 {
				assigns = append(assigns, a)
			}
		}
		return err
	})
	if err != nil {
		return registryPlan{}, err
	}
	for _, a := range assigns {
		owners := registryOwners(a.image, a.resolved, s.registry.cfg.Host, s.registry.ownersFor(a.tenantID))
		if len(owners) == 0 {
			plan.sendable = append(plan.sendable, a.id)
			continue
		}
		creds, pending, errs := s.registry.lookup(ctx, hostID, a.id, owners)
		if pending {
			continue
		}
		plan.sendable = append(plan.sendable, a.id)
		for owner, err := range errs {
			s.log.Warn("registry pull credentials not minted; the assignment goes without them",
				"host", hostID, "run", a.runID, "epoch", a.epoch, "tenant", a.tenantID, "owner", owner, "err", err)
		}
		if len(creds) > 0 {
			if plan.creds == nil {
				plan.creds = map[int64][]proto.RegistryCredential{}
			}
			plan.creds[a.id] = creds
		}
	}
	return plan, nil
}

// attachRegistryCredentials adds an assignment's planned credentials: at
// send time, never stored.
func attachRegistryCredentials(creds []proto.RegistryCredential, data json.RawMessage) json.RawMessage {
	if len(creds) == 0 {
		return data
	}
	var a proto.Assign
	if err := json.Unmarshal(data, &a); err != nil {
		return data
	}
	a.RegistryCredentials = creds
	return proto.Marshal(a)
}

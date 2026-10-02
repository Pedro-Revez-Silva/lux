package server

import (
	"cmp"
	"context"
	"errors"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/marcioapm/lux/internal/ids"
	"github.com/marcioapm/lux/internal/spec"
	"github.com/marcioapm/lux/internal/store"
)

// The tenant's servers: /v1/servers. A server is created on its own, with
// or without a Run, attached to and detached from Runs, and deleted by its
// owner (or with its Run, lifetime run). /v1/runs/{id}/servers is the same
// servers, those attached to one Run.

// TenantServer is a server as /v1/servers shows it.
type TenantServer struct {
	ID            string            `json:"id"`
	Name          string            `json:"name"`
	Hostname      *string           `json:"hostname" nullable:"true" doc:"Its preview host name, stable for its life; null when previews are not configured."`
	URL           *string           `json:"url" nullable:"true" doc:"Its preview URL; null when previews are not configured."`
	State         string            `json:"state" enum:"ready,waking,asleep,stopped,unreachable,exited,no answer" doc:"ready: its Run runs and its port answers. waking: on its way up (a wake asked for, its Run starting, its command starting). asleep: wakes on request and nothing serves it now. stopped: does not wake and its Run is not running, or stopped by someone. unreachable, exited: as the process. no answer: a wake was asked for longer than wakeTimeout ago and no Run came up; the next request asks again."`
	Process       string            `json:"process" enum:"stopped,starting,ready,unreachable,exited" doc:"The state of its process in its Run, as GET /v1/runs/{id}/servers shows it."`
	Desired       string            `json:"desired" enum:"up,down" doc:"down: stopped by request; a new placement leaves it stopped until started again."`
	Port          int               `json:"port"`
	Command       []string          `json:"command" nullable:"true"`
	Workdir       string            `json:"workdir"`
	Env           map[string]string `json:"env"`
	AfterSync     []string          `json:"afterSync" nullable:"true"`
	Labels        map[string]string `json:"labels"`
	Wake          string            `json:"wake" enum:"request,never"`
	IdleAfter     spec.Duration     `json:"idleAfter" doc:"How long without a request before server.idle (0: never)."`
	WakeTimeout   spec.Duration     `json:"wakeTimeout"`
	Lifetime      string            `json:"lifetime" enum:"run,owner"`
	ExpireAfter   *spec.Duration    `json:"expireAfter" nullable:"true" doc:"lifetime owner: deleted after this long without a request (null: never)."`
	ExpiresAt     *time.Time        `json:"expiresAt,omitempty"`
	Owner         string            `json:"owner" doc:"Who created it: a key id or a person's email."`
	RunID         *string           `json:"runId" nullable:"true" doc:"The Run it is attached to, if any."`
	RunName       string            `json:"runName,omitempty"`
	RunState      string            `json:"runState,omitempty"`
	FromSpec      bool              `json:"fromSpec"`
	ExitCode      *int              `json:"exitCode,omitempty"`
	Error         *string           `json:"error,omitempty"`
	Since         time.Time         `json:"since"`
	ReadySince    *time.Time        `json:"readySince" nullable:"true"`
	StopReason    *string           `json:"stopReason" nullable:"true"`
	Epoch         *int              `json:"epoch" nullable:"true"`
	LastRequestAt *time.Time        `json:"lastRequestAt" nullable:"true"`
	IdleAt        *time.Time        `json:"idleAt,omitempty" doc:"Ready: when it goes idle without another request."`
	WakeRequested *time.Time        `json:"wakeRequestedAt,omitempty" doc:"The open wake: when it was asked for."`
	Wakes         int               `json:"wakes" doc:"How many wakes it has asked for."`
	CreatedAt     time.Time         `json:"createdAt"`
	UpdatedAt     time.Time         `json:"updatedAt"`
}

func (s *Server) tenantServer(v serverRow, now time.Time) TenantServer {
	t := TenantServer{ID: v.ID, Name: v.Name, Hostname: optString(s.previewHostname(v.Host)), URL: s.previewURL(v.Host),
		State: v.derive(now), Process: v.State, Desired: "up", Port: v.Port, Command: v.Command, Workdir: v.Workdir, Env: v.Env,
		AfterSync: v.AfterSync, Labels: v.Labels, Wake: v.Wake, IdleAfter: secs(v.IdleAfterS), WakeTimeout: secs(v.WakeTimeoutS),
		Lifetime: v.Lifetime, Owner: v.Owner, RunID: v.RunID, RunName: v.RunName, RunState: v.RunState, FromSpec: v.FromSpec,
		ExitCode: v.ExitCode, Error: v.Error, Since: v.Since, ReadySince: v.ReadySince, StopReason: v.StopReason, Epoch: v.Epoch,
		LastRequestAt: v.LastRequestAt, IdleAt: v.idleAt(), WakeRequested: v.WakeRequestedAt, Wakes: v.Wakes, CreatedAt: v.CreatedAt,
		UpdatedAt: v.UpdatedAt}
	if v.down() {
		t.Desired = "down"
	}
	if v.ExpireAfterS != nil && v.Lifetime == LifetimeOwner {
		d := secs(*v.ExpireAfterS)
		t.ExpireAfter = &d
		at := cmp.Or(v.LastRequestAt, &v.CreatedAt).Add(d.Duration)
		t.ExpiresAt = &at
	}
	return t
}

func secs(n int) spec.Duration { return spec.Duration{Duration: time.Duration(n) * time.Second} }

// ---- validation -------------------------------------------------------------

var labelKeyRe = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9._/-]{0,62}[A-Za-z0-9])?$`)

func checkLabels(labels map[string]string) error {
	for k, v := range labels {
		if !labelKeyRe.MatchString(k) || len(v) > 256 {
			return errf(http.StatusUnprocessableEntity, "invalid_server", "labels: invalid label %q (keys: letters, digits, . _ / -; values up to 256 bytes)", k)
		}
	}
	if len(labels) > 64 {
		return errf(http.StatusUnprocessableEntity, "invalid_server", "labels: at most 64")
	}
	return nil
}

var hostLabelRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// checkHostname turns an owner's hostname into the host stored: the part
// before the preview domain. A single label is relative (the domain is
// appended); a dotted name must already be under the domain. 253 bytes at
// most in all.
func (s *Server) checkHostname(hostname string) (string, error) {
	domain := strings.ToLower(strings.TrimSuffix(s.cfg.Preview.Domain, "."))
	if domain == "" {
		return "", errf(http.StatusUnprocessableEntity, "invalid_server", "hostname: previews are not configured on this lux (preview.domain)")
	}
	h := strings.ToLower(strings.TrimSuffix(hostname, "."))
	if !strings.Contains(hostname, ".") {
		h += "." + domain
	}
	rel, ok := strings.CutSuffix(h, "."+domain)
	if !ok || rel == "" || len(h) > 253 {
		return "", errf(http.StatusUnprocessableEntity, "invalid_server", "hostname: %q is not under the preview domain %s", hostname, domain)
	}
	for _, l := range strings.Split(rel, ".") {
		if !hostLabelRe.MatchString(l) {
			return "", errf(http.StatusUnprocessableEntity, "invalid_server", "hostname: %q: %q is not a DNS label (a-z, 0-9 and -, 1-63, not starting or ending in -)", hostname, l)
		}
	}
	return rel, nil
}

// The bounds of a server's durations.
const (
	maxIdleAfter   = 30 * 24 * time.Hour
	minWakeTimeout = time.Second
	maxWakeTimeout = 24 * time.Hour
	maxExpireAfter = 10 * 365 * 24 * time.Hour
)

func checkWake(wake string) error {
	if wake != WakeNever && wake != WakeRequest {
		return errf(http.StatusUnprocessableEntity, "invalid_server", "wake: want request or never")
	}
	return nil
}

func checkLifetime(lifetime string) error {
	if lifetime != LifetimeOwner && lifetime != LifetimeRun {
		return errf(http.StatusUnprocessableEntity, "invalid_server", "lifetime: want run or owner")
	}
	return nil
}

// durationOr is d in whole seconds, def when unset; bounds checked.
func durationOr(name string, d *spec.Duration, def time.Duration, min, max time.Duration) (int, error) {
	v := def
	if d != nil {
		v = d.Duration
	}
	if v < min || v > max {
		return 0, errf(http.StatusUnprocessableEntity, "invalid_server", "%s: must be between %s and %s", name, min, max)
	}
	return int(v / time.Second), nil
}

// ---- create ------------------------------------------------------------------

// CreateServerInput is a new server of the tenant.
type CreateServerInput struct {
	Name        string            `json:"name" doc:"1-30 of a-z, 0-9 and -, starting with a letter, not ending in -; unique among the servers of the Run it is attached to."`
	Port        int               `json:"port"`
	Command     []string          `json:"command,omitempty" nullable:"true" doc:"argv lux runs in the attached Run's container. None: only the port is exposed."`
	Workdir     string            `json:"workdir,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	AfterSync   []string          `json:"afterSync,omitempty" doc:"argv run before the command when it starts after a repository sync."`
	Labels      map[string]string `json:"labels,omitempty"`
	Hostname    string            `json:"hostname,omitempty" doc:"Its unique preview host name: a full name under the preview domain (any number of labels), or one DNS label without a dot (1-63 characters), to which lux appends the domain. Default: <name>-<8 characters of its id>.<domain>."`
	Wake        string            `json:"wake,omitempty" enum:"request,never" doc:"request: a signed-in request while no running Run serves it asks its owner for one (server.wake_requested on GET /v1/events). Default never."`
	IdleAfter   *spec.Duration    `json:"idleAfter,omitempty" doc:"server.idle after this long without a request while its Run runs (default 10m; 0: never)."`
	WakeTimeout *spec.Duration    `json:"wakeTimeout,omitempty" doc:"How long a wake waits for a Run to make it ready before its page says no answer (default 5m)."`
	Lifetime    string            `json:"lifetime,omitempty" enum:"run,owner" doc:"owner (default without runId, and for every wakeable server): kept until deleted. run: deleted when its Run finishes for good."`
	ExpireAfter *spec.Duration    `json:"expireAfter,omitempty" doc:"lifetime owner: deleted (server.expired) after this long without a request. Default 30 days; 0: never."`
	RunID       string            `json:"runId,omitempty" doc:"Attach it to this Run at once."`
}

type createServerInput struct {
	Body CreateServerInput
}

type tenantServerOutput struct {
	Status int
	Body   TenantServer
}

func (s *Server) createServer(ctx context.Context, in *createServerInput) (*tenantServerOutput, error) {
	p := principal(ctx)
	b := in.Body
	wake := cmp.Or(b.Wake, WakeNever)
	if err := checkWake(wake); err != nil {
		return nil, err
	}
	lifetime := b.Lifetime
	if lifetime == "" {
		lifetime = LifetimeOwner
		if b.RunID != "" && wake == WakeNever {
			lifetime = LifetimeRun
		}
	}
	if err := checkLifetime(lifetime); err != nil {
		return nil, err
	}
	if wake == WakeRequest && lifetime != LifetimeOwner {
		return nil, errf(http.StatusUnprocessableEntity, "invalid_server", "a server that wakes on request has lifetime owner: it outlives the Runs that serve it")
	}
	if lifetime == LifetimeRun && b.RunID == "" {
		return nil, errf(http.StatusUnprocessableEntity, "invalid_server", "lifetime run needs a runId")
	}
	sv := spec.Server{Name: b.Name, Port: b.Port, Command: b.Command, Workdir: b.Workdir, Env: b.Env, AfterSync: b.AfterSync}
	if err := checkServer(spec.RunSpec{}, sv); err != nil {
		return nil, err
	}
	if err := checkLabels(b.Labels); err != nil {
		return nil, err
	}
	idle, err := durationOr("idleAfter", b.IdleAfter, DefaultIdleAfter, 0, maxIdleAfter)
	if err != nil {
		return nil, err
	}
	timeout, err := durationOr("wakeTimeout", b.WakeTimeout, DefaultWakeTimeout, minWakeTimeout, maxWakeTimeout)
	if err != nil {
		return nil, err
	}
	var expire *int
	if lifetime == LifetimeOwner {
		e, err := durationOr("expireAfter", b.ExpireAfter, DefaultExpireAfter, 0, maxExpireAfter)
		if err != nil {
			return nil, err
		}
		if e > 0 {
			expire = &e
		}
	}
	var host *string
	if b.Hostname != "" {
		h, err := s.checkHostname(b.Hostname)
		if err != nil {
			return nil, err
		}
		host = &h
	}
	id := ids.New(ids.Server)
	var out TenantServer
	// Created unattached; with a runId, attached in the same transaction.
	insert := func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO run_servers (id, tenant_id, run_id, name, host, port, command, workdir, env, after_sync, labels,
				wake, idle_after_s, wake_timeout_s, expire_after_s, lifetime, owner)
			VALUES ($1, $2, NULL, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)`,
			id, p.TenantID, b.Name, host, b.Port, nilIfEmpty(b.Command), b.Workdir, nonNilMap(b.Env), nilIfEmpty(b.AfterSync),
			nonNilMap(b.Labels), wake, idle, timeout, expire, lifetime, p.Actor())
		if err := uniqueViolation(err); err != nil {
			return err
		}
		v, err := serverByID(ctx, tx, id)
		if err != nil {
			return err
		}
		return serverEvent(ctx, tx, p.TenantID, v.RunID, id, 0, "server.created", v.ref(), map[string]any{"by": p.Actor(),
			"wake": wake, "lifetime": lifetime})
	}
	if b.RunID == "" {
		err = s.db.Tx(ctx, store.Tenant(p.TenantID), insert)
	} else {
		err = s.changeServers(ctx, p.TenantID, b.RunID, func(tx pgx.Tx) error {
			state, epoch, sp, err := serverRun(ctx, tx, b.RunID)
			if err != nil {
				return err
			}
			if err := checkServer(sp, sv); err != nil {
				return err
			}
			if err := insert(tx); err != nil {
				return err
			}
			return attachTx(ctx, tx, p, id, b.RunID, state, epoch)
		})
	}
	if err != nil {
		return nil, err
	}
	out, err = s.loadTenantServer(ctx, p.TenantID, id)
	if err != nil {
		return nil, err
	}
	return &tenantServerOutput{http.StatusCreated, out}, nil
}

// uniqueViolation turns a unique violation of run_servers into its 409.
func uniqueViolation(err error) error {
	var pe *pgconn.PgError
	if !errors.As(err, &pe) || pe.Code != "23505" {
		return err
	}
	switch pe.ConstraintName {
	case "run_servers_host":
		return errf(http.StatusConflict, "hostname_taken", "that hostname is taken")
	case "run_servers_run_name":
		return errf(http.StatusConflict, "name_taken", "the Run already has a server of that name")
	}
	return err
}

// errNoServerID is the not_found of a server id the tenant does not have.
func errNoServerID(id string) error {
	return errf(http.StatusNotFound, "not_found", "no server %s", id)
}

func serverByID(ctx context.Context, tx pgx.Tx, id string) (serverRow, error) {
	v, err := scanServerRow(tx.QueryRow(ctx, serverSelect+`WHERE sv.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return v, errNoServerID(id)
	}
	return v, err
}

// loadTenantServer reads a server as tenantID sees it ("": an operator,
// every tenant's).
func (s *Server) loadTenantServer(ctx context.Context, tenantID, id string) (TenantServer, error) {
	var out TenantServer
	err := s.db.Tx(ctx, Principal{TenantID: tenantID}.scope(), func(tx pgx.Tx) error {
		v, err := serverByID(ctx, tx, id)
		out = s.tenantServer(v, time.Now())
		return err
	})
	return out, err
}

// ---- attach / detach ----------------------------------------------------------

// attachTx attaches server id (unattached, tenant p's) to runID, locked
// (state, epoch): started now if the Run has a placement on a host
// (scheduled, starting or running: the shim starts it once init is done)
// and it has a command, else at the Run's next placement.
func attachTx(ctx context.Context, tx pgx.Tx, p Principal, id, runID, state string, epoch int) error {
	v, err := lockServer(ctx, tx, p.TenantID, id)
	if err != nil {
		return err
	}
	if v.RunID != nil {
		if *v.RunID == runID {
			return nil
		}
		return errf(http.StatusConflict, "attached", "server %s is attached to %s: detach it there first", id, *v.RunID)
	}
	// A new gen: the process state is this Run's from now on.
	_, err = tx.Exec(ctx, `UPDATE run_servers SET run_id = $2, state = 'stopped', stop_reason = NULL, stopped_epoch = NULL, epoch = NULL,
			active = NULL, exit_code = NULL, error = NULL, ready_since = NULL, since = now(), gen = nextval('run_servers_gen'), updated_at = now()
		WHERE id = $1`, id, runID)
	if err := uniqueViolation(err); err != nil {
		return err
	}
	if err := serverEvent(ctx, tx, p.TenantID, &runID, id, 0, "server.attached", v.ref(), map[string]any{"by": p.Actor(), "runState": state}); err != nil {
		return err
	}
	placed := state == StateScheduled || state == StateStarting || state == StateRunning
	if placed && len(v.Command) > 0 && !v.down() {
		return setServerState(ctx, tx, stateChange{tenantID: p.TenantID, runID: runID, epoch: epoch, state: ServerStarting}, `rs.id = $6`, id)
	}
	return nil
}

// lockServer locks a tenant's server for a change. Under a system scope
// (changeServers), so the tenant is checked here.
func lockServer(ctx context.Context, tx pgx.Tx, tenantID, id string) (serverRow, error) {
	v, err := scanServerRow(tx.QueryRow(ctx, serverSelect+`WHERE sv.id = $1 AND sv.tenant_id = $2 FOR UPDATE OF sv`, id, tenantID))
	if errors.Is(err, pgx.ErrNoRows) {
		return v, errNoServerID(id)
	}
	return v, err
}

// detachTx detaches a server from its Run: its process stops (its Run
// untouched), and it is no longer the Run's.
func detachTx(ctx context.Context, tx pgx.Tx, tenantID string, v serverRow, why string, by string) error {
	if v.RunID == nil {
		return nil
	}
	runID := *v.RunID
	var epoch int
	if err := tx.QueryRow(ctx, `SELECT current_epoch FROM runs WHERE id = $1`, runID).Scan(&epoch); err != nil {
		return err
	}
	if v.State != ServerStopped {
		if err := setServerState(ctx, tx, stateChange{tenantID: tenantID, runID: runID, epoch: epoch, state: ServerStopped, stopReason: "detached"}, `rs.id = $6`, v.ID); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE run_servers SET run_id = NULL, active = NULL, epoch = NULL, wake_requested_at = NULL,
			stop_reason = CASE WHEN stop_reason = 'stopped' THEN 'stopped' ELSE 'detached' END, updated_at = now()
		WHERE id = $1`, v.ID); err != nil {
		return err
	}
	return serverEvent(ctx, tx, tenantID, nil, v.ID, 0, "server.detached", v.ref(), map[string]any{"from": runID, "reason": why, "by": by})
}

type ServerIDPath struct {
	ID string `path:"id" doc:"The server's id (srv_…)."`
}

type attachInput struct {
	ServerIDPath
	Body struct {
		RunID string `json:"runId" doc:"The Run to attach it to: running (its command starts now) or not (it starts at the Run's next placement)."`
	}
}

func (s *Server) attachServer(ctx context.Context, in *attachInput) (*tenantServerOutput, error) {
	p := principal(ctx)
	if in.Body.RunID == "" {
		return nil, errf(http.StatusUnprocessableEntity, "invalid_request", "runId is required")
	}
	err := s.changeServers(ctx, p.TenantID, in.Body.RunID, func(tx pgx.Tx) error {
		state, epoch, sp, err := serverRun(ctx, tx, in.Body.RunID)
		if err != nil {
			return err
		}
		v, err := lockServer(ctx, tx, p.TenantID, in.ID)
		if err != nil {
			return err
		}
		if err := checkServer(sp, spec.Server{Name: v.Name, Port: v.Port, Command: v.Command, Workdir: v.Workdir, Env: v.Env, AfterSync: v.AfterSync}); err != nil {
			return err
		}
		return attachTx(ctx, tx, p, in.ID, in.Body.RunID, state, epoch)
	})
	if err != nil {
		return nil, err
	}
	out, err := s.loadTenantServer(ctx, p.TenantID, in.ID)
	return &tenantServerOutput{http.StatusOK, out}, err
}

// changeServer runs fn on a tenant's server, locked, in a system scope,
// and sends its Run's placement (before and after: a detach) the new set.
func (s *Server) changeServer(ctx context.Context, tenantID, id string, fn func(pgx.Tx, serverRow) error) error {
	var hosts []string
	// Locks go Run first, then server, as a runner's report takes them; the
	// Run is read before either is locked, so a server attached or detached
	// in between is tried once more, then refused.
	var err error
	for try := 0; try < 2; try++ {
		hosts = nil
		err = s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
			var before *string
			if err := tx.QueryRow(ctx, `SELECT run_id FROM run_servers WHERE id = $1 AND tenant_id = $2`, id, tenantID).Scan(&before); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return errNoServerID(id)
				}
				return err
			}
			if before != nil {
				if _, _, _, err := serverRunAny(ctx, tx, *before); err != nil {
					return err
				}
			}
			v, err := lockServer(ctx, tx, tenantID, id)
			if err != nil {
				return err
			}
			if strOf(v.RunID) != strOf(before) {
				return errServerMoved
			}
			hosts, err = changeLockedServer(ctx, tx, v, fn)
			return err
		})
		if !errors.Is(err, errServerMoved) {
			break
		}
	}
	if errors.Is(err, errServerMoved) {
		err = errf(http.StatusConflict, "conflict", "server %s was attached or detached meanwhile: try again", id)
	}
	for _, h := range hosts {
		s.hub.Notify(h)
	}
	return err
}

// errServerMoved: the server's Run changed between reading it and locking.
var errServerMoved = errors.New("server attached or detached meanwhile")

// changeLockedServer runs fn on a server locked with its Run, and returns
// the hosts to notify of the new sets.
func changeLockedServer(ctx context.Context, tx pgx.Tx, v serverRow, fn func(pgx.Tx, serverRow) error) ([]string, error) {
	if err := fn(tx, v); err != nil {
		return nil, err
	}
	var runs []string
	if v.RunID != nil {
		runs = append(runs, *v.RunID)
	}
	var now *string
	if err := tx.QueryRow(ctx, `SELECT run_id FROM run_servers WHERE id = $1`, v.ID).Scan(&now); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	if now != nil && !slices.Contains(runs, *now) {
		runs = append(runs, *now)
	}
	return syncServerSets(ctx, tx, runs...)
}

// syncServerSets sends each Run's placement its server set, and returns
// the hosts to notify.
func syncServerSets(ctx context.Context, tx pgx.Tx, runIDs ...string) ([]string, error) {
	var hosts []string
	for _, r := range runIDs {
		h, err := syncServersTx(ctx, tx, r)
		if err != nil {
			return nil, err
		}
		if h != "" {
			hosts = append(hosts, h)
		}
	}
	return hosts, nil
}

// serverRunAny locks a Run, finished or not.
func serverRunAny(ctx context.Context, tx pgx.Tx, runID string) (state string, epoch int, sp spec.RunSpec, err error) {
	err = tx.QueryRow(ctx, `SELECT state, current_epoch, spec FROM runs WHERE id = $1 FOR UPDATE`, runID).Scan(&state, &epoch, &sp)
	return state, epoch, sp, err
}

func (s *Server) detachServer(ctx context.Context, in *ServerIDPath) (*tenantServerOutput, error) {
	p := principal(ctx)
	err := s.changeServer(ctx, p.TenantID, in.ID, func(tx pgx.Tx, v serverRow) error {
		if v.Lifetime == LifetimeRun {
			return errf(http.StatusConflict, "lifetime_run", "server %s ends with its Run (lifetime run): delete it, or PATCH lifetime owner first", in.ID)
		}
		return detachTx(ctx, tx, p.TenantID, v, "detached", p.Actor())
	})
	if err != nil {
		return nil, err
	}
	out, err := s.loadTenantServer(ctx, p.TenantID, in.ID)
	return &tenantServerOutput{http.StatusOK, out}, err
}

// ---- start / stop / restart ---------------------------------------------------

func (s *Server) tenantServerAction(action string) func(context.Context, *ServerIDPath) (*tenantServerOutput, error) {
	return func(ctx context.Context, in *ServerIDPath) (*tenantServerOutput, error) {
		p := principal(ctx)
		err := s.changeServer(ctx, p.TenantID, in.ID, func(tx pgx.Tx, v serverRow) error {
			if v.RunID == nil {
				if action == "stop" {
					return nil
				}
				return errf(http.StatusConflict, "not_attached", "server %s is attached to no Run", in.ID)
			}
			var state string
			var epoch int
			if err := tx.QueryRow(ctx, `SELECT state, current_epoch FROM runs WHERE id = $1`, *v.RunID).Scan(&state, &epoch); err != nil {
				return err
			}
			if action != "stop" && len(v.Command) > 0 && state != StateRunning {
				// Up again: started at the Run's next placement.
				if v.down() {
					_, err := tx.Exec(ctx, `UPDATE run_servers SET stop_reason = NULL, updated_at = now() WHERE id = $1`, v.ID)
					return err
				}
				return nil
			}
			return serverActionTx(ctx, tx, p.TenantID, *v.RunID, state, epoch, s.runServer(v), action)
		})
		if err != nil {
			return nil, err
		}
		out, err := s.loadTenantServer(ctx, p.TenantID, in.ID)
		return &tenantServerOutput{http.StatusOK, out}, err
	}
}

// ---- get / list ----------------------------------------------------------------

type getTenantServerInput struct {
	ServerIDPath
}

func (s *Server) getTenantServer(ctx context.Context, in *getTenantServerInput) (*tenantServerOutput, error) {
	out, err := s.loadTenantServer(ctx, principal(ctx).TenantID, in.ID)
	if err != nil {
		return nil, err
	}
	return &tenantServerOutput{http.StatusOK, out}, nil
}

type listTenantServersInput struct {
	TenantQuery
	State    string   `query:"state" doc:"Only servers in these states (comma-separated): ready, waking, asleep, stopped, unreachable, exited, no answer."`
	Label    []string `query:"label,explode" doc:"Only servers with this label (key=value); repeat to require several."`
	Wake     string   `query:"wake" enum:"request,never," doc:"Only servers that wake so."`
	Run      string   `query:"run" doc:"Only servers attached to this Run."`
	Hostname string   `query:"hostname" doc:"Only the server of this hostname: a full name under the preview domain or one relative DNS label."`
	Limit    int      `query:"limit" doc:"At most this many, newest first, 1 to 1000 (default 500)."`
}

// Resolve reads every label as given.
func (in *listTenantServersInput) Resolve(ctx huma.Context) []error {
	u := ctx.URL()
	in.Label = u.Query()["label"]
	return nil
}

type tenantServerListOutput struct {
	Body struct {
		Servers []TenantServer `json:"servers"`
		Counts  map[string]int `json:"counts" doc:"How many of the listed (before the state filter) are in each state."`
	} `nameHint:"TenantServerList"`
}

func (s *Server) listTenantServers(ctx context.Context, in *listTenantServersInput) (*tenantServerListOutput, error) {
	p := principal(ctx)
	where := []string{"true"}
	args := []any{}
	arg := func(v any) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}
	// RLS enforces the tenant too, but its OR with lux_system() is no index
	// condition: this lets run_servers_tenant and PK joins serve the list.
	if p.TenantID != "" {
		where = append(where, "sv.tenant_id = "+arg(p.TenantID))
	}
	for _, l := range in.Label {
		k, v, _ := strings.Cut(l, "=")
		where = append(where, "sv.labels @> "+arg(map[string]string{k: v}))
	}
	if in.Wake != "" {
		where = append(where, "sv.wake = "+arg(in.Wake))
	}
	if in.Run != "" {
		where = append(where, "sv.run_id = "+arg(in.Run))
	}
	if in.Hostname != "" {
		host := strings.ToLower(strings.TrimSuffix(in.Hostname, "."))
		if d := s.cfg.Preview.Domain; d != "" {
			host = strings.TrimSuffix(host, "."+strings.ToLower(d))
		}
		where = append(where, "sv.host = "+arg(host))
	}
	limit := in.Limit
	if limit <= 0 || limit > 1000 {
		limit = 500
	}
	var states []string
	if in.State != "" {
		states = strings.Split(in.State, ",")
	}
	out := &tenantServerListOutput{}
	out.Body.Servers = []TenantServer{}
	out.Body.Counts = map[string]int{}
	err := s.db.Tx(ctx, p.scope(), func(tx pgx.Tx) error {
		rows, err := collectServerRows(tx.Query(ctx, serverSelect+`WHERE `+strings.Join(where, " AND ")+
			` ORDER BY sv.created_at DESC, sv.id LIMIT `+arg(limit), args...))
		if err != nil {
			return err
		}
		now := time.Now()
		for _, v := range rows {
			t := s.tenantServer(v, now)
			out.Body.Counts[t.State]++
			if states == nil || slices.Contains(states, t.State) {
				out.Body.Servers = append(out.Body.Servers, t)
			}
		}
		return nil
	})
	return out, err
}

// ---- update / delete ------------------------------------------------------------

// PatchServerInput changes a server: what is given. Its command, port,
// workdir, env and afterSync apply at its next start.
type PatchServerInput struct {
	Port        *int               `json:"port,omitempty"`
	Command     *[]string          `json:"command,omitempty" doc:"[] removes it (a port only)."`
	Workdir     *string            `json:"workdir,omitempty"`
	Env         *map[string]string `json:"env,omitempty"`
	AfterSync   *[]string          `json:"afterSync,omitempty" doc:"[] removes it."`
	Labels      *map[string]string `json:"labels,omitempty" doc:"Replaces them."`
	Wake        *string            `json:"wake,omitempty" enum:"request,never"`
	IdleAfter   *spec.Duration     `json:"idleAfter,omitempty"`
	WakeTimeout *spec.Duration     `json:"wakeTimeout,omitempty"`
	Lifetime    *string            `json:"lifetime,omitempty" enum:"run,owner"`
	ExpireAfter *spec.Duration     `json:"expireAfter,omitempty" doc:"0: never."`
}

type patchServerInput struct {
	ServerIDPath
	Body PatchServerInput
}

func (s *Server) patchServer(ctx context.Context, in *patchServerInput) (*tenantServerOutput, error) {
	p := principal(ctx)
	b := in.Body
	err := s.changeServer(ctx, p.TenantID, in.ID, func(tx pgx.Tx, v serverRow) error {
		sv := spec.Server{Name: v.Name, Port: v.Port, Command: v.Command, Workdir: v.Workdir, Env: v.Env, AfterSync: v.AfterSync}
		labels, wake, lifetime := v.Labels, v.Wake, v.Lifetime
		idle, timeout, expire := v.IdleAfterS, v.WakeTimeoutS, v.ExpireAfterS
		changed := []string{}
		if b.Port != nil {
			sv.Port, changed = *b.Port, append(changed, "port")
		}
		if b.Command != nil {
			sv.Command, changed = nilIfEmpty(*b.Command), append(changed, "command")
		}
		if b.Workdir != nil {
			sv.Workdir, changed = *b.Workdir, append(changed, "workdir")
		}
		if b.Env != nil {
			sv.Env, changed = *b.Env, append(changed, "env")
		}
		if b.AfterSync != nil {
			sv.AfterSync, changed = nilIfEmpty(*b.AfterSync), append(changed, "afterSync")
		}
		if b.Labels != nil {
			if err := checkLabels(*b.Labels); err != nil {
				return err
			}
			labels, changed = *b.Labels, append(changed, "labels")
		}
		if b.Wake != nil {
			if err := checkWake(*b.Wake); err != nil {
				return err
			}
			wake, changed = *b.Wake, append(changed, "wake")
		}
		if b.Lifetime != nil {
			if err := checkLifetime(*b.Lifetime); err != nil {
				return err
			}
			lifetime, changed = *b.Lifetime, append(changed, "lifetime")
		}
		if wake == WakeRequest && lifetime != LifetimeOwner {
			return errf(http.StatusUnprocessableEntity, "invalid_server", "a server that wakes on request has lifetime owner")
		}
		if lifetime == LifetimeRun && v.RunID == nil {
			return errf(http.StatusUnprocessableEntity, "invalid_server", "lifetime run needs an attached Run")
		}
		var err error
		if b.IdleAfter != nil {
			if idle, err = durationOr("idleAfter", b.IdleAfter, 0, 0, maxIdleAfter); err != nil {
				return err
			}
			changed = append(changed, "idleAfter")
		}
		if b.WakeTimeout != nil {
			if timeout, err = durationOr("wakeTimeout", b.WakeTimeout, 0, minWakeTimeout, maxWakeTimeout); err != nil {
				return err
			}
			changed = append(changed, "wakeTimeout")
		}
		if b.ExpireAfter != nil {
			e, err := durationOr("expireAfter", b.ExpireAfter, 0, 0, maxExpireAfter)
			if err != nil {
				return err
			}
			expire = nil
			if e > 0 {
				expire = &e
			}
			changed = append(changed, "expireAfter")
		}
		var sp spec.RunSpec
		if v.RunID != nil {
			if err := tx.QueryRow(ctx, `SELECT spec FROM runs WHERE id = $1`, *v.RunID).Scan(&sp); err != nil {
				return err
			}
		}
		if err := checkServer(sp, sv); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE run_servers SET port = $2, command = $3, workdir = $4, env = $5, after_sync = $6, labels = $7,
				wake = $8, idle_after_s = $9, wake_timeout_s = $10, expire_after_s = $11, lifetime = $12, updated_at = now(),
				wake_requested_at = CASE WHEN $8 = 'never' THEN NULL ELSE wake_requested_at END
			WHERE id = $1`, v.ID, sv.Port, nilIfEmpty(sv.Command), sv.Workdir, nonNilMap(sv.Env), nilIfEmpty(sv.AfterSync), nonNilMap(labels),
			wake, idle, timeout, expire, lifetime); err != nil {
			return err
		}
		v.Labels = labels
		return serverEvent(ctx, tx, p.TenantID, v.RunID, v.ID, 0, "server.updated", v.ref(), map[string]any{"by": p.Actor(), "changed": changed})
	})
	if err != nil {
		return nil, err
	}
	out, err := s.loadTenantServer(ctx, p.TenantID, in.ID)
	return &tenantServerOutput{http.StatusOK, out}, err
}

func (s *Server) deleteServer(ctx context.Context, in *ServerIDPath) (*noContent, error) {
	p := principal(ctx)
	err := s.changeServer(ctx, p.TenantID, in.ID, func(tx pgx.Tx, v serverRow) error {
		return deleteServerTx(ctx, tx, p.TenantID, v, "deleted", p.Actor(), "server.deleted")
	})
	if err != nil {
		return nil, err
	}
	return &noContent{http.StatusNoContent}, nil
}

// deleteServerTx detaches a server (its command stops) and deletes it; its
// hostname is unknown from then on.
func deleteServerTx(ctx context.Context, tx pgx.Tx, tenantID string, v serverRow, why, by, typ string) error {
	if err := detachTx(ctx, tx, tenantID, v, why, by); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM run_servers WHERE id = $1`, v.ID); err != nil {
		return err
	}
	return serverEvent(ctx, tx, tenantID, nil, v.ID, 0, typ, v.ref(), map[string]any{"reason": why, "by": by})
}

// ---- events and log --------------------------------------------------------------

type serverEventsInput struct {
	ServerIDPath
	After string `query:"after" doc:"Only events after this id (up to 1000 at a time)."`
}

func (s *Server) serverEvents(ctx context.Context, in *serverEventsInput) (*listEventsOutput, error) {
	p := principal(ctx)
	after, _ := strconv.ParseInt(in.After, 10, 64)
	out := &listEventsOutput{}
	out.Body.Events = []Event{}
	err := s.db.Tx(ctx, p.scope(), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id, coalesce(server_id, ''), epoch, type, data, created_at FROM run_events
			WHERE server_id = $1 AND id > $2 ORDER BY id LIMIT 1000`, in.ID, after)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e Event
			if err := rows.Scan(&e.ID, &e.ServerID, &e.Epoch, &e.Type, &e.Data, &e.Time); err != nil {
				return err
			}
			s.eventDetail(&e)
			out.Body.Events = append(out.Body.Events, e)
		}
		return rows.Err()
	})
	return out, err
}

type tenantServerLogInput struct {
	ServerIDPath
	Tail int `query:"tail" doc:"The last this many lines, 1 to 5000 (default 200)."`
}

func (s *Server) tenantServerLog(ctx context.Context, in *tenantServerLogInput) (*serverLogOutput, error) {
	p := principal(ctx)
	v, err := s.loadTenantServer(ctx, p.TenantID, in.ID)
	if err != nil {
		return nil, err
	}
	if v.RunID == nil {
		out := &serverLogOutput{}
		out.Body.Lines = []ServerLogLine{}
		return out, nil
	}
	return s.serverLogOf(ctx, p.TenantID, *v.RunID, v.Name, in.Tail)
}

// ---- routes ------------------------------------------------------------------------

// movedConflict documents changeServer's errServerMoved answer.
const movedConflict = "409 conflict when the server was attached or detached twice while this ran: retry."

func (s *Server) serverRoutes(api huma.API) {
	register(s, api, huma.Operation{
		OperationID: "createServer", Method: http.MethodPost, Path: "/v1/servers", Tags: []string{"servers"},
		Summary: "Create a server",
		Description: "A named URL that reaches a port in a Run, independent of Runs: attach it to one (runId, or POST .../attach), " +
			"detach it, and it outlives them (lifetime owner). With wake request, a signed-in request while no running Run serves it " +
			"emits server.wake_requested on GET /v1/events for its owner, once per wake; lux never starts a Run itself.",
		DefaultStatus: http.StatusCreated,
		Errors:        []int{http.StatusBadRequest, http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity},
	}, "run", forTenant(s.createServer))
	register(s, api, huma.Operation{
		OperationID: "listTenantServers", Method: http.MethodGet, Path: "/v1/servers", Tags: []string{"servers"},
		Summary: "List the tenant's servers", Description: "Newest first, with their derived state.",
	}, "read", s.listTenantServers)
	register(s, api, huma.Operation{
		OperationID: "getTenantServer", Method: http.MethodGet, Path: "/v1/servers/{id}", Tags: []string{"servers"},
		Summary: "A server", Errors: []int{http.StatusNotFound},
	}, "read", s.getTenantServer)
	register(s, api, huma.Operation{
		OperationID: "patchServer", Method: http.MethodPatch, Path: "/v1/servers/{id}", Tags: []string{"servers"},
		Summary: "Change a server", Description: "What is given. Command, port, workdir, env and afterSync apply at its next start. " + movedConflict,
		Errors: []int{http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity},
	}, "run", s.patchServer)
	register(s, api, huma.Operation{
		OperationID: "deleteServer", Method: http.MethodDelete, Path: "/v1/servers/{id}", Tags: []string{"servers"},
		Summary: "Delete a server", Description: "Detaches it first (its command stops; its Run is untouched). Its hostname is gone from then on. " + movedConflict,
		DefaultStatus: http.StatusNoContent, Errors: []int{http.StatusNotFound, http.StatusConflict},
	}, "run", s.deleteServer)
	register(s, api, huma.Operation{
		OperationID: "attachServer", Method: http.MethodPost, Path: "/v1/servers/{id}/attach", Tags: []string{"servers"},
		Summary: "Attach a server to a Run",
		Description: "To a running Run: its command starts now. To a stopped one: at its next placement. 409 attached when another Run serves it. " +
			"Its name must be free among the Run's servers (409 name_taken).",
		Errors: []int{http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity},
	}, "run", s.attachServer)
	register(s, api, huma.Operation{
		OperationID: "detachServer", Method: http.MethodPost, Path: "/v1/servers/{id}/detach", Tags: []string{"servers"},
		Summary: "Detach a server from its Run", Description: "Its command stops; the Run is untouched. 409 lifetime_run for a server that ends with its Run. " + movedConflict,
		Errors: []int{http.StatusNotFound, http.StatusConflict},
	}, "run", s.detachServer)
	for _, a := range []struct{ action, summary, doc string }{
		{"start", "Start a server", "Up: started now if its Run runs, else at its next placement. 409 not_attached, no_command."},
		{"stop", "Stop a server", "Down: its command stops, and new placements leave it stopped until it is started again."},
		{"restart", "Restart a server", "As it is defined now. Not running: as start."},
	} {
		register(s, api, huma.Operation{
			OperationID: a.action + "TenantServer", Method: http.MethodPost, Path: "/v1/servers/{id}/" + a.action, Tags: []string{"servers"},
			Summary: a.summary, Description: a.doc + " " + movedConflict,
			Errors: []int{http.StatusNotFound, http.StatusConflict},
		}, "run", s.tenantServerAction(a.action))
	}
	register(s, api, huma.Operation{
		OperationID: "mintServerTicket", Method: http.MethodPost, Path: "/v1/servers/{id}/tickets", Tags: []string{"servers"},
		Summary: "Mint a preview ticket for a server",
		Description: "For its preview's sign-in: `<url>/.lux/auth?ticket=...&to=/path` sets the preview cookie and goes on to the path. " +
			"Single use, 60 seconds.",
		DefaultStatus: http.StatusCreated,
		Errors:        []int{http.StatusNotFound, http.StatusConflict},
	}, "read", s.mintServerTicket)
	register(s, api, huma.Operation{
		OperationID: "serverEvents", Method: http.MethodGet, Path: "/v1/servers/{id}/events", Tags: []string{"servers"},
		Summary: "A server's events", Description: "server.* events, oldest first; the feed (GET /v1/events) carries them too. " +
			"A deleted server's events are kept and listed here (ending with server.deleted or server.expired); an id with no events of this tenant's is an empty list, not 404.",
	}, "read", s.serverEvents)
	register(s, api, huma.Operation{
		OperationID: "tenantServerLog", Method: http.MethodGet, Path: "/v1/servers/{id}/log", Tags: []string{"servers"},
		Summary: "A server's recent output", Description: "From its attached Run, across placements; empty when attached to none.",
		Errors: []int{http.StatusNotFound},
	}, "read", s.tenantServerLog)
}

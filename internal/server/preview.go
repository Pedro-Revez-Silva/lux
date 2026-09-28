package server

import (
	"bytes"
	"cmp"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/spec"
	"github.com/marcioapm/lux/internal/store"
)

// The preview listener: https://<server>-<run suffix>.<domain> reaches a
// Run's server, wherever the Run is now. It has its own address and only
// ever proxies: nothing of luxd's own (/v1, /runner, the console) is
// served on it.
//
// A request is authenticated (Cloudflare Access, or a ticket turned into a
// cookie), then routed by the server's state: a ready server of a running
// Run is reverse-proxied over a tunnel stream through the hub to the
// current placement; a starting one is waited for (hold_for); anything
// else gets a small status page that refreshes itself.

const (
	previewCookie    = "__Host-lux_preview"
	previewCookieTTL = 12 * time.Hour
	previewAuthPath  = "/.lux/auth"
	// activityEvery bounds how often a server's lastRequestAt is written.
	activityEvery = 30 * time.Second
)

type previews struct {
	s    *Server
	mode string // cloudflare-access | ticket
	cf   *cfAccess
	key  []byte // the cookie's HMAC key (luxd_keys), shared by every luxd
	rp   *httputil.ReverseProxy

	mu       sync.Mutex
	pending  map[serverRef]time.Time // requests not yet written
	written  map[serverRef]time.Time // when each was last written
	keysLive map[string]keyCheck     // api keys a cookie names: still live?
}

type serverRef struct{ runID, name string }

type keyCheck struct {
	live  bool
	until time.Time
}

func newPreviews(s *Server) *previews {
	p := &previews{s: s, mode: s.cfg.Preview.Auth, pending: map[serverRef]time.Time{}, written: map[serverRef]time.Time{},
		keysLive: map[string]keyCheck{}}
	if p.mode == "" {
		p.mode = "ticket"
		if s.cfg.ConsoleAuth.Mode == "cloudflare-access" {
			p.mode = "cloudflare-access"
		}
	}
	if p.mode == "cloudflare-access" {
		p.cf = newCFAccess(s.cfg.ConsoleAuth.CFTeam, s.cfg.Preview.CFAud)
	}
	p.rp = &httputil.ReverseProxy{
		Rewrite:        p.rewrite,
		Transport:      p.transport(),
		ModifyResponse: stripCookieDomains,
		ErrorHandler:   p.proxyError,
		// Server-sent events and the like flush as they come.
		FlushInterval: -1,
	}
	return p
}

// init loads (or makes) the cookie key.
func (p *previews) init(ctx context.Context) error {
	if p.mode == "cloudflare-access" && p.s.cfg.Preview.CFAud == "" {
		return errors.New("preview.auth cloudflare-access needs preview.cloudflare_access.aud (LUX_PREVIEW_CF_ACCESS_AUD)")
	}
	k, err := p.s.luxdKey(ctx, "preview-cookie")
	p.key = k
	return err
}

// luxdKey is a key luxd keeps for itself (luxd_keys), made on first use:
// every luxd of one database shares it.
func (s *Server) luxdKey(ctx context.Context, name string) ([]byte, error) {
	fresh := make([]byte, 32)
	if _, err := rand.Read(fresh); err != nil {
		return nil, err
	}
	var key []byte
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO luxd_keys (name, key) VALUES ($1, $2) ON CONFLICT (name) DO NOTHING`, name, fresh); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT key FROM luxd_keys WHERE name = $1`, name).Scan(&key)
	})
	return key, err
}

// ---- host names ------------------------------------------------------------

var runSuffixRe = regexp.MustCompile(`^[a-z2-7]{16}$`)

// parsePreviewHost splits <server>-<run suffix>.<domain> (at the last -)
// into the server's name and the Run's id.
func parsePreviewHost(host, domain string) (name, runID string, ok bool) {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	label, found := strings.CutSuffix(host, "."+strings.ToLower(domain))
	if !found || strings.Contains(label, ".") {
		return "", "", false
	}
	i := strings.LastIndexByte(label, '-')
	if i < 0 {
		return "", "", false
	}
	name, suffix := label[:i], label[i+1:]
	if !spec.ValidServerName(name) || !runSuffixRe.MatchString(suffix) {
		return "", "", false
	}
	return name, "run_" + suffix, true
}

// ---- the cookie ------------------------------------------------------------

// previewUser is who a preview cookie (or an Access token) is for.
type previewUser struct {
	RunID    string `json:"r"`
	TenantID string `json:"t,omitempty"`
	Operator bool   `json:"o,omitempty"`
	KeyID    string `json:"k,omitempty"`
	User     string `json:"u"` // an email, or a key's name
	Exp      int64  `json:"e"`
}

func (p *previews) sign(u previewUser) string {
	b, _ := json.Marshal(u)
	payload := base64.RawURLEncoding.EncodeToString(b)
	m := hmac.New(sha256.New, p.key)
	m.Write([]byte(payload))
	return payload + "." + base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// verify reads a cookie's value: signed by this deployment, not expired.
func (p *previews) verify(v string, now time.Time) (previewUser, bool) {
	var u previewUser
	payload, sig, ok := strings.Cut(v, ".")
	if !ok {
		return u, false
	}
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil {
		return u, false
	}
	m := hmac.New(sha256.New, p.key)
	m.Write([]byte(payload))
	if !hmac.Equal(got, m.Sum(nil)) {
		return u, false
	}
	b, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil || json.Unmarshal(b, &u) != nil || now.Unix() >= u.Exp {
		return u, false
	}
	return u, true
}

// keyLive: a cookie made from an API key lasts only as long as the key.
// Checked at most once a minute per key.
func (p *previews) keyLive(ctx context.Context, keyID string) bool {
	p.mu.Lock()
	c, ok := p.keysLive[keyID]
	p.mu.Unlock()
	if ok && time.Now().Before(c.until) {
		return c.live
	}
	var live bool
	err := p.s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM api_keys WHERE id = $1 AND revoked_at IS NULL)`, keyID).Scan(&live)
	})
	if err != nil {
		return false
	}
	p.mu.Lock()
	p.keysLive[keyID] = keyCheck{live, time.Now().Add(time.Minute)}
	p.mu.Unlock()
	return live
}

// ---- serving ---------------------------------------------------------------

func (p *previews) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name, runID, ok := parsePreviewHost(r.Host, p.s.cfg.Preview.Domain)
	if !ok {
		p.page(w, http.StatusNotFound, pageUnknown, nil)
		return
	}
	if p.mode == "ticket" && r.URL.Path == previewAuthPath {
		p.signIn(w, r, runID)
		return
	}
	user, ok := p.authenticate(r, runID)
	if !ok {
		p.challenge(w, r)
		return
	}
	t, err := p.route(r.Context(), w, name, runID)
	if err != nil || t == nil {
		return
	}
	p.touch(serverRef{runID, name})
	ctx := context.WithValue(r.Context(), previewTargetKey{}, previewTarget{runID: runID, name: name, user: user.User, t: *t})
	p.rp.ServeHTTP(w, r.WithContext(ctx))
}

// authenticate finds the request's user, allowed to read runID.
func (p *previews) authenticate(r *http.Request, runID string) (previewUser, bool) {
	if p.mode == "cloudflare-access" {
		tok := r.Header.Get("Cf-Access-Jwt-Assertion")
		if tok == "" {
			if c, err := r.Cookie("CF_Authorization"); err == nil {
				tok = c.Value
			}
		}
		if tok == "" {
			return previewUser{}, false
		}
		email, id, err := p.cf.user(r.Context(), tok)
		if err != nil {
			return previewUser{}, false
		}
		pr, err := p.s.accessPrincipal(r.Context(), email, id, "read")
		if err != nil {
			return previewUser{}, false
		}
		u := previewUser{RunID: runID, TenantID: pr.TenantID, Operator: pr.Operator, User: email}
		return u, p.mayRead(r.Context(), u)
	}
	for _, c := range r.CookiesNamed(previewCookie) {
		if u, ok := p.verify(c.Value, time.Now()); ok && u.RunID == runID && (u.KeyID == "" || p.keyLive(r.Context(), u.KeyID)) {
			return u, true
		}
	}
	return previewUser{}, false
}

// mayRead: an operator, or someone of the Run's tenant.
func (p *previews) mayRead(ctx context.Context, u previewUser) bool {
	if u.Operator {
		return true
	}
	var tenant string
	err := p.s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT tenant_id FROM runs WHERE id = $1`, u.RunID).Scan(&tenant)
	})
	return err == nil && tenant == u.TenantID
}

// challenge answers a request without a user: a browser asking for a page
// goes to sign in (ticket mode); anything else gets 401.
func (p *previews) challenge(w http.ResponseWriter, r *http.Request) {
	if p.mode == "ticket" && r.Method == http.MethodGet && strings.Contains(r.Header.Get("Accept"), "text/html") {
		to := "https://" + r.Host + r.URL.RequestURI()
		http.Redirect(w, r, strings.TrimRight(p.s.cfg.PublicURL, "/")+"/preview-auth?to="+url.QueryEscape(to), http.StatusFound)
		return
	}
	p.page(w, http.StatusUnauthorized, pageSignIn, nil)
}

// signIn is /.lux/auth?ticket=…&to=/path: a preview ticket for this host's
// Run becomes the cookie, and the browser goes on to the path.
func (p *previews) signIn(w http.ResponseWriter, r *http.Request, runID string) {
	to := r.URL.Query().Get("to")
	if to == "" {
		to = "/"
	}
	if !localPath(to) {
		http.Error(w, "to must be a path", http.StatusBadRequest)
		return
	}
	pr, err := p.s.redeemTicket(r.Context(), r.URL.Query().Get("ticket"), runID, TicketPreview)
	if err != nil {
		p.page(w, http.StatusUnauthorized, pageSignIn, nil)
		return
	}
	if !pr.Can("read") {
		p.page(w, http.StatusUnauthorized, pageSignIn, nil)
		return
	}
	u := previewUser{RunID: runID, TenantID: pr.TenantID, Operator: pr.Operator, KeyID: pr.KeyID, User: pr.Email,
		Exp: time.Now().Add(previewCookieTTL).Unix()}
	if u.User == "" && pr.KeyID != "" {
		_ = p.s.db.Tx(r.Context(), store.System(), func(tx pgx.Tx) error {
			return tx.QueryRow(r.Context(), `SELECT name FROM api_keys WHERE id = $1`, pr.KeyID).Scan(&u.User)
		})
		u.User = cmp.Or(u.User, pr.KeyID)
	}
	http.SetCookie(w, &http.Cookie{Name: previewCookie, Value: p.sign(u), Path: "/", MaxAge: int(previewCookieTTL.Seconds()),
		Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, to, http.StatusFound)
}

// localPath: a path on this host, never another (//evil, /\evil).
func localPath(to string) bool {
	if !strings.HasPrefix(to, "/") || strings.HasPrefix(to, "//") || strings.HasPrefix(to, "/\\") || strings.ContainsAny(to, "\r\n") {
		return false
	}
	u, err := url.Parse(to)
	return err == nil && u.Scheme == "" && u.Host == ""
}

// previewState is what routing a request needs to know.
type previewState struct {
	found       bool
	runState    string
	moving      bool // the Run is being moved (a migration, a drain)
	serverState string
	stopReason  string
	exitCode    *int
	errText     string
}

func (p *previews) state(ctx context.Context, runID, name string) (previewState, error) {
	var st previewState
	err := p.s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		var placementStop string
		err := tx.QueryRow(ctx, `SELECT r.state, coalesce(p.stop_reason, ''), rs.state, coalesce(rs.stop_reason, ''), rs.exit_code, coalesce(rs.error, '')
			FROM runs r JOIN run_servers rs ON rs.run_id = r.id AND rs.name = $2
			LEFT JOIN placements p ON p.run_id = r.id AND p.epoch = r.current_epoch
			WHERE r.id = $1`, runID, name).Scan(&st.runState, &placementStop, &st.serverState, &st.stopReason, &st.exitCode, &st.errText)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		st.found = err == nil
		// Moving: its placement is stopping to move, or it is on its way
		// to the next one after a move.
		st.moving = (st.runState == StateStopping && slices.Contains(movedStops, placementStop)) ||
			(st.stopReason == "migrated" && slices.Contains(startingRunStates, st.runState))
		return err
	})
	return st, err
}

// route decides what a request gets: a target to proxy to, or (nil) a
// status page it has already written. A starting server, or a Run on its
// way to running, is waited for up to hold_for.
func (p *previews) route(ctx context.Context, w http.ResponseWriter, name, runID string) (*streamTarget, error) {
	deadline := time.Now().Add(p.s.cfg.Preview.HoldFor)
	for {
		woken := p.s.wakeups.next(runID)
		st, err := p.state(ctx, runID, name)
		if err != nil {
			p.page(w, http.StatusBadGateway, pageError, nil)
			return nil, err
		}
		if !st.found {
			p.page(w, http.StatusNotFound, pageUnknown, nil)
			return nil, nil
		}
		waiting := false
		switch {
		case st.runState == StateRunning && st.serverState == ServerReady:
			t, err := p.s.resolveTarget(ctx, store.System(), "tunnel", runID, name)
			if err == nil {
				return &t, nil
			}
			// The host is reconnecting to this luxd, perhaps.
			waiting = true
			st.serverState = ServerUnreachable
		case st.runState == StateRunning && st.serverState == ServerStarting:
			waiting = true
		case slices.Contains(startingRunStates, st.runState) || st.moving:
			waiting = true
		}
		if !waiting || !time.Now().Before(deadline) {
			p.statusPage(w, st)
			return nil, nil
		}
		wait(ctx, woken, min(time.Until(deadline), time.Second))
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
}

// startingRunStates: a Run on its way to running.
var startingRunStates = []string{StateSubmitted, StateScheduled, StateProvisioning, StateStarting, StateResuming}

func (p *previews) statusPage(w http.ResponseWriter, st previewState) {
	switch {
	case st.moving:
		p.page(w, http.StatusServiceUnavailable, pageMoving, nil)
	case slices.Contains(startingRunStates, st.runState):
		p.page(w, http.StatusServiceUnavailable, pageStarting, map[string]any{"What": "The Run is starting."})
	case st.runState != StateRunning:
		p.page(w, http.StatusServiceUnavailable, pageRunStopped, map[string]any{"State": st.runState})
	case st.serverState == ServerStopped:
		p.page(w, http.StatusServiceUnavailable, pageStopped, map[string]any{"Reason": st.stopReason})
	case st.serverState == ServerExited:
		code := -1
		if st.exitCode != nil {
			code = *st.exitCode
		}
		p.page(w, http.StatusServiceUnavailable, pageExited, map[string]any{"Code": strconv.Itoa(code), "Error": st.errText})
	case st.serverState == ServerUnreachable:
		p.page(w, http.StatusBadGateway, pageUnreachable, nil)
	default:
		p.page(w, http.StatusServiceUnavailable, pageStarting, map[string]any{"What": "The server is starting."})
	}
}

// ---- the proxy -------------------------------------------------------------

type previewTargetKey struct{}

type previewTarget struct {
	runID, name, user string
	t                 streamTarget
}

// rewrite makes the request the container sees: to the server's port, with
// the preview's host in X-Forwarded-Host, and none of lux's credentials.
func (p *previews) rewrite(pr *httputil.ProxyRequest) {
	pt := pr.In.Context().Value(previewTargetKey{}).(previewTarget)
	// The connection pool is keyed by host: one per placement and port.
	pr.Out.URL.Scheme = "http"
	pr.Out.URL.Host = fmt.Sprintf("%s.%s.e%d:%d", pt.name, strings.TrimPrefix(pt.runID, "run_"), pt.t.epoch, pt.t.port)
	// What the server sees as its host: itself, as a dev server expects
	// (they refuse other hosts by default); the preview's is forwarded.
	pr.Out.Host = "localhost:" + strconv.Itoa(pt.t.port)
	pr.SetXForwarded()
	cleanPreviewHeaders(pr.Out.Header)
	pr.Out.Header.Set("X-Forwarded-Host", pr.In.Host)
	pr.Out.Header.Set("X-Forwarded-Proto", "https")
	pr.Out.Header.Set("X-Lux-User", pt.user)
}

// cleanPreviewHeaders removes lux's and Access's credentials from what goes
// to the container.
func cleanPreviewHeaders(h http.Header) {
	h.Del("Cf-Access-Jwt-Assertion")
	h.Del("X-Lux-User")
	if a := h.Get("Authorization"); strings.HasPrefix(bearerToken(a), "lux") {
		h.Del("Authorization")
	}
	if cs := h.Values("Cookie"); len(cs) > 0 {
		h.Del("Cookie")
		var keep []string
		for _, line := range cs {
			for _, c := range strings.Split(line, ";") {
				c = strings.TrimSpace(c)
				n, _, _ := strings.Cut(c, "=")
				if c == "" || n == previewCookie || n == "CF_Authorization" {
					continue
				}
				keep = append(keep, c)
			}
		}
		if len(keep) > 0 {
			h.Set("Cookie", strings.Join(keep, "; "))
		}
	}
}

// stripCookieDomains removes Domain from every cookie the container sets:
// a preview may set cookies for its own host only.
func stripCookieDomains(resp *http.Response) error {
	cs := resp.Header.Values("Set-Cookie")
	if len(cs) == 0 {
		return nil
	}
	resp.Header.Del("Set-Cookie")
	for _, c := range cs {
		resp.Header.Add("Set-Cookie", stripDomain(c))
	}
	return nil
}

func stripDomain(setCookie string) string {
	parts := strings.Split(setCookie, ";")
	out := parts[:1]
	for _, a := range parts[1:] {
		k, _, _ := strings.Cut(strings.TrimSpace(a), "=")
		if !strings.EqualFold(strings.TrimSpace(k), "domain") {
			out = append(out, a)
		}
	}
	return strings.Join(out, ";")
}

func (p *previews) transport() *http.Transport {
	return &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			pt, ok := ctx.Value(previewTargetKey{}).(previewTarget)
			if !ok {
				return nil, errors.New("no preview target")
			}
			return p.s.dialTunnel(pt.runID, pt.t)
		},
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       60 * time.Second,
		ResponseHeaderTimeout: 5 * time.Minute,
		DisableCompression:    true,
	}
}

func (p *previews) proxyError(w http.ResponseWriter, r *http.Request, err error) {
	if r.Context().Err() != nil {
		return
	}
	p.page(w, http.StatusBadGateway, pageUnreachable, nil)
}

// ---- activity --------------------------------------------------------------

// touch notes a proxied request; its server's lastRequestAt is written at
// most every activityEvery (at once, the first time).
func (p *previews) touch(ref serverRef) {
	now := time.Now()
	p.mu.Lock()
	p.pending[ref] = now
	due := now.Sub(p.written[ref]) >= activityEvery
	p.mu.Unlock()
	if due {
		go p.flush(context.Background(), false)
	}
}

// flush writes what is due (everything, with all).
func (p *previews) flush(ctx context.Context, all bool) {
	now := time.Now()
	p.mu.Lock()
	var refs []serverRef
	var at []time.Time
	for ref, t := range p.pending {
		if all || now.Sub(p.written[ref]) >= activityEvery {
			refs = append(refs, ref)
			at = append(at, t)
			p.written[ref] = now
			delete(p.pending, ref)
		}
	}
	for ref, t := range p.written {
		if now.Sub(t) > 10*activityEvery {
			delete(p.written, ref)
		}
	}
	p.mu.Unlock()
	if len(refs) == 0 {
		return
	}
	runs, names := make([]string, len(refs)), make([]string, len(refs))
	for i, ref := range refs {
		runs[i], names[i] = ref.runID, ref.name
	}
	err := p.s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE run_servers rs SET last_request_at = greatest(rs.last_request_at, u.at)
			FROM unnest($1::text[], $2::text[], $3::timestamptz[]) AS u(run_id, name, at)
			WHERE rs.run_id = u.run_id AND rs.name = u.name`, runs, names, at)
		return err
	})
	if err != nil && ctx.Err() == nil {
		p.s.log.Warn("previews: activity", "err", err)
	}
}

func (p *previews) flushLoop(ctx context.Context) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			p.flush(context.WithoutCancel(ctx), true)
			return
		case <-t.C:
			p.flush(ctx, false)
		}
	}
}

// ---- status pages ----------------------------------------------------------

//go:embed preview.html
var previewFS embed.FS

var previewTmpl = template.Must(template.ParseFS(previewFS, "preview.html"))

type previewPage struct {
	Tone, Title, Message string
	Refresh              bool
}

var (
	pageUnknown     = previewPage{"neutral", "No such preview", "This address is not a server of any Run.", false}
	pageSignIn      = previewPage{"neutral", "Sign-in needed", "Open this preview from wherever you manage its Run to sign in.", false}
	pageError       = previewPage{"red", "Something went wrong", "lux could not look this preview up. Try again in a moment.", true}
	pageStarting    = previewPage{"blue", "Starting", "", true}
	pageMoving      = previewPage{"violet", "Moving", "The Run is moving to another host. Its servers are stopped by the move: start them again from wherever you manage this run.", true}
	pageRunStopped  = previewPage{"neutral", "The Run is not running", "", true}
	pageStopped     = previewPage{"neutral", "Server stopped", "Start it from wherever you manage this run.", true}
	pageExited      = previewPage{"red", "Server exited", "Its command ended. Start it again from wherever you manage this run.", true}
	pageUnreachable = previewPage{"amber", "Not answering", "The server is running but does not accept connections on its port.", true}
)

// page writes a status page; extra fills in its details.
func (p *previews) page(w http.ResponseWriter, status int, pg previewPage, extra map[string]any) {
	data := map[string]any{"Page": pg}
	for k, v := range extra {
		data[k] = v
	}
	var b bytes.Buffer
	if err := previewTmpl.Execute(&b, data); err != nil {
		http.Error(w, pg.Title, status)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Lux-Preview", "status")
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; img-src data:")
	if pg.Refresh {
		h.Set("Retry-After", "5")
	}
	w.WriteHeader(status)
	_, _ = w.Write(b.Bytes())
}

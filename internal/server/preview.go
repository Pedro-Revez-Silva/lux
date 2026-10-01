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
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/store"
)

// The preview listener: https://<server host>.<domain> reaches a server,
// wherever its Run is now. It has its own address and only
// ever proxies: nothing of luxd's own (/v1, /runner, the console) is
// served on it.
//
// A request is authenticated (Cloudflare Access, or a ticket turned into a
// cookie), then routed by the server's state: a ready server of a running
// Run is reverse-proxied over a tunnel stream through the hub to the
// current placement; a starting one is waited for (hold_for); anything
// else gets a small status page that refreshes itself.

const (
	previewCookie = "__Host-lux_preview"
	// previewCookieHTTP: over http (a local demo domain) a browser keeps
	// no __Host- cookie; this one is host-only all the same.
	previewCookieHTTP = "lux_preview"
	// previewCookieTTL is a key's cookie's life: luxd re-checks the key
	// (keyLive) as it goes. A person's (Cloudflare Access, no key) cannot
	// be re-checked without their Access token, so theirs is short.
	previewCookieTTL       = 12 * time.Hour
	previewPersonCookieTTL = time.Hour
	previewAuthPath        = "/.lux/auth"
	// defaultActivityEvery bounds how often a server's lastRequestAt is
	// written (preview.activity_every).
	defaultActivityEvery = 30 * time.Second
)

type previews struct {
	s    *Server
	mode string // cloudflare-access | ticket
	cf   *cfAccess
	key  []byte // the cookie's HMAC key (luxd_keys), shared by every luxd
	rp   *httputil.ReverseProxy

	mu       sync.Mutex
	pending  map[string]time.Time // requests not yet written, by server id
	written  map[string]time.Time // when each was last written
	keysLive map[string]keyCheck  // api keys a cookie names: still live?
}

type keyCheck struct {
	live  bool
	until time.Time
}

// previewTickets: previews are on, and people sign in to them with a
// ticket from the console (not through Cloudflare Access).
func (s *Server) previewTickets() bool {
	return s.preview != nil && s.preview.mode == "ticket"
}

func newPreviews(s *Server) *previews {
	p := &previews{s: s, mode: s.cfg.Preview.Auth, pending: map[string]time.Time{}, written: map[string]time.Time{},
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

// parsePreviewHost is the server host a request's Host names: the part
// before .<domain>, one or more DNS labels.
func parsePreviewHost(host, domain string) (string, bool) {
	host = strings.ToLower(strings.TrimSuffix(stripPort(host), "."))
	rel, found := strings.CutSuffix(host, "."+strings.ToLower(domain))
	if !found || rel == "" || len(host) > 253 {
		return "", false
	}
	for _, l := range strings.Split(rel, ".") {
		if !hostLabelRe.MatchString(l) {
			return "", false
		}
	}
	return rel, true
}

// ---- the cookie ------------------------------------------------------------

// previewUser is who a preview cookie (or an Access token) is for.
type previewUser struct {
	ServerID string `json:"s"`
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
		var err error
		live, err = keyLiveTx(ctx, tx, keyID)
		return err
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

// Paths of the preview host that are lux's own, never the server's.
const (
	previewWaitPath = "/.lux/wait" // a waking page's poll: never wakes
	previewWakePath = "/.lux/wake" // the no-answer page's "Ask again"
)

func (p *previews) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host, ok := parsePreviewHost(r.Host, p.s.cfg.Preview.Domain)
	if !ok {
		p.page(w, http.StatusNotFound, pageUnknown, nil)
		return
	}
	v, found, err := p.lookup(r.Context(), host)
	if err != nil {
		p.page(w, http.StatusBadGateway, pageError, nil)
		return
	}
	if !found {
		// Deleted, or never was: a hostname is never reused while its
		// server lives, and nothing tells the two apart afterwards.
		p.page(w, http.StatusNotFound, pageGone, nil)
		return
	}
	if p.mode == "ticket" && r.URL.Path == previewAuthPath {
		p.signIn(w, r, v)
		return
	}
	user, ok := p.authenticate(r, v)
	if !ok {
		p.challenge(w, r)
		return
	}
	p.touch(v.ID)
	switch r.URL.Path {
	case previewWaitPath:
		p.wait(w, r, v)
		return
	case previewWakePath:
		if r.Method == http.MethodPost {
			if _, err := p.s.requestWake(r.Context(), v.ID, user.User, waitTarget(r)); err != nil {
				p.page(w, http.StatusBadGateway, pageError, nil)
				return
			}
		}
		http.Redirect(w, r, previewWaitPath+"?to="+url.QueryEscape(waitTarget(r)), http.StatusSeeOther)
		return
	}
	t, err := p.route(r.Context(), w, r, v, user)
	if err != nil || t == nil {
		return
	}
	ctx := context.WithValue(r.Context(), previewTargetKey{}, previewTarget{runID: *v.RunID, name: v.Name, user: user.User, t: *t})
	p.rp.ServeHTTP(w, r.WithContext(ctx))
}

// waitTarget is the path a waking page drops into once the server is up:
// ?to= of /.lux/wait and /.lux/wake, the request's own otherwise.
func waitTarget(r *http.Request) string {
	if r.URL.Path == previewWaitPath || r.URL.Path == previewWakePath {
		if to := r.FormValue("to"); localPath(to) && !strings.HasPrefix(to, "/.lux/") {
			return to
		}
		return "/"
	}
	return r.URL.RequestURI()
}

// lookup finds the server of a preview host (in a system scope: the
// request is not authenticated yet).
func (p *previews) lookup(ctx context.Context, host string) (serverRow, bool, error) {
	var v serverRow
	err := p.s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		var err error
		v, err = scanServerRow(tx.QueryRow(ctx, serverSelect+`WHERE sv.host = $1`, host))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return v, false, nil
	}
	return v, err == nil, err
}

// authenticate finds the request's user, allowed to read the server.
func (p *previews) authenticate(r *http.Request, v serverRow) (previewUser, bool) {
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
		u := previewUser{ServerID: v.ID, TenantID: pr.TenantID, Operator: pr.Operator, User: email}
		return u, u.Operator || u.TenantID == v.TenantID
	}
	for _, c := range r.CookiesNamed(p.cookieName()) {
		if u, ok := p.verify(c.Value, time.Now()); ok && u.ServerID == v.ID && (u.KeyID == "" || p.keyLive(r.Context(), u.KeyID)) {
			return u, true
		}
	}
	return previewUser{}, false
}

// cookieName: __Host- (Secure, host-only) over https; over http (a local
// demo domain under localhost, previews.scheme http) a plain host-only one.
func (p *previews) cookieName() string {
	if p.s.cfg.Preview.Scheme == "http" {
		return previewCookieHTTP
	}
	return previewCookie
}

// challenge answers a request without a user: a browser asking for a page
// goes to sign in (ticket mode); anything else gets 401. Nothing wakes.
func (p *previews) challenge(w http.ResponseWriter, r *http.Request) {
	if p.mode == "ticket" && r.Method == http.MethodGet && strings.Contains(r.Header.Get("Accept"), "text/html") {
		to := p.s.previewOrigin(stripPort(r.Host)) + r.URL.RequestURI()
		http.Redirect(w, r, strings.TrimRight(p.s.cfg.PublicURL, "/")+"/preview-auth?to="+url.QueryEscape(to), http.StatusFound)
		return
	}
	p.page(w, http.StatusUnauthorized, pageSignIn, nil)
}

func stripPort(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return host
}

// signIn is /.lux/auth?ticket=…&to=/path: a preview ticket for this server,
// or for the Run it is attached to (POST /v1/runs/{id}/tickets), becomes
// the cookie, and the browser goes on to the path.
func (p *previews) signIn(w http.ResponseWriter, r *http.Request, v serverRow) {
	to := r.URL.Query().Get("to")
	if to == "" {
		to = "/"
	}
	if !localPath(to) {
		http.Error(w, "to must be a path", http.StatusBadRequest)
		return
	}
	pr, err := p.s.redeemTicket(r.Context(), r.URL.Query().Get("ticket"), ticketFor{runID: strOf(v.RunID), serverID: v.ID}, TicketPreview)
	if err != nil || !pr.Can("read") || (!pr.Operator && pr.TenantID != v.TenantID) {
		p.page(w, http.StatusUnauthorized, pageSignIn, nil)
		return
	}
	ttl := previewCookieTTL
	if pr.KeyID == "" {
		ttl = previewPersonCookieTTL
	}
	u := previewUser{ServerID: v.ID, TenantID: pr.TenantID, Operator: pr.Operator, KeyID: pr.KeyID, User: pr.Email,
		Exp: time.Now().Add(ttl).Unix()}
	if u.User == "" && pr.KeyID != "" {
		_ = p.s.db.Tx(r.Context(), store.System(), func(tx pgx.Tx) error {
			return tx.QueryRow(r.Context(), `SELECT name FROM api_keys WHERE id = $1`, pr.KeyID).Scan(&u.User)
		})
		u.User = cmp.Or(u.User, pr.KeyID)
	}
	secure := p.s.cfg.Preview.Scheme != "http"
	http.SetCookie(w, &http.Cookie{Name: p.cookieName(), Value: p.sign(u), Path: "/", MaxAge: int(ttl.Seconds()),
		Secure: secure, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, to, http.StatusFound)
}

// localPath: a path on this host, never another (//evil, /\evil). No
// backslash anywhere (browsers read one as a slash, and http.Redirect's
// cleaning turns /./\evil into /\evil); and, as defence in depth, the
// cleaned path it redirects to must not start with // either.
func localPath(to string) bool {
	if !strings.HasPrefix(to, "/") || strings.HasPrefix(to, "//") || strings.ContainsAny(to, "\\\r\n") {
		return false
	}
	u, err := url.Parse(to)
	return err == nil && u.Scheme == "" && u.Host == "" && !strings.HasPrefix(path.Clean(u.Path), "//")
}

// needsWake: a wakeable server that no running (or starting) Run serves.
func needsWake(v serverRow) bool {
	if v.Wake != WakeRequest || v.down() {
		return false
	}
	if v.RunID == nil {
		return true
	}
	return v.RunState != StateRunning && !slices.Contains(startingRunStates, v.RunState) && !v.Moving
}

// route decides what a request gets: a target to proxy to, or (nil) a page
// it has already written. A server that wakes on request is never held:
// the waking page polls (/.lux/wait). One that does not keeps hold_for: a
// starting server, or a Run on its way to running, is waited for.
func (p *previews) route(ctx context.Context, w http.ResponseWriter, r *http.Request, v serverRow, user previewUser) (*streamTarget, error) {
	deadline := time.Now().Add(p.s.cfg.Preview.HoldFor)
	for {
		woken := p.s.wakeups.next(strOf(v.RunID))
		if v.RunState == StateRunning && v.State == ServerReady {
			t, err := p.s.resolveTarget(ctx, store.System(), "tunnel", *v.RunID, v.Name)
			if err == nil {
				return &t, nil
			}
			// The host is reconnecting to this luxd, perhaps.
			v.State = ServerUnreachable
		}
		if v.Wake == WakeRequest {
			if needsWake(v) {
				if _, err := p.s.requestWake(ctx, v.ID, user.User, r.URL.RequestURI()); err != nil {
					p.page(w, http.StatusBadGateway, pageError, nil)
					return nil, err
				}
			}
			return nil, p.wakingPage(ctx, w, v.ID, r.URL.RequestURI())
		}
		waiting := v.RunID != nil && ((v.RunState == StateRunning && (v.State == ServerStarting || v.State == ServerUnreachable)) ||
			slices.Contains(startingRunStates, v.RunState) || v.Moving)
		if !waiting || !time.Now().Before(deadline) {
			p.statusPage(w, v)
			return nil, nil
		}
		wait(ctx, woken, min(time.Until(deadline), time.Second))
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		nv, found, err := p.lookup(ctx, v.Host)
		if err != nil || !found {
			p.page(w, http.StatusNotFound, pageGone, nil)
			return nil, err
		}
		v = nv
	}
}

// exitCodeText is an exit code for a page, -1 when there is none.
func exitCodeText(code *int) string {
	if code == nil {
		return "-1"
	}
	return strconv.Itoa(*code)
}

func strOf(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// wait is /.lux/wait?to=: a waking page's poll. Into the app once ready;
// the page again otherwise. It never wakes: after wakeTimeout the page
// says no answer, and only a new request (or Ask again) asks again.
func (p *previews) wait(w http.ResponseWriter, r *http.Request, v serverRow) {
	to := waitTarget(r)
	if v.RunState == StateRunning && v.State == ServerReady {
		w.Header().Set("Cache-Control", "no-store")
		http.Redirect(w, r, to, http.StatusSeeOther)
		return
	}
	if v.Wake != WakeRequest {
		p.statusPage(w, v)
		return
	}
	_ = p.wakingPage(r.Context(), w, v.ID, to)
}

// startingRunStates: a Run on its way to running.
var startingRunStates = []string{StateSubmitted, StateScheduled, StateProvisioning, StateStarting, StateResuming}

// statusPage is what a server that does not wake on request shows while it
// does not answer.
func (p *previews) statusPage(w http.ResponseWriter, v serverRow) {
	switch {
	case v.RunID == nil:
		p.page(w, http.StatusServiceUnavailable, pageDetached, map[string]any{"Name": v.Name})
	case v.Moving:
		p.page(w, http.StatusServiceUnavailable, pageMoving, nil)
	case slices.Contains(startingRunStates, v.RunState):
		p.page(w, http.StatusServiceUnavailable, pageStarting, map[string]any{"What": "The Run is starting."})
	case v.RunState != StateRunning:
		p.page(w, http.StatusServiceUnavailable, pageRunStopped, map[string]any{"Name": v.Name, "State": v.RunState})
	case v.State == ServerStopped:
		p.page(w, http.StatusServiceUnavailable, pageStopped, map[string]any{"Reason": strOf(v.StopReason)})
	case v.State == ServerExited:
		p.page(w, http.StatusServiceUnavailable, pageExited, map[string]any{"Code": exitCodeText(v.ExitCode), "Error": strOf(v.Error),
			"LogURL": p.consoleServerURL(v.ID)})
	case v.State == ServerUnreachable:
		p.page(w, http.StatusBadGateway, pageUnreachable, nil)
	default:
		p.page(w, http.StatusServiceUnavailable, pageStarting, map[string]any{"What": "The server is starting."})
	}
}

// consoleServerURL is the server's page in the console.
func (p *previews) consoleServerURL(id string) string {
	return strings.TrimRight(p.s.cfg.PublicURL, "/") + "/servers/" + id
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
	pr.Out.Header.Set("X-Forwarded-Proto", cmp.Or(p.s.cfg.Preview.Scheme, "https"))
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
				if c == "" || n == previewCookie || n == previewCookieHTTP || n == "CF_Authorization" {
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

// touch notes a request to a server: every proxied request counts (pages,
// assets, API calls), and a waking page's polls; a WebSocket's traffic
// after its upgrade does not (it is one request). Its lastRequestAt is
// written at most every activityEvery (at once, the first time).
func (p *previews) touch(id string) {
	now := time.Now()
	p.mu.Lock()
	p.pending[id] = now
	due := now.Sub(p.written[id]) >= p.activityEvery()
	p.mu.Unlock()
	if due {
		go p.flush(context.Background(), false)
	}
}

func (p *previews) activityEvery() time.Duration {
	if e := p.s.cfg.Preview.ActivityEvery; e > 0 {
		return e
	}
	return defaultActivityEvery
}

// flush writes what is due (everything, with all).
func (p *previews) flush(ctx context.Context, all bool) {
	now := time.Now()
	every := p.activityEvery()
	p.mu.Lock()
	var ids []string
	var at []time.Time
	for id, t := range p.pending {
		if all || now.Sub(p.written[id]) >= every {
			ids = append(ids, id)
			at = append(at, t)
			p.written[id] = now
			delete(p.pending, id)
		}
	}
	for id, t := range p.written {
		if now.Sub(t) > 10*every {
			delete(p.written, id)
		}
	}
	p.mu.Unlock()
	if len(ids) == 0 {
		return
	}
	err := p.s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE run_servers rs SET last_request_at = greatest(rs.last_request_at, u.at)
			FROM unnest($1::text[], $2::timestamptz[]) AS u(id, at)
			WHERE rs.id = u.id`, ids, at)
		return err
	})
	if err != nil && ctx.Err() == nil {
		p.s.log.Warn("previews: activity", "err", err)
	}
}

func (p *previews) flushLoop(ctx context.Context) {
	t := time.NewTicker(min(5*time.Second, p.activityEvery()))
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
	// Waking: the steps page (refreshes every 3 seconds, by its own poll).
	Waking bool
}

var (
	pageUnknown      = previewPage{Tone: "neutral", Title: "No such preview", Message: "This address is not a server of this lux."}
	pageGone         = previewPage{Tone: "neutral", Title: "This preview is gone", Message: "No server answers to this address any more: its owner deleted it, or it never was. Nothing will wake it."}
	pageSignIn       = previewPage{Tone: "neutral", Title: "Sign-in needed", Message: "Open this preview from wherever you manage it to sign in."}
	pageError        = previewPage{Tone: "red", Title: "Something went wrong", Message: "lux could not look this preview up. Try again in a moment.", Refresh: true}
	pageStarting     = previewPage{Tone: "blue", Title: "Starting", Refresh: true}
	pageMoving       = previewPage{Tone: "violet", Title: "Moving", Message: "The Run is moving to another host; its servers start again there.", Refresh: true}
	pageRunStopped   = previewPage{Tone: "neutral", Title: "Not running", Message: "It does not wake on request: it runs only while its Run does.", Refresh: true}
	pageDetached     = previewPage{Tone: "neutral", Title: "Not running", Message: "No Run serves it, and it does not wake on request.", Refresh: true}
	pageStopped      = previewPage{Tone: "neutral", Title: "Server stopped", Message: "Start it from wherever you manage it.", Refresh: true}
	pageExited       = previewPage{Tone: "red", Title: "Server exited", Message: "Its command ended. Start it again from wherever you manage it.", Refresh: true}
	pageUnreachable  = previewPage{Tone: "amber", Title: "Not answering", Message: "The server is running but does not accept connections on its port.", Refresh: true}
	pageWaking       = previewPage{Tone: "blue", Title: "Waking", Message: "This usually takes under a minute.", Waking: true}
	pageWakingHost   = previewPage{Tone: "blue", Title: "Waking", Message: "Waiting for its owner to bring a Run up and for a host. Cold hosts take 1-3 minutes; the page keeps trying.", Waking: true}
	pageWakingMoving = previewPage{Tone: "violet", Title: "Moving to another host", Message: "Its Run is moving; the server starts again there.", Waking: true}
	pageNoAnswer     = previewPage{Tone: "amber", Title: "No answer from its owner", Message: "lux asked the orchestrator that owns this server to start it, and nothing started it. lux never starts a Run by itself."}
	pageDidNotStart  = previewPage{Tone: "red", Title: "The server did not start", Message: "Its Run is up, but the command exited during start.", Refresh: false}
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
	if pg.Refresh || pg.Waking {
		h.Set("Retry-After", "3")
	}
	w.WriteHeader(status)
	_, _ = w.Write(b.Bytes())
}

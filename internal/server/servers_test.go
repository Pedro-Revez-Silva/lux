package server

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/ids"
	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/store"
)

func TestParsePreviewHost(t *testing.T) {
	const suffix = "k3jq7x2mfa9vbn4z"
	valid := strings.Replace(suffix, "9", "2", 1) // base32: no 9
	for _, c := range []struct {
		host, name, run string
		ok              bool
	}{
		{"web-" + valid + ".lux.example.com", "web", "run_" + valid, true},
		{"WEB-" + valid + ".Lux.Example.com.", "web", "run_" + valid, true},
		{"web-" + valid + ".lux.example.com:443", "web", "run_" + valid, true},
		{"my-app-" + valid + ".lux.example.com", "my-app", "run_" + valid, true},
		{"web-" + suffix + ".lux.example.com", "", "", false}, // 9 is not base32
		{"web-" + valid + ".other.com", "", "", false},
		{"a.web-" + valid + ".lux.example.com", "", "", false},
		{"web" + valid + ".lux.example.com", "", "", false},
		{"-" + valid + ".lux.example.com", "", "", false},
		{"web--" + valid + ".lux.example.com", "", "", false},
		{"web-short.lux.example.com", "", "", false},
		{"lux.example.com", "", "", false},
		{"x.lux.example.com.evil.com", "", "", false},
	} {
		name, run, ok := parsePreviewHost(c.host, "lux.example.com")
		if ok != c.ok || name != c.name || run != c.run {
			t.Errorf("%s: got %q %q %v, want %q %q %v", c.host, name, run, ok, c.name, c.run, c.ok)
		}
	}
}

func TestPreviewCookie(t *testing.T) {
	p := &previews{key: []byte("0123456789abcdef0123456789abcdef")}
	now := time.Now()
	u := previewUser{RunID: "run_x", TenantID: "t1", User: "a@b.c", Exp: now.Add(time.Hour).Unix()}
	v := p.sign(u)
	if got, ok := p.verify(v, now); !ok || got != u {
		t.Fatalf("verify: %+v %v", got, ok)
	}
	if _, ok := p.verify(v, now.Add(2*time.Hour)); ok {
		t.Fatal("an expired cookie verified")
	}
	payload, sig, _ := strings.Cut(v, ".")
	forged := previewUser{RunID: "run_other", TenantID: "t1", User: "a@b.c", Exp: u.Exp}
	b, _ := json.Marshal(forged)
	for _, bad := range []string{
		"", "x", payload, payload + ".", "." + sig,
		strings.TrimRight(payload, "A") + "B." + sig,
		p.sign(forged)[:len(p.sign(forged))-2] + "xx",
		base64.RawURLEncoding.EncodeToString(b) + "." + sig, // another payload, this signature
	} {
		if _, ok := p.verify(bad, now); ok {
			t.Errorf("verified %q", bad)
		}
	}
	other := &previews{key: []byte("another key, another deployment!")}
	if _, ok := other.verify(v, now); ok {
		t.Fatal("verified with another key")
	}
}

func TestPreviewHeaderHygiene(t *testing.T) {
	h := http.Header{}
	h.Set("Cf-Access-Jwt-Assertion", "jwt")
	h.Set("Authorization", "Bearer lux_secretkey")
	h.Set("X-Lux-User", "forged")
	h.Add("Cookie", "__Host-lux_preview=sig; app=1")
	h.Add("Cookie", "CF_Authorization=tok; other=2")
	cleanPreviewHeaders(h)
	if h.Get("Cf-Access-Jwt-Assertion") != "" || h.Get("Authorization") != "" || h.Get("X-Lux-User") != "" {
		t.Fatalf("credentials left: %v", h)
	}
	if got := h.Values("Cookie"); len(got) != 1 || got[0] != "app=1; other=2" {
		t.Fatalf("cookies: %q", got)
	}
	// The container's own Authorization stays.
	h = http.Header{"Authorization": {"Bearer app-token"}, "Cookie": {"__Host-lux_preview=x"}}
	cleanPreviewHeaders(h)
	if h.Get("Authorization") != "Bearer app-token" || h.Get("Cookie") != "" {
		t.Fatalf("app header: %v", h)
	}

	for in, want := range map[string]string{
		"a=1; Domain=example.com; Path=/; HttpOnly": "a=1; Path=/; HttpOnly",
		"a=1;domain=.example.com":                   "a=1",
		"a=1; Path=/; DOMAIN = x":                   "a=1; Path=/",
		"a=1; Path=/":                               "a=1; Path=/",
		"domain=1; Path=/":                          "domain=1; Path=/",
	} {
		if got := stripDomain(in); got != want {
			t.Errorf("stripDomain(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLocalPath(t *testing.T) {
	for p, want := range map[string]bool{
		"/": true, "/a/b?c=d#e": true, "": false, "a": false, "//evil.com/": false, "/\\evil.com": false,
		"https://evil.com/": false, "/a\r\nSet-Cookie: x": false,
	} {
		if got := localPath(p); got != want {
			t.Errorf("localPath(%q) = %v", p, got)
		}
	}
}

func TestOriginAllowed(t *testing.T) {
	s := &Server{cfg: Config{PublicURL: "https://lux.example.com", AllowedOrigins: []string{"https://console.example.com/"}}}
	for origin, want := range map[string]bool{
		"":                            true,
		"https://lux.example.com":     true,
		"https://LUX.example.com":     true,
		"http://luxd.internal:7070":   true, // the request's own host
		"https://console.example.com": true,
		"https://evil.com":            false,
		"http://lux.example.com":      false,
		"null":                        false,
	} {
		r := httptest.NewRequest(http.MethodGet, "http://luxd.internal:7070/v1/runs/x/exec", nil)
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		if got := s.originAllowed(r); got != want {
			t.Errorf("origin %q: got %v", origin, got)
		}
	}
}

// ---- with a database ---------------------------------------------------------

// serversFixture: tenant t1 with a key, a host, and a running Run r1 at
// epoch 1 on it.
func serversFixture(t *testing.T, s *Server, ctx context.Context) (key string) {
	t.Helper()
	key = ids.Secret("luxk")
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1'), ('t2', 't2')`)
	execSQL(t, s, ctx, `INSERT INTO api_keys (id, tenant_id, name, key_hash, scopes) VALUES ('k1', 't1', 'ci', $1, ARRAY['run'])`, ids.Hash(key))
	execSQL(t, s, ctx, `INSERT INTO hosts (id, name, state) VALUES ('h1', 'h1', 'ready')`)
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, spec, state, current_epoch) VALUES
		('run_aaaaaaaaaaaaaaaa', 't1', '{"workload": {"workdir": "/work"}}', 'running', 1)`)
	execSQL(t, s, ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state) VALUES ('p1', 't1', 'run_aaaaaaaaaaaaaaaa', 'h1', 1, 'running')`)
	return key
}

const r1 = "run_aaaaaaaaaaaaaaaa"

func apiCall(t *testing.T, s *Server, key, method, target string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = strings.NewReader(string(b))
	}
	req := httptest.NewRequest(method, target, rd)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	return w
}

func pendingServers(t *testing.T, s *Server, ctx context.Context) []proto.Servers {
	t.Helper()
	var out []proto.Servers
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT payload FROM host_messages WHERE type = 'servers' ORDER BY id`)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, pgx.RowTo[proto.Servers])
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func getServer(t *testing.T, s *Server, key, name string) RunServer {
	t.Helper()
	w := apiCall(t, s, key, http.MethodGet, "/v1/runs/"+r1+"/servers", nil)
	var out struct{ Servers []RunServer }
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	for _, sv := range out.Servers {
		if sv.Name == name {
			return sv
		}
	}
	t.Fatalf("no server %s: %s", name, w.Body)
	return RunServer{}
}

func serverGen(t *testing.T, s *Server, ctx context.Context, name string) int64 {
	t.Helper()
	var gen int64
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT gen FROM run_servers WHERE run_id = $1 AND name = $2`, r1, name).Scan(&gen)
	})
	if err != nil {
		t.Fatal(err)
	}
	return gen
}

func serverReport(t *testing.T, s *Server, ctx context.Context, epoch int, data map[string]any) {
	t.Helper()
	f := s.handleReport(ctx, "h1", proto.Frame{Type: proto.MsgRunEvent, ID: 1, RunID: r1, Epoch: epoch,
		Data: proto.Marshal(proto.RunEvent{Type: proto.EvServerState, Data: data})})
	if f.Type != proto.MsgAck {
		t.Fatalf("report %v: %s", data, f.Data)
	}
}

// The servers' life: added and started (the placement is sent the set),
// ready and exited as the runner reports them for the current start only,
// stopped by request, stopped with their placement's end, restarted on the
// next placement only if the spec declares them.
func TestServerLifecycle(t *testing.T) {
	s := testServer(t)
	s.cfg.Preview.Domain = "lux.example.com"
	ctx := context.Background()
	key := serversFixture(t, s, ctx)

	w := apiCall(t, s, key, http.MethodPost, "/v1/runs/"+r1+"/servers", map[string]any{
		"name": "web", "port": 3000, "command": []string{"npm", "run", "dev"}, "workdir": "apps/web", "env": map[string]string{"A": "1"}})
	if w.Code != http.StatusCreated {
		t.Fatalf("add: %d %s", w.Code, w.Body)
	}
	var sv RunServer
	_ = json.Unmarshal(w.Body.Bytes(), &sv)
	if sv.State != ServerStarting || sv.URL == nil || *sv.URL != "https://web-aaaaaaaaaaaaaaaa.lux.example.com" || sv.Epoch == nil || *sv.Epoch != 1 {
		t.Fatalf("added: %+v", sv)
	}
	if w := apiCall(t, s, key, http.MethodPost, "/v1/runs/"+r1+"/servers", map[string]any{"name": "web", "port": 1}); w.Code != http.StatusConflict ||
		!strings.Contains(w.Body.String(), "name_taken") {
		t.Fatalf("duplicate: %d %s", w.Code, w.Body)
	}
	if w := apiCall(t, s, key, http.MethodPost, "/v1/runs/"+r1+"/servers", map[string]any{"name": "Bad", "port": 1}); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid: %d %s", w.Code, w.Body)
	}
	// A port-only server is not started, but its port may be tunnelled to.
	if w := apiCall(t, s, key, http.MethodPost, "/v1/runs/"+r1+"/servers", map[string]any{"name": "db", "port": 5432}); w.Code != http.StatusCreated {
		t.Fatalf("add db: %d %s", w.Code, w.Body)
	}
	sets := pendingServers(t, s, ctx)
	last := sets[len(sets)-1]
	if len(last.Servers) != 2 || last.Servers[1].Name != "web" || last.Servers[1].Workdir != "/work/apps/web" || last.Servers[1].Env["A"] != "1" ||
		fmt.Sprint(last.Ports) != "[3000 5432]" || last.Servers[0].Name != "db" || len(last.Servers[0].Command) != 0 {
		t.Fatalf("set: %+v", last)
	}
	gen := last.Servers[1].Gen

	// Reports: a stale gen is ignored; the current one applies.
	serverReport(t, s, ctx, 1, map[string]any{"name": "web", "gen": gen - 1, "state": "ready"})
	if got := getServer(t, s, key, "web"); got.State != ServerStarting {
		t.Fatalf("stale gen applied: %+v", got)
	}
	serverReport(t, s, ctx, 1, map[string]any{"name": "web", "gen": gen, "state": "ready"})
	if got := getServer(t, s, key, "web"); got.State != ServerReady || got.ReadySince == nil {
		t.Fatalf("ready: %+v", got)
	}
	// A port-only server whose port opens is ready.
	serverReport(t, s, ctx, 1, map[string]any{"name": "db", "gen": serverGen(t, s, ctx, "db"), "state": "ready"})
	if got := getServer(t, s, key, "db"); got.State != ServerReady {
		t.Fatalf("db: %+v", got)
	}
	serverReport(t, s, ctx, 1, map[string]any{"name": "web", "gen": gen, "state": "exited", "exitCode": 1, "error": "bind: address already in use"})
	got := getServer(t, s, key, "web")
	if got.State != ServerExited || got.ExitCode == nil || *got.ExitCode != 1 || got.Error == nil || *got.Error != "bind: address already in use" {
		t.Fatalf("exited: %+v", got)
	}

	// Restart: a new gen; the old gen's reports no longer apply.
	if w := apiCall(t, s, key, http.MethodPost, "/v1/runs/"+r1+"/servers/web/restart", nil); w.Code != http.StatusOK {
		t.Fatalf("restart: %d %s", w.Code, w.Body)
	}
	serverReport(t, s, ctx, 1, map[string]any{"name": "web", "gen": gen, "state": "ready"})
	if got := getServer(t, s, key, "web"); got.State != ServerStarting || got.ExitCode != nil {
		t.Fatalf("restarted: %+v", got)
	}
	// Nothing to start without a command.
	if w := apiCall(t, s, key, http.MethodPost, "/v1/runs/"+r1+"/servers/db/start", nil); w.Code != http.StatusConflict ||
		!strings.Contains(w.Body.String(), "no_command") {
		t.Fatalf("start without a command: %d %s", w.Code, w.Body)
	}
	// Stop by request: no longer watched in this placement.
	if w := apiCall(t, s, key, http.MethodPost, "/v1/runs/"+r1+"/servers/db/stop", nil); w.Code != http.StatusOK {
		t.Fatalf("stop: %d %s", w.Code, w.Body)
	}
	if got := getServer(t, s, key, "db"); got.State != ServerStopped || got.StopReason == nil || *got.StopReason != "stopped" || *got.StoppedEpoch != 1 {
		t.Fatalf("stopped: %+v", got)
	}
	last = pendingServers(t, s, ctx)[len(pendingServers(t, s, ctx))-1]
	if len(last.Servers) != 1 || last.Servers[0].Name != "web" {
		t.Fatalf("set after stop: %+v", last)
	}
	serverReport(t, s, ctx, 1, map[string]any{"name": "db", "gen": serverGen(t, s, ctx, "db"), "state": "ready"})
	if got := getServer(t, s, key, "db"); got.State != ServerStopped {
		t.Fatalf("a stopped one was watched: %+v", got)
	}
	// Another tenant sees nothing.
	key2 := ids.Secret("luxk")
	execSQL(t, s, ctx, `INSERT INTO api_keys (id, tenant_id, name, key_hash, scopes) VALUES ('k2', 't2', 'x', $1, ARRAY['run'])`, ids.Hash(key2))
	if w := apiCall(t, s, key2, http.MethodPost, "/v1/runs/"+r1+"/servers/web/stop", nil); w.Code != http.StatusNotFound {
		t.Fatalf("other tenant: %d %s", w.Code, w.Body)
	}

	// The placement ends with a migration: every server stops with it.
	execSQL(t, s, ctx, `UPDATE placements SET stop_reason = 'migrate' WHERE id = 'p1'`)
	code := 0
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		return s.applyStatus(ctx, tx, "t1", r1, 1, proto.Status{State: "exited", ExitCode: &code, Reason: "stopped"})
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"web", "db"} {
		got := getServer(t, s, key, name)
		want := "migrated"
		if name == "db" {
			want = "stopped" // stopped before, by request: stays so
		}
		if got.State != ServerStopped || got.StopReason == nil || *got.StopReason != want || *got.StoppedEpoch != 1 {
			t.Fatalf("%s after the move: %+v", name, got)
		}
	}
	// Start needs a running Run.
	if w := apiCall(t, s, key, http.MethodPost, "/v1/runs/"+r1+"/servers/web/start", nil); w.Code != http.StatusConflict ||
		!strings.Contains(w.Body.String(), "not_running") {
		t.Fatalf("start while stopped: %d %s", w.Code, w.Body)
	}
	// Remove.
	if w := apiCall(t, s, key, http.MethodDelete, "/v1/runs/"+r1+"/servers/db", nil); w.Code != http.StatusNoContent {
		t.Fatalf("remove: %d %s", w.Code, w.Body)
	}
	if w := apiCall(t, s, key, http.MethodGet, "/v1/runs/"+r1+"/servers/db/log", nil); w.Code != http.StatusNotFound {
		t.Fatalf("log of removed: %d %s", w.Code, w.Body)
	}
}

// A spec's servers are created at submit and started on every placement.
func TestSpecServersStartOnEveryPlacement(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	key := serversFixture(t, s, ctx)
	w := apiCall(t, s, key, http.MethodPost, "/v1/runs", map[string]any{
		"image": map[string]any{"ref": "alpine"}, "workload": map[string]any{"command": []string{"sleep", "infinity"},
			"servers": []map[string]any{{"name": "web", "port": 8080, "command": []string{"serve"}}}}})
	if w.Code != http.StatusCreated {
		t.Fatalf("submit: %d %s", w.Code, w.Body)
	}
	var run Run
	_ = json.Unmarshal(w.Body.Bytes(), &run)
	var got []RunServer
	err := s.db.Tx(ctx, store.Tenant("t1"), func(tx pgx.Tx) error {
		var err error
		got, err = s.listServersTx(ctx, tx, run.ID)
		return err
	})
	if err != nil || len(got) != 1 || !got[0].FromSpec || got[0].State != ServerStopped || got[0].StopReason != nil {
		t.Fatalf("at submit: %+v %v", got, err)
	}
	for epoch := 1; epoch <= 2; epoch++ {
		err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
			var sp pendingRun
			sp.ID, sp.TenantID, sp.Epoch = run.ID, "t1", epoch-1
			if err := tx.QueryRow(ctx, `SELECT spec FROM runs WHERE id = $1`, run.ID).Scan(&sp.Spec); err != nil {
				return err
			}
			return s.assign(ctx, tx, sp, &candidateHost{ID: "h1"})
		})
		if err != nil {
			t.Fatal(err)
		}
		sets := pendingServers(t, s, ctx)
		last := sets[len(sets)-1]
		if len(last.Servers) != 1 || last.Servers[0].Name != "web" || last.Servers[0].Port != 8080 {
			t.Fatalf("epoch %d: %+v", epoch, last)
		}
		execSQL(t, s, ctx, `UPDATE placements SET state = 'exited' WHERE run_id = $1`, run.ID)
	}
}

// Tickets: single use, bound to their Run and kind, expiring, and dead
// with the key that minted them.
func TestTickets(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	key := serversFixture(t, s, ctx)
	mint := func(kind string) Ticket {
		t.Helper()
		w := apiCall(t, s, key, http.MethodPost, "/v1/runs/"+r1+"/tickets", map[string]any{"kind": kind})
		if w.Code != http.StatusCreated {
			t.Fatalf("mint: %d %s", w.Code, w.Body)
		}
		var tk Ticket
		_ = json.Unmarshal(w.Body.Bytes(), &tk)
		return tk
	}
	tk := mint(TicketExec)
	if !strings.HasPrefix(tk.Ticket, "tkt_") || tk.RunID != r1 || time.Until(tk.ExpiresAt) > time.Minute+time.Second {
		t.Fatalf("ticket: %+v", tk)
	}
	if _, err := s.redeemTicket(ctx, tk.Ticket, r1, TicketPreview); err == nil {
		t.Fatal("redeemed as another kind")
	}
	if _, err := s.redeemTicket(ctx, tk.Ticket, "run_bbbbbbbbbbbbbbbb", TicketExec); err == nil {
		t.Fatal("redeemed for another run")
	}
	p, err := s.redeemTicket(ctx, tk.Ticket, r1, TicketExec)
	if err != nil || p.TenantID != "t1" || p.KeyID != "k1" || !p.Can("run") {
		t.Fatalf("redeem: %+v %v", p, err)
	}
	if _, err := s.redeemTicket(ctx, tk.Ticket, r1, TicketExec); err == nil {
		t.Fatal("redeemed twice")
	}
	old := mint(TicketExec)
	execSQL(t, s, ctx, `UPDATE stream_tickets SET expires_at = now() - interval '1 second' WHERE token_hash = $1`, ids.Hash(old.Ticket))
	if _, err := s.redeemTicket(ctx, old.Ticket, r1, TicketExec); err == nil {
		t.Fatal("redeemed expired")
	}
	revoked := mint(TicketExec)
	execSQL(t, s, ctx, `UPDATE api_keys SET revoked_at = now() WHERE id = 'k1'`)
	if _, err := s.redeemTicket(ctx, revoked.Ticket, r1, TicketExec); err == nil {
		t.Fatal("redeemed after its key was revoked")
	}
	execSQL(t, s, ctx, `UPDATE api_keys SET revoked_at = NULL WHERE id = 'k1'`)

	// Through the stream route: ?ticket= authenticates; a page of another
	// origin is refused.
	w := apiCall(t, s, "", http.MethodGet, "/v1/runs/"+r1+"/exec?ticket="+mint(TicketExec).Ticket, nil)
	if w.Code != http.StatusServiceUnavailable { // authenticated; the host has no connection here
		t.Fatalf("with a ticket: %d %s", w.Code, w.Body)
	}
	if w := apiCall(t, s, "", http.MethodGet, "/v1/runs/"+r1+"/exec?ticket=tkt_nope", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("bad ticket: %d %s", w.Code, w.Body)
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/runs/"+r1+"/exec", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Origin", "https://evil.example.com")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "bad_origin") {
		t.Fatalf("cross-origin: %d %s", rec.Code, rec.Body)
	}
	// A ticket works nowhere else.
	if w := apiCall(t, s, "", http.MethodGet, "/v1/runs/"+r1+"?ticket="+mint(TicketExec).Ticket, nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("ticket on another route: %d %s", w.Code, w.Body)
	}
}

// fakeRunner stands in for host h1's runner: it opens tunnels to the
// given address, relaying stream frames as a runner does.
func fakeRunner(t *testing.T, s *Server, target string) {
	t.Helper()
	c := &runnerConn{hostID: "h1", send: make(chan proto.Frame, 256), notify: make(chan struct{}, 1), done: make(chan struct{})}
	s.hub.mu.Lock()
	s.hub.conns["h1"] = c
	s.hub.mu.Unlock()
	t.Cleanup(func() {
		s.hub.mu.Lock()
		delete(s.hub.conns, "h1")
		s.hub.mu.Unlock()
		close(c.done)
	})
	conns := map[string]net.Conn{}
	go func() {
		for {
			select {
			case <-c.done:
				return
			case f := <-c.send:
				switch f.Type {
				case proto.MsgStreamOpen:
					conn, err := net.Dial("tcp", target)
					if err != nil {
						s.hub.route(f.Stream, proto.Frame{Type: proto.MsgStreamClose, Stream: f.Stream, Data: proto.Marshal(proto.StreamData{Error: err.Error()})})
						continue
					}
					conns[f.Stream] = conn
					id := f.Stream
					go func() {
						buf := make([]byte, 4096)
						for {
							n, err := conn.Read(buf)
							if n > 0 {
								s.hub.route(id, proto.Frame{Type: proto.MsgStreamData, Stream: id, Data: proto.Marshal(proto.StreamData{Data: append([]byte{}, buf[:n]...)})})
							}
							if err != nil {
								s.hub.route(id, proto.Frame{Type: proto.MsgStreamData, Stream: id, Data: proto.Marshal(proto.StreamData{EOF: true})})
								return
							}
						}
					}()
				case proto.MsgStreamData:
					var d proto.StreamData
					_ = json.Unmarshal(f.Data, &d)
					if conn := conns[f.Stream]; conn != nil {
						if d.EOF {
							conn.(*net.TCPConn).CloseWrite()
						} else {
							conn.Write(d.Data)
						}
					}
				case proto.MsgStreamClose:
					if conn := conns[f.Stream]; conn != nil {
						conn.Close()
						delete(conns, f.Stream)
					}
				}
			}
		}
	}()
}

// The preview listener end to end, with a fake runner: sign-in by ticket,
// the cookie, proxying (headers cleaned, cookies made host-only, the
// server's own host), status pages, and a server-sent event stream.
func TestPreviewProxy(t *testing.T) {
	s := testServer(t)
	s.cfg.PublicURL = "https://luxd.example.com"
	s.cfg.Preview = PreviewConfig{Domain: "lux.example.com", Auth: "ticket", HoldFor: 300 * time.Millisecond}
	ctx := context.Background()
	key := serversFixture(t, s, ctx)
	s.preview = newPreviews(s)
	if err := s.preview.init(ctx); err != nil {
		t.Fatal(err)
	}
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/events":
			w.Header().Set("Content-Type", "text/event-stream")
			for i := range 2 {
				fmt.Fprintf(w, "data: %d\n\n", i)
				w.(http.Flusher).Flush()
			}
		default:
			w.Header().Add("Set-Cookie", "app=1; Domain=lux.example.com; Path=/")
			_ = json.NewEncoder(w).Encode(map[string]any{"host": r.Host, "headers": r.Header, "path": r.URL.RequestURI()})
		}
	}))
	defer app.Close()
	fakeRunner(t, s, strings.TrimPrefix(app.URL, "http://"))

	host := "web-aaaaaaaaaaaaaaaa.lux.example.com"
	do := func(method, path string, hdr http.Header) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "https://"+host+path, nil)
		for k, v := range hdr {
			req.Header[k] = v
		}
		w := httptest.NewRecorder()
		s.preview.ServeHTTP(w, req)
		return w
	}
	// Unknown host: the unknown page.
	req := httptest.NewRequest(http.MethodGet, "https://nothing.lux.example.com/", nil)
	rec := httptest.NewRecorder()
	s.preview.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "No such preview") {
		t.Fatalf("unknown: %d %s", rec.Code, rec.Body)
	}
	// Not signed in: a browser is sent to sign in; anything else, 401.
	w := do(http.MethodGet, "/a?b=c", http.Header{"Accept": {"text/html"}})
	if loc := w.Header().Get("Location"); w.Code != http.StatusFound ||
		loc != "https://luxd.example.com/preview-auth?to="+url.QueryEscape("https://"+host+"/a?b=c") {
		t.Fatalf("challenge: %d %q", w.Code, loc)
	}
	if w := do(http.MethodPost, "/a", http.Header{"Accept": {"text/html"}}); w.Code != http.StatusUnauthorized {
		t.Fatalf("post: %d", w.Code)
	}
	if w := do(http.MethodGet, "/api", http.Header{"Accept": {"application/json"}}); w.Code != http.StatusUnauthorized {
		t.Fatalf("json: %d", w.Code)
	}
	// Sign in: a preview ticket for this Run becomes the cookie.
	mint := func(kind string) string {
		w := apiCall(t, s, key, http.MethodPost, "/v1/runs/"+r1+"/tickets", map[string]any{"kind": kind})
		var tk Ticket
		_ = json.Unmarshal(w.Body.Bytes(), &tk)
		return tk.Ticket
	}
	if w := do(http.MethodGet, "/.lux/auth?ticket="+mint(TicketExec)+"&to=/", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("an exec ticket signed in: %d", w.Code)
	}
	if w := do(http.MethodGet, "/.lux/auth?ticket="+mint(TicketPreview)+"&to=//evil.com/", nil); w.Code != http.StatusBadRequest {
		t.Fatalf("to another host: %d", w.Code)
	}
	w = do(http.MethodGet, "/.lux/auth?ticket="+mint(TicketPreview)+"&to=/a?b=c", nil)
	if w.Code != http.StatusFound || w.Header().Get("Location") != "/a?b=c" {
		t.Fatalf("sign in: %d %v", w.Code, w.Header())
	}
	cookie := w.Result().Cookies()[0]
	if cookie.Name != "__Host-lux_preview" || !cookie.Secure || !cookie.HttpOnly || cookie.Domain != "" || cookie.Path != "/" {
		t.Fatalf("cookie: %+v", cookie)
	}
	signed := http.Header{"Cookie": {cookie.Name + "=" + cookie.Value + "; mine=1"}}

	// No such server yet: unknown. Stopped: its page.
	if w := do(http.MethodGet, "/", signed); w.Code != http.StatusNotFound {
		t.Fatalf("no server: %d", w.Code)
	}
	execSQL(t, s, ctx, `INSERT INTO run_servers (tenant_id, run_id, name, port, state, stop_reason) VALUES ('t1', $1, 'web', 3000, 'stopped', 'migrated')`, r1)
	if w := do(http.MethodGet, "/", signed); w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "Server stopped") ||
		!strings.Contains(w.Body.String(), "migrated") || w.Header().Get("X-Lux-Preview") != "status" {
		t.Fatalf("stopped: %d %s", w.Code, w.Body)
	}
	execSQL(t, s, ctx, `UPDATE run_servers SET state = 'exited', exit_code = 3, error = 'boom <b>' WHERE name = 'web'`)
	if w := do(http.MethodGet, "/", signed); !strings.Contains(w.Body.String(), "Server exited") || !strings.Contains(w.Body.String(), "&lt;b&gt;") ||
		!strings.Contains(w.Body.String(), "<code>3</code>") {
		t.Fatalf("exited: %d %s", w.Code, w.Body)
	}
	// Starting: held for hold_for, then the starting page.
	execSQL(t, s, ctx, `UPDATE run_servers SET state = 'starting' WHERE name = 'web'`)
	start := time.Now()
	if w := do(http.MethodGet, "/", signed); !strings.Contains(w.Body.String(), "Starting") || time.Since(start) < 250*time.Millisecond {
		t.Fatalf("starting: %d %s after %s", w.Code, w.Body, time.Since(start))
	}

	// Ready: proxied.
	execSQL(t, s, ctx, `UPDATE run_servers SET state = 'ready' WHERE name = 'web'`)
	hdr := signed.Clone()
	hdr.Set("Authorization", "Bearer "+key)
	hdr.Set("Cf-Access-Jwt-Assertion", "x")
	w = do(http.MethodGet, "/x?y=1", hdr)
	if w.Code != http.StatusOK {
		t.Fatalf("proxied: %d %s", w.Code, w.Body)
	}
	var seen struct {
		Host    string
		Path    string
		Headers http.Header
	}
	_ = json.Unmarshal(w.Body.Bytes(), &seen)
	if seen.Host != "localhost:3000" || seen.Path != "/x?y=1" || seen.Headers.Get("X-Forwarded-Host") != host ||
		seen.Headers.Get("X-Forwarded-Proto") != "https" || seen.Headers.Get("X-Lux-User") != "ci" ||
		seen.Headers.Get("Authorization") != "" || seen.Headers.Get("Cf-Access-Jwt-Assertion") != "" || seen.Headers.Get("Cookie") != "mine=1" {
		t.Fatalf("what the server saw: %+v", seen)
	}
	if sc := w.Header().Values("Set-Cookie"); len(sc) != 1 || strings.Contains(strings.ToLower(sc[0]), "domain") {
		t.Fatalf("set-cookie: %q", sc)
	}
	// Server-sent events come through.
	w = do(http.MethodGet, "/events", signed)
	sc := bufio.NewScanner(strings.NewReader(w.Body.String()))
	var events []string
	for sc.Scan() {
		if d, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
			events = append(events, d)
		}
	}
	if fmt.Sprint(events) != "[0 1]" {
		t.Fatalf("events: %q", w.Body)
	}
	// Its activity is recorded.
	s.preview.flush(ctx, true)
	var last *time.Time
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT last_request_at FROM run_servers WHERE name = 'web'`).Scan(&last)
	})
	if err != nil || last == nil || time.Since(*last) > time.Minute {
		t.Fatalf("lastRequestAt: %v %v", last, err)
	}
	// A cookie for another Run's host is not this one's.
	other := "web-bbbbbbbbbbbbbbbb.lux.example.com"
	req = httptest.NewRequest(http.MethodGet, "https://"+other+"/", nil)
	req.Header.Set("Cookie", cookie.Name+"="+cookie.Value)
	req.Header.Set("Accept", "text/html")
	rec = httptest.NewRecorder()
	s.preview.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("another run's host: %d", rec.Code)
	}
	// The Run stops: its page.
	execSQL(t, s, ctx, `UPDATE runs SET state = 'stopped'`)
	if w := do(http.MethodGet, "/", signed); !strings.Contains(w.Body.String(), "not running") {
		t.Fatalf("run stopped: %d %s", w.Code, w.Body)
	}
}

// A WebSocket (a dev server's hot reload) goes through the preview proxy
// and its tunnel both ways.
func TestPreviewWebSocket(t *testing.T) {
	s := testServer(t)
	s.cfg.PublicURL = "https://luxd.example.com"
	s.cfg.Preview = PreviewConfig{Domain: "lux.example.com", Auth: "ticket", HoldFor: time.Second}
	ctx := context.Background()
	serversFixture(t, s, ctx)
	s.preview = newPreviews(s)
	if err := s.preview.init(ctx); err != nil {
		t.Fatal(err)
	}
	execSQL(t, s, ctx, `INSERT INTO run_servers (tenant_id, run_id, name, port, state) VALUES ('t1', $1, 'web', 3000, 'ready')`, r1)
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer c.CloseNow()
		for {
			typ, b, err := c.Read(r.Context())
			if err != nil {
				return
			}
			if err := c.Write(r.Context(), typ, append([]byte("echo: "), b...)); err != nil {
				return
			}
		}
	}))
	defer app.Close()
	fakeRunner(t, s, strings.TrimPrefix(app.URL, "http://"))
	cookie := s.preview.sign(previewUser{RunID: r1, TenantID: "t1", User: "ci", Exp: time.Now().Add(time.Hour).Unix()})
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Host = "web-aaaaaaaaaaaaaaaa.lux.example.com"
		s.preview.ServeHTTP(w, r)
	}))
	defer front.Close()
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(wctx, "ws"+strings.TrimPrefix(front.URL, "http")+"/hmr", &websocket.DialOptions{
		HTTPHeader: http.Header{"Cookie": {previewCookie + "=" + cookie}}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	for _, msg := range []string{"one", "two"} {
		if err := c.Write(wctx, websocket.MessageText, []byte(msg)); err != nil {
			t.Fatal(err)
		}
		_, b, err := c.Read(wctx)
		if err != nil || string(b) != "echo: "+msg {
			t.Fatalf("%s: %q %v", msg, b, err)
		}
	}
}

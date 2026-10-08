package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/ids"
	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/spec"
	"github.com/marcioapm/lux/internal/store"
)

// wakeFixture: serversFixture with previews on lux.example.com (tickets),
// and the API key of tenant t2.
func wakeFixture(t *testing.T) (s *Server, ctx context.Context, key, key2 string) {
	t.Helper()
	s = testServer(t)
	s.cfg.PublicURL = "https://luxd.example.com"
	s.cfg.Preview = PreviewConfig{Domain: "lux.example.com", Auth: "ticket", HoldFor: 200 * time.Millisecond}
	ctx = context.Background()
	key = serversFixture(t, s, ctx)
	key2 = ids.Secret("luxk")
	execSQL(t, s, ctx, `INSERT INTO api_keys (id, tenant_id, name, key_hash, scopes) VALUES ('k2', 't2', 'other', $1, ARRAY['run'])`, ids.Hash(key2))
	s.preview = newPreviews(s)
	if err := s.preview.init(ctx); err != nil {
		t.Fatal(err)
	}
	return s, ctx, key, key2
}

func createSrv(t *testing.T, s *Server, key string, body map[string]any) TenantServer {
	t.Helper()
	w := apiCall(t, s, key, http.MethodPost, "/v1/servers", body)
	if w.Code != http.StatusCreated {
		t.Fatalf("create %v: %d %s", body, w.Code, w.Body)
	}
	var out TenantServer
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return out
}

func getSrv(t *testing.T, s *Server, key, id string) TenantServer {
	t.Helper()
	w := apiCall(t, s, key, http.MethodGet, "/v1/servers/"+id, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("get %s: %d %s", id, w.Code, w.Body)
	}
	var out TenantServer
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return out
}

// serverEventsOf lists a server's events of a type (all, with "").
func serverEventsOf(t *testing.T, s *Server, ctx context.Context, id, typ string) []map[string]any {
	t.Helper()
	var out []map[string]any
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT data FROM run_events WHERE server_id = $1 AND ($2 = '' OR type = $2) ORDER BY id`, id, typ)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, pgx.RowTo[map[string]any])
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// previewGet asks the preview listener for path on host, signed in with
// cookie ("" for none).
func previewGet(s *Server, host, path, cookie string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "https://"+host+path, nil)
	req.Header.Set("Accept", "text/html")
	if cookie != "" {
		req.Header.Set("Cookie", previewCookie+"="+cookie)
	}
	w := httptest.NewRecorder()
	s.preview.ServeHTTP(w, req)
	return w
}

func cookieFor(s *Server, id string) string {
	return s.preview.sign(previewUser{ServerID: id, TenantID: "t1", User: "ada@example.com", Exp: time.Now().Add(time.Hour).Unix()})
}

// Creating servers: an owner's hostname under the domain, unique across
// luxd, validated; a generated one otherwise, stable and without a run id;
// other tenants see none of it.
func TestServerHostnames(t *testing.T) {
	s, _, key, key2 := wakeFixture(t)
	a := createSrv(t, s, key, map[string]any{"name": "web", "port": 3000, "command": []string{"serve"}, "wake": "request",
		"hostname": "web.t123.p9.lux.example.com", "labels": map[string]string{"pr": "9"}})
	if a.Hostname == nil || *a.Hostname != "web.t123.p9.lux.example.com" || *a.URL != "https://web.t123.p9.lux.example.com" ||
		a.Lifetime != LifetimeOwner || a.State != SrvAsleep || a.RunID != nil || a.Owner != "k1" ||
		a.IdleAfter.Duration != DefaultIdleAfter || a.WakeTimeout.Duration != DefaultWakeTimeout || a.ExpireAfter == nil {
		t.Fatalf("created: %+v", a)
	}
	b := createSrv(t, s, key, map[string]any{"name": "api", "port": 8080})
	if b.Hostname == nil || *b.Hostname != "api-"+b.ID[4:12]+".lux.example.com" {
		t.Fatalf("generated hostname: %+v", b.Hostname)
	}
	for _, c := range []struct {
		host string
		code int
		err  string
	}{
		{"web.t123.p9.lux.example.com", http.StatusConflict, "hostname_taken"},
		{"WEB.T123.P9.lux.example.com.", http.StatusConflict, "hostname_taken"},
		{"web.example.com", http.StatusUnprocessableEntity, "not under the preview domain"},
		{"lux.example.com", http.StatusUnprocessableEntity, "not under the preview domain"},
		{"we_b.lux.example.com", http.StatusUnprocessableEntity, "not a DNS label"},
		{"-x.lux.example.com", http.StatusUnprocessableEntity, "not a DNS label"},
	} {
		w := apiCall(t, s, key, http.MethodPost, "/v1/servers", map[string]any{"name": "x", "port": 1, "hostname": c.host})
		if w.Code != c.code || !strings.Contains(w.Body.String(), c.err) {
			t.Errorf("%s: %d %s", c.host, w.Code, w.Body)
		}
	}
	// Unique across luxd: another tenant cannot take it either.
	if w := apiCall(t, s, key2, http.MethodPost, "/v1/servers", map[string]any{"name": "web", "port": 1, "hostname": "web.t123.p9.lux.example.com"}); w.Code != http.StatusConflict {
		t.Fatalf("other tenant, same hostname: %d %s", w.Code, w.Body)
	}
	// Tenant isolation: not seen, not changed, not attached.
	if w := apiCall(t, s, key2, http.MethodGet, "/v1/servers/"+a.ID, nil); w.Code != http.StatusNotFound {
		t.Fatalf("other tenant get: %d", w.Code)
	}
	var list struct{ Servers []TenantServer }
	_ = json.Unmarshal(apiCall(t, s, key2, http.MethodGet, "/v1/servers", nil).Body.Bytes(), &list)
	if len(list.Servers) != 0 {
		t.Fatalf("other tenant list: %+v", list)
	}
	for _, c := range []struct{ method, path string }{
		{http.MethodPatch, "/v1/servers/" + a.ID}, {http.MethodDelete, "/v1/servers/" + a.ID},
		{http.MethodPost, "/v1/servers/" + a.ID + "/detach"}, {http.MethodPost, "/v1/servers/" + a.ID + "/stop"},
		{http.MethodPost, "/v1/servers/" + a.ID + "/tickets"},
	} {
		if w := apiCall(t, s, key2, c.method, c.path, map[string]any{}); w.Code != http.StatusNotFound {
			t.Errorf("other tenant %s %s: %d %s", c.method, c.path, w.Code, w.Body)
		}
	}
	if w := apiCall(t, s, key, http.MethodPost, "/v1/servers/"+a.ID+"/attach", map[string]any{"runId": "run_cccccccccccccccc"}); w.Code != http.StatusNotFound {
		t.Fatalf("attach to no run: %d", w.Code)
	}
	// Filters: label, wake, state, hostname.
	_ = json.Unmarshal(apiCall(t, s, key, http.MethodGet, "/v1/servers?label=pr=9", nil).Body.Bytes(), &list)
	if len(list.Servers) != 1 || list.Servers[0].ID != a.ID {
		t.Fatalf("by label: %+v", list.Servers)
	}
	_ = json.Unmarshal(apiCall(t, s, key, http.MethodGet, "/v1/servers?state=asleep", nil).Body.Bytes(), &list)
	if len(list.Servers) != 1 || list.Servers[0].ID != a.ID {
		t.Fatalf("by state: %+v", list.Servers)
	}
	_ = json.Unmarshal(apiCall(t, s, key, http.MethodGet, "/v1/servers?hostname=web.t123.p9.lux.example.com", nil).Body.Bytes(), &list)
	if len(list.Servers) != 1 || list.Servers[0].ID != a.ID {
		t.Fatalf("by hostname: %+v", list.Servers)
	}
	// A wakeable server outlives Runs: lifetime run is refused for it.
	if w := apiCall(t, s, key, http.MethodPost, "/v1/servers", map[string]any{"name": "y", "port": 1, "wake": "request", "lifetime": "run", "runId": r1}); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("wakeable lifetime run: %d", w.Code)
	}
}

// One wake per wake: many concurrent signed-in requests to an asleep
// server emit one server.wake_requested; unauthenticated ones none; after
// wakeTimeout the state is no answer and the next request asks again; a
// ready server resolves the wake.
func TestWakeOncePerWake(t *testing.T) {
	s, ctx, key, _ := wakeFixture(t)
	execSQL(t, s, ctx, `UPDATE runs SET state = 'stopped'`)
	execSQL(t, s, ctx, `UPDATE placements SET state = 'exited'`)
	sv := createSrv(t, s, key, map[string]any{"name": "web", "port": 3000, "command": []string{"serve"}, "wake": "request",
		"hostname": "web.pr1.lux.example.com", "runId": r1})
	if sv.State != SrvAsleep || sv.RunID == nil {
		t.Fatalf("attached to a stopped run: %+v", sv)
	}
	host := "web.pr1.lux.example.com"
	// Not signed in: to sign-in, and no wake.
	for range 3 {
		if w := previewGet(s, host, "/", ""); w.Code != http.StatusFound {
			t.Fatalf("unauthenticated: %d", w.Code)
		}
	}
	if n := len(serverEventsOf(t, s, ctx, sv.ID, "server.wake_requested")); n != 0 {
		t.Fatalf("unauthenticated requests woke it: %d", n)
	}
	cookie := cookieFor(s, sv.ID)
	var wg sync.WaitGroup
	codes := make(chan int, 40)
	for i := range 40 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := previewGet(s, host, "/goals?tab="+string(rune('a'+i%26)), cookie)
			codes <- w.Code
			if !strings.Contains(w.Body.String(), "Asked the orchestrator to start it") {
				t.Errorf("waking page: %s", w.Body)
			}
		}()
	}
	wg.Wait()
	close(codes)
	for c := range codes {
		if c != http.StatusServiceUnavailable {
			t.Fatalf("waking page code %d", c)
		}
	}
	wakes := serverEventsOf(t, s, ctx, sv.ID, "server.wake_requested")
	if len(wakes) != 1 || wakes[0]["by"] != "ada@example.com" || !strings.HasPrefix(wakes[0]["path"].(string), "/goals") ||
		wakes[0]["runId"] != r1 || wakes[0]["serverId"] != sv.ID || wakes[0]["host"] != "web.pr1" {
		t.Fatalf("wakes: %+v", wakes)
	}
	if got := getSrv(t, s, key, sv.ID); got.State != SrvWaking || got.Wakes != 1 || got.WakeRequested == nil {
		t.Fatalf("waking: %+v", got)
	}
	// The page's own polls never wake.
	for range 3 {
		if w := previewGet(s, host, "/.lux/wait?to=/goals", cookie); w.Code != http.StatusServiceUnavailable {
			t.Fatalf("wait: %d", w.Code)
		}
	}
	// Timed out: no answer, and Ask again (or the next request) asks again.
	execSQL(t, s, ctx, `UPDATE run_servers SET wake_requested_at = now() - interval '6 minutes' WHERE id = $1`, sv.ID)
	if got := getSrv(t, s, key, sv.ID); got.State != SrvNoAnswer {
		t.Fatalf("after the timeout: %s", got.State)
	}
	if w := previewGet(s, host, "/.lux/wait?to=/", cookie); !strings.Contains(w.Body.String(), "No answer from its owner") ||
		!strings.Contains(w.Body.String(), "Ask again") {
		t.Fatalf("no answer page: %s", w.Body)
	}
	if n := len(serverEventsOf(t, s, ctx, sv.ID, "server.wake_requested")); n != 1 {
		t.Fatalf("a poll asked again: %d", n)
	}
	// Ask again is a POST: a GET (a link prefetcher) only goes to the page.
	if w := previewGet(s, host, "/.lux/wake?to=/goals", cookie); w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/.lux/wait?to=%2Fgoals" {
		t.Fatalf("GET wake: %d %v", w.Code, w.Header())
	}
	if n := len(serverEventsOf(t, s, ctx, sv.ID, "server.wake_requested")); n != 1 {
		t.Fatalf("a GET asked again: %d", n)
	}
	req := httptest.NewRequest(http.MethodPost, "https://"+host+"/.lux/wake", strings.NewReader("to=%2Fgoals"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", previewCookie+"="+cookie)
	w := httptest.NewRecorder()
	s.preview.ServeHTTP(w, req)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/.lux/wait?to=%2Fgoals" {
		t.Fatalf("ask again: %d %v", w.Code, w.Header())
	}
	if n := len(serverEventsOf(t, s, ctx, sv.ID, "server.wake_requested")); n != 2 {
		t.Fatalf("ask again did not ask: %d", n)
	}
	previewGet(s, host, "/", cookie)
	if n := len(serverEventsOf(t, s, ctx, sv.ID, "server.wake_requested")); n != 2 {
		t.Fatalf("asked twice in one wake: %d", n)
	}
	// The owner resumes the Run; the server becomes ready: the wake is
	// resolved, and the page drops into the app.
	execSQL(t, s, ctx, `UPDATE runs SET state = 'running', current_epoch = 2`)
	execSQL(t, s, ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state) VALUES ('p2', 't1', $1, 'h1', 2, 'running')`, r1)
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error { return s.startAttachedServers(ctx, tx, "t1", r1, 2, afterSyncNever) })
	if err != nil {
		t.Fatal(err)
	}
	if got := getSrv(t, s, key, sv.ID); got.State != SrvWaking || got.Process != ServerStarting {
		t.Fatalf("starting: %+v", got)
	}
	serverReport(t, s, ctx, 2, map[string]any{"name": "web", "gen": serverGen(t, s, ctx, "web"), "state": "ready"})
	if got := getSrv(t, s, key, sv.ID); got.State != SrvReady || got.WakeRequested != nil || got.IdleAt == nil {
		t.Fatalf("ready: %+v", got)
	}
	if w := previewGet(s, host, "/.lux/wait?to=/goals", cookie); w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/goals" {
		t.Fatalf("into the app: %d %v", w.Code, w.Header())
	}
}

// Idle: once per idle period, reset by a request; never for a server not
// ready or whose Run is not running.
func TestIdleOnce(t *testing.T) {
	s, ctx, key, _ := wakeFixture(t)
	sv := createSrv(t, s, key, map[string]any{"name": "web", "port": 3000, "command": []string{"serve"}, "wake": "request",
		"idleAfter": "1m", "runId": r1})
	execSQL(t, s, ctx, `UPDATE run_servers SET state = 'ready', ready_since = now() - interval '5 minutes',
		last_request_at = now() - interval '2 minutes' WHERE id = $1`, sv.ID)
	idles := func() int { return len(serverEventsOf(t, s, ctx, sv.ID, "server.idle")) }
	idleCheck := func() {
		t.Helper()
		if err := s.checkIdle(ctx); err != nil {
			t.Fatal(err)
		}
	}
	for range 3 {
		idleCheck()
	}
	if idles() != 1 {
		t.Fatalf("idle events: %d", idles())
	}
	ev := serverEventsOf(t, s, ctx, sv.ID, "server.idle")[0]
	if ev["idleAfter"] != "1m0s" || ev["runId"] != r1 || ev["lastRequestAt"] == nil {
		t.Fatalf("idle event: %+v", ev)
	}
	// A request (activity) resets it: not idle until another minute
	// without one, then idle again, once.
	s.preview.touch(sv.ID)
	s.preview.flush(ctx, true)
	idleCheck()
	if idles() != 1 {
		t.Fatalf("idle right after a request: %d", idles())
	}
	if got := getSrv(t, s, key, sv.ID); got.IdleAt == nil || time.Until(*got.IdleAt) < 50*time.Second {
		t.Fatalf("idleAt after a request: %+v", got.IdleAt)
	}
	// A minute on: the request, and the last idle before it, as long ago.
	execSQL(t, s, ctx, `UPDATE run_servers SET last_request_at = last_request_at - interval '61 seconds',
		idle_notified_at = idle_notified_at - interval '61 seconds' WHERE id = $1`, sv.ID)
	idleCheck()
	idleCheck()
	if idles() != 2 {
		t.Fatalf("second idle period: %d", idles())
	}
	// Not ready, or its Run not running: never idle.
	execSQL(t, s, ctx, `UPDATE run_servers SET last_request_at = now() - interval '1 hour', idle_notified_at = NULL, state = 'starting' WHERE id = $1`, sv.ID)
	idleCheck()
	execSQL(t, s, ctx, `UPDATE run_servers SET state = 'ready' WHERE id = $1`, sv.ID)
	execSQL(t, s, ctx, `UPDATE runs SET state = 'stopping'`)
	idleCheck()
	if idles() != 2 {
		t.Fatalf("idle while not serving: %d", idles())
	}
}

// Attach and detach while the Run runs and while it is stopped; a second
// Run cannot take an attached server; every new placement starts every
// attached server that is up, and leaves one stopped by request.
func TestAttachDetachAndEveryPlacement(t *testing.T) {
	s, ctx, key, _ := wakeFixture(t)
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, spec, state, current_epoch) VALUES ('run_bbbbbbbbbbbbbbbb', 't1', '{}', 'stopped', 1)`)
	sv := createSrv(t, s, key, map[string]any{"name": "web", "port": 3000, "command": []string{"serve"}, "afterSync": []string{"make"}})
	// Attach to the running Run: started now, the placement is sent it.
	w := apiCall(t, s, key, http.MethodPost, "/v1/servers/"+sv.ID+"/attach", map[string]any{"runId": r1})
	var got TenantServer
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if w.Code != http.StatusOK || got.Process != ServerStarting || got.RunID == nil || *got.RunID != r1 {
		t.Fatalf("attach running: %d %s", w.Code, w.Body)
	}
	sets := pendingServers(t, s, ctx)
	if last := sets[len(sets)-1]; len(last.Servers) != 1 || last.Servers[0].Name != "web" || last.Servers[0].Command[0] != "serve" {
		t.Fatalf("set after attach: %+v", last)
	}
	// Another Run cannot take it.
	if w := apiCall(t, s, key, http.MethodPost, "/v1/servers/"+sv.ID+"/attach", map[string]any{"runId": "run_bbbbbbbbbbbbbbbb"}); w.Code != http.StatusConflict ||
		!strings.Contains(w.Body.String(), `"attached"`) {
		t.Fatalf("attach elsewhere: %d %s", w.Code, w.Body)
	}
	// Detach: its command stops (the set no longer has it), the Run runs on.
	if w := apiCall(t, s, key, http.MethodPost, "/v1/servers/"+sv.ID+"/detach", nil); w.Code != http.StatusOK {
		t.Fatalf("detach: %d %s", w.Code, w.Body)
	}
	sets = pendingServers(t, s, ctx)
	if last := sets[len(sets)-1]; len(last.Servers) != 0 {
		t.Fatalf("set after detach: %+v", last)
	}
	var runState string
	systemScan(t, s, `SELECT state FROM runs WHERE id = $1`, []any{r1}, &runState)
	if runState != StateRunning {
		t.Fatalf("detach touched the run: %s", runState)
	}
	// A lifetime-run server is its Run's: it cannot be detached.
	runOnly := createSrv(t, s, key, map[string]any{"name": "own", "port": 3001, "runId": r1})
	if w := apiCall(t, s, key, http.MethodPost, "/v1/servers/"+runOnly.ID+"/detach", nil); w.Code != http.StatusConflict ||
		!strings.Contains(w.Body.String(), `"lifetime_run"`) {
		t.Fatalf("detach a lifetime-run server: %d %s", w.Code, w.Body)
	}
	if got := getSrv(t, s, key, runOnly.ID); got.RunID == nil || *got.RunID != r1 {
		t.Fatalf("after a refused detach: %+v", got.RunID)
	}
	// Attach to the stopped Run: nothing starts; at its next placement it
	// does, with afterSync when that placement syncs.
	if w := apiCall(t, s, key, http.MethodPost, "/v1/servers/"+sv.ID+"/attach", map[string]any{"runId": "run_bbbbbbbbbbbbbbbb"}); w.Code != http.StatusOK {
		t.Fatalf("attach stopped: %d %s", w.Code, w.Body)
	}
	if got := getSrv(t, s, key, sv.ID); got.Process != ServerStopped || got.Desired != "up" {
		t.Fatalf("attached to a stopped run: %+v", got)
	}
	// A run server added at runtime (not from the spec) comes back too.
	if w := apiCall(t, s, key, http.MethodPost, "/v1/runs/run_bbbbbbbbbbbbbbbb/servers", map[string]any{"name": "extra", "port": 4000,
		"command": []string{"x"}, "start": false}); w.Code != http.StatusCreated {
		t.Fatalf("add: %d %s", w.Code, w.Body)
	}
	assignB := func(epoch int, sync bool) proto.Servers {
		t.Helper()
		err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
			r := pendingRun{ID: "run_bbbbbbbbbbbbbbbb", TenantID: "t1", Epoch: epoch - 1}
			if sync {
				r.PendingSync = []proto.SyncRef{{Repo: "app", Ref: "main"}}
			}
			return s.assign(ctx, tx, r, &candidateHost{ID: "h1"})
		})
		if err != nil {
			t.Fatal(err)
		}
		sets := pendingServers(t, s, ctx)
		execSQL(t, s, ctx, `UPDATE placements SET state = 'exited' WHERE run_id = 'run_bbbbbbbbbbbbbbbb'`)
		return sets[len(sets)-1]
	}
	set := assignB(2, true)
	names := map[string][]string{}
	for _, x := range set.Servers {
		names[x.Name] = x.Command
	}
	if len(names) != 2 || names["web"][0] != "/bin/sh" || !strings.Contains(names["web"][2], "'make' && exec 'serve'") || names["extra"][0] != "x" {
		t.Fatalf("placement 2: %+v", set)
	}
	// The placement ends (a migration); the next starts them again, without
	// afterSync when it does not sync.
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		return stopServersAtEnd(ctx, tx, "t1", "run_bbbbbbbbbbbbbbbb", 2, "migrated")
	})
	if err != nil {
		t.Fatal(err)
	}
	// Stopped by request: stays down on the next placement.
	execSQL(t, s, ctx, `UPDATE run_servers SET stop_reason = 'stopped' WHERE name = 'extra'`)
	set = assignB(3, false)
	if len(set.Servers) != 1 || set.Servers[0].Name != "web" || set.Servers[0].Command[0] != "serve" {
		t.Fatalf("placement 3: %+v", set)
	}
	// Attached to the running Run, detached, attached to the stopped one,
	// started at placement 2, stopped by the move, started at placement 3.
	var types string
	systemScan(t, s, `SELECT string_agg(type || coalesce(':' || (data->>'state'), ''), ' ' ORDER BY id) FROM run_events WHERE server_id = $1`,
		[]any{sv.ID}, &types)
	if want := "server.created server.attached server.state:starting server.state:stopped server.detached " +
		"server.attached server.state:starting server.state:stopped server.state:starting"; types != want {
		t.Fatalf("events:\n got %s\nwant %s", types, want)
	}
}

// Lifetimes: a Run's lifetime-run servers go when it is terminated, not
// when it fails or succeeds (it can be resumed); owner servers are
// detached and stay.
// Deleting a server detaches it first; its hostname is gone. expireAfter
// deletes an owner server unrequested for that long.
func TestServerLifetimes(t *testing.T) {
	s, ctx, key, _ := wakeFixture(t)
	runServer := createSrv(t, s, key, map[string]any{"name": "a", "port": 3000, "runId": r1})
	owner := createSrv(t, s, key, map[string]any{"name": "b", "port": 3001, "runId": r1, "lifetime": "owner", "hostname": "b.lux.example.com"})
	if runServer.Lifetime != LifetimeRun || owner.Lifetime != LifetimeOwner {
		t.Fatalf("lifetimes: %s %s", runServer.Lifetime, owner.Lifetime)
	}
	set := func(state string) error {
		return s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error { return setRunState(ctx, tx, "t1", r1, state, "", 1) })
	}
	if err := set(StateFailed); err != nil {
		t.Fatal(err)
	}
	getSrv(t, s, key, runServer.ID)
	if err := set(StateSucceeded); err != nil {
		t.Fatal(err)
	}
	if got := getSrv(t, s, key, owner.ID); got.RunID == nil {
		t.Fatalf("owner server detached by a success, which a resume can undo: %+v", got)
	}
	getSrv(t, s, key, runServer.ID)
	if err := set(StateTerminated); err != nil {
		t.Fatal(err)
	}
	if w := apiCall(t, s, key, http.MethodGet, "/v1/servers/"+runServer.ID, nil); w.Code != http.StatusNotFound {
		t.Fatalf("run server after terminate: %d", w.Code)
	}
	if got := getSrv(t, s, key, owner.ID); got.RunID != nil {
		t.Fatalf("owner server after terminate: %+v", got)
	}
	if ev := serverEventsOf(t, s, ctx, runServer.ID, "server.deleted"); len(ev) != 1 || ev[0]["reason"] != "run terminated" {
		t.Fatalf("deleted event: %+v", ev)
	}
	// Owner deletion: 404, "This preview is gone".
	cookie := cookieFor(s, owner.ID)
	if w := apiCall(t, s, key, http.MethodDelete, "/v1/servers/"+owner.ID, nil); w.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", w.Code, w.Body)
	}
	if w := previewGet(s, "b.lux.example.com", "/", cookie); w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "This preview is gone") {
		t.Fatalf("after delete: %d %s", w.Code, w.Body)
	}
	// Expiry.
	exp := createSrv(t, s, key, map[string]any{"name": "c", "port": 1, "expireAfter": "1h"})
	never := createSrv(t, s, key, map[string]any{"name": "d", "port": 1, "expireAfter": "0s"})
	visited := createSrv(t, s, key, map[string]any{"name": "e", "port": 1, "expireAfter": "1h"})
	execSQL(t, s, ctx, `UPDATE run_servers SET created_at = now() - interval '2 hours' WHERE id IN ($1, $2, $3)`, exp.ID, never.ID, visited.ID)
	// Created two hours ago, requested ten minutes ago: an hour from that request.
	execSQL(t, s, ctx, `UPDATE run_servers SET last_request_at = now() - interval '10 minutes' WHERE id = $1`, visited.ID)
	if err := s.expireServers(ctx); err != nil {
		t.Fatal(err)
	}
	if w := apiCall(t, s, key, http.MethodGet, "/v1/servers/"+exp.ID, nil); w.Code != http.StatusNotFound {
		t.Fatalf("expired: %d", w.Code)
	}
	getSrv(t, s, key, never.ID)
	if got := getSrv(t, s, key, visited.ID); got.ExpiresAt == nil || got.LastRequestAt == nil ||
		!got.ExpiresAt.Equal(got.LastRequestAt.Add(time.Hour)) {
		t.Fatalf("expiry from the last request: %+v %+v", got.ExpiresAt, got.LastRequestAt)
	}
	if ev := serverEventsOf(t, s, ctx, exp.ID, "server.expired"); len(ev) != 1 {
		t.Fatalf("expired event: %+v", ev)
	}
}

// The feed carries server events, resumable by Last-Event-ID; a server
// event of an unattached server has a null runId; another tenant's feed
// has none of them.
func TestFeedServerEvents(t *testing.T) {
	s, ctx, key, _ := wakeFixture(t)
	var start int64
	systemScan(t, s, `SELECT coalesce(max(id), 0) FROM run_events`, nil, &start)
	sv := createSrv(t, s, key, map[string]any{"name": "web", "port": 3000, "wake": "request", "hostname": "w.lux.example.com"})
	evs, err := s.feedAfter(ctx, Principal{TenantID: "t1", Scopes: []string{"read"}}, start)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].Type != "server.created" || evs[0].RunID != nil || evs[0].ServerID != sv.ID ||
		evs[0].Data["hostname"] != "w.lux.example.com" || evs[0].Data["url"] != "https://w.lux.example.com" || evs[0].Data["name"] != "web" {
		t.Fatalf("feed: %+v", evs)
	}
	b, _ := json.Marshal(evs[0])
	if !strings.Contains(string(b), `"runId":null`) || !strings.Contains(string(b), `"serverId":"`+sv.ID+`"`) {
		t.Fatalf("feed json: %s", b)
	}
	if other, _ := s.feedAfter(ctx, Principal{TenantID: "t2", Scopes: []string{"read"}}, start); len(other) != 0 {
		t.Fatalf("other tenant's feed: %+v", other)
	}
	// Resumed after the first: the next ones only.
	if _, err := s.requestWake(ctx, sv.ID, "ada@example.com", "/x"); err != nil {
		t.Fatal(err)
	}
	evs, _ = s.feedAfter(ctx, Principal{TenantID: "t1", Scopes: []string{"read"}}, evs[0].ID)
	if len(evs) != 1 || evs[0].Type != "server.wake_requested" || evs[0].Data["path"] != "/x" {
		t.Fatalf("resumed feed: %+v", evs)
	}
	// Through the route, with Last-Event-ID.
	req := httptest.NewRequest(http.MethodGet, "/v1/events?follow=false", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Last-Event-ID", itoa(start))
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if !strings.Contains(w.Body.String(), "server.created") || !strings.Contains(w.Body.String(), "server.wake_requested") {
		t.Fatalf("SSE: %s", w.Body)
	}
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

// A resume's sync is validated, kept for the next placement, sent in its
// assignment (servers with afterSync run it first); a running Run's sync is
// sent to its placement, and sync.done with a moved checkout restarts the
// servers with afterSync only.
func TestSyncRequests(t *testing.T) {
	s, ctx, key, _ := wakeFixture(t)
	execSQL(t, s, ctx, `UPDATE runs SET spec = '{"workload": {"workdir": "/w"}, "git": {"repositories": [{"name": "app", "url": "https://x/app.git", "path": "/w/app"}]}}'`)
	hot := createSrv(t, s, key, map[string]any{"name": "hot", "port": 3000, "command": []string{"serve"}, "runId": r1})
	cold := createSrv(t, s, key, map[string]any{"name": "cold", "port": 3001, "command": []string{"serve"}, "afterSync": []string{"npm", "ci"}, "runId": r1})
	// A running Run: bad requests refused; a good one sent to its host.
	for _, c := range []struct {
		body map[string]any
		want int
	}{
		{map[string]any{"sync": []map[string]string{{"repo": "nope", "ref": "main"}}}, http.StatusUnprocessableEntity},
		{map[string]any{"sync": []map[string]string{{"repo": "app", "ref": "-x"}}}, http.StatusUnprocessableEntity},
		{map[string]any{"sync": []map[string]string{}}, http.StatusUnprocessableEntity},
		{map[string]any{"sync": []map[string]string{{"repo": "app", "ref": "feat/x"}}, "requestId": "s1"}, http.StatusAccepted},
	} {
		if w := apiCall(t, s, key, http.MethodPost, "/v1/runs/"+r1+"/sync", c.body); w.Code != c.want {
			t.Fatalf("sync %v: %d %s", c.body, w.Code, w.Body)
		}
	}
	var msg proto.Sync
	systemScan(t, s, `SELECT payload FROM host_messages WHERE type = 'sync' ORDER BY id DESC LIMIT 1`, nil, &msg)
	if msg.RequestID != "s1" || len(msg.Repos) != 1 || msg.Repos[0].Ref != "feat/x" {
		t.Fatalf("sync message: %+v", msg)
	}
	gens := func() (int64, int64) {
		var h, c int64
		systemScan(t, s, `SELECT gen FROM run_servers WHERE id = $1`, []any{hot.ID}, &h)
		systemScan(t, s, `SELECT gen FROM run_servers WHERE id = $1`, []any{cold.ID}, &c)
		return h, c
	}
	hot0, cold0 := gens()
	report := func(changed bool) {
		f := s.handleReport(ctx, "h1", proto.Frame{Type: proto.MsgRunEvent, ID: 9, RunID: r1, Epoch: 1,
			Data: proto.Marshal(proto.RunEvent{Type: proto.EvSyncDone, Data: map[string]any{"requestId": "s1", "changed": changed}})})
		if f.Type != proto.MsgAck {
			t.Fatalf("sync.done: %s", f.Data)
		}
	}
	report(false)
	if h, c := gens(); h != hot0 || c != cold0 {
		t.Fatal("restarted though nothing moved")
	}
	report(true)
	h1, c1 := gens()
	if h1 != hot0 || c1 == cold0 {
		t.Fatalf("after a moved checkout: hot %d→%d, cold %d→%d", hot0, h1, cold0, c1)
	}
	sets := pendingServers(t, s, ctx)
	for _, sv := range sets[len(sets)-1].Servers {
		if sv.Name == "cold" && (sv.Command[0] != "/bin/sh" || !strings.Contains(sv.Command[2], "'npm' 'ci' && exec 'serve'")) {
			t.Fatalf("cold's command: %v", sv.Command)
		}
		if sv.Name == "hot" && sv.Command[0] != "serve" {
			t.Fatalf("hot's command: %v", sv.Command)
		}
	}
	// Not running: 409; resume with sync instead.
	execSQL(t, s, ctx, `UPDATE runs SET state = 'stopped'`)
	execSQL(t, s, ctx, `UPDATE placements SET state = 'exited'`)
	if w := apiCall(t, s, key, http.MethodPost, "/v1/runs/"+r1+"/sync", map[string]any{"sync": []map[string]string{{"repo": "app", "ref": "main"}}}); w.Code != http.StatusConflict {
		t.Fatalf("sync stopped: %d", w.Code)
	}
	if w := apiCall(t, s, key, http.MethodPost, "/v1/runs/"+r1+"/resume", map[string]any{"sync": []map[string]string{{"repo": "zzz", "ref": "main"}}}); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("resume with a bad sync: %d %s", w.Code, w.Body)
	}
	if w := apiCall(t, s, key, http.MethodPost, "/v1/runs/"+r1+"/resume", map[string]any{"sync": []map[string]string{{"repo": "app", "ref": "abc123"}}}); w.Code != http.StatusAccepted {
		t.Fatalf("resume with sync: %d %s", w.Code, w.Body)
	}
	var pending []proto.SyncRef
	systemScan(t, s, `SELECT pending_sync FROM runs WHERE id = $1`, []any{r1}, &pending)
	if len(pending) != 1 || pending[0].Ref != "abc123" {
		t.Fatalf("pending sync: %+v", pending)
	}
}

// A sync's mode, on POST /sync and a resume's sync: a known one is
// carried to the runner (the sync message, the next assignment); an
// unknown one is refused, named. On the assignment, servers run afterSync
// first for a move, only if a checkout moved for a fast-forward (the shim
// decides: UnmovedCommand), and never for a fetch.
func TestSyncModes(t *testing.T) {
	s, ctx, key, _ := wakeFixture(t)
	execSQL(t, s, ctx, `UPDATE runs SET spec = '{"workload": {"workdir": "/w"}, "git": {"repositories": [{"name": "app", "url": "https://x/app.git", "path": "/w/app"}, {"name": "lib", "url": "https://x/lib.git", "path": "/w/lib"}]}}'`)
	createSrv(t, s, key, map[string]any{"name": "cold", "port": 3001, "command": []string{"serve"}, "afterSync": []string{"npm", "ci"}, "runId": r1})
	bad := map[string]any{"sync": []map[string]string{{"repo": "app", "ref": "main", "mode": "rebase"}}}
	w := apiCall(t, s, key, http.MethodPost, "/v1/runs/"+r1+"/sync", bad)
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), `unknown mode \"rebase\"`) {
		t.Fatalf("unknown mode: %d %s", w.Code, w.Body)
	}
	// A host whose runner has no sync modes: move (or none) still goes;
	// fast-forward and fetch are refused, nothing sent.
	before := syncMessages(t, s)
	for _, mode := range []string{"fast-forward", "fetch"} {
		body := map[string]any{"sync": []map[string]string{{"repo": "app", "ref": "main"}, {"repo": "lib", "ref": "main", "mode": mode}}}
		if w := apiCall(t, s, key, http.MethodPost, "/v1/runs/"+r1+"/sync", body); w.Code != http.StatusConflict ||
			!strings.Contains(w.Body.String(), "sync_modes_unsupported") || !strings.Contains(w.Body.String(), "without sync modes") {
			t.Fatalf("mode %s on an old runner: %d %s", mode, w.Code, w.Body)
		}
	}
	if after := syncMessages(t, s); after != before {
		t.Fatalf("a refused sync was sent: %d messages, was %d", after, before)
	}
	for _, mode := range []string{"", "move"} {
		body := map[string]any{"sync": []map[string]string{{"repo": "app", "ref": "main", "mode": mode}}}
		if w := apiCall(t, s, key, http.MethodPost, "/v1/runs/"+r1+"/sync", body); w.Code != http.StatusAccepted {
			t.Fatalf("mode %q on an old runner: %d %s", mode, w.Code, w.Body)
		}
	}
	execSQL(t, s, ctx, `UPDATE hosts SET capabilities = ARRAY['diff', 'sync-modes']`)
	for i, mode := range []string{"", "move", "fast-forward", "fetch"} {
		id := "m" + strconv.Itoa(i)
		body := map[string]any{"requestId": id, "sync": []map[string]string{{"repo": "app", "ref": "main", "mode": mode}}}
		if w := apiCall(t, s, key, http.MethodPost, "/v1/runs/"+r1+"/sync", body); w.Code != http.StatusAccepted {
			t.Fatalf("mode %q: %d %s", mode, w.Code, w.Body)
		}
		var msg proto.Sync
		systemScan(t, s, `SELECT payload FROM host_messages WHERE type = 'sync' ORDER BY id DESC LIMIT 1`, nil, &msg)
		if msg.RequestID != id || len(msg.Repos) != 1 || msg.Repos[0].Mode != mode {
			t.Fatalf("sync message for mode %q: %+v", mode, msg)
		}
	}
	execSQL(t, s, ctx, `UPDATE runs SET state = 'stopped'`)
	execSQL(t, s, ctx, `UPDATE placements SET state = 'exited'`)
	if w := apiCall(t, s, key, http.MethodPost, "/v1/runs/"+r1+"/resume", bad); w.Code != http.StatusUnprocessableEntity ||
		!strings.Contains(w.Body.String(), `unknown mode \"rebase\"`) {
		t.Fatalf("resume with an unknown mode: %d %s", w.Code, w.Body)
	}
	var pending []proto.SyncRef
	systemScan(t, s, `SELECT pending_sync FROM runs WHERE id = $1`, []any{r1}, &pending)
	if len(pending) != 0 {
		t.Fatalf("a refused resume kept its sync: %+v", pending)
	}
	if w := apiCall(t, s, key, http.MethodPost, "/v1/runs/"+r1+"/resume", map[string]any{"sync": []map[string]string{
		{"repo": "app", "ref": "main", "mode": "fast-forward"}, {"repo": "lib", "ref": "v1", "mode": "fetch"}}}); w.Code != http.StatusAccepted {
		t.Fatalf("resume with modes: %d %s", w.Code, w.Body)
	}
	systemScan(t, s, `SELECT pending_sync FROM runs WHERE id = $1`, []any{r1}, &pending)
	if len(pending) != 2 || pending[0].Mode != "fast-forward" || pending[1].Mode != "fetch" {
		t.Fatalf("pending sync: %+v", pending)
	}

	epoch := 1
	assign := func(refs []proto.SyncRef) (proto.Assign, proto.ServerSpec) {
		t.Helper()
		epoch++
		err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
			return s.assign(ctx, tx, pendingRun{ID: r1, TenantID: "t1", Epoch: epoch - 1, PendingSync: refs}, &candidateHost{ID: "h1"})
		})
		if err != nil {
			t.Fatal(err)
		}
		var a proto.Assign
		systemScan(t, s, `SELECT payload FROM host_messages WHERE type = 'assign' ORDER BY id DESC LIMIT 1`, nil, &a)
		sets := pendingServers(t, s, ctx)
		execSQL(t, s, ctx, `UPDATE placements SET state = 'exited' WHERE run_id = $1`, r1)
		if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
			return stopServersAtEnd(ctx, tx, "t1", r1, epoch, "run stopped")
		}); err != nil {
			t.Fatal(err)
		}
		set := sets[len(sets)-1]
		if len(set.Servers) != 1 {
			t.Fatalf("servers: %+v", set)
		}
		return a, set.Servers[0]
	}
	withAfter := func(cmd []string) bool {
		return len(cmd) == 3 && strings.Contains(cmd[2], "'npm' 'ci' && exec 'serve'")
	}
	plain := func(cmd []string) bool { return len(cmd) == 1 && cmd[0] == "serve" }
	for _, c := range []struct {
		name     string
		refs     []proto.SyncRef
		command  func([]string) bool
		unmoved  func([]string) bool
		wantMode string
	}{
		{"no sync", nil, plain, func(c []string) bool { return c == nil }, ""},
		{"move", []proto.SyncRef{{Repo: "app", Ref: "main"}}, withAfter, func(c []string) bool { return c == nil }, ""},
		{"fast-forward", []proto.SyncRef{{Repo: "app", Ref: "main", Mode: "fast-forward"}}, withAfter, plain, "fast-forward"},
		{"fetch", []proto.SyncRef{{Repo: "app", Ref: "main", Mode: "fetch"}}, plain, func(c []string) bool { return c == nil }, "fetch"},
		{"move and fetch", []proto.SyncRef{{Repo: "app", Ref: "main", Mode: "fetch"}, {Repo: "lib", Ref: "main", Mode: "move"}}, withAfter,
			func(c []string) bool { return c == nil }, "fetch"},
	} {
		a, sv := assign(c.refs)
		if len(a.Sync) != len(c.refs) || (len(c.refs) > 0 && a.Sync[0].Mode != c.wantMode) {
			t.Fatalf("%s: assignment's sync %+v", c.name, a.Sync)
		}
		if !c.command(sv.Command) || !c.unmoved(sv.UnmovedCommand) {
			t.Fatalf("%s: command %q, unmovedCommand %q", c.name, sv.Command, sv.UnmovedCommand)
		}
	}
}

// A fast-forward or fetch queued while the host had sync modes, then
// re-registered without them (an older runner) before it was delivered, is
// never delivered, nor redelivered: an old runner drops the mode and moves
// the checkout. A running Run's sync ends failed (git.sync per repository,
// sync.done); an assignment goes without its fast-forward or fetch refs,
// each a failed git.sync. A move is delivered as it was.
func TestSafeSyncNotDeliveredAfterDowngrade(t *testing.T) {
	s, ctx, key, hello, register := syncModesFixture(t)
	// Every unacked message, as a reconnect replays them, and as a poll reads them.
	replay := func() []proto.Frame {
		t.Helper()
		execSQL(t, s, ctx, `UPDATE host_messages SET delivered_at = NULL WHERE host_id = 'h1'`)
		ws, err := s.pendingMessages(ctx, "h1", true)
		if err != nil {
			t.Fatal(err)
		}
		poll, err := s.pendingMessages(ctx, "h1", false)
		if err != nil {
			t.Fatal(err)
		}
		return append(ws, poll...)
	}
	syncEvents := func(requestID string) (failed map[string]string, done int) {
		t.Helper()
		failed = map[string]string{}
		err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
			rows, err := tx.Query(ctx, `SELECT type, coalesce(data->>'repo', ''), coalesce(data->>'status', ''), coalesce(data->>'error', ''),
					coalesce(data->>'changed', '') FROM run_events WHERE run_id = $1 AND data->>'requestId' = $2 ORDER BY id`, r1, requestID)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var typ, repo, status, msg, changed string
				if err := rows.Scan(&typ, &repo, &status, &msg, &changed); err != nil {
					return err
				}
				switch typ {
				case proto.EvGitSync:
					if _, dup := failed[repo]; dup || status != "failed" {
						return fmt.Errorf("git.sync %s: %s (again: %v)", repo, status, dup)
					}
					failed[repo] = msg
				case proto.EvSyncDone:
					if changed != "false" {
						return fmt.Errorf("sync.done changed=%s", changed)
					}
					done++
				}
			}
			return rows.Err()
		})
		if err != nil {
			t.Fatal(err)
		}
		return failed, done
	}
	const gone = "the host's lux-runner no longer supports sync modes"

	register([]string{proto.CapSyncModes})
	for _, body := range []map[string]any{
		{"requestId": "safe", "sync": []map[string]string{{"repo": "app", "ref": "main", "mode": "fetch"}, {"repo": "lib", "ref": "main"}}},
		{"requestId": "move", "sync": []map[string]string{{"repo": "app", "ref": "main", "mode": "move"}}},
	} {
		if w := apiCall(t, s, key, http.MethodPost, "/v1/runs/"+r1+"/sync", body); w.Code != http.StatusAccepted {
			t.Fatalf("sync %v: %d %s", body, w.Code, w.Body)
		}
	}
	register(nil)
	for range 2 {
		var delivered []string
		for _, f := range replay() {
			if f.Type == proto.MsgSync {
				var m proto.Sync
				_ = json.Unmarshal(f.Data, &m)
				delivered = append(delivered, m.RequestID)
			}
		}
		if !slices.Equal(delivered, []string{"move", "move"}) {
			t.Fatalf("syncs delivered to the old runner: %v", delivered)
		}
		failed, done := syncEvents("safe")
		if len(failed) != 2 || failed["app"] != gone || failed["lib"] != gone || done != 1 {
			t.Fatalf("the refused sync: failed %v, sync.done %d", failed, done)
		}
	}

	// resumePolicy never refuses requested resumes only: an assignment no
	// runner started is still placed again.
	refs := assignResumeThenDowngrade(t, s, ctx, hello, register)
	for range 2 {
		for _, f := range replay() {
			if f.Type == proto.MsgAssign {
				t.Fatalf("assignment delivered to the old runner: %s", f.Data)
			}
		}
		var state, reason, placement, exitReason string
		var pending []proto.SyncRef
		systemScan(t, s, `SELECT r.state, r.state_reason, r.pending_sync, p.state, coalesce(p.exit_reason, '') FROM runs r
			JOIN placements p ON p.run_id = r.id AND p.epoch = 2 WHERE r.id = $1`, []any{r1}, &state, &reason, &pending, &placement, &exitReason)
		if state != StateResuming || !slices.Equal(pending, refs) || placement != "lost" || exitReason != gone {
			t.Fatalf("after the refused assignment: run %s (%q), pending %+v, placement %s (%q)", state, reason, pending, placement, exitReason)
		}
		var lost int
		systemScan(t, s, `SELECT count(*) FROM run_events WHERE run_id = $1 AND type = 'state' AND data->>'state' = 'lost'`, []any{r1}, &lost)
		if lost != 1 {
			t.Fatalf("lost %d times", lost)
		}
	}
}

// syncModesFixture: wakeFixture with r1's spec naming repositories app and
// lib, running on h1, and register, which re-registers h1 with caps and
// hello's live placements.
func syncModesFixture(t *testing.T) (s *Server, ctx context.Context, key string, hello *proto.Hello, register func(caps []string)) {
	t.Helper()
	s, ctx, key, _ = wakeFixture(t)
	execSQL(t, s, ctx, `UPDATE runs SET spec = '{"git": {"repositories": [{"name": "app", "url": "https://x/app.git", "path": "/w/app"}, {"name": "lib", "url": "https://x/lib.git", "path": "/w/lib"}]}}'`)
	tok := &hostToken{}
	hello = &proto.Hello{Name: "h1", ProtocolVersion: proto.Version, Capabilities: []string{proto.CapSyncModes},
		Live: []proto.LivePlacement{{RunID: r1, Epoch: 1}}}
	register = func(caps []string) {
		t.Helper()
		hello.Capabilities = caps
		if _, err := s.registerHost(ctx, tok, *hello); err != nil {
			t.Fatal(err)
		}
	}
	return s, ctx, key, hello, register
}

// assignResumeThenDowngrade makes r1 (resumePolicy never) stopped, assigns
// its resume to h1 with a fast-forward sync while h1 has sync modes, then
// re-registers h1 without them and without live placements. It returns the
// assignment's sync; the assignment is refused at its next delivery.
func assignResumeThenDowngrade(t *testing.T, s *Server, ctx context.Context, hello *proto.Hello, register func([]string)) []proto.SyncRef {
	t.Helper()
	execSQL(t, s, ctx, `UPDATE runs SET state = 'stopped', spec = spec || '{"resumePolicy": "never"}'`)
	execSQL(t, s, ctx, `UPDATE placements SET state = 'exited'`)
	execSQL(t, s, ctx, `UPDATE host_messages SET acked_at = now()`)
	register([]string{proto.CapSyncModes})
	refs := []proto.SyncRef{{Repo: "app", Ref: "main", Mode: proto.SyncFastForward}, {Repo: "lib", Ref: "v1"}}
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		return s.assign(ctx, tx, pendingRun{ID: r1, TenantID: "t1", Epoch: 1, PendingSync: refs}, &candidateHost{ID: "h1"})
	}); err != nil {
		t.Fatal(err)
	}
	hello.Live = nil
	register(nil)
	return refs
}

// An assignment refused at delivery (no sync modes) for a Run whose
// terminate is pending is not placed again: the Run ends terminated, its
// secrets dropped, with no resume after it.
func TestRefusedAssignmentOfTerminatingRunEndsTerminated(t *testing.T) {
	s, ctx, _, hello, register := syncModesFixture(t)
	assignResumeThenDowngrade(t, s, ctx, hello, register)
	execSQL(t, s, ctx, `UPDATE runs SET terminate_requested = true WHERE id = $1`, r1)
	s.secrets.put(r1, map[string]string{"TOKEN": "jit"})
	if _, err := s.pendingMessages(ctx, "h1", false); err != nil {
		t.Fatal(err)
	}
	var state, reason, placement string
	var resumes int
	systemScan(t, s, `SELECT r.state, r.state_reason, p.state,
			(SELECT count(*) FROM run_events e WHERE e.run_id = r.id
				AND (e.type = 'resume.requested' OR (e.type = 'state' AND e.data->>'state' = 'resuming')))
		FROM runs r JOIN placements p ON p.run_id = r.id AND p.epoch = 2 WHERE r.id = $1`, []any{r1}, &state, &reason, &placement, &resumes)
	if state != StateTerminated || reason != "terminated; host lost" || placement != "lost" || resumes != 0 {
		t.Fatalf("run %s (%q), placement %s, %d resumes after; want terminated (\"terminated; host lost\"), lost, none", state, reason, placement, resumes)
	}
	if _, held := s.secrets.get(r1); held {
		t.Fatal("secrets still held")
	}
}

// syncMessages counts the sync messages queued for any host.
func syncMessages(t *testing.T, s *Server) (n int) {
	t.Helper()
	systemScan(t, s, `SELECT count(*) FROM host_messages WHERE type = 'sync'`, nil, &n)
	return n
}

// A host's sync modes are what its latest Hello says, through
// registerHost: none, then sync-modes, then none again (an older runner
// back). After each, the API takes a fast-forward or fetch sync only with
// them (no message written otherwise) and the scheduler places a safe-mode
// resume on it only with them; move goes throughout.
func TestSyncModesFollowRegistration(t *testing.T) {
	s, ctx, key, _ := wakeFixture(t)
	execSQL(t, s, ctx, `UPDATE runs SET spec = '{"git": {"repositories": [{"name": "app", "url": "https://x/app.git", "path": "/w/app"}]}}'`)
	tok := &hostToken{}
	for i, c := range []struct {
		caps []string
		safe int
	}{
		{nil, http.StatusConflict},
		{[]string{proto.CapDiff, proto.CapSyncModes}, http.StatusAccepted},
		{nil, http.StatusConflict},
	} {
		if _, err := s.registerHost(ctx, tok, proto.Hello{Name: "h1", ProtocolVersion: proto.Version, Capabilities: c.caps,
			Live: []proto.LivePlacement{{RunID: r1, Epoch: 1}}}); err != nil {
			t.Fatal(err)
		}
		for _, mode := range []string{proto.SyncFastForward, proto.SyncFetch} {
			before := syncMessages(t, s)
			body := map[string]any{"sync": []map[string]string{{"repo": "app", "ref": "main", "mode": mode}}}
			w := apiCall(t, s, key, http.MethodPost, "/v1/runs/"+r1+"/sync", body)
			if w.Code != c.safe {
				t.Fatalf("hello %d %v, mode %s: %d %s", i, c.caps, mode, w.Code, w.Body)
			}
			if wrote := syncMessages(t, s) - before; (c.safe == http.StatusAccepted) != (wrote == 1) {
				t.Fatalf("hello %d, mode %s: %d sync messages written", i, mode, wrote)
			}
		}
		for _, mode := range []string{"", proto.SyncMove} {
			body := map[string]any{"sync": []map[string]string{{"repo": "app", "ref": "main", "mode": mode}}}
			if w := apiCall(t, s, key, http.MethodPost, "/v1/runs/"+r1+"/sync", body); w.Code != http.StatusAccepted {
				t.Fatalf("hello %d, mode %q: %d %s", i, mode, w.Code, w.Body)
			}
		}
		var hosts []*candidateHost
		if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) (err error) {
			hosts, err = s.candidateHosts(ctx, tx, []string{"h1"})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if len(hosts) != 1 {
			t.Fatalf("hello %d: candidates %+v", i, hosts)
		}
		hosts[0].Connected = true
		for _, mode := range []string{proto.SyncFastForward, proto.SyncMove} {
			r := pendingRun{ID: "r", TenantID: "t1", PlaceOn: "h1", PendingSync: []proto.SyncRef{{Repo: "app", Ref: "main", Mode: mode}}}
			blocked := slices.ContainsFunc(hostFit(r, hosts[0]), func(b fitBlocker) bool { return b.Kind == kindSyncModes })
			if want := mode != proto.SyncMove && c.safe != http.StatusAccepted; blocked != want {
				t.Fatalf("hello %d %v, a %s resume: blocked %v, want %v", i, c.caps, mode, blocked, want)
			}
		}
	}
}

// Activity is requests, not open connections: a WebSocket (a dev
// server's hot reload) kept open and busy past idleAfter leaves the
// server idle.
func TestWebSocketIsNotActivity(t *testing.T) {
	s, ctx, key, _ := wakeFixture(t)
	s.cfg.Preview.ActivityEvery = 100 * time.Millisecond
	sv := createSrv(t, s, key, map[string]any{"name": "web", "port": 3000, "command": []string{"serve"}, "wake": "request",
		"idleAfter": "1s", "runId": r1, "hostname": "hmr.lux.example.com"})
	execSQL(t, s, ctx, `UPDATE run_servers SET state = 'ready', ready_since = now() - interval '1 minute' WHERE id = $1`, sv.ID)
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
			if err := c.Write(r.Context(), typ, b); err != nil {
				return
			}
		}
	}))
	defer app.Close()
	fakeRunner(t, s, strings.TrimPrefix(app.URL, "http://"))
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Host = "hmr.lux.example.com"
		s.preview.ServeHTTP(w, r)
	}))
	defer front.Close()
	wctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(wctx, "ws"+strings.TrimPrefix(front.URL, "http")+"/hmr", &websocket.DialOptions{
		HTTPHeader: http.Header{"Cookie": {previewCookie + "=" + cookieFor(s, sv.ID)}}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	// Busy for 2.5s (past idleAfter) on the one connection.
	for end := time.Now().Add(2500 * time.Millisecond); time.Now().Before(end); time.Sleep(100 * time.Millisecond) {
		if err := c.Write(wctx, websocket.MessageText, []byte("ping")); err != nil {
			t.Fatal(err)
		}
		if _, _, err := c.Read(wctx); err != nil {
			t.Fatal(err)
		}
		s.preview.flush(ctx, true)
	}
	if err := s.checkIdle(ctx); err != nil {
		t.Fatal(err)
	}
	if n := len(serverEventsOf(t, s, ctx, sv.ID, "server.idle")); n != 1 {
		t.Fatalf("idle events with a busy WebSocket open: %d", n)
	}
}

// Attached while its Run is already on a host but not yet running (an
// owner that submits a Run and then attaches): it starts in that
// placement, not the next.
func TestAttachToAPlacedRun(t *testing.T) {
	s, ctx, key, _ := wakeFixture(t)
	execSQL(t, s, ctx, `UPDATE runs SET state = 'starting'`)
	execSQL(t, s, ctx, `UPDATE placements SET state = 'starting'`)
	sv := createSrv(t, s, key, map[string]any{"name": "web", "port": 3000, "command": []string{"serve"}, "wake": "request"})
	if w := apiCall(t, s, key, http.MethodPost, "/v1/servers/"+sv.ID+"/attach", map[string]any{"runId": r1}); w.Code != http.StatusOK {
		t.Fatalf("attach: %d %s", w.Code, w.Body)
	}
	got := getSrv(t, s, key, sv.ID)
	sets := pendingServers(t, s, ctx)
	if got.Process != ServerStarting || got.State != SrvWaking || len(sets) == 0 || len(sets[len(sets)-1].Servers) != 1 {
		t.Fatalf("attached to a starting run: %+v %+v", got, sets)
	}
}

// An event of a server with no Run wakes the feed's followers, never a
// Run's (an output stream, a held preview request); a Run's event wakes
// that Run's.
func TestDetachedServerEventsWakeNoRunFollower(t *testing.T) {
	s, ctx, key, _ := wakeFixture(t)
	lctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); s.listenLoop(lctx) }()
	t.Cleanup(func() { cancel(); <-done })
	deadline := time.Now().Add(10 * time.Second)
	for {
		var n int
		systemScan(t, s, `SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND query = 'LISTEN lux_events' AND state = 'idle'`, nil, &n)
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("not listening within 10s")
		}
		time.Sleep(20 * time.Millisecond)
	}
	// The listener's own wake-everyone on connect may still be on its way.
	time.Sleep(200 * time.Millisecond)
	waitClosed := func(ch <-chan struct{}) bool {
		select {
		case <-ch:
			return true
		case <-time.After(2 * time.Second):
			return false
		}
	}
	all, run := s.wakeups.next(""), s.wakeups.next(r1)
	sv := createSrv(t, s, key, map[string]any{"name": "web", "port": 3000, "wake": "request"})
	if !waitClosed(all) {
		t.Fatal("server.created did not wake the feed")
	}
	if closed(run) {
		t.Fatal("an event of a server with no Run woke a Run's followers")
	}
	// Attached, its events are its Run's.
	if w := apiCall(t, s, key, http.MethodPost, "/v1/servers/"+sv.ID+"/attach", map[string]any{"runId": r1}); w.Code != http.StatusOK {
		t.Fatalf("attach: %d %s", w.Code, w.Body)
	}
	if !waitClosed(run) {
		t.Fatal("an event of the Run did not wake its followers")
	}
}

// A wakeable server whose Run is already on its way up or moving shows the
// waking page and asks no one: the Run is coming without a wake.
func TestNoWakeWhileTheRunComes(t *testing.T) {
	s, ctx, key, _ := wakeFixture(t)
	sv := createSrv(t, s, key, map[string]any{"name": "web", "port": 3000, "command": []string{"serve"}, "wake": "request",
		"hostname": "web.pr1.lux.example.com", "runId": r1})
	cookie := cookieFor(s, sv.ID)
	for _, c := range []struct{ run, placement, stopReason, title string }{
		{StateStarting, "starting", "", "Its Run is starting"},
		{StateResuming, "exited", "", "Its Run is starting"},
		{StateScheduled, "starting", "", "Its Run is starting"},
		{StateStopping, "stopping", "migrate", pageWakingMoving.Title},
	} {
		execSQL(t, s, ctx, `UPDATE runs SET state = $1`, c.run)
		execSQL(t, s, ctx, `UPDATE placements SET state = $1, stop_reason = $2`, c.placement, c.stopReason)
		execSQL(t, s, ctx, `UPDATE run_servers SET state = 'starting' WHERE id = $1`, sv.ID)
		w := previewGet(s, "web.pr1.lux.example.com", "/", cookie)
		if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), c.title) {
			t.Fatalf("%s/%s: %d %s", c.run, c.stopReason, w.Code, w.Body)
		}
		if n := len(serverEventsOf(t, s, ctx, sv.ID, "server.wake_requested")); n != 0 {
			t.Fatalf("%s/%s: woke it: %d", c.run, c.stopReason, n)
		}
	}
}

// A wakeable server stopped by request stays down: its page says so, and
// nothing is asked of its owner.
func TestStoppedServerDoesNotWake(t *testing.T) {
	s, ctx, key, _ := wakeFixture(t)
	execSQL(t, s, ctx, `UPDATE runs SET state = 'stopped'`)
	execSQL(t, s, ctx, `UPDATE placements SET state = 'exited'`)
	sv := createSrv(t, s, key, map[string]any{"name": "web", "port": 3000, "command": []string{"serve"}, "wake": "request",
		"hostname": "web.pr1.lux.example.com", "runId": r1})
	if w := apiCall(t, s, key, http.MethodPost, "/v1/servers/"+sv.ID+"/stop", nil); w.Code != http.StatusOK {
		t.Fatalf("stop: %d %s", w.Code, w.Body)
	}
	cookie := cookieFor(s, sv.ID)
	for _, path := range []string{"/", "/.lux/wait?to=/"} {
		w := previewGet(s, "web.pr1.lux.example.com", path, cookie)
		if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), pageStopped.Title) {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body)
		}
	}
	if n := len(serverEventsOf(t, s, ctx, sv.ID, "server.wake_requested")); n != 0 {
		t.Fatalf("a server stopped by request woke: %d", n)
	}
}

// orchestratorNames are products that orchestrate lux; its pages say "the
// orchestrator" or "its owner", never one of these.
var orchestratorNames = []string{"dude"}

// Every preview page, and every waking-page step, is worded without naming
// an orchestrator product.
func TestPreviewPagesNameNoOrchestrator(t *testing.T) {
	s, ctx, key, _ := wakeFixture(t)
	check := func(what, body string) {
		t.Helper()
		for _, n := range orchestratorNames {
			if strings.Contains(strings.ToLower(body), n) {
				t.Errorf("%s names %q: %s", what, n, body)
			}
		}
	}
	all := map[string]any{"Name": "web", "To": "/x", "WaitURL": "/.lux/wait?to=%2Fx", "What": "w", "State": "stopped",
		"Reason": "stopped", "Code": "3", "Error": "e", "LogURL": "https://luxd.example.com/servers/x", "Host": "h1",
		"Asked": "5m ago", "AskAgain": previewWakePath, "ConsoleURL": "https://luxd.example.com/servers/x",
		"Steps": []wakeStep{{Label: "step", State: "done", Elapsed: "1s"}}}
	for name, pg := range map[string]previewPage{
		"unknown": pageUnknown, "gone": pageGone, "signIn": pageSignIn, "error": pageError, "starting": pageStarting,
		"moving": pageMoving, "runStopped": pageRunStopped, "detached": pageDetached, "stopped": pageStopped,
		"exited": pageExited, "unreachable": pageUnreachable, "waking": pageWaking, "wakingHost": pageWakingHost,
		"wakingMoving": pageWakingMoving, "noAnswer": pageNoAnswer, "didNotStart": pageDidNotStart,
	} {
		w := httptest.NewRecorder()
		s.preview.page(w, http.StatusOK, pg, all)
		if w.Body.Len() == 0 || !strings.Contains(w.Body.String(), pg.Title) {
			t.Fatalf("%s: not rendered: %s", name, w.Body)
		}
		check(name, w.Body.String())
	}
	// The waking page's own steps, in each phase of a wake.
	sv := createSrv(t, s, key, map[string]any{"name": "web", "port": 3000, "command": []string{"serve"}, "wake": "request",
		"hostname": "web.pr1.lux.example.com", "runId": r1})
	cookie := cookieFor(s, sv.ID)
	for _, c := range []struct{ name, sql string }{
		{"asleep", `UPDATE runs SET state = 'stopped'`},
		{"asked", `UPDATE run_servers SET wake_requested_at = now()`},
		{"resuming", `UPDATE runs SET state = 'resuming'`},
		{"starting", `UPDATE runs SET state = 'starting'`},
		{"restored", `UPDATE placements SET created_at = now(), volumes_restored_at = now()`},
		{"no answer", `UPDATE runs SET state = 'stopped'; UPDATE run_servers SET wake_requested_at = now() - interval '1 hour'`},
	} {
		execSQL(t, s, ctx, c.sql)
		w := previewGet(s, "web.pr1.lux.example.com", "/.lux/wait?to=/", cookie)
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s: %d %s", c.name, w.Code, w.Body)
		}
		check(c.name, w.Body.String())
	}
}

// A Run that is terminated while its owner server serves: the server stops
// with the placement and is detached, its events say so, and it is never
// detached with a live process. A Run that succeeds keeps it attached,
// stopped: a resume starts it again.
func TestOwnerServerEndsStoppedWithItsRun(t *testing.T) {
	for _, c := range []struct{ name, stopReason, outcome string }{
		{"exit 0", "", StateSucceeded},
		{"terminated", "terminate", StateTerminated},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, ctx, key, _ := wakeFixture(t)
			sv := createSrv(t, s, key, map[string]any{"name": "web", "port": 3000, "command": []string{"serve"}, "runId": r1,
				"lifetime": "owner", "hostname": "web.own.lux.example.com"})
			serverReport(t, s, ctx, 1, map[string]any{"name": "web", "gen": serverGen(t, s, ctx, "web"), "state": "ready"})
			if got := getSrv(t, s, key, sv.ID); got.Process != ServerReady {
				t.Fatalf("not ready: %+v", got)
			}
			if c.stopReason != "" {
				execSQL(t, s, ctx, `UPDATE placements SET stop_reason = $1, state = 'stopping'`, c.stopReason)
			}
			code := 0
			if f := s.handleReport(ctx, "h1", proto.Frame{Type: proto.MsgStatus, ID: 2, RunID: r1, Epoch: 1,
				Data: proto.Marshal(proto.Status{State: "exited", ExitCode: &code, Reason: "exited"})}); f.Type != proto.MsgAck {
				t.Fatalf("exit: %s", f.Data)
			}
			var runState string
			systemScan(t, s, `SELECT state FROM runs WHERE id = $1`, []any{r1}, &runState)
			if runState != c.outcome {
				t.Fatalf("run: %s", runState)
			}
			got := getSrv(t, s, key, sv.ID)
			if c.outcome == StateSucceeded {
				if got.RunID == nil || *got.RunID != r1 || got.Process != ServerStopped {
					t.Fatalf("after the Run succeeded: %+v, want attached and stopped", got)
				}
				return
			}
			if got.RunID != nil || got.Process != ServerStopped || got.StopReason == nil || *got.StopReason != "detached" || got.Epoch != nil {
				t.Fatalf("after the Run ended: %+v", got)
			}
			var tail string
			systemScan(t, s, `SELECT string_agg(type || coalesce(':' || (data->>'state'), ''), ' ' ORDER BY id) FROM
				(SELECT * FROM run_events WHERE server_id = $1 ORDER BY id DESC LIMIT 2) e`, []any{sv.ID}, &tail)
			if tail != "server.state:stopped server.detached" {
				t.Fatalf("last events: %s", tail)
			}
			if d := serverEventsOf(t, s, ctx, sv.ID, "server.detached"); len(d) != 1 || d[0]["reason"] != "run "+c.outcome || d[0]["from"] != r1 {
				t.Fatalf("detached: %+v", d)
			}
		})
	}
}

// An operator acts on any tenant's server by its id (the server names its
// tenant), signs in to its preview with a ticket it mints, and must name a
// tenant to create one.
func TestOperatorServers(t *testing.T) {
	s, ctx, key, _ := wakeFixture(t)
	op := operatorKey(t, s, ctx)
	sv := createSrv(t, s, key, map[string]any{"name": "web", "port": 3000, "command": []string{"serve"}, "hostname": "web.op.lux.example.com"})
	if w := apiCall(t, s, op, http.MethodPost, "/v1/servers", map[string]any{"name": "x", "port": 1}); w.Code != http.StatusBadRequest ||
		!strings.Contains(w.Body.String(), `"tenant_required"`) {
		t.Fatalf("create without a tenant: %d %s", w.Code, w.Body)
	}
	if w := apiCall(t, s, op, http.MethodPost, "/v1/servers?tenant=t2", map[string]any{"name": "x", "port": 1}); w.Code != http.StatusCreated ||
		!strings.Contains(w.Body.String(), `"name":"x"`) {
		t.Fatalf("create for t2: %d %s", w.Code, w.Body)
	}
	if got := getSrv(t, s, op, sv.ID); got.ID != sv.ID {
		t.Fatalf("get: %+v", got)
	}
	if w := apiCall(t, s, op, http.MethodPatch, "/v1/servers/"+sv.ID, map[string]any{"labels": map[string]string{"by": "op"}}); w.Code != http.StatusOK {
		t.Fatalf("patch: %d %s", w.Code, w.Body)
	}
	if w := apiCall(t, s, op, http.MethodPost, "/v1/servers/"+sv.ID+"/attach", map[string]any{"runId": r1}); w.Code != http.StatusOK {
		t.Fatalf("attach: %d %s", w.Code, w.Body)
	}
	w := apiCall(t, s, op, http.MethodPost, "/v1/servers/"+sv.ID+"/tickets", nil)
	var tk struct{ Ticket string }
	_ = json.Unmarshal(w.Body.Bytes(), &tk)
	if w.Code != http.StatusCreated || tk.Ticket == "" {
		t.Fatalf("ticket: %d %s", w.Code, w.Body)
	}
	w = previewGet(s, "web.op.lux.example.com", "/.lux/auth?ticket="+tk.Ticket+"&to=/x", "")
	var cookie string
	for _, c := range w.Result().Cookies() {
		if c.Name == previewCookie {
			cookie = c.Value
		}
	}
	if w.Code != http.StatusFound || w.Header().Get("Location") != "/x" || cookie == "" {
		t.Fatalf("sign in: %d %v", w.Code, w.Header())
	}
	if u, ok := s.preview.verify(cookie, time.Now()); !ok || u.ServerID != sv.ID || u.TenantID != "t1" {
		t.Fatalf("cookie: %+v %v", u, ok)
	}
	if w := apiCall(t, s, op, http.MethodDelete, "/v1/servers/"+sv.ID, nil); w.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", w.Code, w.Body)
	}
}

// Over http (a local demo), the container sees neither of lux's preview
// cookies, its own cookies as sent, and the scheme the browser used.
func TestPreviewHTTPCookiesStayOut(t *testing.T) {
	s, ctx, key, _ := wakeFixture(t)
	s.cfg.Preview.Scheme = "http"
	sv := createSrv(t, s, key, map[string]any{"name": "web", "port": 3000, "command": []string{"serve"}, "runId": r1,
		"hostname": "web.demo.lux.example.com"})
	execSQL(t, s, ctx, `UPDATE run_servers SET state = 'ready', ready_since = now() WHERE id = $1`, sv.ID)
	seen := make(chan http.Header, 1)
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Clone()
		_, _ = w.Write([]byte("ok"))
	}))
	defer app.Close()
	fakeRunner(t, s, strings.TrimPrefix(app.URL, "http://"))
	signed := cookieFor(s, sv.ID)
	req := httptest.NewRequest(http.MethodGet, "http://web.demo.lux.example.com/", nil)
	req.Header.Set("Cookie", previewCookieHTTP+"="+signed+"; app=1; "+previewCookie+"="+signed)
	w := httptest.NewRecorder()
	s.preview.ServeHTTP(w, req)
	if w.Code != http.StatusOK || w.Body.String() != "ok" {
		t.Fatalf("proxied: %d %s", w.Code, w.Body)
	}
	h := <-seen
	if got := h.Values("Cookie"); len(got) != 1 || got[0] != "app=1" {
		t.Fatalf("cookies the container saw: %q", got)
	}
	if h.Get("X-Forwarded-Proto") != "http" || h.Get("X-Forwarded-Host") != "web.demo.lux.example.com" {
		t.Fatalf("forwarded: proto %q host %q", h.Get("X-Forwarded-Proto"), h.Get("X-Forwarded-Host"))
	}
}

// A generated host that is already someone's (an owner chose name-xxxxxxxx)
// is a 409 hostname_taken, from every way a server is made, never a 500.
func TestGeneratedHostClashIsAConflict(t *testing.T) {
	s, ctx, key, _ := wakeFixture(t)
	// Every server named clash gets host "taken", as if its generated
	// suffix had come out so: a trigger after lux's own, as the owner.
	var dbName string
	systemScan(t, s, `SELECT current_database()`, nil, &dbName)
	cfg, err := pgx.ParseConfig(os.Getenv("LUX_TEST_PG"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Database = dbName
	admin, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	if _, err := admin.Exec(ctx, `CREATE FUNCTION test_clash() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN IF NEW.name = 'clash' THEN NEW.host := 'taken'; END IF; RETURN NEW; END $$;
		CREATE TRIGGER zz_test_clash BEFORE INSERT ON run_servers FOR EACH ROW EXECUTE FUNCTION test_clash()`); err != nil {
		t.Fatal(err)
	}
	createSrv(t, s, key, map[string]any{"name": "owner", "port": 1, "hostname": "taken.lux.example.com"})
	// POST /v1/runs/{id}/servers.
	if w := apiCall(t, s, key, http.MethodPost, "/v1/runs/"+r1+"/servers", map[string]any{"name": "clash", "port": 1, "start": false}); w.Code != http.StatusConflict ||
		!strings.Contains(w.Body.String(), `"hostname_taken"`) {
		t.Fatalf("run server: %d %s", w.Code, w.Body)
	}
	// A submitted spec's servers.
	err = s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		return insertSpecServers(ctx, tx, "t1", r1, "k1", spec.RunSpec{Workload: spec.Workload{Servers: []spec.Server{{Name: "clash", Port: 1}}}})
	})
	var he *HTTPError
	if !errors.As(err, &he) || he.Status != http.StatusConflict || he.Code != "hostname_taken" {
		t.Fatalf("spec server: %v", err)
	}
	// POST /v1/servers.
	if w := apiCall(t, s, key, http.MethodPost, "/v1/servers", map[string]any{"name": "clash", "port": 1}); w.Code != http.StatusConflict ||
		!strings.Contains(w.Body.String(), `"hostname_taken"`) {
		t.Fatalf("server: %d %s", w.Code, w.Body)
	}
}

// The Run API removes the Run's own servers only: an owner server attached
// to it is refused, and stays.
func TestRunAPIDoesNotRemoveOwnerServers(t *testing.T) {
	s, _, key, _ := wakeFixture(t)
	own := createSrv(t, s, key, map[string]any{"name": "web", "port": 3000, "runId": r1, "lifetime": "owner"})
	runs := createSrv(t, s, key, map[string]any{"name": "api", "port": 3001, "runId": r1})
	w := apiCall(t, s, key, http.MethodDelete, "/v1/runs/"+r1+"/servers/web", nil)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), `"lifetime_owner"`) ||
		!strings.Contains(w.Body.String(), "DELETE /v1/servers/"+own.ID) {
		t.Fatalf("remove an owner server: %d %s", w.Code, w.Body)
	}
	if got := getSrv(t, s, key, own.ID); got.RunID == nil || *got.RunID != r1 {
		t.Fatalf("after the refusal: %+v", got.RunID)
	}
	if w := apiCall(t, s, key, http.MethodDelete, "/v1/runs/"+r1+"/servers/api", nil); w.Code != http.StatusNoContent {
		t.Fatalf("remove the Run's server: %d %s", w.Code, w.Body)
	}
	if w := apiCall(t, s, key, http.MethodGet, "/v1/servers/"+runs.ID, nil); w.Code != http.StatusNotFound {
		t.Fatalf("the Run's server after removal: %d", w.Code)
	}
}

// A port-only server (no command) of a running Run whose port is not open
// yet is waking, not stopped: there is nothing to start, and lux watches
// the port. Stopped by request, it is stopped.
func TestPortOnlyServerWaitsForItsPort(t *testing.T) {
	s, ctx, key, _ := wakeFixture(t)
	sv := createSrv(t, s, key, map[string]any{"name": "web", "port": 3000, "wake": "request", "runId": r1,
		"hostname": "web.port.lux.example.com"})
	execSQL(t, s, ctx, `UPDATE run_servers SET state = 'stopped', stop_reason = NULL WHERE id = $1`, sv.ID)
	if got := getSrv(t, s, key, sv.ID); got.State != SrvWaking {
		t.Fatalf("port only, not open yet: %s", got.State)
	}
	w := previewGet(s, "web.port.lux.example.com", "/", cookieFor(s, sv.ID))
	if w.Code != http.StatusServiceUnavailable || strings.Contains(w.Body.String(), pageStopped.Title) {
		t.Fatalf("its page: %d %s", w.Code, w.Body)
	}
	execSQL(t, s, ctx, `UPDATE run_servers SET stop_reason = 'stopped' WHERE id = $1`, sv.ID)
	if got := getSrv(t, s, key, sv.ID); got.State != SrvStopped {
		t.Fatalf("port only, stopped by request: %s", got.State)
	}
}

// A deleted server's events stay readable by its id, ending with its
// deletion; an id with no events is an empty list.
func TestDeletedServerEventsStayListed(t *testing.T) {
	s, _, key, key2 := wakeFixture(t)
	sv := createSrv(t, s, key, map[string]any{"name": "web", "port": 3000})
	if w := apiCall(t, s, key, http.MethodDelete, "/v1/servers/"+sv.ID, nil); w.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", w.Code, w.Body)
	}
	list := func(k, id string) (int, []string) {
		w := apiCall(t, s, k, http.MethodGet, "/v1/servers/"+id+"/events", nil)
		var out struct{ Events []struct{ Type string } }
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		types := []string{}
		for _, e := range out.Events {
			types = append(types, e.Type)
		}
		return w.Code, types
	}
	if code, types := list(key, sv.ID); code != http.StatusOK || strings.Join(types, " ") != "server.created server.deleted" {
		t.Fatalf("deleted server's events: %d %v", code, types)
	}
	if code, types := list(key2, sv.ID); code != http.StatusOK || len(types) != 0 {
		t.Fatalf("another tenant: %d %v", code, types)
	}
	if code, types := list(key, "srv_nonenonenonenone"); code != http.StatusOK || len(types) != 0 {
		t.Fatalf("no such server: %d %v", code, types)
	}
}

package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/ids"
	"github.com/marcioapm/lux/internal/store"
)

// renameFixture: tenants t1 and t2, a run key and a read key for t1, a key
// for t2, and a Run of each tenant in state.
func renameFixture(t *testing.T, state string) (s *Server, ctx context.Context, keys map[string]string) {
	t.Helper()
	s = testServer(t)
	ctx = context.Background()
	keys = map[string]string{"run": ids.Secret("luxk"), "read": ids.Secret("luxk"), "t2": ids.Secret("luxk")}
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1'), ('t2', 't2')`)
	execSQL(t, s, ctx, `INSERT INTO api_keys (id, tenant_id, name, key_hash, scopes) VALUES
		('k1', 't1', 'ci', $1, ARRAY['run']), ('k2', 't1', 'ro', $2, ARRAY['read']), ('k3', 't2', 'other', $3, ARRAY['run'])`,
		ids.Hash(keys["run"]), ids.Hash(keys["read"]), ids.Hash(keys["t2"]))
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, name, labels, spec, state, current_epoch) VALUES
		($1, 't1', 'first', '{"team": "a"}', '{"name": "first", "labels": {"team": "a"}, "workload": {"workdir": "/work"}}', $3, 1),
		($2, 't2', 'theirs', '{}', '{"name": "theirs"}', $3, 1)`, r1, r2, state)
	return s, ctx, keys
}

const r2 = "run_bbbbbbbbbbbbbbbb"

// storedName is runs.name and the stored spec's name of a Run.
func storedName(t *testing.T, s *Server, ctx context.Context, id string) (name string, specName *string) {
	t.Helper()
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT name, spec->>'name' FROM runs WHERE id = $1`, id).Scan(&name, &specName)
	})
	if err != nil {
		t.Fatal(err)
	}
	return name, specName
}

// patchName sends body as JSON, or as it is when it is a rawBody.
func patchName(t *testing.T, s *Server, key, id string, body any) (int, string) {
	t.Helper()
	if raw, ok := body.(rawBody); ok {
		return callRaw(s, key, http.MethodPatch, "/v1/runs/"+id, raw)
	}
	return call(t, s, key, http.MethodPatch, "/v1/runs/"+id, body)
}

// rawBody is a request body sent byte for byte, invalid UTF-8 included.
type rawBody string

func callRaw(s *Server, key, method, path string, body rawBody) (int, string) {
	req := httptest.NewRequest(method, path, strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	return w.Code, w.Body.String()
}

func TestRenameRun(t *testing.T) {
	s, ctx, keys := renameFixture(t, StateRunning)
	code, body := patchName(t, s, keys["run"], r1, map[string]any{"name": "window 2: tests"})
	if code != http.StatusOK {
		t.Fatalf("rename: %d %s", code, body)
	}
	var got Run
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err)
	}
	if got.ID != r1 || got.Name != "window 2: tests" || got.Spec.Name != "window 2: tests" || got.State != StateRunning {
		t.Fatalf("answer: id %s name %q spec.name %q state %s", got.ID, got.Name, got.Spec.Name, got.State)
	}
	// Labels, epoch and the rest of the spec are as they were.
	if got.Labels["team"] != "a" || got.Spec.Labels["team"] != "a" || got.Epoch != 1 || got.Spec.Workload.Workdir != "/work" {
		t.Fatalf("rename changed more than the name: %+v", got)
	}
	code, body = call(t, s, keys["run"], http.MethodGet, "/v1/runs/"+r1, nil)
	var fetched Run
	if err := json.Unmarshal([]byte(body), &fetched); code != http.StatusOK || err != nil {
		t.Fatalf("get: %d %s", code, body)
	}
	if fetched.Name != "window 2: tests" || fetched.Spec.Name != "window 2: tests" {
		t.Fatalf("get: name %q spec.name %q", fetched.Name, fetched.Spec.Name)
	}
	code, body = call(t, s, keys["run"], http.MethodGet, "/v1/runs", nil)
	var list struct{ Runs []Run }
	if err := json.Unmarshal([]byte(body), &list); code != http.StatusOK || err != nil || len(list.Runs) != 1 {
		t.Fatalf("list: %d %s", code, body)
	}
	if list.Runs[0].Name != "window 2: tests" || list.Runs[0].Spec.Name != "window 2: tests" {
		t.Fatalf("list: name %q spec.name %q", list.Runs[0].Name, list.Runs[0].Spec.Name)
	}
	if name, specName := storedName(t, s, ctx, r1); name != "window 2: tests" || specName == nil || *specName != "window 2: tests" {
		t.Fatalf("stored: %q %v", name, specName)
	}

	// The event: who, from, to; the same name again records none.
	if code, body := patchName(t, s, keys["run"], r1, map[string]any{"name": "window 2: tests"}); code != http.StatusOK {
		t.Fatalf("same name: %d %s", code, body)
	}
	code, body = call(t, s, keys["run"], http.MethodGet, "/v1/runs/"+r1+"/events", nil)
	var evs struct{ Events []Event }
	if err := json.Unmarshal([]byte(body), &evs); code != http.StatusOK || err != nil {
		t.Fatalf("events: %d %s", code, body)
	}
	var renamed []Event
	for _, e := range evs.Events {
		if e.Type == "renamed" {
			renamed = append(renamed, e)
		}
	}
	if len(renamed) != 1 || renamed[0].Data["from"] != "first" || renamed[0].Data["to"] != "window 2: tests" || renamed[0].Data["by"] != "k1" {
		t.Fatalf("renamed events: %+v", renamed)
	}
	// The tenant's feed (GET /v1/events) carries it with its runId: what
	// a feed consumer refetches the Run on. Not the other tenant's.
	feed, err := s.feedAfter(ctx, Principal{TenantID: "t1", Scopes: []string{"read"}}, 0)
	if err != nil || len(feed) != 1 || feed[0].Type != "renamed" || feed[0].RunID == nil || *feed[0].RunID != r1 {
		t.Fatalf("t1's feed: %+v %v", feed, err)
	}
	if other, err := s.feedAfter(ctx, Principal{TenantID: "t2", Scopes: []string{"read"}}, 0); err != nil || len(other) != 0 {
		t.Fatalf("t2's feed: %+v %v", other, err)
	}

	// "" clears it: runs.name empty, the spec without a name, as a submit
	// without one stores it.
	code, body = patchName(t, s, keys["run"], r1, map[string]any{"name": ""})
	var cleared Run
	if err := json.Unmarshal([]byte(body), &cleared); code != http.StatusOK || err != nil || cleared.Name != "" || cleared.Spec.Name != "" {
		t.Fatalf("clear: %d %s", code, body)
	}
	if name, specName := storedName(t, s, ctx, r1); name != "" || specName != nil {
		t.Fatalf("cleared, stored: %q %v", name, specName)
	}
}

func TestRenameRunRefusals(t *testing.T) {
	s, ctx, keys := renameFixture(t, StateRunning)
	for _, c := range []struct {
		what string
		body any
		want string
	}{
		{"201 bytes", map[string]any{"name": strings.Repeat("a", 201)}, "name: 201 bytes, at most 200"},
		{"invalid UTF-8", rawBody("{\"name\": \"bad \xff\"}"), "name: not valid UTF-8"},
		{"an escaped control character", rawBody(`{"name": "esc\u001b[31m"}`), "name: control character U+001B"},
		{"a control character", map[string]any{"name": "two\nlines"}, "name: control character U+000A"},
		{"no name", map[string]any{}, "name is required"},
		{"a null name", map[string]any{"name": nil}, "name is required"},
	} {
		t.Run(c.what, func(t *testing.T) {
			code, body := patchName(t, s, keys["run"], r1, c.body)
			if code != http.StatusUnprocessableEntity || !strings.Contains(body, c.want) {
				t.Fatalf("%d %s, want 422 %q", code, body, c.want)
			}
		})
	}
	code, body := patchName(t, s, keys["run"], r1, map[string]any{"name": "x", "labels": map[string]string{"a": "b"}})
	if code != http.StatusUnprocessableEntity || !strings.Contains(body, `unknown field \"labels\"`) {
		t.Fatalf("unknown field: %d %s", code, body)
	}
	// Another tenant's Run is not found, as stop finds it.
	code, body = patchName(t, s, keys["t2"], r1, map[string]any{"name": "mine now"})
	stopCode, stopBody := call(t, s, keys["t2"], http.MethodPost, "/v1/runs/"+r1+"/stop", nil)
	if code != http.StatusNotFound || code != stopCode || body != stopBody {
		t.Fatalf("other tenant: rename %d %s, stop %d %s", code, body, stopCode, stopBody)
	}
	// A read key is refused as stop refuses it.
	code, body = patchName(t, s, keys["read"], r1, map[string]any{"name": "read only"})
	stopCode, stopBody = call(t, s, keys["read"], http.MethodPost, "/v1/runs/"+r1+"/stop", nil)
	if code != http.StatusForbidden || code != stopCode || body != stopBody {
		t.Fatalf("read key: rename %d %s, stop %d %s", code, body, stopCode, stopBody)
	}
	if name, specName := storedName(t, s, ctx, r1); name != "first" || specName == nil || *specName != "first" {
		t.Fatalf("a refused rename changed the name: %q %v", name, specName)
	}
	if name, _ := storedName(t, s, ctx, r2); name != "theirs" {
		t.Fatalf("t2's Run: %q", name)
	}
}

// A name is metadata: a terminated Run takes a new one.
func TestRenameTerminatedRun(t *testing.T) {
	s, ctx, keys := renameFixture(t, StateTerminated)
	code, body := patchName(t, s, keys["run"], r1, map[string]any{"name": "archived"})
	var got Run
	if err := json.Unmarshal([]byte(body), &got); code != http.StatusOK || err != nil || got.Name != "archived" || got.State != StateTerminated {
		t.Fatalf("rename: %d %s", code, body)
	}
	if name, specName := storedName(t, s, ctx, r1); name != "archived" || specName == nil || *specName != "archived" {
		t.Fatalf("stored: %q %v", name, specName)
	}
}

// A submit refuses a name PATCH would refuse, with invalid_spec.
func TestSubmitRefusesLongName(t *testing.T) {
	s, _, keys := renameFixture(t, StateRunning)
	run := func(name string) (int, string) {
		return call(t, s, keys["run"], http.MethodPost, "/v1/runs", map[string]any{
			"name": name, "image": map[string]any{"ref": "alpine"}, "workload": map[string]any{"command": []string{"true"}}})
	}
	if code, body := run(strings.Repeat("a", 201)); code != http.StatusUnprocessableEntity || !strings.Contains(body, "invalid_spec") ||
		!strings.Contains(body, "name: 201 bytes, at most 200") {
		t.Fatalf("201 bytes: %d %s", code, body)
	}
	if code, body := run(strings.Repeat("a", 200)); code != http.StatusCreated {
		t.Fatalf("200 bytes: %d %s", code, body)
	}
	for _, name := range []string{"bad\nname", "bad\u007f", "bad\u0085"} {
		if code, body := run(name); code != http.StatusUnprocessableEntity || !strings.Contains(body, "control character") {
			t.Fatalf("control character: %d %s", code, body)
		}
	}
	body := rawBody("{\"name\":\"bad \xff\",\"image\":{\"ref\":\"alpine\"},\"workload\":{\"command\":[\"true\"]}}")
	if code, body := callRaw(s, keys["run"], http.MethodPost, "/v1/runs", body); code != http.StatusUnprocessableEntity ||
		!strings.Contains(body, "name: not valid UTF-8") {
		t.Fatalf("invalid UTF-8: %d %s", code, body)
	}
}

func TestSubmitCaseFoldRawUTF8(t *testing.T) {
	s, _, keys := renameFixture(t, StateRunning)
	runs := queryOne[int](t, s, `SELECT count(*) FROM runs`)
	for _, field := range []string{"name", "Name", "NAME", "naMe", `na\u006de`} {
		t.Run(field, func(t *testing.T) {
			body := rawBody(`{"` + field + `":"bad ` + string([]byte{0xff}) + `","image":{"ref":"alpine"},"workload":{"command":["true"]}}`)
			if code, body := callRaw(s, keys["run"], http.MethodPost, "/v1/runs", body); code != http.StatusUnprocessableEntity ||
				!strings.Contains(body, "name: not valid UTF-8") {
				t.Fatalf("submit %s: %d %s", field, code, body)
			}
			if n := queryOne[int](t, s, `SELECT count(*) FROM runs`); n != runs {
				t.Fatalf("submit %s: %d runs, want %d", field, n, runs)
			}
		})
	}
	body := rawBody(`{"name":"bad ` + string([]byte{0xff}) + `","NAME":"safe alias","image":{"ref":"alpine"},"workload":{"command":["true"]}}`)
	code, response := callRaw(s, keys["run"], http.MethodPost, "/v1/runs", body)
	if code != http.StatusCreated {
		t.Fatalf("safe final alias: %d %s", code, response)
	}
	var got Run
	if err := json.Unmarshal([]byte(response), &got); err != nil || got.Name != "safe alias" || got.Spec.Name != "safe alias" {
		t.Fatalf("safe final alias response: %d %s", code, response)
	}
}

func TestRenameDuplicateNull(t *testing.T) {
	s, ctx, keys := renameFixture(t, StateRunning)
	for _, body := range []rawBody{`{"name":"should-not-stick","name":null}`, `{"name":"should-not-stick","Name":null}`} {
		t.Run(string(body), func(t *testing.T) {
			code, response := patchName(t, s, keys["run"], r1, body)
			if code != http.StatusUnprocessableEntity || !strings.Contains(response, "name is required") {
				t.Fatalf("duplicate null: %d %s", code, response)
			}
			if name, specName := storedName(t, s, ctx, r1); name != "first" || specName == nil || *specName != "first" {
				t.Fatalf("duplicate null changed stored name: %q %v", name, specName)
			}
		})
	}
}

// A rename during a state change that holds the row lock (as applyReport and
// resume do: SELECT ... FOR UPDATE, then a rewrite of state and spec from
// what they read) waits for it, and is applied on top: both the new state
// and the new name are there, in runs.name and spec.name.
func TestRenameRacingAStateChange(t *testing.T) {
	s, ctx, keys := renameFixture(t, StateRunning)
	type answer struct {
		code int
		body string
	}
	done := make(chan answer, 1)
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		var spec map[string]any
		if err := tx.QueryRow(ctx, `SELECT spec FROM runs WHERE id = $1 FOR UPDATE`, r1).Scan(&spec); err != nil {
			return err
		}
		go func() {
			code, body := patchName(t, s, keys["run"], r1, map[string]any{"name": "renamed meanwhile"})
			done <- answer{code, body}
		}()
		// The rename is waiting on the row lock before the state change
		// writes.
		deadline := time.Now().Add(10 * time.Second)
		for {
			var waiting int
			if err := s.db.Tx(ctx, store.System(), func(tx2 pgx.Tx) error {
				return tx2.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
					WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&waiting)
			}); err != nil {
				return err
			}
			if waiting > 0 {
				break
			}
			select {
			case a := <-done:
				t.Fatalf("the rename did not wait for the row: %d %s", a.code, a.body)
			default:
			}
			if time.Now().After(deadline) {
				t.Fatal("the rename never waited on the row lock")
			}
			time.Sleep(20 * time.Millisecond)
		}
		spec["resumePolicy"] = "restart"
		_, err := tx.Exec(ctx, `UPDATE runs SET state = 'stopped', state_reason = 'stop', spec = $2 WHERE id = $1`, r1, spec)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	a := <-done
	if a.code != http.StatusOK {
		t.Fatalf("rename: %d %s", a.code, a.body)
	}
	var state, reason, policy string
	err = s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT state, state_reason, spec->>'resumePolicy' FROM runs WHERE id = $1`, r1).Scan(&state, &reason, &policy)
	})
	if err != nil || state != StateStopped || reason != "stop" || policy != "restart" {
		t.Fatalf("the state change was lost: %s %s %s %v", state, reason, policy, err)
	}
	if name, specName := storedName(t, s, ctx, r1); name != "renamed meanwhile" || specName == nil || *specName != "renamed meanwhile" {
		t.Fatalf("the rename was lost: %q %v", name, specName)
	}
	var got Run
	if err := json.Unmarshal([]byte(a.body), &got); err != nil || got.State != StateStopped || got.Name != "renamed meanwhile" {
		t.Fatalf("answer: %s", a.body)
	}
}

package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/spec"
	"github.com/marcioapm/lux/internal/store"
)

// testShape is an 8-CPU m pool: m-05 (0.5, 2Gi, 10Gi), m-1 (1, 4Gi, 20Gi),
// m-2 (2, 8Gi, 40Gi, the default), m-4 (4, 16Gi, 80Gi), m-8 (8, 32Gi, 160Gi).
var testShape = PoolShape{Family: "m", Arch: "arm64", CPUs: 8, Memory: spec.Bytes(32 * gib), Disk: spec.Bytes(160 * gib), MinCPUs: 0.5, DefaultCPUs: 2}

// sized is the resources of ladder entry cpus of testShape, with the
// default pids and diskGrace submit adds.
func sized(cpus float64) spec.Resources {
	f := cpus / testShape.CPUs
	return withPids(spec.Resources{CPUs: cpus, Memory: spec.Bytes(float64(testShape.Memory) * f), Disk: spec.Bytes(float64(testShape.Disk) * f)})
}

// shapedPool adds a platform static pool whose id and name are id, with
// shape sh (nil: unshaped), and tenant t1.
func shapedPool(t *testing.T, s *Server, id string, sh *PoolShape) {
	t.Helper()
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1') ON CONFLICT DO NOTHING`)
	execSQL(t, s, ctx, `INSERT INTO pools (id, name, provider, shape) VALUES ($1, $1, 'static', $2)`, id, sh)
}

func baseSpec(pool string, res spec.Resources) spec.RunSpec {
	return spec.RunSpec{
		Image:     spec.Image{Ref: "alpine"},
		Workload:  spec.Workload{Adapter: "generic", Command: []string{"true"}},
		Volumes:   []spec.Volume{{Name: "data", Path: "/data", Kind: "state"}},
		Resources: res,
		Placement: spec.Placement{Pool: pool},
	}
}

func submitSized(s *Server, key, pool string, res spec.Resources) (*submitRunOutput, error) {
	return s.submitRun(tenantCtx("t1"), &submitRunInput{IdempotencyKey: key, Body: baseSpec(pool, res)})
}

// runSizing is a Run's stored requested_resources (as JSON text, "" for
// NULL) and resolved_size ("" for NULL).
func runSizing(t *testing.T, s *Server, id string) (requested, size string) {
	t.Helper()
	systemScan(t, s, `SELECT coalesce(requested_resources::text, ''), coalesce(resolved_size, '') FROM runs WHERE id = $1`, []any{id}, &requested, &size)
	return requested, size
}

// apiError is an error answer's code and message.
func apiError(t *testing.T, body []byte) (code, message string) {
	t.Helper()
	var e struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("error body %s: %v", body, err)
	}
	return e.Error.Code, e.Error.Message
}

// On a shaped pool, raw requests round up to the smallest size covering
// every dimension given; a size is that size; none is the default size.
// The Run's spec.resources is the size's, pids the default.
func TestSubmitResolvesShapedSize(t *testing.T) {
	for _, c := range []struct {
		name     string
		res      spec.Resources
		size     string
		want     spec.Resources
		requests string
	}{
		{"3 cpus and 10Gi round up to m-4", spec.Resources{CPUs: 3, Memory: spec.Bytes(10 * gib)}, "m-4-arm64", sized(4), `{"cpus": 3, "memory": 10737418240}`},
		{"1 cpu and 12Gi round up to m-4", spec.Resources{CPUs: 1, Memory: spec.Bytes(12 * gib)}, "m-4-arm64", sized(4), `{"cpus": 1, "memory": 12884901888}`},
		{"exact m-2 stays m-2", spec.Resources{CPUs: 2, Memory: spec.Bytes(8 * gib), Disk: spec.Bytes(40 * gib)}, "m-2-arm64", sized(2), `{"cpus": 2, "disk": 42949672960, "memory": 8589934592}`},
		{"0.25 cpus round up to m-05", spec.Resources{CPUs: 0.25}, "m-05-arm64", sized(0.5), `{"cpus": 0.25}`},
		{"disk alone", spec.Resources{Disk: spec.Bytes(41 * gib)}, "m-4-arm64", sized(4), `{"disk": 44023414784}`},
		{"a size", spec.Resources{Size: "m-1-arm64"}, "m-1-arm64", sized(1), `{"size": "m-1-arm64"}`},
		{"nothing: the default size", spec.Resources{}, "m-2-arm64", sized(2), `{}`},
		{"pids are not the size's", spec.Resources{CPUs: 1, Pids: 64}, "m-1-arm64", func() spec.Resources { r := sized(1); r.Pids = 64; return r }(), `{"cpus": 1}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := testServer(t)
			shapedPool(t, s, "shaped", &testShape)
			out, err := submitSized(s, "", "shaped", c.res)
			if err != nil {
				t.Fatal(err)
			}
			if got := storedResources(t, s, out.Body.ID); got != c.want {
				t.Errorf("stored resources %+v, want %+v", got, c.want)
			}
			if out.Body.Spec.Resources != c.want {
				t.Errorf("answer's resources %+v, want %+v", out.Body.Spec.Resources, c.want)
			}
			requested, size := runSizing(t, s, out.Body.ID)
			if requested != c.requests || size != c.size {
				t.Errorf("stored requested %s size %q, want %s %q", requested, size, c.requests, c.size)
			}
			sz := out.Body.Sizing
			if sz == nil || sz.Resolved == nil || sz.Requested == nil || sz.Resolved.Size != c.size ||
				sz.Resolved.CPUs != c.want.CPUs || sz.Resolved.Memory != c.want.Memory || sz.Resolved.Disk != c.want.Disk {
				t.Errorf("answer's sizing %+v", sz)
			}
			var ev string
			systemScan(t, s, `SELECT data->>'size' FROM run_events WHERE run_id = $1 AND type = 'submitted'`, []any{out.Body.ID}, &ev)
			if ev != c.size {
				t.Errorf("submitted event's size %q, want %q", ev, c.size)
			}
		})
	}
}

// luxd's resource defaults never fill a shaped request before it is
// rounded: a CPU-only request is not inflated by a large default memory or
// disk. They still apply on an unshaped pool, and pids on both.
func TestShapedSubmitIgnoresGlobalDefaults(t *testing.T) {
	s := testServer(t)
	s.cfg.Defaults = spec.Defaults{CPUs: 3, Memory: spec.Bytes(30 * gib), Disk: spec.Bytes(150 * gib), Pids: 512}
	shapedPool(t, s, "shaped", &testShape)
	shapedPool(t, s, "plain", nil)
	out, err := submitSized(s, "", "shaped", spec.Resources{CPUs: 1})
	if err != nil {
		t.Fatal(err)
	}
	want := spec.Resources{CPUs: 1, Memory: spec.Bytes(4 * gib), Disk: spec.Bytes(20 * gib), Pids: 512, DiskGrace: spec.SetDuration(0)}
	if got := storedResources(t, s, out.Body.ID); got != want {
		t.Errorf("cpu-only on a shaped pool: %+v, want m-1 %+v", got, want)
	}
	out, err = submitSized(s, "", "shaped", spec.Resources{})
	if err != nil {
		t.Fatal(err)
	}
	want = spec.Resources{CPUs: 2, Memory: spec.Bytes(8 * gib), Disk: spec.Bytes(40 * gib), Pids: 512, DiskGrace: spec.SetDuration(0)}
	if got := storedResources(t, s, out.Body.ID); got != want {
		t.Errorf("nothing on a shaped pool: %+v, want the pool's default m-2 %+v", got, want)
	}
	out, err = submitSized(s, "", "plain", spec.Resources{CPUs: 1})
	if err != nil {
		t.Fatal(err)
	}
	want = spec.Resources{CPUs: 1, Memory: spec.Bytes(30 * gib), Disk: spec.Bytes(150 * gib), Pids: 512, DiskGrace: spec.SetDuration(0)}
	if got := storedResources(t, s, out.Body.ID); got != want {
		t.Errorf("cpu-only on an unshaped pool: %+v, want luxd's defaults %+v", got, want)
	}
}

// Each refusal has its own code and message, and creates no Run.
func TestShapedSubmitRefusals(t *testing.T) {
	for _, c := range []struct {
		name string
		pool string
		res  spec.Resources
		code string
		msg  string
	}{
		{"size with cpus", "shaped", spec.Resources{Size: "m-2-arm64", CPUs: 2}, "size_conflict", "pool shaped: resources.size cannot be combined with cpus, memory or disk"},
		{"size with disk", "shaped", spec.Resources{Size: "m-2-arm64", Disk: spec.Bytes(gib)}, "size_conflict", "pool shaped: resources.size cannot be combined with cpus, memory or disk"},
		{"unknown size", "shaped", spec.Resources{Size: "m-3-arm64"}, "unknown_size", `pool shaped: unknown size: "m-3-arm64" is not in family "m" architecture "arm64" ladder`},
		{"another arch's size", "shaped", spec.Resources{Size: "m-2-amd64"}, "unknown_size", `pool shaped: unknown size: "m-2-amd64" is not in family "m" architecture "arm64" ladder`},
		{"more cpus than the whole host", "shaped", spec.Resources{CPUs: 16}, "size_too_large", "pool shaped: request cpus=16 exceeds the largest size m-8-arm64 (cpus=8 memory=32Gi disk=160Gi)"},
		{"more memory than the whole host", "shaped", spec.Resources{CPUs: 1, Memory: spec.Bytes(33 * gib)}, "size_too_large", "pool shaped: request cpus=1 memory=33Gi exceeds the largest size m-8-arm64 (cpus=8 memory=32Gi disk=160Gi)"},
		{"negative cpus", "shaped", spec.Resources{CPUs: -1}, "invalid_spec", "invalid spec: resources must not be negative"},
		{"size on an unshaped pool", "plain", spec.Resources{Size: "m-2-arm64"}, "size_unsupported", "pool plain has no shape, so no sizes: ask with resources.cpus, memory and disk"},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := testServer(t)
			shapedPool(t, s, "shaped", &testShape)
			shapedPool(t, s, "plain", nil)
			_, err := submitSized(s, "", c.pool, c.res)
			he := refused(t, err, http.StatusUnprocessableEntity, c.code, "submit")
			if he.Error() != c.msg {
				t.Errorf("message %q, want %q", he.Error(), c.msg)
			}
			if n := queryOne[int](t, s, `SELECT count(*) FROM runs`); n != 0 {
				t.Errorf("%d Runs after a refusal", n)
			}
		})
	}
}

// A raw submit to an unshaped pool is stored exactly as Normalize and
// SplitSecrets leave it, with no request, size or sizing, and its
// submitted event has the keys it always had.
func TestUnshapedSubmitUnchanged(t *testing.T) {
	s := testServer(t)
	shapedPool(t, s, "plain", nil)
	res := spec.Resources{CPUs: 3, Memory: spec.Bytes(10 * gib)}
	key := apiKey(t, s, new("t1"), "run", "read")
	w := apiCall(t, s, key, http.MethodPost, "/v1/runs", baseSpec("plain", res))
	if w.Code != http.StatusCreated {
		t.Fatalf("POST: %d %s", w.Code, w.Body)
	}
	var answer map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &answer); err != nil {
		t.Fatal(err)
	}
	if _, ok := answer["sizing"]; ok {
		t.Errorf("answer has sizing: %s", w.Body)
	}
	id := answer["id"].(string)
	want := baseSpec("plain", res)
	if err := want.Normalize(s.cfg.Defaults); err != nil {
		t.Fatal(err)
	}
	stored, _, _ := want.SplitSecrets()
	wantJSON, _ := json.Marshal(stored)
	var gotSpec, wantSpec map[string]any
	var raw []byte
	systemScan(t, s, `SELECT spec FROM runs WHERE id = $1`, []any{id}, &raw)
	if err := json.Unmarshal(raw, &gotSpec); err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(wantJSON, &wantSpec)
	if !reflect.DeepEqual(gotSpec, wantSpec) {
		t.Errorf("stored spec %s, want %s", raw, wantJSON)
	}
	if requested, size := runSizing(t, s, id); requested != "" || size != "" {
		t.Errorf("requested %q size %q, want both NULL", requested, size)
	}
	var evKeys []string
	systemScan(t, s, `SELECT array_agg(k ORDER BY k) FROM run_events, jsonb_object_keys(data) k WHERE run_id = $1 AND type = 'submitted'`, []any{id}, &evKeys)
	if want := []string{"by", "pool", "poolFrom", "poolOwner"}; !reflect.DeepEqual(evKeys, want) {
		t.Errorf("submitted event keys %v, want %v", evKeys, want)
	}
	g := apiCall(t, s, key, http.MethodGet, "/v1/runs/"+id, nil)
	if g.Code != http.StatusOK {
		t.Fatalf("GET: %d %s", g.Code, g.Body)
	}
	var got struct {
		Sizing json.RawMessage `json:"sizing"`
		Spec   struct {
			Resources map[string]any `json:"resources"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(g.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Sizing != nil {
		t.Errorf("GET sizing %s, want none", got.Sizing)
	}
	if _, ok := got.Spec.Resources["size"]; ok || got.Spec.Resources == nil {
		t.Errorf("GET spec.resources %v, want resources without size", got.Spec.Resources)
	}
}

// An idempotent retry is compared by what was asked, never by what it
// resolved to: the same request returns the Run, another that rounds to
// the same size is a conflict. On an unshaped pool a retry still returns
// the first Run whatever it asks, as before.
func TestShapedIdempotency(t *testing.T) {
	s := testServer(t)
	shapedPool(t, s, "shaped", &testShape)
	shapedPool(t, s, "plain", nil)
	first, err := submitSized(s, "k", "shaped", spec.Resources{CPUs: 3})
	if err != nil {
		t.Fatal(err)
	}
	again, err := submitSized(s, "k", "shaped", spec.Resources{CPUs: 3})
	if err != nil || again.Status != http.StatusOK || again.Body.ID != first.Body.ID {
		t.Fatalf("same request: %v %+v, want 200 with %s", err, again, first.Body.ID)
	}
	for _, other := range []spec.Resources{{CPUs: 4}, {Size: "m-4-arm64"}, {CPUs: 3, Memory: spec.Bytes(gib)}, {}} {
		_, err := submitSized(s, "k", "shaped", other)
		refused(t, err, http.StatusConflict, "idempotency_conflict", "retry with other resources")
	}
	if n := queryOne[int](t, s, `SELECT count(*) FROM runs`); n != 1 {
		t.Fatalf("%d Runs, want 1", n)
	}
	plain, err := submitSized(s, "p", "plain", spec.Resources{CPUs: 1})
	if err != nil {
		t.Fatal(err)
	}
	again, err = submitSized(s, "p", "plain", spec.Resources{CPUs: 2})
	if err != nil || again.Body.ID != plain.Body.ID {
		t.Fatalf("unshaped retry with other resources: %v, want the first Run", err)
	}
}

// submitHTTP POSTs body to /v1/runs with an Idempotency-Key.
func submitHTTP(t *testing.T, s *Server, key, idem string, body any) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/v1/runs", strings.NewReader(string(b)))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Idempotency-Key", idem)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	return w
}

// A retry is compared by the values asked, not their spelling: "4Gi" and
// 4294967296 bytes, 2 and 2.0 CPUs are one request.
func TestShapedIdempotencyComparesValues(t *testing.T) {
	s := testServer(t)
	shapedPool(t, s, "shaped", &testShape)
	key := apiKey(t, s, new("t1"), "run", "read")
	body := func(res map[string]any) map[string]any {
		return map[string]any{"image": map[string]any{"ref": "alpine"}, "workload": map[string]any{"adapter": "generic", "command": []string{"true"}},
			"placement": map[string]any{"pool": "shaped"}, "resources": res}
	}
	first := submitHTTP(t, s, key, "e", body(map[string]any{"cpus": 2, "memory": "4Gi"}))
	if first.Code != http.StatusCreated {
		t.Fatalf("first: %d %s", first.Code, first.Body)
	}
	// json.Marshal writes 2.0 as 2: the body is spelled by hand.
	b := `{"image":{"ref":"alpine"},"workload":{"adapter":"generic","command":["true"]},"placement":{"pool":"shaped"},"resources":{"cpus":2.0,"memory":4294967296}}`
	again := submitHTTP(t, s, key, "e", json.RawMessage(b))
	if again.Code != http.StatusOK {
		t.Fatalf("retry spelled differently: %d %s, want 200", again.Code, again.Body)
	}
	var a, r struct{ ID string }
	_ = json.Unmarshal(first.Body.Bytes(), &a)
	_ = json.Unmarshal(again.Body.Bytes(), &r)
	if a.ID == "" || a.ID != r.ID {
		t.Errorf("retry returned %q, want %q", r.ID, a.ID)
	}
}

// Submits that miss the replay SELECT (the first Run's INSERT has not
// committed) and then meet its unique index compare their request with
// that Run's, as a replay does: another request is 409, the same one is
// that Run (200).
func TestConcurrentShapedIdempotency(t *testing.T) {
	for _, c := range []struct {
		name string
		res  spec.Resources
		code int
	}{
		{"another request", spec.Resources{CPUs: 4}, http.StatusConflict},
		{"the same request", spec.Resources{CPUs: 3}, http.StatusOK},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := testServer(t)
			ctx := testDeadline(t)
			shapedPool(t, s, "shaped", &testShape)
			holder, err := s.db.Pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { holder.Rollback(context.Background()) })
			if _, err := holder.Exec(ctx, `SELECT set_config('lux.system', 'on', true)`); err != nil {
				t.Fatal(err)
			}
			if _, err := holder.Exec(ctx, `INSERT INTO runs (id, tenant_id, spec, state, idempotency_key, pool_id, requested_resources, resolved_size)
				VALUES ('run_first', 't1', '{"image": {"ref": "alpine"}, "workload": {"adapter": "generic", "command": ["true"]}}', 'submitted', 'k',
					'shaped', '{"cpus": 3}', 'm-4-arm64')`); err != nil {
				t.Fatal(err)
			}
			var pid int
			if err := holder.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
				t.Fatal(err)
			}
			var out *submitRunOutput
			done := make(chan error, 1)
			go func() {
				var err error
				out, err = submitSized(s, "k", "shaped", c.res)
				done <- err
			}()
			for {
				var blocked int
				if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
					return tx.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
						WHERE datname = current_database() AND $1 = ANY(pg_blocking_pids(pid))`, pid).Scan(&blocked)
				}); err != nil {
					t.Fatal(err)
				}
				if blocked == 1 {
					break
				}
				select {
				case err := <-done:
					t.Fatalf("the submit finished before the first Run committed: %v", err)
				case <-ctx.Done():
					t.Fatal("the submit never blocked on the first Run")
				case <-time.After(10 * time.Millisecond):
				}
			}
			if err := holder.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			var err2 error
			select {
			case err2 = <-done:
			case <-ctx.Done():
				t.Fatal("the submit did not finish")
			}
			if c.code == http.StatusConflict {
				refused(t, err2, http.StatusConflict, "idempotency_conflict", "another request")
			} else if err2 != nil || out.Status != http.StatusOK || out.Body.ID != "run_first" {
				t.Fatalf("same request: %v %+v, want 200 with run_first", err2, out)
			}
			if n := queryOne[int](t, s, `SELECT count(*) FROM runs`); n != 1 {
				t.Errorf("%d Runs, want 1", n)
			}
		})
	}
}

// getSizing is GET /v1/runs/{id}'s sizing, nil when absent.
func getSizing(t *testing.T, s *Server, key, id string) *RunSizing {
	t.Helper()
	g := apiCall(t, s, key, http.MethodGet, "/v1/runs/"+id, nil)
	var r struct {
		Sizing *RunSizing `json:"sizing"`
	}
	if g.Code != http.StatusOK || json.Unmarshal(g.Body.Bytes(), &r) != nil {
		t.Fatalf("GET: %d %s", g.Code, g.Body)
	}
	return r.Sizing
}

// A Run's sizing.resolved is shown only while its pool is shaped and its
// size is on that pool's current ladder. With the shape removed, a resume
// in place clears the size; with the shape changed so the size is gone, a
// resume asking nothing rounds the Run's resources up to a new size.
func TestShapeRemovedOrChanged(t *testing.T) {
	requested := float64(3)
	asked := &SizeRequest{CPUs: &requested}
	// Family c: no m size is one of its sizes; its whole host is 8 CPUs,
	// 16 GiB, 80 GiB, so c-4 has 4 CPUs, 8 GiB, 40 GiB.
	cShape := PoolShape{Family: "c", Arch: "arm64", CPUs: 8, Memory: spec.Bytes(16 * gib), Disk: spec.Bytes(80 * gib), MinCPUs: 0.5, DefaultCPUs: 2}
	// stoppedM4 is a stopped Run that asked for 3 CPUs in pool "shaped",
	// so has m-4-arm64.
	stoppedM4 := func(t *testing.T) (*Server, string) {
		t.Helper()
		s := testServer(t)
		shapedPool(t, s, "shaped", &testShape)
		return s, stoppedShaped(t, s, "shaped", spec.Resources{CPUs: 3}, i64(gib))
	}
	t.Run("removed", func(t *testing.T) {
		s, id := stoppedM4(t)
		key := apiKey(t, s, new("t1"), "read")
		execSQL(t, s, context.Background(), `UPDATE pools SET shape = NULL WHERE id = 'shaped'`)
		if got := getSizing(t, s, key, id); !reflect.DeepEqual(got, &RunSizing{Requested: asked}) {
			t.Errorf("GET sizing %+v, want the request alone", got)
		}
		out, err := resumeReq(s, id, resumeRequest{})
		if err != nil {
			t.Fatal(err)
		}
		if _, size := runSizing(t, s, id); size != "" {
			t.Errorf("resolved_size %q after a resume in the unshaped pool, want none", size)
		}
		if got := storedResources(t, s, id); got != sized(4) || out.Body.Resize != nil {
			t.Errorf("stored %+v resize %+v, want m-4's resources kept", got, out.Body.Resize)
		}
	})
	t.Run("changed", func(t *testing.T) {
		s, id := stoppedM4(t)
		key := apiKey(t, s, new("t1"), "read")
		execSQL(t, s, context.Background(), `UPDATE pools SET shape = $1 WHERE id = 'shaped'`, cShape)
		if got := getSizing(t, s, key, id); !reflect.DeepEqual(got, &RunSizing{Requested: asked}) {
			t.Errorf("GET sizing %+v, want the request alone", got)
		}
		// m-4's 4 CPUs, 16 GiB, 80 GiB round up to c-8 (8, 16 GiB, 80 GiB).
		out, err := resumeReq(s, id, resumeRequest{})
		if err != nil {
			t.Fatal(err)
		}
		want := withPids(spec.Resources{CPUs: 8, Memory: spec.Bytes(16 * gib), Disk: spec.Bytes(80 * gib)})
		if got := storedResources(t, s, id); got != want {
			t.Errorf("stored %+v, want c-8's %+v", got, want)
		}
		if _, size := runSizing(t, s, id); size != "c-8-arm64" {
			t.Errorf("resolved_size %q, want c-8-arm64", size)
		}
		if rz := out.Body.Resize; rz == nil || rz.Size != "c-8-arm64" || rz.Applied != want {
			t.Errorf("resize %+v", rz)
		}
		wantSizing := &RunSizing{Requested: asked, Resolved: &SizeResolved{Size: "c-8-arm64", CPUs: 8, Memory: spec.Bytes(16 * gib), Disk: spec.Bytes(80 * gib)}}
		if got := getSizing(t, s, key, id); !reflect.DeepEqual(got, wantSizing) {
			t.Errorf("GET sizing %+v, want %+v", got, wantSizing)
		}
	})
	t.Run("same names, new dimensions", func(t *testing.T) {
		s, id := stoppedM4(t)
		key := apiKey(t, s, new("t1"), "read")
		// Doubled memory and disk keep every name: m-4-arm64 is now 4 CPUs,
		// 32 GiB, 160 GiB, while the Run still has 16 GiB, 80 GiB.
		big := testShape
		big.Memory, big.Disk = 2*testShape.Memory, 2*testShape.Disk
		execSQL(t, s, context.Background(), `UPDATE pools SET shape = $1 WHERE id = 'shaped'`, big)
		if got := getSizing(t, s, key, id); !reflect.DeepEqual(got, &RunSizing{Requested: asked}) {
			t.Errorf("GET sizing %+v, want the request alone", got)
		}
		want := withPids(spec.Resources{CPUs: 4, Memory: spec.Bytes(32 * gib), Disk: spec.Bytes(160 * gib)})
		dry, err := resumeReq(s, id, resumeRequest{ValidateOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		if got := storedResources(t, s, id); got != sized(4) {
			t.Errorf("validateOnly stored %+v, want m-4's old %+v", got, sized(4))
		}
		out, err := resumeReq(s, id, resumeRequest{})
		if err != nil {
			t.Fatal(err)
		}
		if got := storedResources(t, s, id); got != want {
			t.Errorf("stored %+v, want the new m-4's %+v", got, want)
		}
		if _, size := runSizing(t, s, id); size != "m-4-arm64" {
			t.Errorf("resolved_size %q, want m-4-arm64", size)
		}
		rz := out.Body.Resize
		if rz == nil || rz.Size != "m-4-arm64" || rz.Applied != want {
			t.Errorf("resize %+v, want m-4-arm64 with %+v", rz, want)
		}
		if !reflect.DeepEqual(dry.Body.Resize, rz) {
			t.Errorf("validateOnly resize %+v, the resume's %+v", dry.Body.Resize, rz)
		}
	})
	t.Run("unchanged", func(t *testing.T) {
		s, id := stoppedM4(t)
		out, err := resumeReq(s, id, resumeRequest{})
		if err != nil {
			t.Fatal(err)
		}
		if got := storedResources(t, s, id); got != sized(4) || out.Body.Resize != nil {
			t.Errorf("stored %+v resize %+v, want m-4's resources and no resize", got, out.Body.Resize)
		}
		if _, size := runSizing(t, s, id); size != "m-4-arm64" {
			t.Errorf("resolved_size %q, want m-4-arm64", size)
		}
	})
	t.Run("larger than the new host", func(t *testing.T) {
		s := testServer(t)
		shapedPool(t, s, "shaped", &testShape)
		shapedPool(t, s, "plain", nil)
		id := stoppedShaped(t, s, "shaped", spec.Resources{CPUs: 8}, i64(gib))
		// m-8's 32 GiB exceed c's whole 16 GiB host: no c size holds it.
		execSQL(t, s, context.Background(), `UPDATE pools SET shape = $1 WHERE id = 'shaped'`, cShape)
		for range 2 {
			_, err := resumeReq(s, id, resumeRequest{})
			refused(t, err, http.StatusUnprocessableEntity, "size_too_large", "a plain resume in place")
		}
		if got := storedResources(t, s, id); got != sized(8) {
			t.Errorf("stored %+v after the refusals, want m-8's %+v", got, sized(8))
		}
		if _, size := runSizing(t, s, id); size != "m-8-arm64" {
			t.Errorf("resolved_size %q after the refusals, want m-8-arm64", size)
		}
		out, err := resumeReq(s, id, resumeRequest{Placement: &resumeTarget{PoolID: "plain"}})
		if err != nil {
			t.Fatalf("leaving with a pool move: %v", err)
		}
		if got := storedResources(t, s, id); got != sized(8) || out.Body.Resize != nil {
			t.Errorf("stored %+v resize %+v, want m-8's resources kept", got, out.Body.Resize)
		}
		if _, size := runSizing(t, s, id); size != "" {
			t.Errorf("resolved_size %q in the unshaped pool, want none", size)
		}
	})
}

// GET and the list carry each shaped Run's request and resolution; an
// unshaped Run has neither.
func TestSizingInGetAndList(t *testing.T) {
	s := testServer(t)
	shapedPool(t, s, "shaped", &testShape)
	shapedPool(t, s, "plain", nil)
	shaped, err := submitSized(s, "", "shaped", spec.Resources{CPUs: 1, Memory: spec.Bytes(12 * gib)})
	if err != nil {
		t.Fatal(err)
	}
	plain, err := submitSized(s, "", "plain", spec.Resources{CPUs: 1})
	if err != nil {
		t.Fatal(err)
	}
	key := apiKey(t, s, new("t1"), "read")
	type sizing struct {
		Requested map[string]any `json:"requested"`
		Resolved  map[string]any `json:"resolved"`
	}
	wantSizing := &sizing{
		Requested: map[string]any{"cpus": 1.0, "memory": float64(12 * gib)},
		Resolved:  map[string]any{"size": "m-4-arm64", "cpus": 4.0, "memory": float64(16 * gib), "disk": float64(80 * gib)},
	}
	var one struct {
		Sizing *sizing `json:"sizing"`
	}
	g := apiCall(t, s, key, http.MethodGet, "/v1/runs/"+shaped.Body.ID, nil)
	if err := json.Unmarshal(g.Body.Bytes(), &one); err != nil || !reflect.DeepEqual(one.Sizing, wantSizing) {
		t.Errorf("GET sizing %s, want %+v", g.Body, wantSizing)
	}
	var list struct {
		Runs []struct {
			ID     string  `json:"id"`
			Sizing *sizing `json:"sizing"`
		} `json:"runs"`
	}
	l := apiCall(t, s, key, http.MethodGet, "/v1/runs", nil)
	if err := json.Unmarshal(l.Body.Bytes(), &list); err != nil || len(list.Runs) != 2 {
		t.Fatalf("list %s: %v", l.Body, err)
	}
	for _, r := range list.Runs {
		switch r.ID {
		case shaped.Body.ID:
			if !reflect.DeepEqual(r.Sizing, wantSizing) {
				t.Errorf("listed shaped sizing %+v", r.Sizing)
			}
		case plain.Body.ID:
			if r.Sizing != nil {
				t.Errorf("listed unshaped sizing %+v", r.Sizing)
			}
		}
	}
}

// stoppedShaped submits res to pool (stoppedRun's fixture, in that pool),
// stopped with its snapshot's placement at peak disk use.
func stoppedShaped(t *testing.T, s *Server, pool string, res spec.Resources, peakDisk *int64) string {
	t.Helper()
	ctx := context.Background()
	out, err := submitSized(s, "", pool, res)
	if err != nil {
		t.Fatal(err)
	}
	id := out.Body.ID
	execSQL(t, s, ctx, `INSERT INTO hosts (id, name, pool_id, state, capacity, last_heartbeat)
		VALUES ('ha', 'ha', $1, 'ready', '{"cpus":8,"memory":34359738368}', now()) ON CONFLICT DO NOTHING`, pool)
	execSQL(t, s, ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state, resources, peak_disk_bytes)
		VALUES ('p-'||$1, 't1', $1, 'ha', 1, 'exited', '{}', $2)`, id, peakDisk)
	execSQL(t, s, ctx, `INSERT INTO snapshots (id, tenant_id, run_id, placement_id, epoch, manifest, host_id, uploaded)
		VALUES ('snap-'||$1, 't1', $1, 'p-'||$1, 1, '{"volumes":[]}', 'ha', true)`, id)
	execSQL(t, s, ctx, `UPDATE runs SET state = 'stopped', current_epoch = 1, snapshot_id = 'snap-'||id WHERE id = $1`, id)
	return id
}

func resumeReq(s *Server, id string, req resumeRequest) (*resumeOutput, error) {
	return s.resumeRun(tenantCtx("t1"), &resumeRunInput{RunPath: RunPath{ID: id}, Body: &req})
}

// A resume into a shaped pool re-resolves the Run there: its current
// resources rounded up, or what the resume asked for. validateOnly answers
// the same resize and changes nothing.
func TestResumeIntoShapedPoolResolves(t *testing.T) {
	for _, c := range []struct {
		name string
		res  *resumeResources
		size string
		want spec.Resources
	}{
		{"current resources rounded up", nil, "m-4-arm64", sized(4)},
		{"a size", &resumeResources{Size: "m-1-arm64"}, "m-1-arm64", sized(1)},
		{"raw resources rounded up", &resumeResources{CPUs: f64(1.5)}, "m-2-arm64", sized(2)},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := testServer(t)
			shapedPool(t, s, "plain", nil)
			shapedPool(t, s, "shaped", &testShape)
			id := stoppedShaped(t, s, "plain", spec.Resources{CPUs: 3, Memory: spec.Bytes(10 * gib), Disk: spec.Bytes(20 * gib)}, i64(gib))
			before := storedResources(t, s, id)
			req := resumeRequest{Placement: &resumeTarget{PoolID: "shaped"}, Resources: c.res, ValidateOnly: true}
			dry, err := resumeReq(s, id, req)
			if err != nil {
				t.Fatal(err)
			}
			if got := storedResources(t, s, id); got != before || storedState(t, s, id) != StateStopped {
				t.Fatalf("validateOnly changed the Run: %+v", got)
			}
			if _, size := runSizing(t, s, id); size != "" {
				t.Fatalf("validateOnly stored size %q", size)
			}
			req.ValidateOnly = false
			out, err := resumeReq(s, id, req)
			if err != nil {
				t.Fatal(err)
			}
			if got := storedResources(t, s, id); got != c.want {
				t.Errorf("stored %+v, want %+v", got, c.want)
			}
			if _, size := runSizing(t, s, id); size != c.size {
				t.Errorf("stored size %q, want %q", size, c.size)
			}
			rz := out.Body.Resize
			if rz == nil || rz.Size != c.size || rz.Applied != c.want {
				t.Fatalf("answer's resize %+v", rz)
			}
			if !reflect.DeepEqual(dry.Body.Resize, rz) {
				t.Errorf("validateOnly resize %+v, the resume's %+v", dry.Body.Resize, rz)
			}
			if out.Body.Sizing == nil || out.Body.Sizing.Resolved == nil || out.Body.Sizing.Resolved.Size != c.size {
				t.Errorf("answer's sizing %+v", out.Body.Sizing)
			}
			if got := resumeEvent(t, s, id)["resources"].(map[string]any)["size"]; got != c.size {
				t.Errorf("event's resources.size %v, want %s", got, c.size)
			}
		})
	}
}

// A Run moved out of a shaped pool loses its size and keeps its
// resources; size is refused there.
func TestResumeOutOfShapedPool(t *testing.T) {
	s := testServer(t)
	shapedPool(t, s, "plain", nil)
	shapedPool(t, s, "shaped", &testShape)
	id := stoppedShaped(t, s, "shaped", spec.Resources{CPUs: 3}, i64(gib))
	_, err := resumeReq(s, id, resumeRequest{Placement: &resumeTarget{PoolID: "plain"}, Resources: &resumeResources{Size: "m-4-arm64"}})
	refused(t, err, http.StatusUnprocessableEntity, "size_unsupported", "size into an unshaped pool")
	if _, size := runSizing(t, s, id); size != "m-4-arm64" {
		t.Fatalf("after a refusal: size %q", size)
	}
	out, err := resumeReq(s, id, resumeRequest{Placement: &resumeTarget{PoolID: "plain"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := storedResources(t, s, id); got != sized(4) || out.Body.Resize != nil {
		t.Errorf("stored %+v resize %+v, want m-4's resources kept and no resize", got, out.Body.Resize)
	}
	requested, size := runSizing(t, s, id)
	if size != "" || requested != `{"cpus": 3}` {
		t.Errorf("requested %s size %q, want the submit's request and no size", requested, size)
	}
	if out.Body.Sizing == nil || out.Body.Sizing.Resolved != nil {
		t.Errorf("answer's sizing %+v, want the request alone", out.Body.Sizing)
	}
}

// A resume's size in its own shaped pool: larger applies; smaller only
// when the saved state still fits its disk with headroom, refused
// otherwise (never kept at the old disk); refusals change nothing.
func TestResumeShapedSize(t *testing.T) {
	const below = "size m-1-arm64 has 20.0 GiB of disk, less than the Run has now: "
	const tail = "; a shaped pool never keeps a larger disk than its size, so choose a size with more disk"
	for _, c := range []struct {
		name      string
		peak      *int64
		placement string // the snapshot's placement's state; "": exited
		res       resumeResources
		code      string
		size      string
		want      spec.Resources
		msg       string
	}{
		{"larger size", i64(gib), "", resumeResources{Size: "m-8-arm64"}, "", "m-8-arm64", sized(8), ""},
		{"raw cpus round up", i64(gib), "", resumeResources{CPUs: f64(3)}, "", "m-4-arm64", sized(4), ""},
		{"smaller, saved state fits", i64(30 * gib), "", resumeResources{Size: "m-2-arm64"}, "", "m-2-arm64", sized(2), ""},
		{"smaller, saved state too large", i64(30 * gib), "", resumeResources{Size: "m-1-arm64"}, "size_below_saved_state", "", spec.Resources{},
			below + "its saved state used up to 30.0 GiB, which needs at least 37.5 GiB of disk (that plus max(25%, 1 GiB))" + tail},
		{"smaller, no measurement", nil, "", resumeResources{CPUs: f64(1)}, "size_below_saved_state", "", spec.Resources{},
			below + "no disk use is recorded for the snapshot it resumes from" + tail},
		{"smaller, measurement not final", i64(gib), "lost", resumeResources{Size: "m-1-arm64"}, "size_below_saved_state", "", spec.Resources{},
			below + "the placement that took the snapshot it resumes from ended without reporting its final disk use" + tail},
		{"size with cpus", i64(gib), "", resumeResources{Size: "m-2-arm64", CPUs: f64(2)}, "size_conflict", "", spec.Resources{},
			"pool shaped: resources.size cannot be combined with cpus, memory or disk"},
		{"unknown size", i64(gib), "", resumeResources{Size: "m-16-arm64"}, "unknown_size", "", spec.Resources{},
			`pool shaped: unknown size: "m-16-arm64" is not in family "m" architecture "arm64" ladder`},
		{"oversized", i64(gib), "", resumeResources{Memory: bytesPtr(64 * gib)}, "size_too_large", "", spec.Resources{},
			"pool shaped: request memory=64Gi exceeds the largest size m-8-arm64 (cpus=8 memory=32Gi disk=160Gi)"},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := testServer(t)
			shapedPool(t, s, "shaped", &testShape)
			id := stoppedShaped(t, s, "shaped", spec.Resources{Size: "m-4-arm64"}, c.peak)
			if c.placement != "" {
				execSQL(t, s, context.Background(), `UPDATE placements SET state = $2 WHERE run_id = $1`, id, c.placement)
			}
			res := c.res
			if c.code != "" {
				// validateOnly answers the refusal the resume meets.
				for _, dry := range []bool{true, false} {
					_, err := resumeReq(s, id, resumeRequest{Resources: &res, ValidateOnly: dry})
					he := refused(t, err, http.StatusUnprocessableEntity, c.code, fmt.Sprintf("resume (validateOnly %v)", dry))
					if he.Error() != c.msg {
						t.Errorf("validateOnly %v: message %q, want %q", dry, he.Error(), c.msg)
					}
				}
				if got, state := storedResources(t, s, id), storedState(t, s, id); got != sized(4) || state != StateStopped {
					t.Errorf("after a refusal: %s %+v", state, got)
				}
				if _, size := runSizing(t, s, id); size != "m-4-arm64" {
					t.Errorf("after a refusal: size %q", size)
				}
				return
			}
			out, err := resumeReq(s, id, resumeRequest{Resources: &res})
			if err != nil {
				t.Fatal(err)
			}
			if got := storedResources(t, s, id); got != c.want {
				t.Errorf("stored %+v, want %+v", got, c.want)
			}
			if rz := out.Body.Resize; rz == nil || rz.Size != c.size || rz.Disk != nil {
				t.Errorf("resize %+v, want size %s and no disk kept", rz, c.size)
			}
			if requested, size := runSizing(t, s, id); size != c.size || requested != `{"size": "m-4-arm64"}` {
				t.Errorf("stored requested %s size %q: the submit's request stays, the size is the resume's", requested, size)
			}
		})
	}
}

// A resume's diskGrace in a shaped pool is applied beside a size, and
// alone keeps the Run's size: the grace is no size dimension.
func TestResumeShapedDiskGrace(t *testing.T) {
	grace := spec.SetDuration(5 * time.Minute)
	for _, c := range []struct {
		name string
		res  resumeResources
		want spec.Resources
	}{
		{"alone", resumeResources{DiskGrace: &spec.Duration{Duration: 5 * time.Minute}}, sized(4)},
		{"with a size", resumeResources{Size: "m-8-arm64", DiskGrace: &spec.Duration{Duration: 5 * time.Minute}}, sized(8)},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := testServer(t)
			shapedPool(t, s, "shaped", &testShape)
			id := stoppedShaped(t, s, "shaped", spec.Resources{Size: "m-4-arm64"}, i64(gib))
			res := c.res
			out, err := resumeReq(s, id, resumeRequest{Resources: &res})
			if err != nil {
				t.Fatal(err)
			}
			want := c.want
			want.DiskGrace = grace
			if got := storedResources(t, s, id); got != want {
				t.Errorf("stored %+v, want %+v", got, want)
			}
			if rz := out.Body.Resize; rz == nil || rz.Applied != want {
				t.Errorf("resize %+v, want applied %+v", rz, want)
			}
			if _, size := runSizing(t, s, id); size != fmt.Sprintf("m-%g-arm64", c.want.CPUs) {
				t.Errorf("size %q, want the size of %+v", size, c.want)
			}
		})
	}
}

// size on a resume of an unshaped Run is refused; its raw resize keeps
// today's disk-kept rule.
func TestResumeUnshapedSize(t *testing.T) {
	s := testServer(t)
	shapedPool(t, s, "plain", nil)
	id := stoppedShaped(t, s, "plain", resizeBase, i64(2*gib))
	_, err := resumeReq(s, id, resumeRequest{Resources: &resumeResources{Size: "m-2-arm64"}})
	refused(t, err, http.StatusUnprocessableEntity, "size_unsupported", "size on an unshaped pool")
	out, err := resumeReq(s, id, resumeRequest{Resources: &resumeResources{Disk: spec.Bytes(gib)}})
	if err != nil {
		t.Fatal(err)
	}
	if rz := out.Body.Resize; rz == nil || rz.Disk == nil || rz.Size != "" || rz.Applied.Disk != resizeBase.Disk {
		t.Errorf("resize %+v, want the disk kept and no size", rz)
	}
}

// A pool's shape is checked at create and update (422 invalid_pool),
// stored, listed with its sizes, and removed by a set without it; an
// unshaped pool lists neither.
func TestPoolShapeAPI(t *testing.T) {
	s := testServer(t)
	execSQL(t, s, context.Background(), `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`)
	key := apiKey(t, s, new("t1"), "admin", "read")
	shape := func(mod func(m map[string]any)) map[string]any {
		m := map[string]any{"family": "m", "arch": "arm64", "cpus": 8, "memory": "32Gi", "disk": "160Gi", "minCpus": 0.5, "defaultCpus": 2}
		if mod != nil {
			mod(m)
		}
		return m
	}
	for _, c := range []struct {
		name   string
		shape  map[string]any
		inText string
	}{
		{"cpus not a power of two", shape(func(m map[string]any) { m["cpus"] = 6 }), "nominal CPUs 6 is not a power of two"},
		{"minimum below 0.5", shape(func(m map[string]any) { m["minCpus"] = 0.25 }), "minimum CPUs 0.25 is below 0.5"},
		{"default not a size", shape(func(m map[string]any) { m["defaultCpus"] = 3 }), "default CPUs 3 is not a ladder entry"},
		{"unknown arch", shape(func(m map[string]any) { m["arch"] = "riscv64" }), `invalid architecture "riscv64"`},
		{"bad family", shape(func(m map[string]any) { m["family"] = "M_" }), `invalid family "M_"`},
		{"indivisible memory", shape(func(m map[string]any) { m["memory"] = 32*gib + 1 }), "not divisible"},
		{"no disk", shape(func(m map[string]any) { delete(m, "disk") }), "memory and disk must be positive"},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := apiCall(t, s, key, http.MethodPost, "/v1/pools", map[string]any{"name": "shaped", "provider": "static", "shape": c.shape})
			if code, msg := apiError(t, w.Body.Bytes()); w.Code != http.StatusUnprocessableEntity || code != "invalid_pool" ||
				!strings.HasPrefix(msg, "invalid pool shape: ") || !strings.Contains(msg, c.inText) {
				t.Errorf("POST: %d %s, want 422 invalid_pool with %q", w.Code, w.Body, c.inText)
			}
		})
	}
	if n := queryOne[int](t, s, `SELECT count(*) FROM pools`); n != 0 {
		t.Fatalf("%d pools stored after refusals", n)
	}
	w := apiCall(t, s, key, http.MethodPost, "/v1/pools", map[string]any{"name": "shaped", "provider": "static", "shape": shape(nil)})
	if w.Code != http.StatusOK {
		t.Fatalf("POST: %d %s", w.Code, w.Body)
	}
	apiCall(t, s, key, http.MethodPost, "/v1/pools", map[string]any{"name": "plain", "provider": "static"})
	var list struct {
		Pools []Pool `json:"pools"`
	}
	l := apiCall(t, s, key, http.MethodGet, "/v1/pools", nil)
	if err := json.Unmarshal(l.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	wantSizes := []PoolSize{
		{Name: "m-05-arm64", CPUs: 0.5, Memory: spec.Bytes(2 * gib), Disk: spec.Bytes(10 * gib), Fraction: 0.0625},
		{Name: "m-1-arm64", CPUs: 1, Memory: spec.Bytes(4 * gib), Disk: spec.Bytes(20 * gib), Fraction: 0.125},
		{Name: "m-2-arm64", CPUs: 2, Memory: spec.Bytes(8 * gib), Disk: spec.Bytes(40 * gib), Fraction: 0.25, Default: true},
		{Name: "m-4-arm64", CPUs: 4, Memory: spec.Bytes(16 * gib), Disk: spec.Bytes(80 * gib), Fraction: 0.5},
		{Name: "m-8-arm64", CPUs: 8, Memory: spec.Bytes(32 * gib), Disk: spec.Bytes(160 * gib), Fraction: 1},
	}
	for _, pl := range list.Pools {
		switch pl.Name {
		case "shaped":
			if pl.Shape == nil || *pl.Shape != testShape || !reflect.DeepEqual(pl.Sizes, wantSizes) {
				t.Errorf("shaped pool: shape %+v sizes %+v", pl.Shape, pl.Sizes)
			}
		case "plain":
			if pl.Shape != nil || pl.Sizes != nil {
				t.Errorf("plain pool: shape %+v sizes %+v", pl.Shape, pl.Sizes)
			}
		}
	}
	// The latest pool event's shape.* changes: each key's old and new value.
	shapeChanges := func() map[string]struct{ New, Old any } {
		t.Helper()
		var raw []byte
		systemScan(t, s, `SELECT data->'changes' FROM pool_events e JOIN pools p ON p.id = e.pool_id WHERE p.name = 'shaped' ORDER BY e.id DESC LIMIT 1`, nil, &raw)
		var all map[string]struct{ New, Old any }
		if err := json.Unmarshal(raw, &all); err != nil {
			t.Fatalf("changes %s: %v", raw, err)
		}
		out := map[string]struct{ New, Old any }{}
		for k, v := range all {
			if strings.HasPrefix(k, "shape.") {
				out[k] = v
			}
		}
		return out
	}
	created := map[string]struct{ New, Old any }{
		"shape.family": {New: "m"}, "shape.arch": {New: "arm64"}, "shape.cpus": {New: 8.0}, "shape.memory": {New: float64(32 * gib)},
		"shape.disk": {New: float64(160 * gib)}, "shape.minCpus": {New: 0.5}, "shape.defaultCpus": {New: 2.0},
	}
	if got := shapeChanges(); !reflect.DeepEqual(got, created) {
		t.Errorf("created event's shape changes %v, want %v", got, created)
	}

	// A pool read back and sent as a marker carries the read-only sizes:
	// they are ignored, and the shape stays. A marker carrying the shape,
	// a writable field, is refused.
	readBack := map[string]any{"name": "shaped", "isDefault": true,
		"sizes": []map[string]any{{"name": "m-2-arm64", "cpus": 2, "memory": 8 * gib, "disk": 40 * gib, "fraction": 0.25, "default": true}}}
	if w := apiCall(t, s, key, http.MethodPost, "/v1/pools", readBack); w.Code != http.StatusOK {
		t.Fatalf("marker with the read-only sizes: %d %s", w.Code, w.Body)
	}
	const storedShape = `SELECT shape FROM pools WHERE name = 'shaped'`
	if stored := queryOne[*PoolShape](t, s, storedShape); stored == nil || *stored != testShape {
		t.Errorf("shape after a marker with sizes: %+v, want %+v", stored, testShape)
	}
	withShape := map[string]any{"name": "shaped", "isDefault": true, "shape": shape(func(m map[string]any) { m["defaultCpus"] = 4 })}
	w = apiCall(t, s, key, http.MethodPost, "/v1/pools", withShape)
	if code, msg := apiError(t, w.Body.Bytes()); w.Code != http.StatusUnprocessableEntity || code != "invalid_pool" || !strings.Contains(msg, "(got also shape)") {
		t.Errorf("marker with a shape: %d %s %s, want 422 invalid_pool with (got also shape)", w.Code, code, msg)
	}
	if stored := queryOne[*PoolShape](t, s, storedShape); stored == nil || *stored != testShape {
		t.Errorf("shape after a refused marker: %+v", stored)
	}

	// A set without the shape removes it.
	if w := apiCall(t, s, key, http.MethodPost, "/v1/pools", map[string]any{"name": "shaped", "provider": "static"}); w.Code != http.StatusOK {
		t.Fatalf("POST: %d %s", w.Code, w.Body)
	}
	if stored := queryOne[*PoolShape](t, s, storedShape); stored != nil {
		t.Errorf("shape after a set without it: %+v", *stored)
	}
	removed := map[string]struct{ New, Old any }{}
	for k, v := range created {
		removed[k] = struct{ New, Old any }{Old: v.New}
	}
	if got := shapeChanges(); !reflect.DeepEqual(got, removed) {
		t.Errorf("removal event's shape changes %v, want %v", got, removed)
	}
}

// A shape's diskHeadroom is optional: absent reads as 10 GiB and is stored
// absent; 0 up to less than disk is stored as given; negative, equal to or
// above disk is 422 invalid_pool and stores nothing.
func TestPoolShapeDiskHeadroom(t *testing.T) {
	s := testServer(t)
	execSQL(t, s, context.Background(), `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`)
	key := apiKey(t, s, new("t1"), "admin", "read")
	post := func(headroom any) *httptest.ResponseRecorder {
		sh := map[string]any{"family": "m", "arch": "arm64", "cpus": 8, "memory": "32Gi", "disk": "160Gi", "minCpus": 0.5, "defaultCpus": 2}
		if headroom != nil {
			sh["diskHeadroom"] = headroom
		}
		return apiCall(t, s, key, http.MethodPost, "/v1/pools", map[string]any{"name": "shaped", "provider": "static", "shape": sh})
	}
	for _, c := range []struct {
		name     string
		headroom any
		want     string
	}{
		{"negative", -1, "diskHeadroom -1 must be at least 0 and less than disk 171798691840"},
		{"the whole disk", "160Gi", "diskHeadroom 171798691840 must be at least 0 and less than disk 171798691840"},
		{"above the disk", "200Gi", "diskHeadroom 214748364800 must be at least 0 and less than disk 171798691840"},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := post(c.headroom)
			if code, msg := apiError(t, w.Body.Bytes()); w.Code != http.StatusUnprocessableEntity || code != "invalid_pool" ||
				msg != "invalid pool shape: "+c.want {
				t.Errorf("POST: %d %s, want 422 invalid_pool %q", w.Code, w.Body, c.want)
			}
			if n := queryOne[int](t, s, `SELECT count(*) FROM pools`); n != 0 {
				t.Fatalf("%d pools stored after a refusal", n)
			}
		})
	}
	const stored = `SELECT shape->'diskHeadroom' FROM pools WHERE name = 'shaped'`
	for _, c := range []struct {
		name     string
		headroom any
		want     *int64
		runner   string
	}{
		{"absent: the default", nil, nil, "10737418240"},
		{"zero", 0, i64(0), "0"},
		{"one byte under the disk", 160*gib - 1, i64(160*gib - 1), "171798691839"},
		{"20Gi", "20Gi", i64(20 * gib), "21474836480"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if w := post(c.headroom); w.Code != http.StatusOK {
				t.Fatalf("POST: %d %s", w.Code, w.Body)
			}
			got := queryOne[*int64](t, s, stored)
			if (got == nil) != (c.want == nil) || got != nil && *got != *c.want {
				t.Errorf("stored diskHeadroom %v, want %v", got, c.want)
			}
			sh := queryOne[*PoolShape](t, s, `SELECT shape FROM pools WHERE name = 'shaped'`)
			if env := sh.runnerEnv(); env["LUX_RUNNER_DISK_HEADROOM"] != c.runner {
				t.Errorf("runner env %v, want LUX_RUNNER_DISK_HEADROOM=%s", env, c.runner)
			}
		})
	}
	if (*PoolShape)(nil).runnerEnv() != nil {
		t.Error("an unshaped pool's runner env is not empty")
	}
}

// An ec2 pool's instance types the catalog knows must have the shape's
// arch and at least its CPUs and memory; a type it lacks is not checked.
func TestPoolShapeChecksInstanceTypes(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`)
	execSQL(t, s, ctx, `INSERT INTO instance_types (provider, region, instance_type, vcpus, memory_bytes, architectures, fetched_at) VALUES
		('ec2', 'eu-west-1', 'm8g.2xlarge', 8, $1, '{arm64}', now()),
		('ec2', 'eu-west-1', 'm8g.4xlarge', 16, $2, '{arm64}', now()),
		('ec2', 'eu-west-1', 'c8g.2xlarge', 8, $3, '{arm64}', now()),
		('ec2', 'eu-west-1', 'm7i.2xlarge', 8, $1, '{x86_64}', now()),
		('ec2', 'eu-west-1', 'multi.2xlarge', 8, $1, '{x86_64,arm64}', now())`, 32*gib, 64*gib, 16*gib)
	key := apiKey(t, s, new("t1"), "admin", "read")
	shape := map[string]any{"family": "m", "arch": "arm64", "cpus": 8, "memory": "32Gi", "disk": "160Gi", "minCpus": 0.5, "defaultCpus": 2}
	for _, c := range []struct {
		name      string
		primary   string
		fallbacks []string
		inText    string
	}{
		{"matching primary and a larger fallback", "m8g.2xlarge", []string{"m8g.4xlarge"}, ""},
		{"a type the catalog lacks", "m8g.2xlarge", []string{"x9.huge"}, ""},
		{"fallback with less memory", "m8g.2xlarge", []string{"c8g.2xlarge"}, "template.fallbackInstanceTypes[0] c8g.2xlarge: fallback capacities"},
		{"primary of another arch", "m7i.2xlarge", nil, `template.instanceType m7i.2xlarge: fallback architecture "amd64" differs from shape "arm64"`},
		{"type offering several archs, the shape's among them", "multi.2xlarge", nil, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			tmpl := map[string]any{"region": "eu-west-1", "instanceType": c.primary}
			if c.fallbacks != nil {
				tmpl["fallbackInstanceTypes"] = c.fallbacks
			}
			w := apiCall(t, s, key, http.MethodPost, "/v1/pools", map[string]any{"name": "burst", "provider": "ec2", "template": tmpl, "shape": shape})
			if c.inText == "" {
				if w.Code != http.StatusOK {
					t.Errorf("POST: %d %s", w.Code, w.Body)
				}
				return
			}
			if code, msg := apiError(t, w.Body.Bytes()); w.Code != http.StatusUnprocessableEntity || code != "invalid_pool" || !strings.Contains(msg, c.inText) {
				t.Errorf("POST: %d %s, want 422 with %q", w.Code, w.Body, c.inText)
			}
		})
	}
}

func TestParseShape(t *testing.T) {
	got, err := ParseShape("family=m,arch=arm64,cpus=8,memory=32Gi,disk=160Gi,min-cpus=0.5,default-cpus=2")
	if err != nil || *got != testShape {
		t.Fatalf("ParseShape: %+v %v, want %+v", got, err, testShape)
	}
	got, err = ParseShape("family=m,arch=arm64,cpus=8,memory=32Gi,disk=160Gi,min-cpus=0.5,default-cpus=2,disk-headroom=20Gi")
	if err != nil || got.DiskHeadroom == nil || *got.DiskHeadroom != spec.Bytes(20*gib) {
		t.Fatalf("ParseShape with disk-headroom: %+v %v, want 20 GiB", got, err)
	}
	for _, c := range []struct{ in, want string }{
		{"family=m,arch=arm64,cpus=8,memory=32Gi,disk=160Gi,min-cpus=0.5", "missing default-cpus"},
		{"family=m,arch=arm64,cpus=x,memory=32Gi,disk=160Gi,min-cpus=0.5,default-cpus=2", `cpus: invalid value "x"`},
		{"family=m,arch=arm64,cpus=8,memory=lots,disk=160Gi,min-cpus=0.5,default-cpus=2", `memory: invalid value "lots"`},
		{"family=m,arch=arm64,cpus=8,memory=32Gi,disk=160Gi,min-cpus=0.5,default-cpus=2,disk-headroom=x", `disk-headroom: invalid value "x"`},
		{"family=m,size=8", "want family|arch|cpus"},
	} {
		if _, err := ParseShape(c.in); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("ParseShape(%q): %v, want %q", c.in, err, c.want)
		}
	}
	var he *HTTPError
	if m, err := ValidShape(PoolShape{}); m != nil || !errors.As(err, &he) || he.Code != "invalid_pool" {
		t.Errorf("ValidShape of a zero shape: %v %v", m, err)
	}
	if m, err := ValidShape(testShape); err != nil || m.Default().Name != "m-2-arm64" {
		t.Errorf("ValidShape of testShape: %v", err)
	}
}

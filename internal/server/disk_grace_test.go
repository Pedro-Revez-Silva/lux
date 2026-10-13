package server

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/spec"
)

// submitDiskGrace POSTs a Run whose resources are res (raw JSON) as tenant
// t1 to a luxd whose default disk grace is def. Returns the response and
// the server.
func submitDiskGrace(t *testing.T, def time.Duration, res string) (*Server, int, map[string]any) {
	t.Helper()
	s := testServer(t)
	s.cfg.Defaults.DiskGrace = def
	execSQL(t, s, context.Background(), `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`)
	key := apiKey(t, s, new("t1"), "run", "read")
	var body map[string]any
	if err := json.Unmarshal([]byte(`{"image":{"ref":"alpine"},"workload":{"command":["true"]},"resources":`+res+`}`), &body); err != nil {
		t.Fatal(err)
	}
	w := apiCall(t, s, key, http.MethodPost, "/v1/runs", body)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return s, w.Code, out
}

// luxd fills its default disk grace into a Run's stored spec at submit, so
// the runner gets it in the assignment; an explicit value, 0 included,
// stands over a non-zero default. The stored value is kept when the
// default changes later.
func TestSubmitFillsTheDefaultDiskGrace(t *testing.T) {
	for _, c := range []struct {
		name string
		def  time.Duration
		res  string
		want string
	}{
		{"absent, default 5m", 5 * time.Minute, `{}`, "5m0s"},
		{"absent, builtin default", 0, `{}`, "0s"},
		{"explicit 0 over a 5m default", 5 * time.Minute, `{"diskGrace":"0s"}`, "0s"},
		{"explicit 0 as a number over a 5m default", 5 * time.Minute, `{"diskGrace":0}`, "0s"},
		{"explicit 90s over a 5m default", 5 * time.Minute, `{"diskGrace":"90s"}`, "1m30s"},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, code, out := submitDiskGrace(t, c.def, c.res)
			if code != http.StatusCreated && code != http.StatusAccepted {
				t.Fatalf("submit: %d %v", code, out)
			}
			id, _ := out["id"].(string)
			res, _ := out["spec"].(map[string]any)["resources"].(map[string]any)
			if res["diskGrace"] != c.want {
				t.Errorf("answer's resources %v, want diskGrace %q", res, c.want)
			}
			var stored string
			systemScan(t, s, `SELECT spec->'resources'->>'diskGrace' FROM runs WHERE id = $1`, []any{id}, &stored)
			if stored != c.want {
				t.Errorf("stored diskGrace %q, want %q", stored, c.want)
			}
			// A later change of the default does not reach a stored Run.
			s.cfg.Defaults.DiskGrace = 42 * time.Second
			if got := storedResources(t, s, id).DiskGrace; got.Duration.String() != c.want || !got.Set {
				t.Errorf("stored resources' diskGrace %+v, want set to %s", got, c.want)
			}
		})
	}
}

// The assignment the runner gets carries the Run's diskGrace in its spec.
func TestAssignCarriesDiskGrace(t *testing.T) {
	s := testServer(t)
	s.cfg.Defaults.DiskGrace = 5 * time.Minute
	s.cfg.LeaseDuration = time.Minute
	id := submitOnHA(t, s, resizeBase)
	execSQL(t, s, context.Background(), `UPDATE runs SET pool_id = 'default' WHERE id = $1`, id)
	s.hub.polled("ha")
	schedule(t, s)
	var a proto.Assign
	systemScan(t, s, `SELECT payload FROM host_messages WHERE type = $1 AND run_id = $2`, []any{proto.MsgAssign, id}, &a)
	if g := a.Spec.Resources.DiskGrace; !g.Set || g.Duration.Duration != 5*time.Minute {
		t.Fatalf("assigned diskGrace %+v, want 5m", g)
	}
}

// A diskGrace submit would refuse is 422 invalid_spec, naming it; a value
// that is no duration at all is refused while decoding the body, 400
// bad_request.
func TestSubmitRefusesABadDiskGrace(t *testing.T) {
	for _, c := range []struct {
		name, res string
		status    int
		code      string
		details   []any
		message   string
	}{
		{"negative", `{"diskGrace":"-1s"}`, http.StatusUnprocessableEntity, "invalid_spec", []any{"resources.diskGrace must not be negative"}, ""},
		{"over an hour", `{"diskGrace":"2h"}`, http.StatusUnprocessableEntity, "invalid_spec", []any{"resources.diskGrace must be at most 1h0m0s, got 2h0m0s"}, ""},
		{"not a duration", `{"diskGrace":"soon"}`, http.StatusBadRequest, "bad_request", nil, `invalid JSON body: invalid duration "soon"`},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, code, out := submitDiskGrace(t, 0, c.res)
			e, _ := out["error"].(map[string]any)
			details, _ := e["details"].([]any)
			if code != c.status || e["code"] != c.code || !reflect.DeepEqual(details, c.details) {
				t.Fatalf("%d %v, want %d %s with details %v", code, out, c.status, c.code, c.details)
			}
			if c.message != "" && e["message"] != c.message {
				t.Errorf("message %q, want %q", e["message"], c.message)
			}
		})
	}
}

// A resume's resources.diskGrace replaces the Run's, 0 included; absent
// leaves it; out of range is 422 and changes nothing. The new placement's
// grace starts from zero (the runner's disklimit.Watch is per placement).
func TestResumeDiskGrace(t *testing.T) {
	five := spec.SetDuration(5 * time.Minute)
	base := resizeBase
	base.DiskGrace = five
	for _, c := range []struct {
		name   string
		r      *resumeResources
		status int
		want   spec.OptionalDuration
	}{
		{"absent: kept", &resumeResources{CPUs: f64(1)}, 0, five},
		{"to 0", &resumeResources{DiskGrace: &spec.Duration{}}, 0, spec.SetDuration(0)},
		{"to 10m", &resumeResources{DiskGrace: &spec.Duration{Duration: 10 * time.Minute}}, 0, spec.SetDuration(10 * time.Minute)},
		{"negative", &resumeResources{DiskGrace: &spec.Duration{Duration: -time.Second}}, http.StatusUnprocessableEntity, five},
		{"over an hour", &resumeResources{DiskGrace: &spec.Duration{Duration: 2 * time.Hour}}, http.StatusUnprocessableEntity, five},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := testServer(t)
			id := stoppedRun(t, s, base, i64(gib))
			out, err := resumeWith(s, id, c.r)
			if c.status != 0 {
				refused(t, err, c.status, "invalid_spec", "resume")
			} else if err != nil {
				t.Fatal(err)
			} else if out.Body.Resize == nil || out.Body.Resize.Applied.DiskGrace != c.want {
				t.Errorf("answer's resize %+v, want applied diskGrace %+v", out.Body.Resize, c.want)
			}
			if got := storedResources(t, s, id).DiskGrace; got != c.want {
				t.Errorf("stored diskGrace %+v, want %+v", got, c.want)
			}
		})
	}
}

// A resume of a Run already resuming whose first resume asked for
// diskGrace 0s: the same 0s is a retry, 202 with the first's resize; any
// other diskGrace is 409. Neither changes the Run.
func TestResumeRetryComparesDiskGrace(t *testing.T) {
	base := resizeBase
	base.DiskGrace = spec.SetDuration(5 * time.Minute)
	first := &resumeResources{DiskGrace: &spec.Duration{}}
	for _, c := range []struct {
		name   string
		retry  *resumeResources
		status int // 0: 202
	}{
		{"the same 0s", &resumeResources{DiskGrace: &spec.Duration{}}, 0},
		{"the Run's earlier 5m", &resumeResources{DiskGrace: &spec.Duration{Duration: 5 * time.Minute}}, http.StatusConflict},
		{"1s", &resumeResources{DiskGrace: &spec.Duration{Duration: time.Second}}, http.StatusConflict},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := testServer(t)
			id := stoppedRun(t, s, base, i64(gib))
			out1, err := resumeWith(s, id, first)
			if err != nil {
				t.Fatal(err)
			}
			if out1.Body.Resize == nil {
				t.Fatal("first resume: no resize in the answer")
			}
			out2, err := resumeWith(s, id, c.retry)
			if c.status != 0 {
				refused(t, err, c.status, "not_resumable", "retry")
			} else if err != nil {
				t.Fatal(err)
			} else if got := out2.Body.Resize; got == nil || !reflect.DeepEqual(*got, *out1.Body.Resize) {
				t.Errorf("retry's resize %+v, want the first's %+v", got, out1.Body.Resize)
			}
			var events int
			systemScan(t, s, `SELECT count(*) FROM run_events WHERE run_id = $1 AND type = 'resume.requested'`, []any{id}, &events)
			if got := storedResources(t, s, id).DiskGrace; got != spec.SetDuration(0) || events != 1 {
				t.Errorf("after the retry: diskGrace %+v and %d resume.requested, want 0s and 1", got, events)
			}
		})
	}
}

// Through HTTP, as the CLI sends it: a string duration in the resume's
// resources.
func TestResumeDiskGraceHTTP(t *testing.T) {
	s := testServer(t)
	id := stoppedRun(t, s, resizeBase, i64(gib))
	key := apiKey(t, s, new("t1"), "run", "read")
	w := apiCall(t, s, key, http.MethodPost, "/v1/runs/"+id+"/resume", map[string]any{"resources": map[string]any{"diskGrace": "5m"}})
	if w.Code != http.StatusAccepted {
		t.Fatalf("POST: %d %s", w.Code, w.Body)
	}
	if got := storedResources(t, s, id).DiskGrace; got != spec.SetDuration(5*time.Minute) {
		t.Fatalf("stored %+v, want 5m", got)
	}
}

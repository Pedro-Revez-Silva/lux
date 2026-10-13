package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/spec"
)

// dryRun is resumeTo with validateOnly, and no secret values.
func dryRun(s *Server, id, poolID string, res *resumeResources) (*resumeOutput, error) {
	req := &resumeRequest{Resources: res, ValidateOnly: true}
	if poolID != "" {
		req.Placement = &resumeTarget{PoolID: poolID}
	}
	return s.resumeRun(tenantCtx("t1"), &resumeRunInput{RunPath: RunPath{ID: id}, Body: req})
}

// runSnapshot is everything a dry run must leave alone.
type runSnapshot struct {
	Binding runBinding
	Spec    string
	Events  int
	Held    string // the held secret values, as JSON; "" when none are held
}

func snapshotRun(t *testing.T, s *Server, id string) runSnapshot {
	t.Helper()
	var r runSnapshot
	r.Binding = binding(t, s, id)
	systemScan(t, s, `SELECT spec::text, (SELECT count(*) FROM run_events WHERE run_id = $1) FROM runs WHERE id = $1`, []any{id}, &r.Spec, &r.Events)
	if v, held := s.secrets.get(id); held {
		r.Held = jsonText(v)
	}
	return r
}

// A dry run on a live Run is 200 valid with what the resume would apply
// (the move, the resize), names the state it did not check, and changes
// nothing: pool_id, spec, state, secrets, events, the held secrets. Its
// own placement counts against the tenant's Run limit, so that limit is not
// checked.
func TestResumeValidateOnlyOnALiveRun(t *testing.T) {
	for _, state := range []string{StateScheduled, StateStarting, StateRunning, StateStopping} {
		t.Run(state, func(t *testing.T) {
			s := testServer(t)
			ctx := context.Background()
			id, big := movedRun(t, s)
			execSQL(t, s, ctx, `UPDATE runs SET state = $2 WHERE id = $1`, id, state)
			execSQL(t, s, ctx, `UPDATE tenants SET max_concurrent_runs = 1 WHERE id = 't1'`)
			s.secrets.put(id, map[string]string{"TOKEN": "live"})
			before := snapshotRun(t, s, id)
			if before.Held == "" {
				t.Fatal("fixture: no secrets held")
			}
			out, err := dryRun(s, id, big, &resumeResources{CPUs: f64(8), Memory: bytesPtr(32 * gib)})
			if err != nil {
				t.Fatal(err)
			}
			a := out.Body
			if out.Status != http.StatusOK || !a.Valid || a.StateUnchecked != state || a.Run != nil {
				t.Fatalf("answer %d %+v", out.Status, a)
			}
			if a.Placement == nil || a.Placement.PoolID != big || a.Placement.From == nil || a.Placement.From.ID != "default" || !a.Placement.PlatformUnchecked {
				t.Errorf("placement %+v", a.Placement)
			}
			if a.Resize == nil || a.Resize.Applied.CPUs != 8 || int64(a.Resize.Applied.Memory) != 32*gib {
				t.Errorf("resize %+v", a.Resize)
			}
			if after := snapshotRun(t, s, id); after != before {
				t.Errorf("after the dry run: %+v, want %+v", after, before)
			}
		})
	}
	// A stopped Run: valid, its state checked.
	s := testServer(t)
	id, big := movedRun(t, s)
	before := snapshotRun(t, s, id)
	out, err := dryRun(s, id, big, nil)
	if err != nil || !out.Body.Valid || out.Body.StateUnchecked != "" {
		t.Fatalf("stopped: %v %+v", err, out)
	}
	if after := snapshotRun(t, s, id); after != before {
		t.Errorf("after the dry run: %+v, want %+v", after, before)
	}
	// A real resume after it still works, and is the first resume.requested.
	if _, err := resumeTo(s, id, big, nil); err != nil {
		t.Fatal(err)
	}
	if b := binding(t, s, id); b.PoolID != big || b.State != StateResuming {
		t.Errorf("the real resume after the dry run: %+v", b)
	}
}

// A stopped Run at its tenant's Run limit (another Run running) is refused
// by the dry run with the status, code and message of the real resume.
func TestResumeValidateOnlyQuotaOnAStoppedRun(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	id, big := movedRun(t, s)
	other := submitOnHA(t, s, resizeBase)
	execSQL(t, s, ctx, `UPDATE runs SET state = 'running' WHERE id = $1`, other)
	execSQL(t, s, ctx, `UPDATE tenants SET max_concurrent_runs = 1 WHERE id = 't1'`)
	before := snapshotRun(t, s, id)
	_, dryErr := dryRun(s, id, big, nil)
	dry := refused(t, dryErr, http.StatusTooManyRequests, "quota_exceeded", "dry run")
	if after := snapshotRun(t, s, id); after != before {
		t.Errorf("after the refused dry run: %+v, want %+v", after, before)
	}
	_, realErr := resumeTo(s, id, big, nil)
	if real := refused(t, realErr, http.StatusTooManyRequests, "quota_exceeded", "resume"); real.Message != dry.Message {
		t.Errorf("dry run says %q, the resume %q", dry.Message, real.Message)
	}
}

// A dry run on a Run already resuming answers as a retry would: the pool
// its resume moved it to is valid with that resume's placement, the pool
// it left 409; neither writes an event.
func TestResumeValidateOnlyWhileResuming(t *testing.T) {
	s := testServer(t)
	id, big := movedRun(t, s)
	first, err := resumeTo(s, id, big, &resumeResources{CPUs: f64(8)})
	if err != nil {
		t.Fatal(err)
	}
	before := snapshotRun(t, s, id)
	out, err := dryRun(s, id, big, nil)
	if err != nil {
		t.Fatal(err)
	}
	a, fp := out.Body, first.Body.Placement
	if out.Status != http.StatusOK || !a.Valid || a.StateUnchecked != "" || a.Placement == nil ||
		a.Placement.PoolID != fp.PoolID || a.Placement.From == nil || *a.Placement.From != *fp.From || a.Placement.PlatformUnchecked != fp.PlatformUnchecked {
		t.Errorf("dry run on the same pool: %d %+v, want valid with the first's placement %+v", out.Status, a, fp)
	}
	_, err = dryRun(s, id, "default", nil)
	refused(t, err, http.StatusConflict, "not_resumable", "dry run into the pool it left")
	if after := snapshotRun(t, s, id); after != before {
		t.Errorf("after the dry runs: %+v, want %+v", after, before)
	}
	if events := resumeRequests(t, s, id); events != 1 {
		t.Errorf("%d resume.requested, want 1", events)
	}
}

// A dry run is refused as the real resume is, with the same status, code
// and message, and changes nothing; on a live Run too.
func TestResumeValidateOnlyRefusesAsTheResumeWould(t *testing.T) {
	for _, c := range []struct {
		name  string
		setup func(t *testing.T, s *Server, id string) string
		res   *resumeResources
		code  string
	}{
		{"unknown_pool", func(t *testing.T, s *Server, id string) string { return "pool_nope" }, nil, "unknown_pool"},
		{"platform_mismatch", func(t *testing.T, s *Server, id string) string {
			execSQL(t, s, context.Background(), `UPDATE placements SET image_platforms = '{linux/amd64}' WHERE run_id = $1`, id)
			execSQL(t, s, context.Background(), `UPDATE hosts SET arch = 'arm64' WHERE id = 'hb'`)
			return poolID(t, s, "big", "t1")
		}, nil, "platform_mismatch"},
		{"pool_unfit", func(t *testing.T, s *Server, id string) string { return poolID(t, s, "big", "t1") },
			&resumeResources{CPUs: f64(32)}, "pool_unfit"},
		{"invalid resources", func(t *testing.T, s *Server, id string) string { return "" },
			&resumeResources{CPUs: f64(0)}, "invalid_spec"},
	} {
		for _, live := range []bool{false, true} {
			name := c.name
			if live {
				name += ", running"
			}
			t.Run(name, func(t *testing.T) {
				s := testServer(t)
				id, _ := movedRun(t, s)
				target := c.setup(t, s, id)
				if live {
					execSQL(t, s, context.Background(), `UPDATE runs SET state = 'running' WHERE id = $1`, id)
				}
				before := snapshotRun(t, s, id)
				_, dryErr := dryRun(s, id, target, c.res)
				dry := refused(t, dryErr, http.StatusUnprocessableEntity, c.code, "dry run")
				if after := snapshotRun(t, s, id); after != before {
					t.Errorf("after the refused dry run: %+v, want %+v", after, before)
				}
				if live {
					return
				}
				_, realErr := resumeTo(s, id, target, c.res)
				real := refused(t, realErr, http.StatusUnprocessableEntity, c.code, "resume")
				if real.Message != dry.Message {
					t.Errorf("dry run says %q, the resume %q", dry.Message, real.Message)
				}
			})
		}
	}
}

// The checks that need no pool: a terminated Run or one with resumePolicy
// never is refused, an unknown sync repository and an invalid added
// repository too; a secret name to remove that the Run lacks is refused,
// but missing values are not. Nothing changes.
func TestResumeValidateOnlyOtherChecks(t *testing.T) {
	for _, c := range []struct {
		name   string
		setup  func(t *testing.T, s *Server, id string)
		req    resumeRequest
		status int
		code   string
	}{
		{"terminated", func(t *testing.T, s *Server, id string) {
			execSQL(t, s, context.Background(), `UPDATE runs SET state = 'terminated' WHERE id = $1`, id)
		}, resumeRequest{}, http.StatusConflict, "not_resumable"},
		{"resumePolicy never", func(t *testing.T, s *Server, id string) {
			setSpec(t, s, id, "{resumePolicy}", `"never"`)
		}, resumeRequest{}, http.StatusConflict, "not_resumable"},
		{"sync of an unknown repository", nil,
			resumeRequest{Sync: []proto.SyncRef{{Repo: "nope", Ref: "main"}}}, http.StatusUnprocessableEntity, "invalid_request"},
		{"removing a secret it lacks", nil,
			resumeRequest{RemoveSecrets: []string{"NOPE"}}, http.StatusUnprocessableEntity, "invalid_spec"},
		{"an added repository without a url", nil,
			resumeRequest{Git: &resumeGit{Repositories: []spec.Repository{{Name: "x"}}}}, http.StatusUnprocessableEntity, ""},
		{"no secret values", nil, resumeRequest{}, 0, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := testServer(t)
			id, _ := movedRun(t, s)
			if c.setup != nil {
				c.setup(t, s, id)
			}
			before := snapshotRun(t, s, id)
			req := c.req
			req.ValidateOnly = true
			out, err := s.resumeRun(tenantCtx("t1"), &resumeRunInput{RunPath: RunPath{ID: id}, Body: &req})
			if c.status == 0 {
				if err != nil || !out.Body.Valid {
					t.Fatalf("%v %+v, want valid", err, out)
				}
			} else {
				refused(t, err, c.status, c.code, "dry run")
			}
			if after := snapshotRun(t, s, id); after != before {
				t.Errorf("after the dry run: %+v, want %+v", after, before)
			}
		})
	}
}

// Through the HTTP API: 200 with valid and placement, no Run fields; GET
// shows the Run unmoved.
func TestResumeValidateOnlyHTTP(t *testing.T) {
	s := testServer(t)
	id, big := movedRun(t, s)
	key := apiKey(t, s, new("t1"), "run", "read")
	w := apiCall(t, s, key, http.MethodPost, "/v1/runs/"+id+"/resume", map[string]any{
		"validateOnly": true, "placement": map[string]any{"poolId": big}})
	if w.Code != http.StatusOK {
		t.Fatalf("POST: %d %s", w.Code, w.Body)
	}
	var answer map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &answer); err != nil {
		t.Fatal(err)
	}
	pl, _ := answer["placement"].(map[string]any)
	if answer["valid"] != true || pl["poolId"] != big || answer["id"] != nil || answer["state"] != nil {
		t.Errorf("answer %s", w.Body)
	}
	g := apiCall(t, s, key, http.MethodGet, "/v1/runs/"+id, nil)
	var run struct {
		State  string `json:"state"`
		PoolID string `json:"poolId"`
	}
	if json.Unmarshal(g.Body.Bytes(), &run) != nil || run.State != StateStopped || run.PoolID != "default" {
		t.Errorf("GET after the dry run: %s", g.Body)
	}
}

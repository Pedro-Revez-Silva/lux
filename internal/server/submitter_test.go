package server

import (
	"context"
	"net/http"
	"testing"

	"github.com/marcioapm/lux/internal/ids"
	"github.com/marcioapm/lux/internal/spec"
)

// submitterFixture: tenants t1 and t2, a run key for each, an operator key.
func submitterFixture(t *testing.T) (*Server, map[string]string) {
	t.Helper()
	s := testServer(t)
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1'), ('t2', 't2')`)
	keys := map[string]string{"t1": ids.Secret("luxk"), "t2": ids.Secret("luxk"), "op": ids.Secret("luxk")}
	execSQL(t, s, ctx, `INSERT INTO api_keys (id, tenant_id, name, key_hash, scopes) VALUES
		('k1', 't1', 'ci-bot', $1, ARRAY['run', 'read']), ('k2', 't2', 'other', $2, ARRAY['run', 'read']),
		('ko', NULL, 'ops', $3, ARRAY['operator'])`,
		ids.Hash(keys["t1"]), ids.Hash(keys["t2"]), ids.Hash(keys["op"]))
	return s, keys
}

var minimalRun = []byte(`{"image": {"ref": "alpine"}, "workload": {"adapter": "generic", "command": ["true"]}}`)

// A Run records the key it was submitted with, or the person signed in.
func TestRunSubmitterRecorded(t *testing.T) {
	s, keys := submitterFixture(t)
	code, out := postJSON(t, s, keys["t1"], "/v1/runs", minimalRun)
	if code != http.StatusCreated {
		t.Fatalf("submit: %d %v", code, out)
	}
	byKey := out["id"].(string)
	if k := queryOne[*string](t, s, `SELECT submitted_by_key FROM runs WHERE id = $1`, byKey); k == nil || *k != "k1" {
		t.Errorf("submitted_by_key: %v", k)
	}
	if e := queryOne[*string](t, s, `SELECT submitted_by_email FROM runs WHERE id = $1`, byKey); e != nil {
		t.Errorf("submitted_by_email for a key: %v", *e)
	}
	person := context.WithValue(context.Background(), principalKey, Principal{TenantID: "t1", Email: "ada@example.com", Scopes: []string{"admin"}})
	sub, err := s.submitRun(person, &submitRunInput{Body: spec.RunSpec{
		Image: spec.Image{Ref: "alpine"}, Workload: spec.Workload{Adapter: "generic", Command: []string{"true"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if e := queryOne[*string](t, s, `SELECT submitted_by_email FROM runs WHERE id = $1`, sub.Body.ID); e == nil || *e != "ada@example.com" {
		t.Errorf("submitted_by_email: %v", e)
	}
	if k := queryOne[*string](t, s, `SELECT submitted_by_key FROM runs WHERE id = $1`, sub.Body.ID); k != nil {
		t.Errorf("submitted_by_key for a person: %v", *k)
	}

	var run Run
	if code := getJSON(t, s, keys["t1"], "/v1/runs/"+byKey, &run); code != 200 || run.SubmittedBy == nil ||
		*run.SubmittedBy != (RunSubmitter{KeyID: "k1", KeyName: "ci-bot"}) {
		t.Errorf("key submitter: %d %+v", code, run.SubmittedBy)
	}
	run = Run{}
	if code := getJSON(t, s, keys["t1"], "/v1/runs/"+sub.Body.ID, &run); code != 200 || run.SubmittedBy == nil ||
		*run.SubmittedBy != (RunSubmitter{Email: "ada@example.com"}) {
		t.Errorf("person submitter: %d %+v", code, run.SubmittedBy)
	}
}

// An operator key is named to operators only; a revoked key says so; a Run
// from before tracking has no submitter.
func TestRunSubmitterNaming(t *testing.T) {
	s, keys := submitterFixture(t)
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, spec, state, submitted_by_key) VALUES
		('r_op', 't1', '{}', 'running', 'ko'), ('r_old', 't1', '{}', 'running', NULL), ('r_rev', 't1', '{}', 'running', 'k1')`)
	var run Run
	if code := getJSON(t, s, keys["t1"], "/v1/runs/r_op", &run); code != 200 || run.SubmittedBy == nil || *run.SubmittedBy != (RunSubmitter{KeyID: "ko"}) {
		t.Errorf("operator key to a tenant: %d %+v", code, run.SubmittedBy)
	}
	run = Run{}
	if code := getJSON(t, s, keys["op"], "/v1/runs/r_op", &run); code != 200 || run.SubmittedBy == nil || *run.SubmittedBy != (RunSubmitter{KeyID: "ko", KeyName: "ops"}) {
		t.Errorf("operator key to an operator: %d %+v", code, run.SubmittedBy)
	}
	run = Run{}
	if code := getJSON(t, s, keys["t1"], "/v1/runs/r_old", &run); code != 200 || run.SubmittedBy != nil {
		t.Errorf("before tracking: %d %+v", code, run.SubmittedBy)
	}
	// A second t1 key, revoked: its Run says so.
	execSQL(t, s, ctx, `INSERT INTO api_keys (id, tenant_id, name, key_hash, scopes, revoked_at) VALUES ('k1old', 't1', 'old-bot', 'h-old', ARRAY['run'], now())`)
	execSQL(t, s, ctx, `UPDATE runs SET submitted_by_key = 'k1old' WHERE id = 'r_rev'`)
	run = Run{}
	if code := getJSON(t, s, keys["t1"], "/v1/runs/r_rev", &run); code != 200 || run.SubmittedBy == nil ||
		*run.SubmittedBy != (RunSubmitter{KeyID: "k1old", KeyName: "old-bot", Revoked: true}) {
		t.Errorf("revoked key: %d %+v", code, run.SubmittedBy)
	}
	if code := getJSON(t, s, keys["t2"], "/v1/runs/r_rev", &run); code != http.StatusNotFound {
		t.Errorf("another tenant's Run: %d", code)
	}
	// A key id of another tenant on t1's own Run: t1 gets the id, never t2's key name.
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, spec, state, submitted_by_key) VALUES ('r_foreign', 't1', '{}', 'running', 'k2')`)
	execSQL(t, s, ctx, `UPDATE api_keys SET revoked_at = now() WHERE id = 'k2'`)
	run = Run{}
	if code := getJSON(t, s, keys["t1"], "/v1/runs/r_foreign", &run); code != 200 || run.SubmittedBy == nil || *run.SubmittedBy != (RunSubmitter{KeyID: "k2"}) {
		t.Errorf("foreign key id: %d %+v", code, run.SubmittedBy)
	}
	// An operator, also narrowed to t1, names every key.
	for _, path := range []string{"/v1/runs/r_foreign", "/v1/runs/r_foreign?tenant=t1"} {
		run = Run{}
		if code := getJSON(t, s, keys["op"], path, &run); code != 200 || run.SubmittedBy == nil || *run.SubmittedBy != (RunSubmitter{KeyID: "k2", KeyName: "other", Revoked: true}) {
			t.Errorf("operator %s: %d %+v", path, code, run.SubmittedBy)
		}
	}
	run = Run{}
	if code := getJSON(t, s, keys["op"], "/v1/runs/r_op?tenant=t1", &run); code != 200 || run.SubmittedBy == nil || *run.SubmittedBy != (RunSubmitter{KeyID: "ko", KeyName: "ops"}) {
		t.Errorf("narrowed operator, operator key: %d %+v", code, run.SubmittedBy)
	}
}

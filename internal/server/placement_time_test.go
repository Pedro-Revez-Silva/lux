package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/marcioapm/lux/internal/ids"
)

// The writers of placement time's inputs, through the API and the
// scheduler: a submit starts the Run's need for a host, an assignment hands
// it to the placement and clears it, a resume starts it again.
func TestPlacementTimeWriters(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	s.cfg.LeaseDuration = time.Minute
	namedPools(t, s, "pool")
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`)
	key := ids.Secret("luxk")
	execSQL(t, s, ctx, `INSERT INTO api_keys (id, tenant_id, name, key_hash, scopes) VALUES ('k1', 't1', 'k', $1, ARRAY['run', 'read'])`, ids.Hash(key))
	execSQL(t, s, ctx, `INSERT INTO hosts (id, name, pool_id, state, capacity, last_heartbeat)
		VALUES ('h', 'h', 'pool', 'ready', '{"cpus": 4, "memory": 16384}', now())`)
	s.hub.polled("h")

	w := apiCall(t, s, key, http.MethodPost, "/v1/runs", map[string]any{
		"image": map[string]any{"ref": "alpine"}, "workload": map[string]any{"command": []string{"sleep", "1"}},
		"placement": map[string]any{"pool": "pool"}, "resources": map[string]any{"cpus": 1, "memory": 1024}})
	if w.Code != http.StatusCreated {
		t.Fatalf("submit: %d %s", w.Code, w.Body)
	}
	var run Run
	_ = json.Unmarshal(w.Body.Bytes(), &run)
	// Submitted: needing a host since its creation.
	if !queryOne[bool](t, s, `SELECT needs_host_since = created_at FROM runs WHERE id = $1`, run.ID) {
		t.Fatal("a submitted Run does not need a host since its creation")
	}
	// Placed 3 seconds later (moved back so the gap is measurable).
	execSQL(t, s, ctx, `UPDATE runs SET created_at = created_at - interval '3 seconds', needs_host_since = needs_host_since - interval '3 seconds' WHERE id = $1`, run.ID)
	if err := s.scheduleOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if st := queryOne[string](t, s, `SELECT state FROM runs WHERE id = $1`, run.ID); st != StateScheduled {
		t.Fatalf("state %s, want scheduled", st)
	}
	if !queryOne[bool](t, s, `SELECT r.needs_host_since IS NULL AND p.needed_since = r.created_at FROM runs r JOIN placements p ON p.run_id = r.id AND p.epoch = 1 WHERE r.id = $1`, run.ID) {
		t.Fatal("assignment: the placement's needed_since is not the submit time, or the Run still needs a host")
	}
	if wait := queryOne[float64](t, s, `SELECT rpt.wait FROM `+runsFrom+` WHERE r.id = $1`, run.ID); wait < 2.9 || wait > 3.5 {
		t.Fatalf("placement wait %v, want ~3s", wait)
	}
	// Stopped, then resumed: needing a host again, from the resume.
	execSQL(t, s, ctx, `UPDATE placements SET state = 'exited', ended_at = now() - interval '1 minute' WHERE run_id = $1`, run.ID)
	execSQL(t, s, ctx, `UPDATE runs SET state = 'stopped', snapshot_id = NULL WHERE id = $1`, run.ID)
	if w := apiCall(t, s, key, http.MethodPost, "/v1/runs/"+run.ID+"/resume", map[string]any{}); w.Code != http.StatusOK && w.Code != http.StatusAccepted {
		t.Fatalf("resume: %d %s", w.Code, w.Body)
	}
	if !queryOne[bool](t, s, `SELECT state = 'resuming' AND now() - needs_host_since < interval '5 seconds' FROM runs WHERE id = $1`, run.ID) {
		t.Fatal("a resumed Run does not need a host since its resume")
	}
}

package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/spec"
)

// Through the HTTP API: POST /v1/runs?validateOnly=true binds the query
// and answers 200 with the dry run's keys and none of a Run's; a plain
// POST still flattens the embedded Run, pool and poolId included.
func TestSubmitValidateOnlyHTTP(t *testing.T) {
	s, _, _ := planningFixture(t, 0)
	at := time.Now()
	s.now = func() time.Time { return at }
	observePlanningHost(t, s, "h", "ready", proto.Capacity{CPUs: 4, Memory: 40, Disk: 40, Runs: 4}, map[string]string{})
	execSQL(t, s, context.Background(), `UPDATE hosts SET capabilities=$1 WHERE id='h'`, []string{proto.CapSpotReclaim})
	seedHistory(t, s, "h", ageEntry{at.Add(-30 * time.Second), freeVec{4, 40, 40, 4}})
	key := apiKey(t, s, new("t1"), "run", "read")
	sp := baseSpec("burst", spec.Resources{CPUs: 1, Memory: 10, Disk: 10})
	sp.Spot = true

	w := apiCall(t, s, key, http.MethodPost, "/v1/runs?validateOnly=true", sp)
	if w.Code != http.StatusOK {
		t.Fatalf("validateOnly: %d %s", w.Code, w.Body)
	}
	var answer map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &answer); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"id", "state", "pool", "poolId"} {
		if _, ok := answer[k]; ok {
			t.Errorf("validateOnly answer has a Run's %q: %s", k, w.Body)
		}
	}
	if answer["valid"] != true || answer["effectivePoolId"] != "pool1" || answer["effectivePool"] != "burst" {
		t.Errorf("validateOnly answer %s", w.Body)
	}
	blockers, _ := answer["blockers"].([]any)
	if len(blockers) != 1 {
		t.Fatalf("blockers %s", w.Body)
	}
	b, _ := blockers[0].(map[string]any)
	if b["code"] != blockNotSettled || b["remainingSeconds"] != float64(31) || b["hosts"] != float64(1) {
		t.Errorf("blocker %v, want space_not_settled on 1 host, remainingSeconds 31", b)
	}
	if n := queryOne[int](t, s, `SELECT count(*) FROM runs`); n != 0 {
		t.Fatal("validateOnly over HTTP created a Run")
	}

	w = apiCall(t, s, key, http.MethodPost, "/v1/runs", sp)
	if w.Code != http.StatusAccepted && w.Code != http.StatusCreated {
		t.Fatalf("POST: %d %s", w.Code, w.Body)
	}
	answer = nil
	if err := json.Unmarshal(w.Body.Bytes(), &answer); err != nil {
		t.Fatal(err)
	}
	if answer["id"] == nil || answer["pool"] != "burst" || answer["poolId"] != "pool1" {
		t.Errorf("POST answer %s: want id, pool burst, poolId pool1", w.Body)
	}
	for _, k := range []string{"valid", "effectivePoolId", "blockers"} {
		if _, ok := answer[k]; ok {
			t.Errorf("POST answer has validateOnly's %q: %s", k, w.Body)
		}
	}
}

func TestSubmitValidateOnlyReadOnlyAndSpotParity(t *testing.T) {
	s, _, _ := planningFixture(t, 0)
	ctx := context.Background()
	observePlanningHost(t, s, "h", "ready", proto.Capacity{CPUs: 4, Memory: 40, Disk: 40, Runs: 4}, map[string]string{})
	execSQL(t, s, ctx, `UPDATE hosts SET capabilities=$1 WHERE id='h'`, []string{proto.CapSpotReclaim})
	seedAged(t, s, "h", freeVec{4, 40, 40, 4}, 2*time.Minute)
	sp := baseSpec("burst", spec.Resources{CPUs: 1, Memory: 10, Disk: 10})
	sp.Spot = true
	before := queryOne[string](t, s, `SELECT history::text FROM host_capacity_age WHERE host_id='h'`)
	for range 2 {
		out, err := s.submitRun(tenantCtx("t1"), &submitRunInput{ValidateOnly: true, Body: sp})
		if err != nil {
			t.Fatal(err)
		}
		if !out.Body.Valid || !out.Body.WouldStartNow || out.Body.EligibleHost != "h" {
			t.Fatalf("answer=%+v", out.Body)
		}
	}
	if n := queryOne[int](t, s, `SELECT count(*) FROM runs`); n != 0 {
		t.Fatal("dry-run created run")
	}
	if n := queryOne[int](t, s, `SELECT count(*) FROM run_events`); n != 0 {
		t.Fatal("dry-run event")
	}
	if after := queryOne[string](t, s, `SELECT history::text FROM host_capacity_age WHERE host_id='h'`); after != before {
		t.Fatal("age mutation")
	}
	out, err := s.submitRun(tenantCtx("t1"), &submitRunInput{Body: sp})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.scheduleOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if state := queryOne[string](t, s, `SELECT state FROM runs WHERE id=$1`, out.Body.ID); state != StateScheduled {
		t.Fatalf("real admission=%s", state)
	}
}

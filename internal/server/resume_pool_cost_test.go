package server

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"testing"
)

// A Run that moved pools is priced per placement, each at its own host's
// rate, and its cost is attributed to the pool of each placement's host,
// not to the pool the Run is bound to now: the first half hour on blue's
// $0.40/h host is 0.05 in blue, the second on red's $0.80/h host 0.10 in
// red, though runs.pool_id is red.
func TestCostOfARunAcrossTwoPools(t *testing.T) {
	s, keys := costFixture(t)
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO hosts (id, tenant_id, name, pool_id, state, registered_at) VALUES
		('hblue', 't1', 'hblue', 'blue', 'ready', $1), ('hred', 't1', 'hred', 'red', 'ready', $1)`, at("10:00"))
	execSQL(t, s, ctx, `INSERT INTO host_rates (host_id, valid_from, per_hour, currency, cap_cpus, cap_memory, source) VALUES
		('hblue', $1, 0.40, 'USD', 8, $2, 'static'), ('hred', $1, 0.80, 'USD', 8, $2, 'static')`, at("10:00"), 32*gib)
	placeRun(t, s, "t1", "moved", StateStopping, "hblue", place("moved", 2, 8, "10:00", "10:30"))
	execSQL(t, s, ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state, resources, created_at, ended_at)
		VALUES ('p-moved-2', 't1', 'moved', 'hred', 2, 'exited', '{"cpus":2,"memory":8589934592}', $1, $2)`, at("10:30"), at("11:00"))
	execSQL(t, s, ctx, `UPDATE runs SET pool_id = 'red', current_epoch = 2 WHERE id = 'moved'`)
	finish(t, s, "t1", "moved", StateStopped)
	drain(t, s)

	_, c := getCost(t, s, keys["t1"], "moved")
	if len(c.Lines) != 1 || c.Lines[0].Amount != "0.15" {
		t.Fatalf("lines %+v, want one of 0.15", c.Lines)
	}
	var perHost []string
	for _, p := range c.Lines[0].Details["placements"].([]any) {
		d := p.(map[string]any)
		perHost = append(perHost, fmt.Sprintf("%v=%v@%v", d["hostId"], d["amount"], d["ratePerHour"]))
	}
	sort.Strings(perHost)
	if fmt.Sprint(perHost) != "[hblue=0.05@0.400000000 hred=0.1@0.800000000]" {
		t.Errorf("placements %v", perHost)
	}

	var got CostSummaryBody
	if code := getJSON(t, s, keys["t1"], costPath("&group=pool"), &got); code != http.StatusOK {
		t.Fatalf("summary: %d", code)
	}
	byPool := map[string]string{}
	for _, row := range got.Totals {
		byPool[row.Group["pool"]] = row.Amount
	}
	if len(byPool) != 2 || byPool["blue"] != "0.05" || byPool["red"] != "0.1" {
		t.Errorf("by pool %v, want blue 0.05 and red 0.1", byPool)
	}
}

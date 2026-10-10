package server

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/store"
)

// gp3Host adds t1's provider host "disk" (8 CPUs, 32 GiB, on-demand at
// $0.40/h) billed from 10:00, its compute period, and, when volumes is not
// "", its recorded volumes and a block-storage period at perHour.
func gp3Host(t *testing.T, s *Server, volumes, perHour string) {
	t.Helper()
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO hosts (id, tenant_id, name, state, provision_requested_at, registered_at, instance_type, market, zone, volumes)
		VALUES ('disk', 't1', 'disk', 'ready', $1, $1, 'm7i.2xlarge', 'on-demand', 'eu-north-1a', nullif($2, '')::jsonb)`, at("10:00"), volumes)
	execSQL(t, s, ctx, `INSERT INTO host_rates (host_id, valid_from, per_hour, currency, cap_cpus, cap_memory, source)
		VALUES ('disk', $1, 0.40, 'USD', 8, $2, 'aws-pricing')`, at("10:00"), 32*gib)
	if perHour != "" {
		addDiskRate(t, s, perHour)
	}
}

func addDiskRate(t *testing.T, s *Server, perHour string) {
	t.Helper()
	execSQL(t, s, context.Background(), `INSERT INTO host_rates (host_id, family, valid_from, per_hour, currency, cap_cpus, cap_memory, source)
		VALUES ('disk', 'block-storage', $1, $2::numeric, 'USD', 8, $3, 'ec2-ebs-pricing')`, at("10:00"), perHour, 32*gib)
}

const gp3Root = `[{"type":"gp3","sizeGiB":100,"iops":3000,"throughputMiBps":125}]`

// familyLines is a Run's lux-priced lines as "family item amount final".
func familyLines(t *testing.T, s *Server, key, runID string) (lines []string, source string) {
	t.Helper()
	code, c := getCost(t, s, key, runID)
	if code != http.StatusOK {
		t.Fatalf("GET cost of %s: %d", runID, code)
	}
	for _, l := range c.Lines {
		if l.Source == "compute" {
			lines = append(lines, fmt.Sprintf("%s %s %s %v", l.Family, l.Item, l.Amount, l.Final))
		}
	}
	for _, src := range c.Sources {
		if src.Source == "compute" {
			source = src.Status
		}
	}
	return lines, source
}

// The worked example on a host with a disk at $0.04/h: each placement gets a
// compute and a block-storage snapshot, priced by the same shares, as two
// lines of the compute source (block storage named by its volumes), each
// with its placements; byFamily shows Block storage apart from Compute.
func TestDrainBlockStorageLines(t *testing.T) {
	s, keys := costFixture(t)
	gp3Host(t, s, gp3Root, "0.04")
	for _, p := range workedExample {
		placeRun(t, s, "t1", p.RunID, StateStopping, "disk", p)
		finish(t, s, "t1", p.RunID, StateSucceeded)
	}
	drain(t, s)
	for run, want := range map[string]string{
		"A": "[block-storage gp3:100GiB 0.005 true compute m7i.2xlarge 0.05 true]",
		"B": "[block-storage gp3:100GiB 0.015 true compute m7i.2xlarge 0.15 true]",
		"C": "[block-storage gp3:100GiB 0.005 true compute m7i.2xlarge 0.05 true]",
	} {
		if lines, src := familyLines(t, s, keys["t1"], run); fmt.Sprint(lines) != want || src != "final" {
			t.Errorf("%s: %v, source %q; want %s, final", run, lines, src, want)
		}
	}
	var n int
	systemScan(t, s, `SELECT count(*) FROM cost_placement_snapshots WHERE finalized AND family IN ('compute', 'block-storage')`, nil, &n)
	if n != 6 {
		t.Errorf("%d finalized snapshots, want 2 per placement", n)
	}
	_, c := getCost(t, s, keys["t1"], "B")
	var families []string
	for _, f := range c.ByFamily {
		families = append(families, f.Family+"="+f.DisplayName+":"+f.Amount)
	}
	if fmt.Sprint(families) != "[block-storage=Block storage:0.015 compute=Compute:0.15]" {
		t.Errorf("byFamily %v", families)
	}
	for _, l := range c.Lines {
		ps := l.Details["placements"].([]any)
		if len(ps) != 1 || ps[0].(map[string]any)["hostId"] != "disk" || ps[0].(map[string]any)["share"] != 0.5 {
			t.Errorf("%s details %v", l.Family, l.Details)
		}
		if _, ok := ps[0].(map[string]any)["market"]; ok != (l.Family == "compute") {
			t.Errorf("%s: market only on compute: %v", l.Family, ps[0])
		}
	}
	var hours string
	systemScan(t, s, `SELECT string_agg(family || '=' || trim_scale(amount)::text, ' ' ORDER BY family) FROM cost_hourly WHERE run_id = 'B'`, nil, &hours)
	if hours != "block-storage=0.015 compute=0.15" {
		t.Errorf("B's hourly rows: %s", hours)
	}
}

// Volumes not known yet: no block-storage amount, never zero: the source is
// incomplete and retried, while compute is priced; once the volumes and
// their rate arrive, the retry prices it and the Run becomes final. An
// ended placement's snapshots then freeze: a later rate does not reprice
// either family.
func TestDrainBlockStorageMissingThenFinal(t *testing.T) {
	s, keys := costFixture(t)
	ctx := context.Background()
	gp3Host(t, s, "", "")
	placeRun(t, s, "t1", "A", StateRunning, "disk", place("A", 2, 8, "10:00", "11:00"))
	finish(t, s, "t1", "A", StateSucceeded)
	drain(t, s)
	if lines, src := familyLines(t, s, keys["t1"], "A"); fmt.Sprint(lines) != "[compute m7i.2xlarge 0.1 false]" || src != "incomplete" {
		t.Fatalf("unknown volumes: %v, source %q", lines, src)
	}
	var lastError string
	systemScan(t, s, `SELECT last_error FROM cost_sources WHERE run_id = 'A'`, nil, &lastError)
	if !strings.HasPrefix(lastError, "placement p-A on host disk has no block-storage rate") || strings.Contains(lastError, "compute") {
		t.Errorf("last_error %q", lastError)
	}
	var computeFrozen, diskAmount *string
	systemScan(t, s, `SELECT (SELECT amount::text FROM cost_placement_snapshots WHERE placement_id = 'p-A' AND family = 'compute' AND finalized),
		(SELECT amount::text FROM cost_placement_snapshots WHERE placement_id = 'p-A' AND family = 'block-storage')`, nil, &computeFrozen, &diskAmount)
	if computeFrozen == nil || diskAmount != nil {
		t.Errorf("compute frozen %v, block storage %v; want frozen compute, no block-storage amount", computeFrozen, diskAmount)
	}

	execSQL(t, s, ctx, `UPDATE hosts SET volumes = $1 WHERE id = 'disk'`, gp3Root)
	addDiskRate(t, s, "0.04")
	execSQL(t, s, ctx, `UPDATE cost_sources SET next_at = now() - interval '1 second' WHERE run_id = 'A'`)
	if err := s.pollDueCostSources(ctx); err != nil {
		t.Fatal(err)
	}
	drain(t, s)
	if lines, src := familyLines(t, s, keys["t1"], "A"); fmt.Sprint(lines) != "[block-storage gp3:100GiB 0.01 true compute m7i.2xlarge 0.1 true]" || src != "final" {
		t.Fatalf("priced: %v, source %q", lines, src)
	}

	execSQL(t, s, ctx, `UPDATE host_rates SET per_hour = 9 WHERE host_id = 'disk'`)
	execSQL(t, s, ctx, `SELECT lux_cost_enqueue('A', 'retry')`)
	drain(t, s)
	if lines, src := familyLines(t, s, keys["t1"], "A"); fmt.Sprint(lines) != "[block-storage gp3:100GiB 0.01 true compute m7i.2xlarge 0.1 true]" || src != "final" {
		t.Errorf("frozen snapshots repriced: %v, source %q", lines, src)
	}
}

// A static host has no block storage (it registered itself): its Runs'
// compute is final on its own.
func TestDrainBlockStorageStaticHostNone(t *testing.T) {
	s, keys := costFixture(t)
	costHosts(t, s)
	placeRun(t, s, "t1", "A", StateRunning, "static", place("A", 2, 8, "10:00", "10:30"))
	finish(t, s, "t1", "A", StateSucceeded)
	drain(t, s)
	if lines, src := familyLines(t, s, keys["t1"], "A"); fmt.Sprint(lines) != "[compute static 0.05 true]" || src != "final" {
		t.Errorf("static: %v, source %q", lines, src)
	}
}

// hostHourRows is a host's rows as "family allocated unallocated" per hour.
func hostHourRows(t *testing.T, s *Server, host string) string {
	t.Helper()
	var rows *string
	systemScan(t, s, `SELECT string_agg(to_char(hour AT TIME ZONE 'UTC', 'HH24') || ' ' || family || ' ' || trim_scale(allocated)::text || ' ' || trim_scale(unallocated)::text,
		', ' ORDER BY hour, family) FROM cost_hourly WHERE host_id = $1 AND run_id IS NULL`, []any{host}, &rows)
	if rows == nil {
		return ""
	}
	return *rows
}

// Host-hour rows per family: compute and block storage each split their own
// rate by the same shares, allocated + unallocated = that family's cost of
// the hour. A block-storage period opened later rewinds the host's cursor,
// so hours already refreshed are rebuilt with it.
func TestHostHoursPerFamily(t *testing.T) {
	s, _ := costFixture(t)
	ctx := context.Background()
	s.cfg.Costs.Batch = 10
	s.cfg.Costs.Hourly = 48 * time.Hour
	hour := time.Now().UTC().Truncate(time.Hour).Add(-3 * time.Hour)
	execSQL(t, s, ctx, `INSERT INTO hosts (id, tenant_id, name, pool_id, state, provision_requested_at, registered_at, terminated_at, capacity, volumes)
		VALUES ('hh', 't1', 'hh', 'p', 'terminated', $1, $1, $2, '{"cpus":8,"memory":34359738368}', $3)`, hour, hour.Add(2*time.Hour), gp3Root)
	execSQL(t, s, ctx, `INSERT INTO host_rates (host_id, valid_from, valid_to, per_hour, currency, cap_cpus, cap_memory, source)
		VALUES ('hh', $1, $2, 0.40, 'USD', 8, $3, 'aws-pricing')`, hour, hour.Add(2*time.Hour), 32*gib)
	// A: 2 of 8 CPUs over the first hour.
	execSQL(t, s, ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state, resources, created_at, ended_at)
		VALUES ('p-hh', 't1', 'r1', 'hh', 1, 'exited', '{"cpus":2,"memory":8589934592}', $1, $2)`, hour, hour.Add(time.Hour))
	for range 3 {
		if err := s.updateHostHours(ctx); err != nil {
			t.Fatal(err)
		}
	}
	h := func(n int) string { return hour.Add(time.Duration(n) * time.Hour).Format("15") }
	if got, want := hostHourRows(t, s, "hh"), h(0)+" compute 0.1 0.3, "+h(1)+" compute 0 0.4"; got != want {
		t.Fatalf("compute only: %s, want %s", got, want)
	}

	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		opened, err := openBlockStorageRate(ctx, tx, "hh", map[string]BlockStoragePrice{"gp3": {Currency: "USD", PerGBMonth: "0.292"}}, "ec2-ebs-pricing", s.cfg.Costs.Hourly)
		if err == nil && !opened {
			t.Error("no period opened")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := s.updateHostHours(ctx); err != nil {
			t.Fatal(err)
		}
	}
	// 100 GiB × 0.292 / 730 = 0.04 an hour: A a quarter of it in the first hour.
	want := h(0) + " block-storage 0.01 0.03, " + h(0) + " compute 0.1 0.3, " + h(1) + " block-storage 0 0.04, " + h(1) + " compute 0 0.4"
	if got := hostHourRows(t, s, "hh"); got != want {
		t.Errorf("per family: %s, want %s", got, want)
	}
}

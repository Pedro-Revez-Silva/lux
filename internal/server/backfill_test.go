package server

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/store"
)

// backfillFixture: t1's ec2 pool "burst" (eu-north-1) with host "disk"
// (launched 10:00, terminated 11:00) and a finished Run A on it whose
// compute is final, as it was before luxd recorded volumes: its host's
// volumes are unknown and it has no block-storage snapshot.
func backfillFixture(t *testing.T) (*Server, map[string]string, *fakePriceProvider) {
	t.Helper()
	s, keys := costFixture(t)
	ctx := context.Background()
	p := &fakePriceProvider{blockStorage: map[string]BlockStoragePrice{"gp3": gp3eun1Answer}}
	s.cfg.Costs.Prices = map[string]PriceProvider{"ec2": p}
	s.cfg.Costs.PricesRefresh = DefaultPricesRefresh
	s.cfg.Costs.Hourly = DefaultPricesRefresh * 400
	s.cfg.Costs.Every = DefaultCostsEvery
	s.cfg.Costs.Batch = DefaultCostsBatch
	execSQL(t, s, ctx, `INSERT INTO pools (id, tenant_id, name, provider, template) VALUES ('pb', 't1', 'burst', 'ec2', '{"region":"eu-north-1"}')`)
	gp3Host(t, s, "[]", "")
	execSQL(t, s, ctx, `UPDATE hosts SET pool_id = 'pb', provider_id = 'i-disk', launch_template = '{"region":"eu-north-1"}',
		capacity = '{"cpus":8,"memory":34359738368}', state = 'terminated', terminated_at = $1 WHERE id = 'disk'`, at("11:00"))
	execSQL(t, s, ctx, `UPDATE host_rates SET source = 'ec2-pricing' WHERE host_id = 'disk'`)
	placeRun(t, s, "t1", "A", StateRunning, "disk", place("A", 2, 8, "10:00", "11:00"))
	finish(t, s, "t1", "A", StateSucceeded)
	drain(t, s)
	execSQL(t, s, ctx, `UPDATE hosts SET volumes = NULL WHERE id = 'disk'`)
	if lines, src := familyLines(t, s, keys["t1"], "A"); fmt.Sprint(lines) != "[compute m7i.2xlarge 0.1 true]" || src != "final" {
		t.Fatalf("before: %v %s", lines, src)
	}
	return s, keys, p
}

// snapshotRow is A's compute snapshot as stored, every column.
func snapshotRow(t *testing.T, s *Server) string {
	t.Helper()
	var row string
	systemScan(t, s, `SELECT row(s.*)::text FROM cost_placement_snapshots s WHERE placement_id = 'p-A' AND family = 'compute'`, nil, &row)
	return row
}

// The backfill: a dry run reports the host, its hours and Runs and changes
// nothing; the real one records the assumed disk, opens its block-storage
// period at the list price, and the finished Run gains a final
// block-storage line while its compute snapshot and amount are byte for
// byte as they were; a second run finds nothing to do.
func TestBackfillVolumes(t *testing.T) {
	s, keys, _ := backfillFixture(t)
	ctx := context.Background()
	// A launch that never registered: no capacity; recorded, not priced.
	execSQL(t, s, ctx, `INSERT INTO hosts (id, tenant_id, name, pool_id, state, provider_id, provision_requested_at, terminated_at, launch_template)
		VALUES ('ghost', 't1', 'ghost', 'pb', 'terminated', 'i-ghost', $1, $2, '{"region":"eu-north-1"}')`, at("10:00"), at("10:05"))
	before := snapshotRow(t, s)
	_, c := getCost(t, s, keys["t1"], "A")
	computeBefore := c.Lines[0].Amount
	root, err := ParseVolume("type=gp3,size=100,iops=3000,throughput=125")
	if err != nil {
		t.Fatal(err)
	}
	req := BackfillVolumes{Pool: "burst", Tenant: "t1", Volumes: []HostVolume{root}, DryRun: true}
	rep, err := s.BackfillVolumes(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Hosts) != 2 || rep.Hosts[0].ID != "disk" || rep.Hosts[1].ID != "ghost" || rep.Runs != 1 || fmt.Sprint(rep.Hosts[0].Runs) != "[A]" ||
		rep.Hosts[0].PerHour != "0.011452055" || rep.Hours != 2 {
		t.Errorf("dry run: %+v", rep)
	}
	var volumes *string
	var rates, queued int
	systemScan(t, s, `SELECT (SELECT volumes::text FROM hosts WHERE id = 'disk'),
		(SELECT count(*) FROM host_rates WHERE family = 'block-storage'), (SELECT count(*) FROM cost_pending)`, nil, &volumes, &rates, &queued)
	if volumes != nil || rates != 0 || queued != 0 {
		t.Fatalf("dry run changed something: volumes %v, rates %d, queued %d", volumes, rates, queued)
	}

	req.DryRun = false
	// A Run on the never-registered host: no period opens there, so the
	// backfill re-queues nothing on it and does not report it.
	placeRun(t, s, "t1", "G", StateStopped, "ghost", place("G", 1, 1, "10:00", "10:05"))
	if rep, err = s.BackfillVolumes(ctx, req); err != nil || len(rep.Hosts) != 2 || rep.Runs != 1 ||
		fmt.Sprint(rep.Hosts[0].Runs) != "[A]" || len(rep.Hosts[1].Runs) != 0 {
		t.Fatalf("apply: %+v %v", rep, err)
	}
	if p := pending(t, s, "G"); p != "" {
		t.Errorf("G queued: %q", p)
	}
	var ghostRates int
	var ghostVolumes *string
	systemScan(t, s, `SELECT (SELECT volumes::text FROM hosts WHERE id = 'ghost'), (SELECT count(*) FROM host_rates WHERE host_id = 'ghost')`, nil, &ghostVolumes, &ghostRates)
	if ghostVolumes == nil || ghostRates != 0 {
		t.Errorf("unregistered host: volumes %v, %d rates", ghostVolumes, ghostRates)
	}
	systemScan(t, s, `SELECT volumes::text FROM hosts WHERE id = 'disk'`, nil, &volumes)
	if volumes == nil || *volumes != `[{"iops": 3000, "type": "gp3", "assumed": true, "sizeGiB": 100, "throughputMiBps": 125}]` {
		t.Errorf("volumes %v", volumes)
	}
	var details string
	systemScan(t, s, `SELECT details::text FROM host_rates WHERE host_id = 'disk' AND family = 'block-storage' AND valid_from = $1 AND valid_to = $2`,
		[]any{at("10:00"), at("11:00")}, &details)
	var d struct {
		Volumes []HostVolume `json:"volumes"`
	}
	if err := json.Unmarshal([]byte(details), &d); err != nil || len(d.Volumes) != 1 || d.Volumes[0] != root {
		t.Errorf("rate details %s: volumes %+v, want [%+v] (%v)", details, d.Volumes, root, err)
	}
	if _, src := familyLines(t, s, keys["t1"], "A"); src != "ok" {
		t.Errorf("queued Run's source %q, want ok until evaluated", src)
	}
	drain(t, s)
	// A holds 2 of 8 CPUs for the disk's hour: 0.011452055 / 4.
	lines, src := familyLines(t, s, keys["t1"], "A")
	if fmt.Sprint(lines) != "[block-storage gp3:100GiB 0.002863014 true compute m7i.2xlarge 0.1 true]" || src != "final" {
		t.Errorf("after: %v %s", lines, src)
	}
	if after := snapshotRow(t, s); after != before {
		t.Errorf("compute snapshot changed:\n%s\n%s", before, after)
	}
	_, c = getCost(t, s, keys["t1"], "A")
	for _, l := range c.Lines {
		if l.Family == "compute" && l.Amount != computeBefore {
			t.Errorf("compute amount %s, was %s", l.Amount, computeBefore)
		}
	}

	again, err := s.BackfillVolumes(ctx, req)
	if err != nil || len(again.Hosts) != 0 || again.Runs != 0 {
		t.Errorf("second run: %+v %v", again, err)
	}
	if pending(t, s, "A") != "" {
		t.Errorf("second run queued A")
	}
}

// A host still live when luxd first records its volumes (one launched
// before the upgrade): the price loop opens its block-storage period and
// re-queues the Runs already final on it, so a finished Run gains a final
// block-storage line while its compute snapshot row stays byte for byte.
func TestBlockStorageLiveHostRequeuesFinalRuns(t *testing.T) {
	s, keys, _ := backfillFixture(t)
	ctx := context.Background()
	execSQL(t, s, ctx, `UPDATE hosts SET state = 'ready', terminated_at = NULL WHERE id = 'disk'`)
	before := snapshotRow(t, s)
	if err := s.storeVolumes(ctx, "disk", []HostVolume{{Type: "gp3", SizeGiB: 100, IOPS: 3000, ThroughputMiBps: 125}}); err != nil {
		t.Fatal(err)
	}
	s.refreshBlockStorage(ctx)
	if p := pending(t, s, "A"); p == "" {
		t.Fatalf("A was not queued")
	}
	drain(t, s)
	lines, src := familyLines(t, s, keys["t1"], "A")
	if fmt.Sprint(lines) != "[block-storage gp3:100GiB 0.002863014 true compute m7i.2xlarge 0.1 true]" || src != "final" {
		t.Errorf("after: %v %s", lines, src)
	}
	if after := snapshotRow(t, s); after != before {
		t.Errorf("compute snapshot changed:\n%s\n%s", before, after)
	}
}

// The price loop opens a block-storage period under the host's cost-host
// lock: a host-hour refresh that holds it and then writes the cursor
// cannot overwrite the period's rewind.
func TestBlockStorageRewindWaitsForCostHostLock(t *testing.T) {
	s, _, _ := backfillFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	execSQL(t, s, ctx, `UPDATE hosts SET state = 'ready', terminated_at = NULL WHERE id = 'disk'`)
	if err := s.storeVolumes(ctx, "disk", []HostVolume{{Type: "gp3", SizeGiB: 100, IOPS: 3000, ThroughputMiBps: 125}}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Hour)
	execSQL(t, s, ctx, `INSERT INTO cost_host_refresh (host_id, next_hour) VALUES ('disk', $1)`, now.Add(-2*time.Hour))
	locked, release, held := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		held <- s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
			if err := lockCostHost(ctx, tx, "disk"); err != nil {
				return err
			}
			close(locked)
			<-release
			_, err := tx.Exec(ctx, `UPDATE cost_host_refresh SET next_hour = $1 WHERE host_id = 'disk'`, now)
			return err
		})
	}()
	<-locked
	done := make(chan struct{})
	go func() { s.refreshBlockStorage(ctx); close(done) }()
	if err := waitLocked(s, 1, done); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-held; err != nil {
		t.Fatal(err)
	}
	<-done
	var next time.Time
	systemScan(t, s, `SELECT next_hour FROM cost_host_refresh WHERE host_id = 'disk'`, nil, &next)
	if !next.Before(now) {
		t.Errorf("cursor %v: the rewind was overwritten", next)
	}
}

// A Run that went final before compute snapshots existed (migration 027)
// has none to keep frozen: re-evaluating it would price its compute anew.
// The backfill leaves it as it is and reports it as skipped, while still
// recording the host's disk and period.
func TestBackfillVolumesSkipsRunsWithoutComputeSnapshots(t *testing.T) {
	s, keys, _ := backfillFixture(t)
	ctx := context.Background()
	execSQL(t, s, ctx, `DELETE FROM cost_placement_snapshots WHERE placement_id = 'p-A'`)
	execSQL(t, s, ctx, `UPDATE host_rates SET per_hour = 0.80 WHERE host_id = 'disk' AND family = 'compute'`)
	root, err := ParseVolume("type=gp3,size=100")
	if err != nil {
		t.Fatal(err)
	}
	req := BackfillVolumes{Pool: "burst", Tenant: "t1", Volumes: []HostVolume{root}, DryRun: true}
	for _, dry := range []bool{true, false} {
		req.DryRun = dry
		rep, err := s.BackfillVolumes(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		want := []BackfillSkipped{{Run: "A", Host: "disk", Reason: skipLegacyCompute}}
		if rep.Runs != 0 || len(rep.Hosts) != 1 || len(rep.Hosts[0].Runs) != 0 || !slices.Equal(rep.Skipped, want) {
			t.Errorf("dry run %v: %+v", dry, rep)
		}
	}
	if p := pending(t, s, "A"); p != "" {
		t.Errorf("A queued: %q", p)
	}
	drain(t, s)
	if lines, src := familyLines(t, s, keys["t1"], "A"); fmt.Sprint(lines) != "[compute m7i.2xlarge 0.1 true]" || src != "final" {
		t.Errorf("after: %v %s", lines, src)
	}
	var rates int
	systemScan(t, s, `SELECT count(*) FROM host_rates WHERE host_id = 'disk' AND family = 'block-storage'`, nil, &rates)
	if rates != 1 {
		t.Errorf("%d block-storage periods", rates)
	}
}

// ParseVolume takes the operator's flag; an unknown type, key or a missing
// size is refused.
func TestParseVolume(t *testing.T) {
	v, err := ParseVolume("type=gp3,size=100,iops=3000,throughput=125")
	if err != nil || v != (HostVolume{Type: "gp3", SizeGiB: 100, IOPS: 3000, ThroughputMiBps: 125, Assumed: true}) {
		t.Errorf("%+v %v", v, err)
	}
	for _, bad := range []string{"type=floppy,size=1", "type=gp3", "type=gp3,size=1,color=red", "type=gp3,size=-1", "gp3"} {
		if _, err := ParseVolume(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

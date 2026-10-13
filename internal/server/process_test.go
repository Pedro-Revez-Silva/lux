package server

import (
	"context"
	"math"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/ids"
	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/store"
)

// The system tick records luxd's own process on its control sample, and a
// heartbeat its runner's on the host sample; a heartbeat from a runner that
// predates the report leaves the runner's columns empty, and any goroutine
// count Go's int holds fits its column.
func TestSampleProcesses(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	s.cfg.DiskPaths = []string{}
	if err := s.sampleSystem(ctx); err != nil {
		t.Fatal(err)
	}
	execSQL(t, s, ctx, `INSERT INTO hosts (id, name, state) VALUES ('h1', 'h1', 'ready'), ('h2', 'h2', 'ready'), ('h3', 'h3', 'ready')`)
	started := time.Now().Add(-time.Hour).UTC().Truncate(time.Microsecond)
	peak := int64(40 << 20)
	runner := &proto.ProcessUsage{Started: started, CPUSeconds: 12.5, RSSBytes: 30 << 20, PeakRSSBytes: &peak, HeapBytes: 8 << 20, Goroutines: 42}
	huge := &proto.ProcessUsage{Started: started, Goroutines: math.MaxInt32 + 10}
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		if err := sampleHost(ctx, tx, "h1", nil, runner); err != nil {
			return err
		}
		if err := sampleHost(ctx, tx, "h3", nil, huge); err != nil {
			return err
		}
		return sampleHost(ctx, tx, "h2", nil, nil)
	})
	if err != nil {
		t.Fatal(err)
	}
	var luxd, h1, h2, h3 procRow
	err = s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT `+procCols+` FROM control_samples`).Scan(luxd.dest()...); err != nil {
			return err
		}
		for id, r := range map[string]*procRow{"h1": &h1, "h2": &h2, "h3": &h3} {
			if err := tx.QueryRow(ctx, `SELECT `+procCols+` FROM host_samples WHERE host_id = $1`, id).Scan(r.dest()...); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if luxd.started == nil || luxd.cpu == nil || *luxd.rss <= 0 || luxd.peak != nil && *luxd.peak < *luxd.rss || *luxd.heap <= 0 || *luxd.goroutines < 2 {
		t.Fatalf("luxd: %+v", luxd)
	}
	if h1.started == nil || !h1.started.Equal(started) || *h1.cpu != 12.5 || *h1.rss != 30<<20 || *h1.peak != 40<<20 || *h1.heap != 8<<20 || *h1.goroutines != 42 {
		t.Fatalf("h1: %+v", h1)
	}
	if h2 != (procRow{}) {
		t.Fatalf("h2 (an older runner): %+v", h2)
	}
	if h3.goroutines == nil || *h3.goroutines != math.MaxInt32+10 || h3.peak != nil {
		t.Fatalf("h3: %+v", h3)
	}
}

// A restart starts the process's CPU counter again. History takes a CPU
// rate only between samples of one process, so the restart shows as a
// point without one rather than as a negative or huge rate; a rollup
// bucket keeps its last reading's counter and start, not the maximum
// counter, which may be the previous process's.
func TestProcessRestartHistory(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	opKey := ids.Secret("luxk")
	execSQL(t, s, ctx, `INSERT INTO api_keys (id, tenant_id, name, key_hash, scopes) VALUES ('ko', NULL, 'o', $1, ARRAY['operator'])`, ids.Hash(opKey))
	execSQL(t, s, ctx, `INSERT INTO hosts (id, name, state) VALUES ('h1', 'h1', 'ready')`)
	hour := time.Now().UTC().Truncate(time.Hour).Add(-3 * time.Hour)
	first, second := hour.Add(-24*time.Hour), hour.Add(70*time.Second)
	// Minute 0: the first process; minute 1: it, then (from +70s) the
	// second; minute 2: the second.
	points := []struct {
		off     time.Duration
		started time.Time
		cpu     float64
		peak    int64
	}{
		{5 * time.Second, first, 500, 10},
		{35 * time.Second, first, 530, 30},
		{65 * time.Second, first, 560, 20},
		{95 * time.Second, second, 3, 90},
		{125 * time.Second, second, 9, 15},
		{155 * time.Second, second, 21, 15},
	}
	for _, p := range points {
		at := hour.Add(p.off)
		execSQL(t, s, ctx, `INSERT INTO system_samples (tenant_id, res, at) VALUES ('', 0, $1)`, at)
		// A luxd restart is a new process id; a runner's keeps its host's.
		instance := map[bool]string{true: "luxd_1", false: "luxd_2"}[p.started.Equal(first)]
		execSQL(t, s, ctx, `INSERT INTO control_samples (instance, hostname, res, at, `+procCols+`) VALUES ($1, 'ctl', 0, $2, $3, $4, 100, $5, 50, 7)`,
			instance, at, p.started, p.cpu, p.peak)
		execSQL(t, s, ctx, `INSERT INTO host_samples (host_id, res, at, `+procCols+`) VALUES ('h1', 0, $1, $2, $3, 100, $4, 50, 7)`,
			at, p.started, p.cpu, p.peak)
	}
	if err := s.rollupHistory(ctx); err != nil {
		t.Fatal(err)
	}
	type want struct {
		started time.Time
		cores   *float64
		peak    int64
	}
	f := func(v float64) *float64 { return &v }
	// Raw: rates within each process; none across the restart.
	raw := []want{{first, nil, 10}, {first, f(1), 30}, {first, f(1), 20}, {second, nil, 90}, {second, f(0.2), 15}, {second, f(0.4), 15}}
	for _, c := range []struct {
		res          string
		runner, luxd []want
	}{
		{"0", raw, raw},
		// Minutes. The runner keeps its host's id across its restart:
		// minute 1 ends in the second process (its last reading, 3 at
		// +95s, not the first's 560); minute 2's rate is against it. A
		// luxd restart is a new id, so each process rolls up its own
		// minute 1. Peaks are the bucket's maximum.
		{"60",
			[]want{{first, nil, 30}, {second, nil, 90}, {second, f((21 - 3) / 60.0), 15}},
			[]want{{first, nil, 30}, {first, f((560 - 530) / 60.0), 20}, {second, nil, 90}, {second, f((21 - 3) / 60.0), 15}}},
	} {
		q := url.Values{"res": {c.res}, "from": {hour.Add(-time.Second).Format(time.RFC3339Nano)}, "to": {hour.Add(time.Hour).Format(time.RFC3339Nano)}}.Encode()
		var runner, luxd []*ProcessSample
		for _, sm := range historyRequest(t, s, opKey, "/v1/hosts/h1/history?"+q).Samples {
			runner = append(runner, sm.Runner)
		}
		ctl := historyRequest(t, s, opKey, "/v1/history?"+q).Control
		if ctl == nil || len(ctl.Luxd) != 2 || ctl.Luxd[0].Instance != "luxd_1" || ctl.Luxd[1].Instance != "luxd_2" {
			t.Fatalf("res %s: control %+v", c.res, ctl)
		}
		for _, l := range ctl.Luxd {
			for _, ls := range l.Samples {
				luxd = append(luxd, &ls.ProcessSample)
			}
		}
		for name, got := range map[string][]*ProcessSample{"runner": runner, "luxd": luxd} {
			want := map[string][]want{"runner": c.runner, "luxd": c.luxd}[name]
			if len(got) != len(want) {
				t.Fatalf("%s res %s: %d samples, want %d", name, c.res, len(got), len(want))
			}
			for i, w := range want {
				p := got[i]
				if p == nil || !p.Started.Equal(w.started) || *p.PeakRSSBytes != w.peak || *p.RSSBytes != 100 || *p.HeapBytes != 50 || *p.Goroutines != 7 {
					t.Fatalf("%s res %s sample %d: %+v, want %+v", name, c.res, i, p, w)
				}
				if (w.cores == nil) != (p.CPUCores == nil) || w.cores != nil && math.Abs(*w.cores-*p.CPUCores) > 1e-9 {
					t.Errorf("%s res %s sample %d: cpuCores %v, want %v", name, c.res, i, deref(p.CPUCores), deref(w.cores))
				}
			}
		}
	}
}

// A sample without a process reading (an older runner's heartbeat) has no
// process in history, and the next reading's rate spans the gap.
func TestProcessRateSkipsMissing(t *testing.T) {
	var r procRate
	start := time.Unix(1000, 0)
	at := func(s int) time.Time { return time.Unix(int64(2000+s), 0) }
	row := func(v float64) procRow { return procRow{started: &start, cpu: &v} }
	first := row(10)
	if p := first.sample(&r, at(0)); p == nil || p.CPUCores != nil {
		t.Fatalf("first: %+v", p)
	}
	var none procRow
	if p := none.sample(&r, at(10)); p != nil {
		t.Fatalf("missing: %+v", p)
	}
	after := row(30)
	if p := after.sample(&r, at(20)); p == nil || p.CPUCores == nil || *p.CPUCores != 1 {
		t.Fatalf("after the gap: %+v", p)
	}
}

// A heartbeat's filesystem free bytes, and a disk-scaling runner's
// filesystem capacity and headroom, are stored on the host's sample and
// read back from its history: raw, and a minute's rollup (free the mean,
// capacity and headroom the maximum). An older runner's leave them absent.
func TestHostHistoryCarriesTheFilesystem(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	opKey := ids.Secret("luxk")
	execSQL(t, s, ctx, `INSERT INTO api_keys (id, tenant_id, name, key_hash, scopes) VALUES ('ko', NULL, 'o', $1, ARRAY['operator'])`, ids.Hash(opKey))
	execSQL(t, s, ctx, `INSERT INTO hosts (id, name, state) VALUES ('h1', 'h1', 'ready'), ('h2', 'h2', 'ready')`)
	// One transaction per heartbeat, as the hub's: a sample is stamped at
	// its transaction's now().
	sample := func(host string, u *proto.HostUsage) {
		t.Helper()
		if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error { return sampleHost(ctx, tx, host, u, nil) }); err != nil {
			t.Fatal(err)
		}
	}
	// Capacity and headroom change between samples, as across runner
	// restarts on a resized volume; each column's maximum is neither its
	// minimum, its first, its last nor its mean.
	type fs struct{ free, capacity, headroom int64 }
	samples := []fs{
		{100 << 30, 152 << 30, 10 << 30},
		{110 << 30, 155 << 30, 12 << 30},
		{120 << 30, 150 << 30, 9 << 30},
		{110 << 30, 153 << 30, 11 << 30},
	}
	for _, f := range samples {
		sample("h1", &proto.HostUsage{DiskBytes: f.capacity - f.free, FSFreeBytes: &f.free, FSCapacityBytes: &f.capacity, DiskHeadroomBytes: &f.headroom})
	}
	sample("h2", &proto.HostUsage{DiskBytes: 7})
	q := url.Values{"res": {"0"}, "since": {"1h"}}.Encode()
	raw := historyRequest(t, s, opKey, "/v1/hosts/h1/history?"+q).Samples
	if len(raw) != len(samples) {
		t.Fatalf("%d raw samples, want %d", len(raw), len(samples))
	}
	for i, f := range samples {
		sm := raw[i]
		if sm.DiskFreeBytes == nil || *sm.DiskFreeBytes != f.free || *sm.FSCapacityBytes != f.capacity || *sm.DiskHeadroomBytes != f.headroom {
			t.Errorf("raw sample %d: free %v capacity %v headroom %v, want %+v", i, sm.DiskFreeBytes, sm.FSCapacityBytes, sm.DiskHeadroomBytes, f)
		}
	}
	old := historyRequest(t, s, opKey, "/v1/hosts/h2/history?"+q).Samples
	if len(old) != 1 || old[0].DiskFreeBytes != nil || old[0].FSCapacityBytes != nil || old[0].DiskHeadroomBytes != nil || *old[0].DiskBytes != 7 {
		t.Errorf("older runner's sample %+v", old)
	}
	// Into a closed minute, keeping their order and distinct times, then
	// rolled up.
	execSQL(t, s, ctx, `UPDATE host_samples SET at = date_trunc('minute', now()) - interval '3 minutes' + (at - date_trunc('second', at))`)
	if err := s.rollupHistory(ctx); err != nil {
		t.Fatal(err)
	}
	q = url.Values{"res": {"60"}, "since": {"1h"}}.Encode()
	mins := historyRequest(t, s, opKey, "/v1/hosts/h1/history?"+q).Samples
	if len(mins) != 1 {
		t.Fatalf("%d minute rollups, want 1", len(mins))
	}
	if m := mins[0]; m.DiskFreeBytes == nil || m.FSCapacityBytes == nil || m.DiskHeadroomBytes == nil ||
		*m.DiskFreeBytes != 110<<30 || *m.FSCapacityBytes != 155<<30 || *m.DiskHeadroomBytes != 12<<30 {
		t.Errorf("minute rollup free %s capacity %s headroom %s, want 110, 155 and 12 GiB",
			fmtPtr(m.DiskFreeBytes), fmtPtr(m.FSCapacityBytes), fmtPtr(m.DiskHeadroomBytes))
	}
}

func fmtPtr(p *int64) string {
	if p == nil {
		return "absent"
	}
	return strconv.FormatInt(*p, 10)
}

package server

import (
	"context"
	"math"
	"net/url"
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

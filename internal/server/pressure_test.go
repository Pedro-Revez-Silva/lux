package server

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/marcioapm/lux/internal/proto"
)

// Heartbeats raise a placement's peak CPU and stall totals and never lower
// them; a heartbeat without them (an older runner, or no PSI) leaves NULL,
// not 0; the exit report's smaller values change nothing; the Run's JSON
// carries them per placement and rolled up.
func TestPeakCPUAndStallsOnPlacement(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	key := serversFixture(t, s, ctx)
	beat := func(u *proto.Usage) {
		t.Helper()
		if err := s.heartbeat(ctx, "h1", proto.Heartbeat{Leases: []proto.LivePlacement{{RunID: r1, Epoch: 1, State: "running", Usage: u}}}); err != nil {
			t.Fatal(err)
		}
	}
	type stored struct{ peak, cpu, mem, memFull, io, ioFull *float64 }
	read := func() stored {
		t.Helper()
		var x stored
		row := queryOne[[]*float64](t, s, `SELECT ARRAY[peak_cpu_cores, cpu_stall_seconds, memory_stall_seconds, memory_full_stall_seconds,
			io_stall_seconds, io_full_stall_seconds] FROM placements WHERE run_id = $1 AND epoch = 1`, r1)
		x.peak, x.cpu, x.mem, x.memFull, x.io, x.ioFull = row[0], row[1], row[2], row[3], row[4], row[5]
		return x
	}

	beat(&proto.Usage{CPUSeconds: 5, PeakMemoryBytes: 1 << 20})
	if x := read(); x.peak != nil || x.cpu != nil || x.mem != nil || x.memFull != nil || x.io != nil || x.ioFull != nil {
		t.Fatalf("an older runner's heartbeat stored %+v, want every field NULL", x)
	}

	beat(&proto.Usage{CPUSeconds: 10, PeakCPUCores: f64(1.5), CPUCores: f64(1.5), CPUStallSeconds: f64(2), MemoryStallSeconds: f64(0),
		MemoryFullStallSeconds: f64(0), IOStallSeconds: f64(4), IOFullStallSeconds: f64(1)})
	beat(&proto.Usage{CPUSeconds: 20, PeakCPUCores: f64(3.25), CPUCores: f64(3.25), CPUStallSeconds: f64(6), MemoryStallSeconds: f64(0.5),
		MemoryFullStallSeconds: f64(0.25), IOStallSeconds: f64(4), IOFullStallSeconds: f64(1)})
	want := fmt.Sprint(3.25, 6, 0.5, 0.25, 4, 1)
	stored6 := func() string {
		t.Helper()
		x := read()
		return fmt.Sprint(val(x.peak), val(x.cpu), val(x.mem), val(x.memFull), val(x.io), val(x.ioFull))
	}
	// A restarted runner's first heartbeat: a fresh peak and counters,
	// every one lower.
	beat(&proto.Usage{CPUSeconds: 21, PeakCPUCores: f64(1), CPUCores: f64(1), CPUStallSeconds: f64(5), MemoryStallSeconds: f64(0.375),
		MemoryFullStallSeconds: f64(0.125), IOStallSeconds: f64(3), IOFullStallSeconds: f64(0.5)})
	if got := stored6(); got != want {
		t.Fatalf("after a lower heartbeat %s, want %s kept", got, want)
	}
	stopUsageRun(t, s, ctx, key)
	code := 0
	if err := reportUsageStatus(s, ctx, proto.Status{State: "exited", ExitCode: &code, Reason: "stopped",
		Usage: &proto.Usage{PeakCPUCores: f64(1), CPUStallSeconds: f64(1), MemoryStallSeconds: f64(0.1),
			MemoryFullStallSeconds: f64(0.1), IOStallSeconds: f64(1), IOFullStallSeconds: f64(0.5)}}); err != nil {
		t.Fatal(err)
	}
	if got := stored6(); got != want {
		t.Fatalf("after a lower exit report %s, want %s kept", got, want)
	}

	// A second placement on the Run: the Run's peak is the maximum, its
	// stalls the sum; a placement without them adds nothing.
	execSQL(t, s, ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state, peak_cpu_cores, cpu_stall_seconds,
			memory_stall_seconds, memory_full_stall_seconds, io_stall_seconds, io_full_stall_seconds)
		VALUES ('p2', 't1', $1, 'h1', 2, 'exited', 2, 1.5, 2, 3, 0.5, 0.75), ('p3', 't1', $1, 'h1', 3, 'exited', NULL, NULL, NULL, NULL, NULL, NULL)`, r1)
	var run Run
	if code := getJSON(t, s, key, "/v1/runs/"+r1, &run); code != http.StatusOK {
		t.Fatalf("get run: %d", code)
	}
	if len(run.Placements) != 3 || run.Placements[0].PeakCPUCores == nil || *run.Placements[0].PeakCPUCores != 3.25 ||
		run.Placements[2].PeakCPUCores != nil || run.Placements[2].CPUStallSeconds != nil {
		t.Fatalf("placements %+v", run.Placements)
	}
	u := run.Usage
	got := fmt.Sprint(val(u.PeakCPUCores), val(u.CPUStallSeconds), val(u.MemoryStallSeconds), val(u.MemoryFullStallSeconds),
		val(u.IOStallSeconds), val(u.IOFullStallSeconds))
	if want := fmt.Sprint(3.25, 7.5, 2.5, 3.25, 4.5, 1.75); got != want {
		t.Fatalf("run usage %s, want %s", got, want)
	}

	// A Run whose placements reported none of them: absent, never 0.
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, spec, state, current_epoch) VALUES ('run_bbbbbbbbbbbbbbbb', 't1', '{"workload": {"workdir": "/work"}}', 'exited', 1)`)
	execSQL(t, s, ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state) VALUES ('p4', 't1', 'run_bbbbbbbbbbbbbbbb', 'h1', 1, 'exited')`)
	var old Run
	if code := getJSON(t, s, key, "/v1/runs/run_bbbbbbbbbbbbbbbb", &old); code != http.StatusOK {
		t.Fatalf("get run: %d", code)
	}
	u = old.Usage
	if u.PeakCPUCores != nil || u.CPUStallSeconds != nil || u.MemoryStallSeconds != nil || u.MemoryFullStallSeconds != nil ||
		u.IOStallSeconds != nil || u.IOFullStallSeconds != nil {
		t.Fatalf("usage of a Run without the figures: %v %v %v %v %v %v, want all absent", val(u.PeakCPUCores), val(u.CPUStallSeconds),
			val(u.MemoryStallSeconds), val(u.MemoryFullStallSeconds), val(u.IOStallSeconds), val(u.IOFullStallSeconds))
	}
}

// A placement's raw samples carry its CPU rate per interval and the stall
// counters; a rollup keeps the bucket's highest CPU rate and the counters'
// maximum, and the Run's history serves the stalls as rates.
func TestRollupPeakCPUAndStalls(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	key := serversFixture(t, s, ctx)
	hour := time.Now().UTC().Truncate(time.Hour).Add(-2 * time.Hour)
	// Two samples in each of two minutes: CPU rates 1, 6 | 2, 3; cpu
	// stall 0, 30 | 60, 120 seconds, and memory, memory full, io and io
	// full stall that ×2, ×3, ×4 and ×5.
	for i, off := range []time.Duration{0, 30 * time.Second, time.Minute, 90 * time.Second} {
		st := []float64{0, 30, 60, 120}[i]
		execSQL(t, s, ctx, `INSERT INTO placement_samples (run_id, epoch, tenant_id, res, at, cpu_seconds, peak_cpu_cores,
				cpu_stall, mem_stall, mem_full_stall, io_stall, io_full_stall)
			VALUES ($1, 1, 't1', 0, $2, $3, $4, $5, $6, $7, $8, $9)`, r1, hour.Add(off), float64(i*10),
			[]float64{1, 6, 2, 3}[i], st, 2*st, 3*st, 4*st, 5*st)
	}
	if err := s.rollupHistory(ctx); err != nil {
		t.Fatal(err)
	}
	q := "from=" + url.QueryEscape(hour.Add(-time.Second).Format(time.RFC3339)) + "&to=" + url.QueryEscape(hour.Add(time.Hour).Format(time.RFC3339))
	for _, c := range []struct {
		res  string
		want []string
	}{
		// Minutes: the peaks 6 and 3; the cpu stall counter 30 then 120,
		// a rate of 90 s over 60 s, and the others' multiples of it.
		{"60", []string{"peak=6 stall=[absent absent absent absent absent]", "peak=3 stall=[1.5 3 4.5 6 7.5]"}},
		{"3600", []string{"peak=6 stall=[absent absent absent absent absent]"}},
	} {
		h := historyRequest(t, s, key, "/v1/runs/"+r1+"/history?res="+c.res+"&"+q)
		var got []string
		for _, sm := range h.Samples {
			got = append(got, fmt.Sprintf("peak=%v stall=%v", val(sm.PeakCPUCores),
				[]any{val(sm.CPUStallRate), val(sm.MemoryStallRate), val(sm.MemoryFullStallRate), val(sm.IOStallRate), val(sm.IOFullStallRate)}))
		}
		if fmt.Sprint(got) != fmt.Sprint(c.want) {
			t.Errorf("res %s: %v, want %v", c.res, got, c.want)
		}
	}
	// The stored buckets: the latest minute's and the hour's maxima.
	for res, want := range map[int]string{60: "[3 120 240 360 480 600]", 3600: "[6 120 240 360 480 600]"} {
		row := queryOne[[]*float64](t, s, `SELECT ARRAY[peak_cpu_cores, cpu_stall, mem_stall, mem_full_stall, io_stall, io_full_stall]
			FROM placement_samples WHERE res = $1 ORDER BY at DESC LIMIT 1`, res)
		var got []any
		for _, v := range row {
			got = append(got, val(v))
		}
		if fmt.Sprint(got) != want {
			t.Errorf("res %d stored %v, want %s", res, got, want)
		}
	}
}

// A heartbeat writes the rate over its interval and the stall counters into
// the raw sample; absent values are NULL.
func TestHeartbeatSamplesCPURateAndStalls(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	serversFixture(t, s, ctx)
	u := &proto.Usage{CPUSeconds: 3, CPUCores: f64(0.75), PeakCPUCores: f64(2), CPUStallSeconds: f64(1.25), MemoryFullStallSeconds: f64(0.5)}
	if err := s.heartbeat(ctx, "h1", proto.Heartbeat{Leases: []proto.LivePlacement{{RunID: r1, Epoch: 1, State: "running", Usage: u}}}); err != nil {
		t.Fatal(err)
	}
	row := queryOne[[]*float64](t, s, `SELECT ARRAY[peak_cpu_cores, cpu_stall, mem_stall, mem_full_stall, io_stall, io_full_stall]
		FROM placement_samples WHERE run_id = $1 AND res = 0`, r1)
	var got []any
	for _, v := range row {
		got = append(got, val(v))
	}
	if want := []any{0.75, 1.25, "absent", 0.5, "absent", "absent"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("sample %v, want %v", got, want)
	}
}

func val(v *float64) any {
	if v == nil {
		return "absent"
	}
	return *v
}

package server

import (
	"context"
	"math"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/store"
)

// An exit status carrying usage above 2^31-1 (bytes) and fractional CPU
// seconds ends the placement, and every value is stored exactly.
func TestExitStatusRecordsLargeUsage(t *testing.T) {
	usage := proto.Usage{
		PeakMemoryBytes: 3 * gib,
		PeakDiskBytes:   5 * gib,
		PeakPids:        40000, // above int2: pins the int cast, not smallint
		// Not representable in float4 (it rounds to 123456.7890625).
		CPUSeconds: 123456.789,
		NetRxBytes: 6 * gib,
		NetTxBytes: 7 * gib,
	}
	// "failed" is a runner that could not finish the placement cleanly: still
	// a requested stop, so the Run is stopped, and its usage is kept.
	for _, status := range []string{"exited", "failed"} {
		t.Run(status, func(t *testing.T) {
			s := testServer(t)
			ctx := context.Background()
			key := serversFixture(t, s, ctx)
			stopUsageRun(t, s, ctx, key)

			code := 137
			u := usage
			st := proto.Status{State: status, ExitCode: &code, Reason: "stopped", Usage: &u, OutputSeq: 3 * gib}
			if status == "failed" {
				st.ExitCode, st.Message = nil, "container lost"
			}
			if err := reportUsageStatus(s, ctx, st); err != nil {
				t.Fatalf("exit status: %v", err)
			}
			wantUsageStates(t, s, ctx, "after the exit", "exited", StateStopped)
			got, outputSeq := storedUsage(t, s, ctx)
			if got != usage {
				t.Fatalf("stored usage %+v, want %+v", got, usage)
			}
			if outputSeq != 3*gib {
				t.Fatalf("stored output_seq %d, want %d", outputSeq, 3*gib)
			}
		})
	}
}

// Peaks a heartbeat raised survive an exit status reporting smaller ones:
// greatest() keeps the heartbeat's values.
func TestExitStatusKeepsHeartbeatPeaks(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	key := serversFixture(t, s, ctx)
	// Each peak is rejected or rounded by a narrower cast: bytes above int4,
	// pids at int4's maximum (above int2), and cpu whose .25 float4 cannot
	// hold at 1e10.
	peaks := proto.Usage{
		PeakMemoryBytes: 9 * gib,
		PeakDiskBytes:   9 * gib,
		PeakPids:        math.MaxInt32,
		CPUSeconds:      1e10 + 0.25,
		NetRxBytes:      11 * gib,
		NetTxBytes:      13 * gib,
	}
	hbUsage := peaks
	hb := proto.Heartbeat{Leases: []proto.LivePlacement{{RunID: r1, Epoch: 1, State: "running", Usage: &hbUsage}}}
	if err := s.heartbeat(ctx, "h1", hb); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	stopUsageRun(t, s, ctx, key)

	code := 0
	smaller := proto.Usage{
		PeakMemoryBytes: 3 * gib,
		PeakDiskBytes:   3 * gib,
		PeakPids:        412,
		CPUSeconds:      2.5,
		NetRxBytes:      3 * gib,
		NetTxBytes:      3 * gib,
	}
	if err := reportUsageStatus(s, ctx, proto.Status{State: "exited", ExitCode: &code, Reason: "stopped", Usage: &smaller}); err != nil {
		t.Fatalf("exit status: %v", err)
	}
	wantUsageStates(t, s, ctx, "after the exit", "exited", StateStopped)
	if got, _ := storedUsage(t, s, ctx); got != peaks {
		t.Fatalf("stored usage %+v, want the heartbeat's %+v", got, peaks)
	}
}

// A snapshot's output sequence above 2^31-1 is stored exactly.
func TestSnapshotRecordsLargeOutputSeq(t *testing.T) {
	s, ctx := reportFixture(t)
	sd := snapshotB("snapB", 1)
	sd.OutputSeq = 3 * gib
	if f := reportSnapshot(t, s, "hb", "rb", 1, sd); f.Type != proto.MsgAck || ackRefused(t, f) {
		t.Fatalf("report: %s %s", f.Type, f.Data)
	}
	var got int64
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT output_seq FROM placements WHERE run_id = 'rb' AND epoch = 1`).Scan(&got)
	})
	if err != nil || got != sd.OutputSeq {
		t.Fatalf("stored output_seq %d (%v), want %d", got, err, sd.OutputSeq)
	}
}

// stopUsageRun requests r1's stop and reports its placement stopping.
func stopUsageRun(t *testing.T, s *Server, ctx context.Context, key string) {
	t.Helper()
	if w := apiCall(t, s, key, http.MethodPost, "/v1/runs/"+r1+"/stop", nil); w.Code/100 != 2 {
		t.Fatalf("stop: %d %s", w.Code, w.Body)
	}
	if err := reportUsageStatus(s, ctx, proto.Status{State: "stopping"}); err != nil {
		t.Fatal(err)
	}
	wantUsageStates(t, s, ctx, "before the exit", "stopping", StateStopping)
}

func reportUsageStatus(s *Server, ctx context.Context, st proto.Status) error {
	return s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		return s.applyStatus(ctx, tx, "t1", r1, 1, st)
	})
}

func storedUsage(t *testing.T, s *Server, ctx context.Context) (u proto.Usage, outputSeq int64) {
	t.Helper()
	var seq *int64
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT peak_memory_bytes, peak_disk_bytes, peak_pids, cpu_seconds, net_rx_bytes, net_tx_bytes, output_seq
			FROM placements WHERE run_id = $1 AND epoch = 1`, r1).Scan(
			&u.PeakMemoryBytes, &u.PeakDiskBytes, &u.PeakPids, &u.CPUSeconds, &u.NetRxBytes, &u.NetTxBytes, &seq)
	})
	if err != nil {
		t.Fatal(err)
	}
	if seq != nil {
		outputSeq = *seq
	}
	return u, outputSeq
}

// wantUsageStates fails unless r1's epoch-1 placement and the Run are in
// the given states.
func wantUsageStates(t *testing.T, s *Server, ctx context.Context, when, wantPlacement, wantRun string) {
	t.Helper()
	var placement, run string
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT p.state, r.state FROM placements p JOIN runs r ON r.id = p.run_id
			WHERE p.run_id = $1 AND p.epoch = 1`, r1).Scan(&placement, &run)
	})
	if err != nil {
		t.Fatal(err)
	}
	if placement != wantPlacement || run != wantRun {
		t.Fatalf("%s: placement %s, run %s; want %s, %s", when, placement, run, wantPlacement, wantRun)
	}
}

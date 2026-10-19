package runner

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/marcioapm/lux/internal/proto"
)

// mkfifoAt replaces path (if any) with a FIFO that has no writer.
func mkfifoAt(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
}

// The shim's exit file is read as written; a FIFO left in its place reads
// as no exit file (crashed), at once.
func TestReadExitRefusesFIFO(t *testing.T) {
	f := newFinishFixture(t)
	exit := filepath.Join(f.r.cfg.DataDir, "rt", proto.ExitFile(1))
	writeFile(t, exit, `{"exitCode":3,"reason":"exited","lastSeq":9}`)
	if e := f.p.readExit(0); e.Code != 3 || e.Reason != "exited" || e.OutputSeq != 9 || !e.shimReported {
		t.Fatalf("exit from a regular file %+v", e)
	}
	mkfifoAt(t, exit)
	var e *exitRecord
	boundedFIFO(t, exit, func() { e = f.p.readExit(5) })
	if e.Reason != "crashed" || !e.Failed || e.shimReported {
		t.Fatalf("exit from a FIFO %+v", e)
	}
}

// A FIFO in place of the output file is left out of the exit snapshot at
// once; the snapshot still has its artifacts.
func TestSnapshotRefusesOutputFIFO(t *testing.T) {
	f := newFinishFixture(t)
	f.release()
	f.p.assign.Spec.Artifacts.Paths = []string{"/data/state"}
	out := filepath.Join(f.r.cfg.DataDir, "rt", proto.OutputFile(1))
	writeFile(t, out, `{"seq":1}`+"\n")
	sd, err := f.p.snapshot(context.Background(), context.Background())
	if err != nil || sd.Output == nil {
		t.Fatalf("snapshot of a regular output file %+v %v", sd, err)
	}
	if s := blobContent(t, f.r.blobPath(sd.Output.BlobID)); s != `{"seq":1}`+"\n" {
		t.Fatalf("output blob %q", s)
	}
	mkfifoAt(t, out)
	boundedFIFO(t, out, func() { sd, err = f.p.snapshot(context.Background(), context.Background()) })
	if err != nil || sd.Output != nil || len(sd.Artifacts) != 1 || sd.Artifacts[0].Path != "/data/state" {
		t.Fatalf("snapshot with an output FIFO %+v %v", sd, err)
	}
}

// Tailing an output file that is a FIFO returns at once, handling nothing.
func TestTailRecordsRefusesFIFO(t *testing.T) {
	out := filepath.Join(t.TempDir(), proto.OutputFile(1))
	writeFile(t, out, `{"seq":1}`+"\n"+`{"seq":2}`+"\n")
	var seqs []int64
	collect := func(r proto.Record) error { seqs = append(seqs, r.Seq); return nil }
	if err := tailRecords(context.Background(), out, 0, false, func() bool { return true }, collect); err != nil || len(seqs) != 2 {
		t.Fatalf("records %v %v", seqs, err)
	}
	mkfifoAt(t, out)
	seqs = nil
	boundedFIFO(t, out, func() {
		_ = tailRecords(context.Background(), out, 0, true, func() bool { return true }, collect)
	})
	if len(seqs) != 0 {
		t.Fatalf("records from a FIFO %v", seqs)
	}
}

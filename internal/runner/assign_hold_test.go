package runner

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/marcioapm/lux/internal/egress"
	"github.com/marcioapm/lux/internal/podman"
	"github.com/marcioapm/lux/internal/proto"
)

// holdRunner is a Runner with LUX_TEST_ASSIGN_HOLD at a file that exists,
// on the polling transport (acks queue in pollAcks). A placement it takes
// up fails at once: its podman is /bin/false.
func holdRunner(t *testing.T) (*Runner, string) {
	t.Helper()
	dir := t.TempDir()
	hold := filepath.Join(dir, "hold")
	if err := os.WriteFile(hold, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	r := &Runner{cfg: Config{DataDir: dir, AssignHold: hold}, log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		pm: &podman.Podman{Bin: "/bin/false"}, egress: &egress.Firewall{},
		placements: map[string]*placement{}, subs: map[string]context.CancelFunc{}, control: newSerialQueues()}
	r.conn = newConn(r)
	r.conn.polling = true
	return r, hold
}

// dispatchAndDrain dispatches f as luxd would and returns once the Run's
// control queue has handled it.
func dispatchAndDrain(t *testing.T, r *Runner, ctx context.Context, f proto.Frame, during func()) {
	t.Helper()
	r.conn.dispatch(ctx, f)
	if during != nil {
		during()
	}
	drained := make(chan struct{})
	r.control.enqueue(f.RunID, func() { close(drained) })
	select {
	case <-drained:
	case <-time.After(10 * time.Second):
		t.Fatal("the held assignment was never handled")
	}
}

func assignFrame(id int64) proto.Frame {
	return proto.Frame{Type: proto.MsgAssign, ID: id, RunID: "run_x", Epoch: 2,
		Data: proto.Marshal(proto.Assign{RunID: "run_x", TenantID: "t1", Epoch: 2})}
}

func pollAcks(r *Runner) []int64 {
	r.conn.mu.Lock()
	defer r.conn.mu.Unlock()
	return append([]int64(nil), r.conn.pollAcks...)
}

// The connection ends while an assignment is held: the runner neither
// takes it up nor acks it, so luxd redelivers it to a hold on the next
// connection.
func TestACancelledAssignHoldTakesNothingUp(t *testing.T) {
	for _, c := range []struct {
		name            string
		released        bool // the hold file is gone before the dispatch
		cancelWhileHeld bool // else the context is cancelled before the dispatch
	}{
		{"cancelled while held", false, true},
		{"cancelled before the hold", false, false},
		// Cancellation still wins over the release.
		{"cancelled and released", true, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, hold := holdRunner(t)
			if c.released {
				if err := os.Remove(hold); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var during func()
			if c.cancelWhileHeld {
				during = func() {
					time.Sleep(300 * time.Millisecond)
					cancel()
				}
			} else {
				cancel()
			}
			dispatchAndDrain(t, r, ctx, assignFrame(7), during)
			r.mu.Lock()
			p := r.placements["run_x"]
			r.mu.Unlock()
			if p != nil {
				t.Fatalf("placement %d taken up after its hold was cancelled", p.epoch)
			}
			if acks := pollAcks(r); len(acks) != 0 {
				t.Fatalf("acked %v after the hold was cancelled", acks)
			}
		})
	}
}

// Released, the same held assignment is handled and acked. The runner
// already holds epoch 2 (a redelivery), so handling it starts nothing.
func TestAReleasedAssignHoldIsAcked(t *testing.T) {
	r, hold := holdRunner(t)
	held := &placement{r: r, runID: "run_x", epoch: 2, phase: "running"}
	r.placements["run_x"] = held
	dispatchAndDrain(t, r, context.Background(), assignFrame(7), func() {
		time.Sleep(300 * time.Millisecond)
		if len(pollAcks(r)) != 0 {
			t.Errorf("acked while the hold file exists")
		}
		if err := os.Remove(hold); err != nil {
			t.Fatal(err)
		}
	})
	if acks := pollAcks(r); len(acks) != 1 || acks[0] != 7 {
		t.Fatalf("acks %v, want [7]", acks)
	}
	if r.placements["run_x"] != held {
		t.Fatal("the redelivered assignment replaced the held placement")
	}
}

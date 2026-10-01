package adapter

import (
	"fmt"
	"strings"
	"testing"

	"github.com/marcioapm/lux/internal/proto"
)

// burst delivers n steers of size bytes while the first one's prompt_async
// is held, releases it, and returns once every steer was answered: the
// Run is still up, so only what the adapter holds for a live Run remains.
func burst(t *testing.T, n, size int) (*ACP, *fakeBus, *inputSink, uint64) {
	t.Helper()
	a, b, _, sink, _ := ocWithBus(t)
	hold := make(chan struct{})
	b.mu.Lock()
	b.hold, b.dropParts = hold, true
	b.mu.Unlock()
	before := heap()
	for i := range n {
		a.Deliver(proto.Input{RequestID: fmt.Sprintf("r%d", i), Text: strings.Repeat(string(rune('a'+i%26)), size)})
		if i == 0 {
			b.postedID(t, 0)
		}
	}
	b.mu.Lock()
	b.hold = nil
	b.mu.Unlock()
	close(hold)
	return a, b, sink, before
}

// Steers queued behind a slow prompt_async are not kept once delivered and
// read: 250 of 64 KiB (under the pending-input limits) leave the heap as
// it was, both while the last one still waits its turn and once the queue
// is empty.
func TestOpenCodeDeliveredSteersAreNotRetained(t *testing.T) {
	const n, size = 250, 64 << 10
	a, b, _, sink, _ := ocWithBus(t)
	first, late := make(chan struct{}), make(chan struct{})
	release := func(c chan struct{}) {
		select {
		case <-c:
		default:
			close(c)
		}
	}
	// Before the fake server closes, which waits for held requests.
	t.Cleanup(func() { release(first); release(late) })
	b.mu.Lock()
	b.holdN, b.dropParts = map[int]chan struct{}{0: first, n - 2: late}, true
	b.mu.Unlock()
	before := heap()
	for i := range n {
		a.Deliver(proto.Input{RequestID: fmt.Sprintf("r%d", i), Text: strings.Repeat(string(rune('a'+i%26)), size)})
		if i == 0 {
			b.postedID(t, 0)
		}
	}
	release(first)
	// r(n-2) is in flight and r(n-1) still queued; the rest were read.
	onBus(t, a, b.answer(b.postedID(t, n-3)))
	sink.wait(t, fmt.Sprintf("consumed r%d", n-3))
	if grew := int64(heap()) - int64(before); grew > n*size/8 {
		t.Fatalf("heap grew %d bytes with %d of %d steers of %d bytes read and one queued", grew, n-2, n, size)
	}
	release(late)
	last := fmt.Sprintf("r%d", n-1)
	sink.wait(t, "accepted "+last+" ")
	onBus(t, a, b.answer(b.postedID(t, n-1)))
	sink.wait(t, "consumed "+last)
	if grew := int64(heap()) - int64(before); grew > n*size/8 {
		t.Fatalf("heap grew %d bytes after %d steers of %d bytes were delivered and read", grew, n, size)
	}
}

// A burst past the pending-input limit: the steers over it fail at once,
// saying why; those within it are delivered and read, and not retained.
func TestOpenCodeSteerBurstIsBounded(t *testing.T) {
	const n, size = 1024, 64 << 10
	a, b, sink, before := burst(t, n, size)
	// r0 is in flight; r1..r256 wait; r257.. are over the limit.
	last := fmt.Sprintf("r%d", maxPendingSteers)
	sink.wait(t, "accepted "+last+" ")
	onBus(t, a, b.answer(b.postedID(t, maxPendingSteers)))
	sink.wait(t, "consumed "+last)
	failed, consumed := 0, 0
	for _, l := range sink.lines() {
		switch {
		case strings.HasPrefix(l, "failed r") && strings.HasSuffix(l, "too many inputs wait for the agent (256 inputs or 32 MiB)"):
			failed++
		case strings.HasPrefix(l, "consumed r"):
			consumed++
		}
	}
	if failed != n-maxPendingSteers-1 || consumed != maxPendingSteers+1 {
		t.Fatalf("%d failed, %d consumed of %d; want %d, %d", failed, consumed, n, n-maxPendingSteers-1, maxPendingSteers+1)
	}
	if grew := int64(heap()) - int64(before); grew > n*size/16 {
		t.Fatalf("heap grew %d bytes after a burst of %d steers of %d bytes", grew, n, size)
	} else {
		t.Logf("heap grew %d bytes after a burst of %d steers of %d bytes", grew, n, size)
	}
}

// The byte limit: two waiting steers of 20 MiB are over 32 MiB, so the
// second fails at once; the first is still delivered.
func TestOpenCodePendingSteerBytesAreBounded(t *testing.T) {
	a, b, w, sink, first := ocWithBus(t)
	hold := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-hold:
		default:
			close(hold)
		}
	})
	b.mu.Lock()
	b.holdN, b.dropParts = map[int]chan struct{}{0: hold}, true
	b.mu.Unlock()
	a.Deliver(proto.Input{RequestID: "r0", Text: "x"})
	b.postedID(t, 0)
	a.Deliver(proto.Input{RequestID: "r1", Text: strings.Repeat("y", 20<<20)})
	a.Deliver(proto.Input{RequestID: "r2", Text: strings.Repeat("z", 20<<20)})
	sink.wait(t, "failed r2: "+errPendingSteersLimit)
	close(hold)
	sink.wait(t, "accepted r1 ")
	onBus(t, a, b.answer(b.postedID(t, 1)))
	b.setLoop(false)
	w.resolve(first, ocResult)
	checkLines(t, w, sink, "idle", "busy", "accepted prompt next_step receipt=false",
		"failed r2: "+errPendingSteersLimit, "accepted r0 next_step receipt=true", "accepted r1 next_step receipt=true",
		"consumed r0", "consumed r1", "turn_end", "idle")
}

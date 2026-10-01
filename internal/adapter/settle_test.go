package adapter

import (
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/marcioapm/lux/internal/proto"
)

// testClock is settle's clock, moved only by the test: fire runs the next
// look settle scheduled, synchronously, at the time it was due.
type testClock struct {
	mu     sync.Mutex
	t      time.Time
	due    []*testTimer
	delays []time.Duration
}

type testTimer struct {
	at  time.Time
	f   func()
	off bool
}

func newTestClock() *testClock { return &testClock{t: time.UnixMilli(1790776809335)} }

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) afterFunc(d time.Duration, f func()) func() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	tm := &testTimer{at: c.t.Add(d), f: f}
	c.due = append(c.due, tm)
	c.delays = append(c.delays, d)
	return func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		if tm.off {
			return false
		}
		tm.off = true
		c.due = slices.DeleteFunc(c.due, func(x *testTimer) bool { return x == tm })
		return true
	}
}

// waitDue waits until settle has scheduled its next look, which it does as
// the last thing of a look that keeps waiting.
func (c *testClock) waitDue(t *testing.T) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(2 * time.Millisecond) {
		c.mu.Lock()
		n := len(c.due)
		c.mu.Unlock()
		if n > 0 {
			return
		}
	}
	t.Fatal("settle scheduled no next look")
}

// fire moves the clock to settle's next look and runs it.
func (c *testClock) fire(t *testing.T) {
	t.Helper()
	c.waitDue(t)
	c.mu.Lock()
	tm := c.due[0]
	c.due = c.due[1:]
	tm.off = true
	c.t = tm.at
	c.mu.Unlock()
	tm.f()
}

func (c *testClock) lastDelays(n int) []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.delays[max(0, len(c.delays)-n):])
}

func (b *fakeBus) posts() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.posted)
}

func wantPosts(t *testing.T, b *fakeBus, n int, when string) {
	t.Helper()
	if got := b.posts(); got != n {
		t.Fatalf("%s: %d prompt_async, want %d", when, got, n)
	}
}

// settleStart: a steer accepted into the running loop, which ends without
// answering it; OpenCode reports no loop. Returns once settle's first look
// has seen it stored, unanswered and idle.
func settleStart(t *testing.T) (*ACP, *fakeBus, *agentWire, *inputSink, *testClock, string) {
	t.Helper()
	clk := newTestClock()
	a, b, w, sink, first := ocWithBusClock(t, clk)
	a.Deliver(proto.Input{RequestID: "s1", Text: "x"})
	sink.wait(t, "accepted s1")
	msg := b.postedID(t, 0)
	b.setLoop(false)
	w.resolve(first, ocResult)
	clk.waitDue(t)
	wantPosts(t, b, 1, "first look at a stored, unanswered steer with no loop")
	return a, b, w, sink, clk, msg
}

// A steer seen stored, unanswered and idle once is not failed: the step
// answering it may not be stored yet. It comes before the threshold
// (3×settleEvery after a normal end): consumed. OpenCode never reported a
// loop after the ACP turn, so that step was the turn's own: one turn end.
func TestOpenCodeSettleWaitsForALateAnswer(t *testing.T) {
	_, b, w, sink, clk, msg := settleStart(t)
	clk.fire(t) // +1 s
	wantPosts(t, b, 1, "second look, before the threshold")
	b.answer(msg)
	clk.fire(t) // +2 s
	wantPosts(t, b, 1, "after the answer")
	checkLines(t, w, sink, "idle", "busy", "accepted prompt next_step receipt=false",
		"accepted s1 next_step receipt=true", "turn_end", "consumed s1", "idle")
}

// OpenCode reports a loop before the threshold: the steer is waited for,
// never resent, and consumed when that loop answers it.
func TestOpenCodeSettleWaitsForABusyLoop(t *testing.T) {
	_, b, w, sink, clk, msg := settleStart(t)
	clk.fire(t)
	b.setLoop(true)
	clk.fire(t)
	for range 5 { // well past the threshold, still busy
		clk.fire(t)
	}
	wantPosts(t, b, 1, "while a loop runs")
	b.answer(msg)
	b.setLoop(false)
	clk.fire(t)
	wantPosts(t, b, 1, "after the answer")
	checkLines(t, w, sink, "idle", "busy", "accepted prompt next_step receipt=false",
		"accepted s1 next_step receipt=true", "turn_end", "consumed s1", "turn_end", "idle")
}

// Unanswered and idle for 3×settleEvery after a normal end: the loop that
// could have read it ran, so it fails as uncertain and is never sent again.
func TestOpenCodeSettleFailsAfterNormalEnd(t *testing.T) {
	_, b, w, sink, clk, _ := settleStart(t)
	clk.fire(t) // +1 s
	clk.fire(t) // +2 s
	clk.fire(t) // +3 s
	wantPosts(t, b, 1, "at 3×settleEvery after a normal end")
	checkLines(t, w, sink, "idle", "busy", "accepted prompt next_step receipt=false",
		"accepted s1 next_step receipt=true", "turn_end", "failed s1: "+uncertainNoStep, "idle")
}

// A step answering a user message lux did not send (another client's, or
// one OpenCode stored under its own id) stored after the steer had the
// steer in context: the steer is uncertain, fails, and is not sent again.
func TestOpenCodeSettleForeignStepMakesSteerUncertain(t *testing.T) {
	a, b, w, sink, clk, _ := settleStart(t)
	foreign := a.bus.messageID(clk.now()) // sorts after the steer
	b.storeUser(foreign)
	b.answer(foreign)
	for range 3 {
		clk.fire(t)
	}
	wantPosts(t, b, 1, "a steer a later foreign step may have read")
	checkLines(t, w, sink, "idle", "busy", "accepted prompt next_step receipt=false",
		"accepted s1 next_step receipt=true", "turn_end", "failed s1: "+uncertainNoStep, "idle")
}

// A steer OpenCode refuses over HTTP while a steer sent over HTTP in the
// same loop is unread is not sent as a second session/prompt: OpenCode
// would store it under an id of its own, and a step answering it would
// read the HTTP steer without lux seeing it. It starts the next turn; the
// HTTP steer is consumed by its own step, and nothing is posted again.
func TestOpenCodeNoACPFallbackWhileHTTPSteerUnread(t *testing.T) {
	clk := newTestClock()
	a, b, w, sink, first := ocWithBusClock(t, clk)
	a.Deliver(proto.Input{RequestID: "http", Text: "execute once"})
	sink.wait(t, "accepted http")
	httpID := b.postedID(t, 0)
	b.setStatus(http.StatusBadRequest)
	a.Deliver(proto.Input{RequestID: "fallback", Text: "another instruction"})
	b.postedID(t, 1)
	w.none() // no session/prompt during the loop
	waitQueued(t, a, 1)
	b.setStatus(http.StatusNoContent)
	onBus(t, a, b.answer(httpID))
	b.setLoop(false)
	w.resolve(first, ocResult)
	second, _ := w.next("session/prompt")
	sink.wait(t, "accepted fallback")
	w.resolve(second, ocResult)
	checkLines(t, w, sink, "idle", "busy", "accepted prompt next_step receipt=false",
		"accepted http next_step receipt=true", "consumed http", "turn_end",
		"busy", "accepted fallback next_step receipt=false", "turn_end", "idle")
	wantPosts(t, b, 2, "the HTTP steer and the refused one")
}

// The mixed path: HTTP steer A stored, then B refused over HTTP. Were B
// sent as a second session/prompt, OpenCode would store it under an id of
// its own and the step answering it would read A unseen by lux. Whichever
// way B goes, A is never posted again: it is consumed or fails as
// uncertain, and B is accepted once.
func TestOpenCodeOwnACPFallbackNeverResendsReadSteer(t *testing.T) {
	clk := newTestClock()
	a, b, w, sink, first := ocWithBusClock(t, clk)
	a.Deliver(proto.Input{RequestID: "http", Text: "execute once"})
	sink.wait(t, "accepted http")
	b.setStatus(http.StatusBadRequest)
	a.Deliver(proto.Input{RequestID: "fallback", Text: "another instruction"})
	b.postedID(t, 1)
	var joined string
	select {
	case m := <-w.sent:
		// B went as a second session/prompt: OpenCode stores it after A,
		// and the loop's next step answers it.
		joined = string(m["id"])
		vendor := a.bus.messageID(clk.now())
		b.storeUser(vendor)
		b.answer(vendor)
	case <-time.After(300 * time.Millisecond):
	}
	b.setStatus(http.StatusNoContent)
	b.setLoop(false)
	w.resolve(first, ocResult)
	if joined != "" {
		w.resolve(joined, ocResult)
	}
	for range 3 {
		clk.fire(t)
		wantPosts(t, b, 2, "the original two")
	}
	if joined == "" {
		next, _ := w.next("session/prompt")
		sink.wait(t, "accepted fallback")
		w.resolve(next, ocResult)
	}
	sink.waitLast(t, "idle")
	wantPosts(t, b, 2, "the original two")
	checkLines(t, w, sink, "idle", "busy", "accepted prompt next_step receipt=false",
		"accepted http next_step receipt=true", "turn_end", "failed http: "+uncertainNoStep,
		"busy", "accepted fallback next_step receipt=false", "turn_end", "idle")
}

// After an interrupt, OpenCode refuses the steer sent again (400): it
// failed with the reason of that case, and since nothing was sent no other
// loop ran: one turn end.
func TestOpenCodeRefusedCarryDoesNotInventLoop(t *testing.T) {
	clk := newTestClock()
	a, b, w, sink, first := ocWithBusClock(t, clk)
	a.Deliver(proto.Input{RequestID: "s1", Text: "x"})
	sink.wait(t, "accepted s1")
	a.Deliver(proto.Input{RequestID: "int-1", Interrupt: true})
	w.next("session/cancel")
	sink.wait(t, "accepted int-1")
	b.setStatus(http.StatusBadRequest)
	b.setLoop(false)
	w.resolve(first, ocCancelled)
	clk.fire(t)
	wantPosts(t, b, 2, "the refused resend")
	checkLines(t, w, sink, "idle", "busy", "accepted prompt next_step receipt=false",
		"accepted s1 next_step receipt=true", "accepted int-1 next_turn receipt=false", "turn_end",
		"failed s1: sending it again after the turn was cancelled: not sent: prompt_async: 400 Bad Request", "idle")
}

// A steer stored, then a user message lux did not send stored after it and
// answered, then an interrupt: that step had the steer in context, so it
// may have been read. It fails as uncertain and is not sent again.
func TestOpenCodeInterruptDoesNotCarryASteerAStepFollowed(t *testing.T) {
	clk := newTestClock()
	a, b, w, sink, first := ocWithBusClock(t, clk)
	a.Deliver(proto.Input{RequestID: "s1", Text: "x"})
	sink.wait(t, "accepted s1")
	foreign := a.bus.messageID(clk.now()) // sorts after the steer
	b.storeUser(foreign)
	onBus(t, a, b.answer(foreign))
	a.Deliver(proto.Input{RequestID: "int-1", Interrupt: true})
	w.next("session/cancel")
	sink.wait(t, "accepted int-1")
	b.setLoop(false)
	w.resolve(first, ocCancelled)
	for range 3 {
		clk.fire(t)
		if sink.has("failed s1") {
			break
		}
	}
	wantPosts(t, b, 1, "a steer a step followed before the interrupt")
	checkLines(t, w, sink, "idle", "busy", "accepted prompt next_step receipt=false",
		"accepted s1 next_step receipt=true", "accepted int-1 next_turn receipt=false", "turn_end",
		"failed s1: "+uncertainCancelled, "idle")
}

// waitQueued waits until n inputs wait for the next turn.
func waitQueued(t *testing.T, a *ACP, n int) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); ; time.Sleep(2 * time.Millisecond) {
		a.mu.Lock()
		got := len(a.queue)
		a.mu.Unlock()
		if got == n {
			return
		}
		if time.Now().After(end) {
			t.Fatalf("%d inputs queued, want %d", got, n)
		}
	}
}

// After an interrupt the threshold is one settleEvery: the first look only
// notes the steer, the second sends it again, once.
func TestOpenCodeSettleResendsOnceAfterCancel(t *testing.T) {
	clk := newTestClock()
	a, b, w, sink, first := ocWithBusClock(t, clk)
	a.Deliver(proto.Input{RequestID: "s1", Text: "x"})
	sink.wait(t, "accepted s1")
	a.Deliver(proto.Input{RequestID: "int-1", Interrupt: true})
	w.next("session/cancel")
	b.setLoop(false)
	w.resolve(first, ocCancelled)
	clk.waitDue(t)
	wantPosts(t, b, 1, "first look after the cancel")
	clk.fire(t)
	wantPosts(t, b, 2, "second look after the cancel")
	again := b.postedID(t, 1)
	clk.fire(t)
	b.answer(again)
	b.setLoop(false)
	clk.fire(t)
	wantPosts(t, b, 2, "after the answer")
	checkCarried(t, w, sink, "s1")
}

// A steer sent again and still never read, OpenCode idle, is not sent a
// third time: once it has been so for settleGiveUp it fails, and the Run
// goes idle.
func TestOpenCodeSettleGivesUp(t *testing.T) {
	clk := newTestClock()
	a, b, w, sink, first := ocWithBusClock(t, clk)
	a.Deliver(proto.Input{RequestID: "s1", Text: "x"})
	sink.wait(t, "accepted s1")
	a.Deliver(proto.Input{RequestID: "int-1", Interrupt: true})
	w.next("session/cancel")
	sink.wait(t, "accepted int-1")
	b.setLoop(false)
	w.resolve(first, ocCancelled)
	clk.fire(t)
	wantPosts(t, b, 2, "the one resend")
	b.setLoop(false) // the resent copy is dropped too
	looks, limit := 0, int(settleGiveUp/time.Second)+3
	for ; looks < limit && !sink.has("failed s1"); looks++ {
		clk.fire(t)
		wantPosts(t, b, 2, "after the resend")
	}
	if looks < int(settleGiveUp/time.Second) || looks > limit-1 {
		t.Fatalf("failed after %d looks a second apart, want about %v", looks, settleGiveUp)
	}
	checkLines(t, w, sink, "idle", "busy", "accepted prompt next_step receipt=false",
		"accepted s1 next_step receipt=true", "accepted int-1 next_turn receipt=false", "turn_end",
		"failed s1: OpenCode stored it and never read it, also when sent again", "turn_end", "idle")
}

// A steer OpenCode accepted (204) and never stored, with no loop running,
// fails after settleGiveUp instead of being waited for until the Run ends.
func TestOpenCodeSettleGivesUpOnAMessageNeverStored(t *testing.T) {
	clk := newTestClock()
	a, b, w, sink, first := ocWithBusClock(t, clk)
	a.Deliver(proto.Input{RequestID: "s1", Text: "x"})
	sink.wait(t, "accepted s1")
	b.mu.Lock()
	b.stored = nil // lost by OpenCode
	b.mu.Unlock()
	b.setLoop(false)
	w.resolve(first, ocResult)
	looks, limit := 0, int(settleGiveUp/time.Second)+3
	for ; looks < limit && !sink.has("failed s1"); looks++ {
		clk.fire(t)
	}
	if looks < int(settleGiveUp/time.Second) || looks > limit-1 {
		t.Fatalf("failed after %d looks a second apart, want about %v", looks, settleGiveUp)
	}
	wantPosts(t, b, 1, "a message never stored")
	checkLines(t, w, sink, "idle", "busy", "accepted prompt next_step receipt=false",
		"accepted s1 next_step receipt=true", "turn_end", "failed s1: OpenCode accepted it and never stored it", "idle")
}

// GET /session/status failing after the ACP turn, with a loop OpenCode
// started for a late steer: everything lux sent is read, but that loop may
// still run, so no turn end and no idle until OpenCode says it is idle.
func TestOpenCodeSettleErrorsReportNoEndWhileALoopMayRun(t *testing.T) {
	clk := newTestClock()
	a, b, w, sink, first := ocWithBusClock(t, clk)
	hold := b.holdPosts()
	a.Deliver(proto.Input{RequestID: "late", Text: "x"})
	msg := b.postedID(t, 0)
	w.resolve(first, ocResult)
	waitHeld(t, a)
	b.setStatusFail(true)
	close(hold)
	sink.wait(t, "accepted late")
	onBus(t, a, b.answer(msg)) // the steer's own loop
	for range 3 {
		clk.fire(t)
		noTurnEnd(t, sink, "while OpenCode's status is unknown")
	}
	b.setStatusFail(false)
	b.setLoop(false)
	clk.fire(t)
	checkLines(t, w, sink, "idle", "busy", "accepted prompt next_step receipt=false",
		"accepted late next_step receipt=true", "consumed late", "turn_end", "turn_end", "idle")
}

// GET errors while settling back off, doubling up to settleMaxBackoff; a
// good answer brings the interval back to settleEvery.
func TestOpenCodeSettleBacksOffOnErrors(t *testing.T) {
	_, b, w, sink, clk, msg := settleStart(t)
	b.setStatusFail(true)
	for range 7 {
		clk.fire(t)
	}
	want := []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second, 30 * time.Second, 30 * time.Second}
	if got := clk.lastDelays(7); !slices.Equal(got, want) {
		t.Fatalf("delays after GET errors %v, want %v", got, want)
	}
	b.setStatusFail(false)
	b.setLoop(true)
	clk.fire(t)
	if got := clk.lastDelays(1); got[0] != time.Second {
		t.Fatalf("delay after a good answer %v", got)
	}
	b.answer(msg)
	b.setLoop(false)
	clk.fire(t)
	wantPosts(t, b, 1, "after the answer")
	checkLines(t, w, sink, "idle", "busy", "accepted prompt next_step receipt=false",
		"accepted s1 next_step receipt=true", "turn_end", "consumed s1", "turn_end", "idle")
}

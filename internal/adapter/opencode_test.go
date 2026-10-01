package adapter

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/marcioapm/lux/internal/proto"
)

const ocSession = "ses_f0d8b1671ffexBr8v2rWwmuXTt"

// ocStarted drives an ACP adapter through initialize, session/new and the
// first prompt's session/prompt; it returns that prompt's RPC id.
func ocStarted(t *testing.T, a *ACP) (*agentWire, *inputSink, string) {
	t.Helper()
	w, sink := startWire(t, a, proto.ShimConfig{Prompt: "Run `sleep 20 && echo FIRST`"})
	id, _ := w.next("initialize")
	w.send(`{"jsonrpc":"2.0","id":` + id + `,"result":{"protocolVersion":1,"agentCapabilities":{"loadSession":true},"agentInfo":{"name":"OpenCode","version":"1.18.31"}}}`)
	id, _ = w.next("session/new")
	w.send(`{"jsonrpc":"2.0","id":` + id + `,"result":{"sessionId":"` + ocSession + `"}}`)
	first, _ := w.next("session/prompt")
	sink.wait(t, "accepted prompt")
	return w, sink, first
}

// The result both prompts of a joined turn get (opencode-acp-prompt-1).
const ocResult = `"result":{"stopReason":"end_turn","usage":{"inputTokens":6,"outputTokens":5,"totalTokens":8380,"cachedReadTokens":8286,"cachedWriteTokens":83},"_meta":{}}`

// The result of a prompt whose turn was cancelled.
const ocCancelled = `"result":{"stopReason":"cancelled","_meta":{}}`

// resolve answers the adapter's session/prompt id with result.
func (w *agentWire) resolve(id, result string) {
	w.t.Helper()
	w.send(`{"jsonrpc":"2.0","id":` + id + `,` + result + `}`)
}

// checkLines waits for the log's last line, then ends the agent's process
// and waits for the adapter's Run to return (joining the adapter's own
// goroutines), and compares the complete log.
func checkLines(t *testing.T, w *agentWire, sink *inputSink, want ...string) {
	t.Helper()
	sink.waitLast(t, want[len(want)-1])
	w.exit()
	if got := sink.lines(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// OpenCode without its HTTP server: a steer during a turn is sent at once
// as a second session/prompt, which OpenCode joins to the running loop.
// Both resolve together at the loop's end: one turn end, one idle.
func TestOpenCodeSteerJoinsTurnOverACP(t *testing.T) {
	a := NewOpenCode()
	w, sink, first := ocStarted(t, a)
	a.Deliver(proto.Input{RequestID: "steer-1", Text: "Before anything else, run `echo STEER`"})
	second, p := w.next("session/prompt")
	if str(p, "sessionId") != ocSession {
		t.Fatalf("params %v", p)
	}
	sink.wait(t, "accepted steer-1")
	w.resolve(first, ocResult)
	w.resolve(second, ocResult)
	checkLines(t, w, sink, "idle", "busy", "accepted prompt next_step receipt=false",
		"accepted steer-1 next_step receipt=false", "turn_end", "idle")
}

// Generic ACP keeps its queue: input during a turn is sent when the turn
// ends, and accepted only once its session/prompt is written.
func TestACPQueuesUntilTurnEnds(t *testing.T) {
	a := NewACP()
	w, sink, first := ocStarted(t, a)
	a.Deliver(proto.Input{RequestID: "later", Text: "x"})
	w.none()
	w.resolve(first, ocResult)
	second, _ := w.next("session/prompt")
	sink.wait(t, "accepted later next_turn receipt=false")
	w.resolve(second, ocResult)
	checkLines(t, w, sink, "idle", "busy", "accepted prompt next_turn receipt=false", "turn_end",
		"busy", "accepted later next_turn receipt=false", "turn_end", "idle")
}

// An ACP prompt whose write fails is failed, never accepted.
func TestACPPromptNotWrittenFails(t *testing.T) {
	a := NewACP()
	w, sink, first := ocStarted(t, a)
	w.resolve(first, ocResult)
	sink.waitLast(t, "idle")
	a.rpc.lw.close()
	a.Deliver(proto.Input{RequestID: "lost", Text: "x"})
	sink.wait(t, "failed lost: process stdin is closed")
	if sink.has("accepted lost") {
		t.Fatalf("%q", sink.lines())
	}
}

// fakeBus is OpenCode's HTTP server as the adapter uses it: prompt_async
// stores a user message, emit publishes an event (and stores the assistant
// message it reports), and GET /session/{id}/message and /session/status
// answer from what is stored.
type fakeBus struct {
	srv    *httptest.Server
	mu     sync.Mutex
	events chan string
	posted []map[string]any
	status int
	// hold, if set, is waited on before prompt_async answers; holdN[n]
	// instead, for the nth.
	hold  chan struct{}
	holdN map[int]chan struct{}
	// stored: the session's messages; loop: OpenCode runs one.
	stored []map[string]string
	loop   bool
	// drop ends the open event stream.
	drop chan struct{}
	gets int
	// statusFail: GET /session/status answers 500.
	statusFail bool
	// dropParts: posted keeps no text, so a test can measure the adapter's
	// heap alone.
	dropParts bool
}

func newFakeBus(t *testing.T) *fakeBus {
	b := &fakeBus{events: make(chan string, 64), status: http.StatusNoContent, drop: make(chan struct{}, 1)}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /event", func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		b.gets++
		b.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"server.connected\",\"properties\":{}}\n\n")
		w.(http.Flusher).Flush()
		for {
			select {
			case <-r.Context().Done():
				return
			case <-b.drop:
				return
			case e := <-b.events:
				fmt.Fprintf(w, "data: %s\n\n", e)
				w.(http.Flusher).Flush()
			}
		}
	})
	mux.HandleFunc("POST /session/{id}/prompt_async", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		body["session"] = r.PathValue("id")
		body["directory"] = r.Header.Get("x-opencode-directory")
		b.mu.Lock()
		b.posted = append(b.posted, body)
		if b.dropParts {
			delete(body, "parts")
		}
		st, hold := b.status, b.hold
		if h, ok := b.holdN[len(b.posted)-1]; ok {
			hold = h
		}
		if st/100 == 2 {
			// Stored, and joined to the running loop or starting one.
			id, _ := body["messageID"].(string)
			b.stored = append(b.stored, map[string]string{"id": id, "role": "user"})
			b.loop = true
		}
		b.mu.Unlock()
		if hold != nil {
			<-hold
		}
		w.WriteHeader(st)
	})
	mux.HandleFunc("GET /session/{id}/message", func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		var out []map[string]any
		for _, m := range b.stored {
			out = append(out, map[string]any{"info": map[string]string{"id": m["id"], "role": m["role"], "parentID": m["parentID"]}, "parts": []any{}})
		}
		b.mu.Unlock()
		_ = json.NewEncoder(w).Encode(out)
	})
	mux.HandleFunc("GET /session/status", func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		loop, fail := b.loop, b.statusFail
		b.mu.Unlock()
		if fail {
			http.Error(w, "down", http.StatusInternalServerError)
			return
		}
		if loop {
			fmt.Fprintf(w, `{"%s":{"type":"busy"}}`, ocSession)
			return
		}
		fmt.Fprint(w, `{}`)
	})
	b.srv = httptest.NewServer(mux)
	t.Cleanup(b.srv.Close)
	return b
}

func (b *fakeBus) port() int { return b.srv.Listener.Addr().(*net.TCPAddr).Port }

// answer stores a model step answering parent, as OpenCode does before it
// publishes it, under an id of OpenCode's own generator.
func (b *fakeBus) answer(parent string) string {
	b.mu.Lock()
	id := fmt.Sprintf("msg_%012x%014d", time.Now().UnixMilli()*0x1000&(1<<48-1), len(b.stored))
	b.stored = append(b.stored, map[string]string{"id": id, "role": "assistant", "parentID": parent})
	b.mu.Unlock()
	return assistant(parent)
}

func (b *fakeBus) setLoop(on bool) {
	b.mu.Lock()
	b.loop = on
	b.mu.Unlock()
}

// setStatus is the status prompt_async answers with.
func (b *fakeBus) setStatus(code int) {
	b.mu.Lock()
	b.status = code
	b.mu.Unlock()
}

func (b *fakeBus) setStatusFail(fail bool) {
	b.mu.Lock()
	b.statusFail = fail
	b.mu.Unlock()
}

// holdPosts makes every prompt_async wait for the returned channel to close.
func (b *fakeBus) holdPosts() chan struct{} {
	hold := make(chan struct{})
	b.mu.Lock()
	b.hold = hold
	b.mu.Unlock()
	return hold
}

// storeUser stores a user message lux did not send.
func (b *fakeBus) storeUser(id string) {
	b.mu.Lock()
	b.stored = append(b.stored, map[string]string{"id": id, "role": "user"})
	b.mu.Unlock()
}

// postedID is the messageID of the nth prompt_async, once it came.
func (b *fakeBus) postedID(t *testing.T, n int) string {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
		b.mu.Lock()
		if len(b.posted) > n {
			id, _ := b.posted[n]["messageID"].(string)
			b.mu.Unlock()
			return id
		}
		b.mu.Unlock()
	}
	t.Fatalf("no prompt_async #%d", n+1)
	return ""
}

// assistant is a model step answering the user message parent, as
// opencode 1.18.31 reports it (opencode-acp-legacy-1).
func assistant(parent string) string {
	return `{"id":"evt_0f29ddf72001","type":"message.updated","properties":{"sessionID":"` + ocSession + `","info":{"id":"msg_0f275509d001A2KKdHyv8k2LwM","role":"assistant","parentID":"` + parent + `","sessionID":"` + ocSession + `","time":{"created":1790774169757}}}}`
}

// onBus hands the adapter one bus event and returns once it has handled it.
func onBus(t *testing.T, a *ACP, ev string) {
	t.Helper()
	var e busEvent
	if err := json.Unmarshal([]byte(ev), &e); err != nil {
		t.Fatal(err)
	}
	a.onBus(e)
}

const ocIdle = `{"type":"session.idle","properties":{"sessionID":"` + ocSession + `"}}`

func ocWithBus(t *testing.T) (*ACP, *fakeBus, *agentWire, *inputSink, string) {
	t.Helper()
	return ocWithBusClock(t, nil)
}

// ocWithBusClock is ocWithBus with settle on clk (a wall clock if nil).
func ocWithBusClock(t *testing.T, clk *testClock) (*ACP, *fakeBus, *agentWire, *inputSink, string) {
	t.Helper()
	b := newFakeBus(t)
	a := NewOpenCode()
	a.bus = newOpencodeBus(b.port(), "/workspace")
	a.settleEvery = 20 * time.Millisecond
	if clk != nil {
		a.clock, a.settleEvery = clk, time.Second
	}
	b.setLoop(true) // the first prompt's loop
	w, sink, first := ocStarted(t, a)
	for end := time.Now().Add(5 * time.Second); !a.bus.isConnected(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatal("bus never connected")
		}
	}
	return a, b, w, sink, first
}

// noConsumed fails if anything was reported read.
func noConsumed(t *testing.T, sink *inputSink, when string) {
	t.Helper()
	if sink.has("consumed") {
		t.Fatalf("%s: %q", when, sink.lines())
	}
}

// With OpenCode's HTTP server: a steer goes through prompt_async under a
// client message id, and is consumed at the first model step answering
// that message, not before; two steers of the same text are told apart by
// their ids.
func TestOpenCodeSteerReceiptFromBus(t *testing.T) {
	a, b, w, sink, first := ocWithBus(t)
	a.Deliver(proto.Input{RequestID: "steer-2", Text: "Before anything else, run `echo STEER`"})
	sink.wait(t, "accepted steer-2 next_step receipt=true")
	w.none() // no second ACP prompt
	b.mu.Lock()
	post := b.posted[0]
	b.mu.Unlock()
	msgID, _ := post["messageID"].(string)
	parts, _ := json.Marshal(post["parts"])
	if !strings.HasPrefix(msgID, "msg_") || len(msgID) != 30 || post["session"] != ocSession || post["directory"] != "/workspace" ||
		string(parts) != `[{"text":"Before anything else, run `+"`echo STEER`"+`","type":"text"}]` {
		t.Fatalf("prompt_async %v", post)
	}
	a.Deliver(proto.Input{RequestID: "steer-3", Text: "Before anything else, run `echo STEER`"})
	sink.wait(t, "accepted steer-3 next_step receipt=true")
	msgID3 := b.postedID(t, 1)
	// The running step (the sleep) still answers the first prompt: neither
	// steer is read.
	onBus(t, a, b.answer("msg_0f274ed8a001VXpumPDla0AnsH"))
	noConsumed(t, sink, "an unrelated step")
	onBus(t, a, b.answer(msgID))
	if l := sink.lines(); l[len(l)-1] != "consumed steer-2" || slices.Contains(l, "consumed steer-3") {
		t.Fatalf("step answering steer-2: %q", l)
	}
	onBus(t, a, b.answer(msgID))
	onBus(t, a, b.answer(msgID3))
	b.setLoop(false)
	w.resolve(first, ocResult)
	checkLines(t, w, sink, "idle", "busy", "accepted prompt next_step receipt=false",
		"accepted steer-2 next_step receipt=true", "accepted steer-3 next_step receipt=true",
		"consumed steer-2", "consumed steer-3", "turn_end", "idle")
}

// Two steers stored during one tool call are both in the context of the
// model step after it, which names only the newer as its parent: both are
// read, and the turn ends once.
func TestOpenCodeOneStepReadsSeveralSteers(t *testing.T) {
	a, b, w, sink, first := ocWithBus(t)
	a.Deliver(proto.Input{RequestID: "s1", Text: "x"})
	a.Deliver(proto.Input{RequestID: "s2", Text: "y"})
	sink.wait(t, "accepted s2")
	b.events <- b.answer(b.postedID(t, 1))
	sink.wait(t, "consumed s2")
	b.setLoop(false)
	w.resolve(first, ocResult)
	checkLines(t, w, sink, "idle", "busy", "accepted prompt next_step receipt=false",
		"accepted s1 next_step receipt=true", "accepted s2 next_step receipt=true",
		"consumed s1", "consumed s2", "turn_end", "idle")
}

// Interrupted with steers unread: OpenCode's cancelled loop never reads
// them, so they are sent again (same request id, new message id) and start
// the next turn, followed on the bus; consumed once, nothing fails. A step
// answering the cancelled copy is not the steer read.
func TestOpenCodeSteerCarriedPastInterrupt(t *testing.T) {
	a, b, w, sink, first := ocWithBus(t)
	a.Deliver(proto.Input{RequestID: "s1", Text: "x"})
	sink.wait(t, "accepted s1")
	firstID := b.postedID(t, 0)
	a.Deliver(proto.Input{RequestID: "int-1", Interrupt: true})
	w.next("session/cancel")
	sink.wait(t, "accepted int-1")
	b.setLoop(false)
	w.resolve(first, ocCancelled)
	again := b.postedID(t, 1)
	if again == firstID {
		t.Fatalf("sent again under the same id %q", again)
	}
	w.none()                       // no empty prompt for the interrupt
	onBus(t, a, b.answer(firstID)) // the cancelled copy: not ours any more
	noConsumed(t, sink, "a step answering the cancelled copy")
	b.events <- b.answer(again)
	sink.wait(t, "consumed s1")
	b.setLoop(false)
	b.events <- ocIdle
	checkCarried(t, w, sink, "s1")
}

// A steer whose prompt_async is still in flight when the interrupted turn
// ends is not lost: the turn waits for it, and once accepted it is sent
// again, as a steer the turn left unread.
func TestOpenCodeReservedSteerCarriedPastInterrupt(t *testing.T) {
	a, b, w, sink, first := ocWithBus(t)
	hold := b.holdPosts()
	a.Deliver(proto.Input{RequestID: "s1", Text: "x"})
	b.postedID(t, 0)
	a.Deliver(proto.Input{RequestID: "int-1", Interrupt: true})
	w.next("session/cancel")
	b.setLoop(false)
	w.resolve(first, ocCancelled)
	waitHeld(t, a)
	b.mu.Lock()
	b.hold = nil
	b.mu.Unlock()
	close(hold)
	again := b.postedID(t, 1)
	b.events <- b.answer(again)
	sink.wait(t, "consumed s1")
	b.setLoop(false)
	b.events <- ocIdle
	checkLines(t, w, sink, "idle", "busy", "accepted prompt next_step receipt=false", "accepted int-1 next_turn receipt=false",
		"accepted s1 next_step receipt=true", "turn_end", "consumed s1", "turn_end", "idle")
}

// A step answering a message lux did not send reads no steer, however the
// two generators' ids sort: OpenCode's per-millisecond counter can be ahead
// of lux's in the same millisecond, so its original prompt sorts after a
// steer sent at once.
func TestOpenCodeUntrackedParentReadsNothing(t *testing.T) {
	b := newOpencodeBus(1, "/")
	steer, prompt := "msg_0f29d9777001AAAAAAAAAAAAAA", "msg_0f29d9777002AAAAAAAAAAAAAA"
	b.track(steer, "s1", 0)
	if got := b.answered(prompt); len(got) != 0 {
		t.Fatalf("original prompt parent %s receipts later steer %s: %q", prompt, steer, got)
	}
	if got := b.answered(steer); !slices.Equal(got, []string{"s1"}) {
		t.Fatalf("the steer's own step: %q", got)
	}
}

// A resumed session whose stored messages carry ids ahead of this clock
// (written on a host whose clock ran ahead): a steer still sorts after
// them, and a step answering one of them reads no steer.
func TestOpenCodeSteerSortsAfterStoredMessages(t *testing.T) {
	a, b, w, sink, first := ocWithBus(t)
	ahead := fmt.Sprintf("msg_%012x%s", (time.Now().Add(time.Hour).UnixMilli()*0x1000+7)&(1<<48-1), "BBBBBBBBBBBBBB")
	b.storeUser(ahead)
	a.Deliver(proto.Input{RequestID: "s1", Text: "x"})
	sink.wait(t, "accepted s1")
	steer := b.postedID(t, 0)
	if steer <= ahead {
		t.Fatalf("steer %s sorts before the stored %s", steer, ahead)
	}
	onBus(t, a, b.answer(ahead))
	noConsumed(t, sink, "a step answering the stored message")
	onBus(t, a, b.answer(steer))
	b.setLoop(false)
	w.resolve(first, ocResult)
	checkLines(t, w, sink, "idle", "busy", "accepted prompt next_step receipt=false",
		"accepted s1 next_step receipt=true", "consumed s1", "turn_end", "idle")
}

// Message ids sort after each other, as OpenCode orders messages by id,
// within a millisecond, past its 4,096 counter values, and when the clock
// steps back.
func TestOpenCodeMessageIDsAscend(t *testing.T) {
	b := newOpencodeBus(1, "/")
	now := time.UnixMilli(1790776809335)
	prev := ""
	for i := range 5 {
		id := b.messageID(now.Add(time.Duration(i/2) * time.Millisecond))
		if id <= prev || !strings.HasPrefix(id, "msg_0f29d977") {
			t.Fatalf("%s after %s", id, prev)
		}
		prev = id
	}
	for i := range 5001 {
		at := now.Add(time.Millisecond)
		switch {
		case i == 5000:
			at = now.Add(2 * time.Millisecond)
		case i%1000 == 999:
			at = now.Add(-time.Second)
		}
		id := b.messageID(at)
		if id <= prev {
			t.Fatalf("#%d: %s after %s", i, id, prev)
		}
		prev = id
	}
}

// A steer that reaches OpenCode as its loop ends starts a loop of its own:
// the Run stays busy until the bus says that loop ended, and no turn end is
// reported while it runs; then one for each loop.
func TestOpenCodeLateSteerKeepsRunBusy(t *testing.T) {
	a, b, w, sink, first := ocWithBus(t)
	a.Deliver(proto.Input{RequestID: "late", Text: "x"})
	sink.wait(t, "accepted late")
	msgID := b.postedID(t, 0)
	w.resolve(first, ocResult)
	waitBusTurn(t, a)
	b.events <- b.answer(msgID)
	sink.wait(t, "consumed late")
	noTurnEnd(t, sink, "while the steer's loop runs")
	b.setLoop(false)
	b.events <- ocIdle
	checkLines(t, w, sink, "idle", "busy", "accepted prompt next_step receipt=false",
		"accepted late next_step receipt=true", "consumed late", "turn_end", "turn_end", "idle")
}

// waitBusTurn waits until the adapter has handled the ACP turn's result.
func waitBusTurn(t *testing.T, a *ACP) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		a.mu.Lock()
		bt := a.busTurn
		a.mu.Unlock()
		if bt {
			return
		}
		if time.Now().After(end) {
			t.Fatal("the ACP result was never handled")
		}
	}
}

// prompt_async answering only after the ACP turn has ended: the steer was
// reserved against that turn, so the Run stays busy for the loop it
// started, whose end is found by asking OpenCode even though no
// session.idle arrives. No turn end is reported while that loop runs.
func TestOpenCodeLateHTTPAcceptance(t *testing.T) {
	a, b, w, sink, first := ocWithBus(t)
	hold := b.holdPosts()
	a.Deliver(proto.Input{RequestID: "late", Text: "x"})
	msgID := b.postedID(t, 0)
	w.resolve(first, ocResult)
	waitHeld(t, a)
	close(hold)
	sink.wait(t, "accepted late")
	b.events <- b.answer(msgID)
	sink.wait(t, "consumed late")
	noTurnEnd(t, sink, "while the steer's loop runs")
	b.setLoop(false) // and its session.idle is lost
	checkLines(t, w, sink, "idle", "busy", "accepted prompt next_step receipt=false",
		"accepted late next_step receipt=true", "consumed late", "turn_end", "turn_end", "idle")
}

// A steer read by the running loop whose prompt_async answers only after
// the ACP result: that loop is the only one, so the turn ends once, after
// the steer's acceptance.
func TestOpenCodeReservedAlreadyReadEndsOnce(t *testing.T) {
	a, b, w, sink, first := ocWithBus(t)
	hold := b.holdPosts()
	a.Deliver(proto.Input{RequestID: "late", Text: "x"})
	msgID := b.postedID(t, 0)
	onBus(t, a, b.answer(msgID))
	b.setLoop(false)
	w.resolve(first, ocResult)
	waitHeld(t, a)
	close(hold)
	checkLines(t, w, sink, "idle", "busy", "accepted prompt next_step receipt=false",
		"accepted late next_step receipt=true", "consumed late", "turn_end", "idle")
}

// The original loop reads a steer whose prompt_async is still in flight and
// ends; the step answering it reaches lux only after the ACP result, and
// before the acceptance. Seeing it late is not another loop: that steer's
// admission had not completed after the ACP result. One turn end.
func TestOpenCodeHeldOriginalAnswerSeenLateEndsOnce(t *testing.T) {
	a, b, w, sink, first := ocWithBus(t)
	hold := b.holdPosts()
	a.Deliver(proto.Input{RequestID: "late", Text: "x"})
	msg := b.postedID(t, 0)
	b.setLoop(false)
	w.resolve(first, ocResult)
	waitHeld(t, a)
	onBus(t, a, b.answer(msg))
	close(hold)
	checkLines(t, w, sink, "idle", "busy", "accepted prompt next_step receipt=false",
		"accepted late next_step receipt=true", "consumed late", "turn_end", "idle")
}

// The complement of the test above: the original loop ends without reading
// the steer, OpenCode stores it and runs a new loop that answers it and
// ends, all before prompt_async's response reaches lux. lux cannot tell
// this from the original loop's step seen late, so it reports one turn end
// (one transcript turn holding two loops), and only once OpenCode is idle.
func TestOpenCodeLoopAnsweredBeforeHTTPResponseEndsOnceAfterIdle(t *testing.T) {
	a, b, w, sink, first := ocWithBus(t)
	hold := b.holdPosts()
	a.Deliver(proto.Input{RequestID: "late", Text: "x"})
	msg := b.postedID(t, 0)
	b.setLoop(false)
	w.resolve(first, ocResult)
	waitHeld(t, a)
	b.setLoop(true) // the steer's own loop
	onBus(t, a, b.answer(msg))
	noTurnEnd(t, sink, "while the second loop runs")
	b.setLoop(false)
	close(hold)
	checkLines(t, w, sink, "idle", "busy", "accepted prompt next_step receipt=false",
		"accepted late next_step receipt=true", "consumed late", "turn_end", "idle")
}

// noTurnEnd fails if a turn end was reported.
func noTurnEnd(t *testing.T, sink *inputSink, when string) {
	t.Helper()
	if slices.Contains(sink.lines(), "turn_end") {
		t.Fatalf("turn end %s: %q", when, sink.lines())
	}
}

// waitHeld waits until the adapter has handled the ACP result of a turn
// whose steers' prompt_async are still in flight.
func waitHeld(t *testing.T, a *ACP) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		a.mu.Lock()
		held := a.busTurn && a.reserved > 0
		a.mu.Unlock()
		if held {
			return
		}
		if time.Now().After(end) {
			t.Fatal("the ACP result was never handled")
		}
	}
}

// The event stream drops across the step that answers the steer: on
// reconnect, and when the ACP turn ends, lux reads OpenCode's stored
// messages, so the steer is still reported read and the Run goes idle.
func TestOpenCodeReceiptAcrossReconnect(t *testing.T) {
	a, b, w, sink, first := ocWithBus(t)
	a.Deliver(proto.Input{RequestID: "s", Text: "x"})
	sink.wait(t, "accepted s")
	b.drop <- struct{}{}
	b.answer(b.postedID(t, 0)) // stored, never published
	b.setLoop(false)
	w.resolve(first, ocResult)
	checkLines(t, w, sink, "idle", "busy", "accepted prompt next_step receipt=false",
		"accepted s next_step receipt=true", "consumed s", "turn_end", "idle")
}

// prompt_async refusing the steer (it never reached OpenCode): it goes as
// a second ACP prompt instead.
func TestOpenCodeFallsBackToACP(t *testing.T) {
	a, b, w, sink, first := ocWithBus(t)
	b.setStatus(http.StatusBadRequest)
	a.Deliver(proto.Input{RequestID: "s", Text: "x"})
	second, _ := w.next("session/prompt")
	sink.wait(t, "accepted s next_step receipt=false")
	w.resolve(first, ocResult)
	w.resolve(second, ocResult)
	sink.waitLast(t, "idle")
}

// A /event request OpenCode takes while starting and never answers
// (1.18.31 does this) is retried, so the stream still connects.
func TestOpenCodeBusRetriesUnansweredStream(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			<-r.Context().Done() // no headers, ever
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"server.connected\",\"properties\":{}}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()
	b := newOpencodeBus(srv.Listener.Addr().(*net.TCPAddr).Port, "/")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.follow(ctx, func(busEvent) {}, nil)
	if !b.waitConnected(context.Background(), 8*time.Second) {
		t.Fatalf("never connected: %v", b.err())
	}
}

// The command gets a loopback --port only when lux builds it.
func TestOpenCodeCommand(t *testing.T) {
	a := NewOpenCode()
	argv, _ := a.Command(proto.ShimConfig{Command: []string{"opencode", "acp"}, Workdir: "/w"})
	if len(argv) != 6 || argv[2] != "--port" || argv[4] != "--hostname" || argv[5] != "127.0.0.1" || a.bus == nil || a.bus.dir != "/w" {
		t.Fatalf("argv %q", argv)
	}
	for _, cmd := range [][]string{{"opencode", "acp", "--port", "5000"}, {"my-agent"}} {
		b := NewOpenCode()
		if argv, _ := b.Command(proto.ShimConfig{Command: cmd}); strings.Join(argv, " ") != strings.Join(cmd, " ") || b.bus != nil {
			t.Fatalf("%q -> %q", cmd, argv)
		}
	}
	if argv, _ := NewACP().Command(proto.ShimConfig{Command: []string{"opencode", "acp"}}); len(argv) != 2 {
		t.Fatalf("generic acp %q", argv)
	}
}

// A server that answers GET /event and closes the stream at once is not
// asked again in a hot loop: every end of the stream is followed by a
// backoff.
func TestOpenCodeBusBacksOffAfterCleanEOF(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	b := newOpencodeBus(srv.Listener.Addr().(*net.TCPAddr).Port, "/")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); b.follow(ctx, func(busEvent) {}, nil) }()
	time.Sleep(200 * time.Millisecond)
	cancel()
	<-done
	mu.Lock()
	defer mu.Unlock()
	// 100 ms, then 200 ms, each at least half that: at most 4 requests.
	if calls > 4 {
		t.Fatalf("%d GET /event in 200 ms", calls)
	}
}

// heldSink holds the adapter's report of a turn end until release closes;
// nothing the adapter cancels can end that wait.
type heldSink struct {
	*inputSink
	entered, release, finished chan struct{}
}

func (s *heldSink) Event(typ string, v any) {
	if typ == "acp.turn_end" {
		close(s.entered)
		<-s.release
		defer close(s.finished)
	}
	s.inputSink.Event(typ, v)
}

// Run returns only after the adapter's own goroutines have: a prompt
// waiter still reporting its turn's end when the agent exits holds Run
// until it is done.
func TestOpenCodeRunJoinsItsWorkers(t *testing.T) {
	a := NewOpenCode()
	sink := &heldSink{inputSink: &inputSink{}, entered: make(chan struct{}), release: make(chan struct{}), finished: make(chan struct{})}
	w := startWireSink(t, a, proto.ShimConfig{Prompt: "p"}, sink)
	id, _ := w.next("initialize")
	w.send(`{"jsonrpc":"2.0","id":` + id + `,"result":{"protocolVersion":1}}`)
	id, _ = w.next("session/new")
	w.send(`{"jsonrpc":"2.0","id":` + id + `,"result":{"sessionId":"` + ocSession + `"}}`)
	first, _ := w.next("session/prompt")
	w.resolve(first, ocResult)
	<-sink.entered
	w.out.Close()
	// Run's shutdown has begun once its context is cancelled.
	for end := time.Now().Add(5 * time.Second); a.runCtx().Err() == nil; time.Sleep(time.Millisecond) {
		if time.Now().After(end) {
			t.Fatal("Run never began to shut down")
		}
	}
	select {
	case <-w.done:
		t.Fatal("Run returned while the prompt waiter was still reporting the turn end")
	case <-time.After(300 * time.Millisecond):
	}
	close(sink.release)
	<-w.done
	select {
	case <-sink.finished:
	default:
		t.Fatal("Run returned before the prompt waiter finished")
	}
}

// An OpenCode adapter leaves nothing running once its Run has returned:
// its steering worker, bus follower and prompt waiters end with it.
func TestOpenCodeRunLeavesNoGoroutines(t *testing.T) {
	b := newFakeBus(t)
	// stable is the goroutine count once it has stopped falling.
	stable := func() int {
		n, same := runtime.NumGoroutine(), 0
		for same < 5 {
			time.Sleep(20 * time.Millisecond)
			if m := runtime.NumGoroutine(); m < n {
				n, same = m, 0
			} else {
				same++
			}
		}
		return n
	}
	// The fake server's own connections come and go: count from a warm
	// start, then look for growth per lifecycle.
	lifecycle := func() {
		a := NewOpenCode()
		a.bus = newOpencodeBus(b.port(), "/workspace")
		b.setLoop(true)
		w, sink, first := ocStarted(t, a)
		for !a.bus.isConnected() {
			time.Sleep(5 * time.Millisecond)
		}
		a.Deliver(proto.Input{RequestID: "s", Text: "x"})
		sink.wait(t, "accepted s")
		b.setLoop(false)
		w.resolve(first, ocResult)
		sink.wait(t, "turn_end")
		_ = a.Stop()
		w.exit()
		w.out.Close()
	}
	lifecycle()
	b.srv.CloseClientConnections()
	base := stable()
	for range 10 {
		lifecycle()
	}
	b.srv.CloseClientConnections()
	n := stable()
	if n > base {
		buf := make([]byte, 1<<20)
		t.Fatalf("%d goroutines after 10 lifecycles, %d before\n%s", n, base, buf[:runtime.Stack(buf, true)])
	}
}

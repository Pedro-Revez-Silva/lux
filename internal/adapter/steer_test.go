package adapter

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/marcioapm/lux/internal/proto"
)

// inputSink records what an adapter reports about inputs, activity and
// turn ends, in order.
type inputSink struct {
	mu  sync.Mutex
	log []string
}

func (s *inputSink) add(l string) {
	s.mu.Lock()
	s.log = append(s.log, l)
	s.mu.Unlock()
}
func (s *inputSink) lines() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string{}, s.log...)
}
func (s *inputSink) Stdout([]byte)                                   {}
func (s *inputSink) EndMessage()                                     {}
func (s *inputSink) Stderr([]byte)                                   {}
func (s *inputSink) Session(string)                                  {}
func (s *inputSink) Stream(string, string, string, func(string) any) {}
func (s *inputSink) Event(typ string, _ any) {
	if strings.HasSuffix(typ, "turn_end") {
		s.add("turn_end")
	}
}
func (s *inputSink) Activity(idle bool) {
	if idle {
		s.add("idle")
	} else {
		s.add("busy")
	}
}
func (s *inputSink) InputAccepted(in proto.Input, d Delivery) {
	s.add(fmt.Sprintf("accepted %s %s receipt=%v", in.RequestID, d.Lands, d.Receipt))
}
func (s *inputSink) InputConsumed(id string) { s.add("consumed " + id) }
func (s *inputSink) InputFailed(in proto.Input, err error) {
	s.add(fmt.Sprintf("failed %s: %v", in.RequestID, err))
}

// checkCarried checks a steer carried past an interrupt: accepted in the
// interrupted turn, consumed once after that turn's end and before the
// next one's, never failed, and the Run idle at the end.
func checkCarried(t *testing.T, w *agentWire, sink *inputSink, id string) {
	t.Helper()
	sink.waitLast(t, "idle")
	w.exit()
	l := sink.lines()
	at := func(want string, from int) int {
		for i := from; i < len(l); i++ {
			if strings.HasPrefix(l[i], want) {
				return i
			}
		}
		return -1
	}
	acc := at("accepted "+id+" ", 0)
	end1 := at("turn_end", 0)
	cons := at("consumed "+id, 0)
	end2 := at("turn_end", end1+1)
	n := 0
	for _, x := range l {
		if x == "consumed "+id {
			n++
		}
		if strings.HasPrefix(x, "failed") {
			t.Fatalf("failed: %q", l)
		}
	}
	if acc < 0 || acc > end1 || cons < end1 || end2 < cons || n != 1 || l[len(l)-1] != "idle" {
		t.Fatalf("carried %s: %q", id, l)
	}
}

// has reports whether the sink has a line starting with prefix.
func (s *inputSink) has(prefix string) bool {
	return slices.ContainsFunc(s.lines(), func(l string) bool { return strings.HasPrefix(l, prefix) })
}

// wait polls until the sink has a line with want, or fails.
func (s *inputSink) wait(t *testing.T, want string) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
		if s.has(want) {
			return
		}
	}
	t.Fatalf("no %q in %q", want, s.lines())
}

// waitLast polls until the sink's last line is want, after a turn end.
func (s *inputSink) waitLast(t *testing.T, want string) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
		l := s.lines()
		if len(l) > 1 && l[len(l)-1] == want && slices.Contains(l, "turn_end") {
			return
		}
	}
	t.Fatalf("last line is not %q: %q", want, s.lines())
}

// agentWire is the agent's end of an adapter's stdio: the test reads what
// the adapter sends and writes what the agent answers.
type agentWire struct {
	t    *testing.T
	in   *bufio.Scanner
	out  io.WriteCloser
	sent chan map[string]json.RawMessage
	// done closes when the adapter's Run has returned.
	done chan struct{}
}

func startWire(t *testing.T, ad Adapter, cfg proto.ShimConfig) (*agentWire, *inputSink) {
	t.Helper()
	sink := &inputSink{}
	return startWireSink(t, ad, cfg, sink), sink
}

// startWireSink is startWire reporting to sink.
func startWireSink(t *testing.T, ad Adapter, cfg proto.ShimConfig, sink Sink) *agentWire {
	t.Helper()
	toAgent, fromAdapter := io.Pipe()
	fromAgent, toAdapter := io.Pipe()
	w := &agentWire{t: t, in: bufio.NewScanner(toAgent), out: toAdapter, sent: make(chan map[string]json.RawMessage, 64), done: make(chan struct{})}
	go func() {
		for w.in.Scan() {
			var m map[string]json.RawMessage
			_ = json.Unmarshal(w.in.Bytes(), &m)
			w.sent <- m
		}
	}()
	p := &Process{Cmd: &exec.Cmd{}, Stdin: fromAdapter, Stdout: fromAgent, Stderr: io.NopCloser(strings.NewReader(""))}
	go func() {
		defer close(w.done)
		_ = ad.Run(context.Background(), p, cfg, sink)
	}()
	t.Cleanup(func() { toAdapter.Close(); toAgent.Close() })
	return w
}

// exit closes the agent's stdout, as its process exiting does, and waits
// for the adapter's Run to return.
func (w *agentWire) exit() {
	w.t.Helper()
	w.out.Close()
	select {
	case <-w.done:
	case <-time.After(5 * time.Second):
		w.t.Fatal("Run did not return after the agent exited")
	}
}

// next returns the next message the adapter sent; method must match.
func (w *agentWire) next(method string) (id string, params map[string]json.RawMessage) {
	w.t.Helper()
	select {
	case m := <-w.sent:
		var got string
		_ = json.Unmarshal(m["method"], &got)
		if got != method {
			w.t.Fatalf("adapter sent %s, want %s", got, method)
		}
		_ = json.Unmarshal(m["params"], &params)
		return string(m["id"]), params
	case <-time.After(5 * time.Second):
		w.t.Fatalf("adapter sent no %s", method)
	}
	return "", nil
}

func waitTimeout() <-chan time.Time { return time.After(5 * time.Second) }

func (w *agentWire) none() {
	w.t.Helper()
	select {
	case m := <-w.sent:
		w.t.Fatalf("adapter sent %s", m["method"])
	case <-time.After(100 * time.Millisecond):
	}
}

func (w *agentWire) send(line string) {
	w.t.Helper()
	if _, err := io.WriteString(w.out, line+"\n"); err != nil {
		w.t.Fatal(err)
	}
}

func str(m map[string]json.RawMessage, k string) string {
	var s string
	_ = json.Unmarshal(m[k], &s)
	return s
}

const (
	cxThread = "01a0f274-d88c-71b0-942c-1bfdf6e19884"
	cxTurn   = "01a0f274-d9bc-7a52-a1a0-0854785aabb4"
)

// codexStarted drives a Codex adapter through initialize (reporting
// userAgent), thread/start and the prompt's turn/start, as recorded from
// codex 0.155.1 (codex-appserver-1).
func codexStarted(t *testing.T, userAgent string) (*Codex, *agentWire, *inputSink) {
	t.Helper()
	c := NewCodex()
	w, sink := startWire(t, c, proto.ShimConfig{Prompt: "Run `sleep 20 && echo FIRST`"})
	id, _ := w.next("initialize")
	w.send(`{"id":` + id + `,"result":{"userAgent":"` + userAgent + ` (Mac OS 26.5.1; arm64) unknown (lux; 1)","codexHome":"/h","platformFamily":"unix","platformOs":"macos"}}`)
	id, _ = w.next("thread/start")
	w.send(`{"id":` + id + `,"result":{"thread":{"id":"` + cxThread + `","status":{"type":"idle"}}}}`)
	id, p := w.next("turn/start")
	if str(p, "clientUserMessageId") != "prompt" {
		t.Fatalf("turn/start without the prompt's id: %s", p["clientUserMessageId"])
	}
	w.send(`{"id":` + id + `,"result":{"turn":{"id":"` + cxTurn + `","items":[],"itemsView":"notLoaded","status":"inProgress","error":null}}}`)
	w.send(`{"method":"turn/started","params":{"threadId":"` + cxThread + `","turn":{"id":"` + cxTurn + `","status":"inProgress"}}}`)
	w.send(`{"method":"item/started","params":{"item":{"type":"userMessage","id":"01a0f274-da9b","clientId":"prompt","content":[{"type":"text","text":"Run","text_elements":[]}]},"threadId":"` + cxThread + `","turnId":"` + cxTurn + `"}}`)
	sink.wait(t, "consumed prompt")
	return c, w, sink
}

func cxCompleted(status string) string {
	return `{"method":"turn/completed","params":{"threadId":"` + cxThread + `","turn":{"id":"` + cxTurn + `","status":"` + status + `"}}}`
}

// A steer on Codex 0.155: accepted when turn/steer returns, consumed when
// its userMessage item (clientId = the request id) starts, one turn.
func TestCodexSteerReceipt(t *testing.T) {
	c, w, sink := codexStarted(t, "lux/0.155.1")
	c.Deliver(proto.Input{RequestID: "steer-352FCA", Text: "Before anything else, run `echo STEER`"})
	id, p := w.next("turn/steer")
	if str(p, "expectedTurnId") != cxTurn || str(p, "clientUserMessageId") != "steer-352FCA" {
		t.Fatalf("turn/steer params %v", p)
	}
	w.send(`{"id":` + id + `,"result":{"turnId":"` + cxTurn + `"}}`)
	sink.wait(t, "accepted steer-352FCA")
	w.send(`{"method":"item/completed","params":{"item":{"type":"commandExecution","id":"call_1","command":"/bin/zsh -lc 'sleep 20 && echo FIRST'","status":"completed","aggregatedOutput":"FIRST\n"},"threadId":"` + cxThread + `","turnId":"` + cxTurn + `"}}`)
	w.send(`{"method":"item/started","params":{"item":{"type":"userMessage","id":"01a0f275-520f","clientId":"steer-352FCA","content":[{"type":"text","text":"Before anything else","text_elements":[]}]},"threadId":"` + cxThread + `","turnId":"` + cxTurn + `","startedAtMs":1790774170127},"emittedAtMs":1790774170128}`)
	// The item repeats as completed; one consumed only.
	w.send(`{"method":"item/completed","params":{"item":{"type":"userMessage","id":"01a0f275-520f","clientId":"steer-352FCA","content":[]},"threadId":"` + cxThread + `","turnId":"` + cxTurn + `"}}`)
	w.send(`{"method":"item/started","params":{"item":{"type":"userMessage","id":"01a0f275-520f","clientId":"steer-352FCA","content":[]},"threadId":"` + cxThread + `","turnId":"` + cxTurn + `"}}`)
	w.send(cxCompleted("completed"))
	sink.waitLast(t, "idle")
	want := []string{"busy", "accepted prompt next_step receipt=true", "consumed prompt",
		"accepted steer-352FCA next_step receipt=true", "consumed steer-352FCA", "turn_end", "idle"}
	if got := sink.lines()[1:]; strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// Codex 0.144 emits the userMessage item on admission, mid-tool: there it
// is no receipt, so none is promised and none is reported.
func TestCodexOldVersionHasNoReceipt(t *testing.T) {
	for ua, want := range map[string]bool{"lux/0.144.1": false, "lux/0.155.0": true, "lux/0.159.2": true, "lux/1.0.0": true, "weird": false} {
		if codexReceipt(ua+" (x)") != want {
			t.Errorf("%s: receipt %v", ua, !want)
		}
	}
	c := NewCodex()
	w, sink := startWire(t, c, proto.ShimConfig{Prompt: "p"})
	id, _ := w.next("initialize")
	w.send(`{"id":` + id + `,"result":{"userAgent":"lux/0.144.1 (Mac OS 26.5.1; arm64) unknown (lux; 1)"}}`)
	id, _ = w.next("thread/start")
	w.send(`{"id":` + id + `,"result":{"thread":{"id":"` + cxThread + `"}}}`)
	id, _ = w.next("turn/start")
	w.send(`{"id":` + id + `,"result":{"turn":{"id":"` + cxTurn + `","status":"inProgress"}}}`)
	sink.wait(t, "accepted prompt")
	c.Deliver(proto.Input{RequestID: "steer-4B54AB", Text: "s"})
	id, _ = w.next("turn/steer")
	w.send(`{"id":` + id + `,"result":{"turnId":"` + cxTurn + `"}}`)
	sink.wait(t, "accepted steer-4B54AB")
	w.send(`{"method":"item/started","params":{"item":{"type":"userMessage","id":"01a0f28d-960d","clientId":"steer-4B54AB","content":[]},"threadId":"` + cxThread + `","turnId":"` + cxTurn + `"}}`)
	w.send(cxCompleted("completed"))
	sink.waitLast(t, "idle")
	w.exit()
	for _, l := range sink.lines() {
		if strings.HasPrefix(l, "consumed") || strings.HasPrefix(l, "failed") || strings.Contains(l, "receipt=true") {
			t.Fatalf("0.144: %q", sink.lines())
		}
	}
}

// Input that arrives while turn/start is in flight is steered into that
// turn as soon as its id is known, not held for the next turn.
func TestCodexInputWhileStartingIsSteered(t *testing.T) {
	c := NewCodex()
	w, sink := startWire(t, c, proto.ShimConfig{Prompt: "p"})
	id, _ := w.next("initialize")
	w.send(`{"id":` + id + `,"result":{"userAgent":"lux/0.155.1"}}`)
	id, _ = w.next("thread/start")
	w.send(`{"id":` + id + `,"result":{"thread":{"id":"` + cxThread + `"}}}`)
	startID, _ := w.next("turn/start")
	c.Deliver(proto.Input{RequestID: "early", Text: "e"})
	w.none()
	w.send(`{"id":` + startID + `,"result":{"turn":{"id":"` + cxTurn + `","status":"inProgress"}}}`)
	id, p := w.next("turn/steer")
	if str(p, "expectedTurnId") != cxTurn || str(p, "clientUserMessageId") != "early" {
		t.Fatalf("turn/steer params %v", p)
	}
	w.send(`{"id":` + id + `,"result":{"turnId":"` + cxTurn + `"}}`)
	sink.wait(t, "accepted early next_step")
}

// Only a steer refused because its turn is gone starts a turn of its own;
// any other refusal is reported as the input's failure.
func TestCodexSteerErrors(t *testing.T) {
	c, w, sink := codexStarted(t, "lux/0.155.1")
	c.Deliver(proto.Input{RequestID: "bad", Text: "x"})
	id, _ := w.next("turn/steer")
	w.send(`{"id":` + id + `,"error":{"code":-32600,"message":"cannot steer a review turn"}}`)
	sink.wait(t, "failed bad: turn/steer: cannot steer a review turn")
	w.none()

	for _, msg := range []string{"no active turn to steer", "expected active turn id `" + cxTurn + "` but found `01a0f2bb-dd3b`"} {
		c.Deliver(proto.Input{RequestID: "late", Text: "y"})
		id, _ = w.next("turn/steer")
		w.send(`{"id":` + id + `,"error":{"code":-32600,"message":"` + msg + `"}}`)
		w.none() // queued: the turn in progress (for the adapter) runs on
		w.send(cxCompleted("completed"))
		id, p := w.next("turn/start")
		if str(p, "clientUserMessageId") != "late" {
			t.Fatalf("turn/start params %v", p)
		}
		w.send(`{"id":` + id + `,"result":{"turn":{"id":"` + cxTurn + `","status":"inProgress"}}}`)
		sink.wait(t, "accepted late")
		c.inputs.forget("late")
	}
	for _, l := range sink.lines() {
		if strings.HasPrefix(l, "failed late") {
			t.Fatalf("%q", sink.lines())
		}
	}
}

// A steer accepted into a turn that is then interrupted is never read in
// it (Codex drops a turn's pending steers): it starts the next turn, under
// the same request id, and is consumed there, once; nothing fails.
func TestCodexSteerCarriedPastInterrupt(t *testing.T) {
	c, w, sink := codexStarted(t, "lux/0.155.1")
	c.Deliver(proto.Input{RequestID: "s1", Text: "y"})
	id, _ := w.next("turn/steer")
	w.send(`{"id":` + id + `,"result":{"turnId":"` + cxTurn + `"}}`)
	sink.wait(t, "accepted s1")
	go c.Deliver(proto.Input{RequestID: "int-1", Interrupt: true})
	id, p := w.next("turn/interrupt")
	if str(p, "turnId") != cxTurn {
		t.Fatalf("turn/interrupt %v", p)
	}
	w.send(`{"id":` + id + `,"result":{}}`)
	w.send(cxCompleted("interrupted"))
	id, p = w.next("turn/start")
	if str(p, "clientUserMessageId") != "s1" {
		t.Fatalf("turn/start %v", p)
	}
	const next = "01a0f2bb-dd3b-71c3-8d08-f3a1cb53a3e9"
	w.send(`{"id":` + id + `,"result":{"turn":{"id":"` + next + `","status":"inProgress"}}}`)
	w.send(`{"method":"item/started","params":{"item":{"type":"userMessage","id":"u2","clientId":"s1","content":[]},"threadId":"` + cxThread + `","turnId":"` + next + `"}}`)
	w.send(`{"method":"turn/completed","params":{"threadId":"` + cxThread + `","turn":{"id":"` + next + `","status":"completed"}}}`)
	sink.wait(t, "accepted int-1 next_step receipt=false")
	checkCarried(t, w, sink, "s1")
}

// Stopping the Run is the one end of a turn that fails its unread steers.
func TestCodexSteerFailsWhenRunStops(t *testing.T) {
	c, w, sink := codexStarted(t, "lux/0.155.1")
	c.Deliver(proto.Input{RequestID: "s1", Text: "y"})
	id, _ := w.next("turn/steer")
	w.send(`{"id":` + id + `,"result":{"turnId":"` + cxTurn + `"}}`)
	sink.wait(t, "accepted s1")
	go c.Stop()
	id, _ = w.next("turn/interrupt")
	w.send(`{"id":` + id + `,"result":{}}`)
	w.send(cxCompleted("interrupted"))
	sink.wait(t, "failed s1: the Run stopped before the agent read it")
}

// A late turn/steer result and turn/completed both find a steer its turn
// left unread: it is carried once. Steers are carried in the order they
// came, not the order Codex answered their turn/steer.
func TestCodexCarriesEachSteerOnce(t *testing.T) {
	c, w, sink := codexStarted(t, "lux/0.155.1")
	accepted, done, release := make(chan string, 8), make(chan string, 8), make(chan struct{})
	c.onSteer = func(stage, id string) {
		if stage == "done" {
			done <- id
			return
		}
		accepted <- id
		<-release
	}
	c.Deliver(proto.Input{RequestID: "s1", Text: "one"})
	c.Deliver(proto.Input{RequestID: "s2", Text: "two"})
	steers := map[string]string{}
	for range 2 {
		id, p := w.next("turn/steer")
		steers[str(p, "clientUserMessageId")] = id
	}
	// Codex answers s2 first; each result is held after its acceptance.
	for _, s := range []string{"s2", "s1"} {
		w.send(`{"id":` + steers[s] + `,"result":{"turnId":"` + cxTurn + `"}}`)
		if got := <-accepted; got != s {
			t.Fatalf("accepted %s, want %s", got, s)
		}
	}
	w.send(cxCompleted("interrupted"))
	// Sent: the outbound messages from here on, by method and input.
	sent := map[string]int{}
	id, p := w.next("turn/start")
	sent["turn/start "+str(p, "clientUserMessageId")]++
	if str(p, "clientUserMessageId") != "s1" {
		t.Fatalf("the next turn starts with %s, not s1, which came first", str(p, "clientUserMessageId"))
	}
	// Now both held results check whether their turn ended.
	close(release)
	<-done
	<-done
	const next = "01a0f2bb-dd3b-71c3-8d08-f3a1cb53a3e9"
	w.send(`{"id":` + id + `,"result":{"turn":{"id":"` + next + `","status":"inProgress"}}}`)
	id, p = w.next("turn/steer")
	sent["turn/steer "+str(p, "clientUserMessageId")]++
	w.send(`{"id":` + id + `,"result":{"turnId":"` + next + `"}}`)
	<-accepted
	<-done
	for _, s := range []string{"s1", "s2"} {
		w.send(`{"method":"item/started","params":{"item":{"type":"userMessage","id":"u-` + s + `","clientId":"` + s + `","content":[]},"threadId":"` + cxThread + `","turnId":"` + next + `"}}`)
	}
	w.send(`{"method":"turn/completed","params":{"threadId":"` + cxThread + `","turn":{"id":"` + next + `","status":"completed"}}}`)
	sink.waitLast(t, "idle")
	w.none()
	w.exit()
	if want := map[string]int{"turn/start s1": 1, "turn/steer s2": 1}; fmt.Sprint(sent) != fmt.Sprint(want) {
		t.Fatalf("after the interrupt the adapter sent %v, want %v", sent, want)
	}
	for _, x := range sink.lines() {
		if strings.HasPrefix(x, "failed") {
			t.Fatalf("%q", sink.lines())
		}
	}
}

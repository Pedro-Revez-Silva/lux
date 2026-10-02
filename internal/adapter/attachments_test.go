package adapter

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/spec"
)

var (
	imgBytes = []byte("\x89PNG\r\n\x1a\nIMG")
	imgData  = base64.StdEncoding.EncodeToString(imgBytes)
	img      = spec.Attachment{Name: "shot.png", ContentType: "image/png", Data: imgData, Path: "/home/agent/.lux-inputs/s/1-shot.png"}
)

func withImage(id, text string) proto.Input {
	return proto.Input{RequestID: id, Text: text, Attachments: []spec.Attachment{img}}
}

func jsonOf(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// An input without images is the one text block every protocol got
// before images existed, byte for byte.
func TestInputContentTextOnlyUnchanged(t *testing.T) {
	before := jsonOf(t, []map[string]string{{"type": "text", "text": "hi \"there\""}})
	for _, d := range []int{dialectClaude, dialectCodex, dialectACP, dialectOpenCode} {
		if got := jsonOf(t, inputContent(d, proto.Input{Text: "hi \"there\""})); got != before {
			t.Errorf("dialect %d: %s, want %s", d, got, before)
		}
	}
	if got := jsonOf(t, inputContent(dialectACP, proto.Input{})); got != `[{"text":"","type":"text"}]` {
		t.Errorf("empty input: %s", got)
	}
}

// Each protocol's message for image+text and image only: images first.
func TestInputContentImages(t *testing.T) {
	text := `{"text":"what is this?","type":"text"}`
	for _, c := range []struct {
		dialect int
		block   string
	}{
		{dialectClaude, `{"source":{"data":"` + imgData + `","media_type":"image/png","type":"base64"},"type":"image"}`},
		{dialectCodex, `{"path":"/home/agent/.lux-inputs/s/1-shot.png","type":"localImage"}`},
		{dialectACP, `{"data":"` + imgData + `","mimeType":"image/png","type":"image"}`},
		{dialectOpenCode, `{"filename":"shot.png","mime":"image/png","type":"file","url":"data:image/png;base64,` + imgData + `"}`},
	} {
		if got := jsonOf(t, inputContent(c.dialect, withImage("s", "what is this?"))); got != "["+c.block+","+text+"]" {
			t.Errorf("dialect %d image+text:\n got %s\nwant %s", c.dialect, got, "["+c.block+","+text+"]")
		}
		if got := jsonOf(t, inputContent(c.dialect, withImage("s", ""))); got != "["+c.block+"]" {
			t.Errorf("dialect %d image only: %s", c.dialect, got)
		}
	}
	// Codex with an image the shim could not write: inline.
	noFile := proto.Input{Attachments: []spec.Attachment{{Name: "a.png", ContentType: "image/png", Data: imgData}}}
	if got := jsonOf(t, inputContent(dialectCodex, noFile)); got != `[{"type":"image","url":"data:image/png;base64,`+imgData+`"}]` {
		t.Errorf("codex inline: %s", got)
	}
}

// content is a Claude user line's message content.
func claudeContent(t *testing.T, w *agentWire) (string, string) {
	t.Helper()
	select {
	case m := <-w.sent:
		var msg struct {
			Content json.RawMessage `json:"content"`
		}
		_ = json.Unmarshal(m["message"], &msg)
		return str(m, "uuid"), string(msg.Content)
	case <-waitTimeout():
		t.Fatal("no user line")
	}
	return "", ""
}

const (
	clImage = `{"source":{"data":"` + "iVBORw0KGgpJTUc=" + `","media_type":"image/png","type":"base64"},"type":"image"}`
)

// Claude Code: the prompt's and a steer's images go in their user line;
// a steer cancelled by an interrupt is written again with its images.
func TestClaudeAttachments(t *testing.T) {
	if imgData != "iVBORw0KGgpJTUc=" {
		t.Fatalf("fixture %s", imgData)
	}
	c := NewClaude()
	w, sink := startWire(t, c, proto.ShimConfig{PromptAttachments: []spec.Attachment{img}})
	prompt, content := claudeContent(t, w)
	if prompt != claudeUUID("prompt") || content != "["+clImage+"]" {
		t.Fatalf("prompt line %s %s", prompt, content)
	}
	w.send(clLifecycle(prompt, "queued"))
	w.send(clLifecycle(prompt, "started"))
	c.Deliver(withImage("s", "and this?"))
	steer, content := claudeContent(t, w)
	if want := "[" + clImage + `,{"text":"and this?","type":"text"}]`; content != want {
		t.Fatalf("steer line %s, want %s", content, want)
	}
	w.send(clLifecycle(steer, "queued"))
	sink.wait(t, "accepted s")
	c.Deliver(proto.Input{RequestID: "int-1", Interrupt: true})
	<-w.sent // control_request
	w.send(strings.Replace(clResult, `"completed"`, `"aborted_streaming"`, 1))
	w.send(clLifecycle(prompt, "cancelled"))
	w.send(clLifecycle(steer, "cancelled"))
	again, content := claudeContent(t, w)
	if want := "[" + clImage + `,{"text":"and this?","type":"text"}]`; again != claudeUUID("s#1") || content != want {
		t.Fatalf("resent %s %s, want %s", again, content, want)
	}
	// Read: its payload is no longer held.
	w.send(clLifecycle(again, "queued"))
	w.send(clLifecycle(again, "started"))
	sink.wait(t, "consumed s")
	c.mu.Lock()
	held := c.sent[again]
	c.mu.Unlock()
	if len(held.Attachments) != 0 || held.Text != "" {
		t.Fatalf("a line read still holds its payload: %+v", held)
	}
}

// Codex: images go as localImage items (the shim's files) in turn/start
// and turn/steer, before the text; a steer its interrupted turn left
// unread starts the next turn with them. The bytes are not held.
func TestCodexAttachments(t *testing.T) {
	c := NewCodex()
	w, sink := startWire(t, c, proto.ShimConfig{Prompt: "look", PromptAttachments: []spec.Attachment{img}})
	id, _ := w.next("initialize")
	w.send(`{"id":` + id + `,"result":{"userAgent":"lux/0.155.1"}}`)
	id, _ = w.next("thread/start")
	w.send(`{"id":` + id + `,"result":{"thread":{"id":"` + cxThread + `"}}}`)
	id, p := w.next("turn/start")
	local := `{"path":"/home/agent/.lux-inputs/s/1-shot.png","type":"localImage"}`
	if got := string(p["input"]); got != "["+local+`,{"text":"look","type":"text"}]` {
		t.Fatalf("turn/start input %s", got)
	}
	w.send(`{"id":` + id + `,"result":{"turn":{"id":"` + cxTurn + `","status":"inProgress"}}}`)
	w.send(`{"method":"item/started","params":{"item":{"type":"userMessage","id":"u1","clientId":"prompt","content":[]},"threadId":"` + cxThread + `","turnId":"` + cxTurn + `"}}`)
	sink.wait(t, "consumed prompt")
	c.Deliver(withImage("s1", ""))
	id, p = w.next("turn/steer")
	if got := string(p["input"]); got != "["+local+"]" {
		t.Fatalf("turn/steer input %s", got)
	}
	w.send(`{"id":` + id + `,"result":{"turnId":"` + cxTurn + `"}}`)
	sink.wait(t, "accepted s1")
	for _, in := range c.inputs.unread(cxTurn) {
		if in.Attachments[0].Data != "" {
			t.Fatalf("held with its bytes: %+v", in)
		}
	}
	go c.Deliver(proto.Input{RequestID: "int-1", Interrupt: true})
	id, _ = w.next("turn/interrupt")
	w.send(`{"id":` + id + `,"result":{}}`)
	w.send(cxCompleted("interrupted"))
	_, p = w.next("turn/start")
	if str(p, "clientUserMessageId") != "s1" || string(p["input"]) != "["+local+"]" {
		t.Fatalf("carried turn/start %s %s", p["clientUserMessageId"], p["input"])
	}
}

// acpStarted is ocStarted with an agent that does or does not advertise
// promptCapabilities.image.
func acpStarted(t *testing.T, a *ACP, images bool, cfg proto.ShimConfig) (*agentWire, *inputSink) {
	t.Helper()
	w, sink := startWire(t, a, cfg)
	id, _ := w.next("initialize")
	caps := `{"loadSession":true}`
	if images {
		caps = `{"loadSession":true,"promptCapabilities":{"image":true,"audio":false,"embeddedContext":true}}`
	}
	w.send(`{"jsonrpc":"2.0","id":` + id + `,"result":{"protocolVersion":1,"agentCapabilities":` + caps + `}}`)
	id, _ = w.next("session/new")
	w.send(`{"jsonrpc":"2.0","id":` + id + `,"result":{"sessionId":"` + ocSession + `"}}`)
	return w, sink
}

// ACP: an agent that takes images gets them as image blocks before the
// text; an input interrupting a turn is sent, with its images, when the
// cancelled prompt returns.
func TestACPAttachments(t *testing.T) {
	a := NewACP()
	w, sink := acpStarted(t, a, true, proto.ShimConfig{PromptAttachments: []spec.Attachment{img}})
	first, p := w.next("session/prompt")
	block := `{"data":"` + imgData + `","mimeType":"image/png","type":"image"}`
	if got := string(p["prompt"]); got != "["+block+"]" {
		t.Fatalf("prompt %s", got)
	}
	sink.wait(t, "accepted prompt")
	in := withImage("s", "and?")
	in.Interrupt = true
	a.Deliver(in)
	w.next("session/cancel")
	w.resolve(first, ocCancelled)
	second, p := w.next("session/prompt")
	if got := string(p["prompt"]); got != "["+block+`,{"text":"and?","type":"text"}]` {
		t.Fatalf("after the interrupt: %s", got)
	}
	w.resolve(second, ocResult)
	sink.wait(t, "accepted s next_turn")
}

// An ACP agent that did not advertise images: the input fails with the
// contract's error, nothing is sent, and the next input still goes.
func TestACPWithoutImagesFails(t *testing.T) {
	a := NewACP()
	w, sink := acpStarted(t, a, false, proto.ShimConfig{})
	a.Deliver(withImage("img", "look"))
	sink.wait(t, "failed img: the agent does not take images")
	a.Deliver(proto.Input{RequestID: "txt", Text: "plain"})
	_, p := w.next("session/prompt")
	if got := string(p["prompt"]); got != `[{"text":"plain","type":"text"}]` {
		t.Fatalf("next prompt %s", got)
	}
	if sink.has("accepted img") {
		t.Fatalf("%q", sink.lines())
	}
}

// An interrupting input with images, to an ACP agent without image
// support, fails before anything reaches the agent: the running turn is
// not cancelled.
func TestACPWithoutImagesInterruptKeepsTurn(t *testing.T) {
	a := NewACP()
	w, sink := acpStarted(t, a, false, proto.ShimConfig{Prompt: "go"})
	first, _ := w.next("session/prompt")
	sink.wait(t, "accepted prompt")
	in := withImage("s", "and?")
	in.Interrupt = true
	a.Deliver(in)
	sink.wait(t, "failed s: the agent does not take images")
	w.none()
	w.resolve(first, ocResult)
	checkLines(t, w, sink, "idle", "busy", "accepted prompt next_turn receipt=false",
		"failed s: the agent does not take images", "turn_end", "idle")
}

// OpenCode without image support, steered over ACP while busy (no HTTP
// server): a steer with images fails; no second session/prompt is sent.
func TestOpenCodeSteerACPWithoutImagesFails(t *testing.T) {
	a := NewOpenCode()
	w, sink, first := ocStarted(t, a)
	a.Deliver(withImage("img2", "x"))
	sink.wait(t, "failed img2: the agent does not take images")
	w.none()
	w.resolve(first, ocResult)
	checkLines(t, w, sink, "idle", "busy", "accepted prompt next_step receipt=false",
		"failed img2: the agent does not take images", "turn_end", "idle")
}

// bigImage is an input whose image data is n bytes (the adapter does not
// decode it).
func bigImage(id string, n int) proto.Input {
	return proto.Input{RequestID: id, Text: "x", Attachments: []spec.Attachment{{Name: "big.png", ContentType: "image/png", Data: strings.Repeat("A", n)}}}
}

// ACP: inputs waiting for the next turn are held under the pending-input
// byte budget, image data included; one past it fails at once, and an
// interrupt past it cancels nothing.
func TestACPQueueBytesAreBounded(t *testing.T) {
	a := NewACP()
	w, sink := acpStarted(t, a, true, proto.ShimConfig{Prompt: "go"})
	first, _ := w.next("session/prompt")
	sink.wait(t, "accepted prompt")
	const size = 7 << 20 // a 5 MiB image's base64
	for i := range 5 {
		a.Deliver(bigImage(fmt.Sprintf("q%d", i), size))
	}
	sink.wait(t, "failed q4: "+errPendingSteersLimit)
	over := bigImage("int", size)
	over.Interrupt = true
	a.Deliver(over)
	sink.wait(t, "failed int: "+errPendingSteersLimit)
	w.none()
	for i := range 4 {
		if sink.has(fmt.Sprintf("failed q%d", i)) {
			t.Fatalf("q%d failed within the budget: %q", i, sink.lines())
		}
	}
	w.resolve(first, ocResult)
	_, p := w.next("session/prompt")
	if !strings.Contains(string(p["prompt"]), `"type":"image"`) {
		t.Fatal("q0 not sent with its image")
	}
	sink.wait(t, "accepted q0")
}

// Claude Code: lines written and not yet started are held under the same
// budget; once one is read its bytes no longer count.
func TestClaudeSentBytesAreBounded(t *testing.T) {
	c := NewClaude()
	w, sink := startWire(t, c, proto.ShimConfig{})
	w.send(`{"type":"system","subtype":"init","session_id":"s","capabilities":["msg_lifecycle_v1"]}`)
	const size = 7 << 20
	var uuids []string
	for i := range 5 {
		c.Deliver(bigImage(fmt.Sprintf("q%d", i), size))
		if i < 4 {
			u, _ := claudeContent(t, w)
			uuids = append(uuids, u)
		}
	}
	sink.wait(t, "failed q4: "+errPendingSteersLimit)
	w.send(clLifecycle(uuids[0], "started"))
	sink.wait(t, "consumed q0")
	c.Deliver(bigImage("q5", size))
	if u, _ := claudeContent(t, w); u != claudeUUID("q5") {
		t.Fatalf("wrote %s, want q5's line", u)
	}
	if sink.has("failed q5") {
		t.Fatalf("%q", sink.lines())
	}
}

// OpenCode: a steer's images go to prompt_async as file parts with data
// URLs before the text part; dropped by an interrupt, it is sent again
// with them.
func TestOpenCodeAttachments(t *testing.T) {
	a, b, w, sink, first := ocWithBus(t)
	a.Deliver(withImage("s1", "see"))
	sink.wait(t, "accepted s1 next_step receipt=true")
	firstID := b.postedID(t, 0)
	want := `[{"filename":"shot.png","mime":"image/png","type":"file","url":"data:image/png;base64,` + imgData + `"},{"text":"see","type":"text"}]`
	b.mu.Lock()
	parts := jsonOf(t, b.posted[0]["parts"])
	b.mu.Unlock()
	if parts != want {
		t.Fatalf("prompt_async parts %s\nwant %s", parts, want)
	}
	a.Deliver(proto.Input{RequestID: "int-1", Interrupt: true})
	w.next("session/cancel")
	b.setLoop(false)
	w.resolve(first, ocCancelled)
	again := b.postedID(t, 1)
	b.mu.Lock()
	parts = jsonOf(t, b.posted[1]["parts"])
	b.mu.Unlock()
	if again == firstID || parts != want {
		t.Fatalf("sent again as %s with %s", again, parts)
	}
	b.events <- b.answer(again)
	sink.wait(t, "consumed s1")
	b.setLoop(false)
	b.events <- ocIdle
	checkCarried(t, w, sink, "s1")
}

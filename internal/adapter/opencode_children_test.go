package adapter

import (
	"bufio"
	"encoding/json"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/marcioapm/lux/internal/proto"
)

// Sessions of the captures (testdata/opencode-1.18.21-children-*.ndjson):
// the ACP session that called the task tool, and the child it ran.
const (
	hostsParent = "ses_ecc7cfcd9ffeUVgVM0hMDoyFbu"
	hostsChild  = "ses_ecc7cf31effeGCzeSYD4SeLE8p"
	lsParent    = "ses_ecc83b0d7ffeueYDyq32uXgXJR"
	lsChild     = "ses_ecc83a4ffffejMEWKnostn9ZHh"
)

// childSink keeps the opencode.* records and lux.warning messages, in
// order, as JSON. With gate set, an Event gate returns a channel for is
// held until that channel is closed, after its record is kept.
type childSink struct {
	*inputSink
	mu   sync.Mutex
	recs []string
	gate func(rec string) chan struct{}
}

func (s *childSink) Event(typ string, v any) {
	if strings.HasPrefix(typ, "opencode.") || typ == proto.EvWarning {
		b, _ := json.Marshal(v)
		rec := typ + " " + string(b)
		s.mu.Lock()
		s.recs = append(s.recs, rec)
		gate := s.gate
		s.mu.Unlock()
		if gate != nil {
			if ch := gate(rec); ch != nil {
				<-ch
			}
		}
	}
	s.inputSink.Event(typ, v)
}

func (s *childSink) records() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.recs)
}

// typed are the records of type typ, their data only.
func (s *childSink) typed(typ string) []string {
	var out []string
	for _, r := range s.records() {
		if d, ok := strings.CutPrefix(r, typ+" "); ok {
			out = append(out, d)
		}
	}
	return out
}

// childRun is an OpenCode adapter whose Run's session is run, reporting
// to a childSink, with a bus on port (none listening if 0) and the test
// clock clk; nothing else of Run is started.
func childRun(t *testing.T, run string, port int, clk *testClock) (*ACP, *childSink) {
	t.Helper()
	a := NewOpenCode()
	sink := &childSink{inputSink: &inputSink{}}
	a.sink, a.session = sink, run
	if clk != nil {
		a.clock = clk
	}
	a.bus = newOpencodeBus(port, "/workspace")
	t.Cleanup(func() {
		a.mu.Lock()
		a.closed = true
		a.mu.Unlock()
		a.bg.Wait()
	})
	return a, sink
}

// replay hands every event of a capture to onBus, in order.
func replay(t *testing.T, a *ACP, file string) {
	t.Helper()
	f, err := os.Open("testdata/" + file)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		onBus(t, a, sc.Text())
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
}

// joined waits for a's spawned work and returns the sink's records.
func joined(a *ACP, sink *childSink) []string {
	a.bg.Wait()
	return sink.records()
}

func wantRecords(t *testing.T, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("records:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// A child that reads a file and replies (opencode 1.18.21 with
// permission: allow): its session, the task call that runs it, each
// change of its tool call's status, and each completed assistant message
// with its text, once, in the order the bus said them. The parent's own
// parts and messages, the deltas, and the repeated events (task running
// twice, each message completed twice, session.updated) add nothing. The
// tool call's pending, replayed at once, is followed by its running within
// pendingHold: only the running is written.
func TestOpenCodeChildRelayHosts(t *testing.T) {
	a, sink := childRun(t, hostsParent, 0, nil)
	replay(t, a, "opencode-1.18.21-children-hosts.ndjson")
	model := `"model":{"modelID":"claude-haiku-4.5","providerID":"llmproxy-anthropic"}`
	part := `{"sessionID":"` + hostsChild + `","messageID":"msg_133830cef001F84jOG3tnyJjdy","callID":"toolu_01G5mHt6xdXxdhhZ6uD19pUS","tool":"read",`
	wantRecords(t, joined(a, sink), []string{
		`opencode.session {"id":"` + hostsChild + `","parentID":"` + hostsParent + `","title":"Read /etc/hosts first line (@general subagent)","createdAt":1791865588961}`,
		`opencode.task {"sessionID":"` + hostsParent + `","callID":"toolu_01L91pCabVxisGJueHYPC6dV","childSessionID":"` + hostsChild + `",` + model + `,"status":"running"}`,
		`opencode.part ` + part + `"status":"running","input":"{\"filePath\":\"/etc/hosts\",\"limit\":1}","time":{"start":1791865590382}}`,
		`opencode.part ` + part + `"status":"completed","title":"etc/hosts","input":"{\"filePath\":\"/etc/hosts\",\"limit\":1}","output":"\u003cpath\u003e/etc/hosts\u003c/path\u003e\n\u003ctype\u003efile\u003c/type\u003e\n\u003ccontent\u003e\n1: ##\n\n(Showing lines 1-1 of 15. Use offset=2 to continue.)\n\u003c/content\u003e","time":{"start":1791865590382,"end":1791865590387}}`,
		`opencode.message {"sessionID":"` + hostsChild + `","messageID":"msg_133830cef001F84jOG3tnyJjdy","role":"assistant","text":"","cost":0,"tokens":{"input":3,"output":78,"reasoning":0,"cacheRead":0,"cacheWrite":7979},"completed":1791865590417}`,
		`opencode.message {"sessionID":"` + hostsChild + `","messageID":"msg_133831292001AzLx0oDP7xJP2N","role":"assistant","text":"##","cost":0,"tokens":{"input":5,"output":4,"reasoning":0,"cacheRead":7979,"cacheWrite":137},"completed":1791865591204}`,
		`opencode.task {"sessionID":"` + hostsParent + `","callID":"toolu_01L91pCabVxisGJueHYPC6dV","childSessionID":"` + hostsChild + `",` + model + `,"status":"completed"}`,
	})
}

// The other capture: the task call is pending (no child yet) before the
// child exists, and the child's first message has text before its tool
// call. Replaying the whole capture again adds nothing: the bus repeats
// the task's running after its completed, and that is not written again.
func TestOpenCodeChildRelayLsOnceEach(t *testing.T) {
	a, sink := childRun(t, lsParent, 0, nil)
	replay(t, a, "opencode-1.18.21-children-ls.ndjson")
	first := joined(a, sink)
	var kinds []string
	for _, r := range first {
		typ, d, _ := strings.Cut(r, " ")
		var m map[string]any
		_ = json.Unmarshal([]byte(d), &m)
		k := strings.TrimPrefix(typ, "opencode.")
		for _, f := range []string{asString(m["status"]), asString(m["text"])} {
			if f != "" {
				k += " " + f
			}
		}
		kinds = append(kinds, k)
	}
	wantRecords(t, kinds, []string{
		"session", "task running", "part running", "part completed",
		"message I'll read the current working directory to list all the files.",
		"message Here are the files in the current working directory:\n\na.txt\nb.txt",
		"task completed",
	})
	replay(t, a, "opencode-1.18.21-children-ls.ndjson")
	wantRecords(t, joined(a, sink), first)
}

func asString(v any) string { s, _ := v.(string); return s }

// Sessions of another Run on the same server (or the server's own) are
// not the Run's: replayed alone, or interleaved with the Run's own tree,
// they add nothing.
func TestOpenCodeChildRelayIgnoresOtherRuns(t *testing.T) {
	a, sink := childRun(t, "ses_notInEitherCapture", 0, nil)
	replay(t, a, "opencode-1.18.21-children-hosts.ndjson")
	replay(t, a, "opencode-1.18.21-children-ls.ndjson")
	if got := joined(a, sink); len(got) != 0 {
		t.Fatalf("records for sessions not of the Run: %q", got)
	}

	a, sink = childRun(t, lsParent, 0, nil)
	hosts, _ := os.ReadFile("testdata/opencode-1.18.21-children-hosts.ndjson")
	ls, _ := os.ReadFile("testdata/opencode-1.18.21-children-ls.ndjson")
	h, l := strings.Split(strings.TrimSpace(string(hosts)), "\n"), strings.Split(strings.TrimSpace(string(ls)), "\n")
	for i := range max(len(h), len(l)) {
		if i < len(h) {
			onBus(t, a, h[i])
		}
		if i < len(l) {
			onBus(t, a, l[i])
		}
	}
	got := joined(a, sink)
	if len(got) != 7 {
		t.Fatalf("%d records, want the ls capture's 7:\n%s", len(got), strings.Join(got, "\n"))
	}
	for _, r := range got {
		if strings.Contains(r, hostsChild) || strings.Contains(r, hostsParent) {
			t.Fatalf("a record of the other Run's tree: %s", r)
		}
	}
}

// ocTool is a message.part.updated of a tool call.
func ocTool(session, call, tool, status string, state map[string]any) string {
	st := map[string]any{"status": status, "input": map[string]any{}}
	for k, v := range state {
		st[k] = v
	}
	b, _ := json.Marshal(map[string]any{"type": "message.part.updated", "properties": map[string]any{"sessionID": session,
		"part": map[string]any{"id": "prt_" + call, "type": "tool", "sessionID": session, "messageID": "msg_" + session, "callID": call, "tool": tool, "state": st}}})
	return string(b)
}

func ocSessionCreated(id, parent string) string {
	return `{"type":"session.created","properties":{"sessionID":"` + id + `","info":{"id":"` + id + `","parentID":"` + parent + `","title":"t","time":{"created":1}}}}`
}

// A task call inside a child runs a grandchild: its session, its task
// call (in the child's session) and its tool calls are the Run's too,
// learnt from the task call's metadata even before its session.created.
func TestOpenCodeChildRelayNested(t *testing.T) {
	a, sink := childRun(t, ocSession, 0, nil)
	task := func(session, call, child, status string) string {
		return ocTool(session, call, "task", status, map[string]any{"metadata": map[string]any{"sessionId": child}})
	}
	onBus(t, a, task(ocSession, "c1", "ses_kid", "running"))
	onBus(t, a, ocSessionCreated("ses_kid", ocSession))
	onBus(t, a, task("ses_kid", "c2", "ses_grandkid", "running"))
	onBus(t, a, ocTool("ses_grandkid", "c3", "bash", "running", nil))
	onBus(t, a, ocSessionCreated("ses_grandkid", "ses_kid"))
	onBus(t, a, ocSessionCreated("ses_stranger", "ses_elsewhere"))
	onBus(t, a, ocTool("ses_stranger", "c4", "bash", "running", nil))
	onBus(t, a, task("ses_kid", "c2", "ses_grandkid", "error"))
	wantRecords(t, joined(a, sink), []string{
		`opencode.task {"sessionID":"` + ocSession + `","callID":"c1","childSessionID":"ses_kid","status":"running"}`,
		`opencode.session {"id":"ses_kid","parentID":"` + ocSession + `","title":"t","createdAt":1}`,
		`opencode.task {"sessionID":"ses_kid","callID":"c2","childSessionID":"ses_grandkid","status":"running"}`,
		`opencode.part {"sessionID":"ses_kid","messageID":"msg_ses_kid","callID":"c2","tool":"task","status":"running","time":{}}`,
		`opencode.part {"sessionID":"ses_grandkid","messageID":"msg_ses_grandkid","callID":"c3","tool":"bash","status":"running","time":{}}`,
		`opencode.session {"id":"ses_grandkid","parentID":"ses_kid","title":"t","createdAt":1}`,
		`opencode.task {"sessionID":"ses_kid","callID":"c2","childSessionID":"ses_grandkid","status":"error"}`,
		`opencode.part {"sessionID":"ses_kid","messageID":"msg_ses_kid","callID":"c2","tool":"task","status":"error","time":{}}`,
	})
}

// A session up to 63 levels below the Run's is of the Run (a walk of 64
// sessions, the Run's included), one deeper is not; a cycle of parents
// that never reaches the Run's ends the walk and relays nothing.
func TestOpenCodeChildAncestryBound(t *testing.T) {
	chain := func(depth int) []string {
		var evs []string
		parent := ocSession
		for i := 1; i <= depth; i++ {
			id := "ses_d" + strconv.Itoa(i)
			evs = append(evs, ocSessionCreated(id, parent))
			parent = id
		}
		return append(evs, ocTool(parent, "c_deep", "bash", "running", nil))
	}
	for _, c := range []struct {
		name    string
		events  []string
		relayed bool
	}{
		{"62 levels", chain(62), true},
		{"63 levels", chain(63), true},
		{"64 levels", chain(64), false},
		{"cycle", []string{ocSessionCreated("ses_a", "ses_b"), ocSessionCreated("ses_b", "ses_a"),
			ocTool("ses_a", "c_deep", "bash", "running", nil)}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			a, sink := childRun(t, ocSession, 0, nil)
			done := make(chan struct{})
			go func() {
				defer close(done)
				for _, ev := range c.events {
					onBus(t, a, ev)
				}
			}()
			within(t, done, "the walk up the parents")
			got := joinedTyped(a, sink, proto.EvOpenCodePart)
			if relayed := slices.ContainsFunc(got, func(r string) bool { return strings.Contains(r, `"callID":"c_deep"`) }); relayed != c.relayed || len(got) > 1 {
				t.Fatalf("parts %q, want relayed %v", got, c.relayed)
			}
		})
	}
}

// A child's tool call's input, output and error are redacted, then cut
// (input at 300 bytes, output and error at 2 KiB, on a rune boundary);
// output only once completed, error only on error.
func TestOpenCodeChildPartClippedAndRedacted(t *testing.T) {
	a, sink := childRun(t, ocSession, 0, nil)
	onBus(t, a, ocSessionCreated("ses_kid", ocSession))
	long := strings.Repeat("é", 2000) // 4000 bytes
	in := map[string]any{"command": "echo " + testSecret + " " + long}
	onBus(t, a, ocTool("ses_kid", "c1", "bash", "running", map[string]any{"input": in, "output": "partial " + testSecret}))
	onBus(t, a, ocTool("ses_kid", "c1", "bash", "completed", map[string]any{"input": in, "output": testSecret + " " + long}))
	onBus(t, a, ocTool("ses_kid", "c2", "bash", "error", map[string]any{"input": map[string]any{"x": 1}, "error": "failed: " + testSecret}))
	var parts []proto.OpenCodePart
	for _, d := range sink.typed(proto.EvOpenCodePart) {
		var p proto.OpenCodePart
		if err := json.Unmarshal([]byte(d), &p); err != nil {
			t.Fatal(err)
		}
		parts = append(parts, p)
	}
	if len(parts) != 3 {
		t.Fatalf("%d parts: %+v", len(parts), parts)
	}
	for _, r := range sink.records() {
		if strings.Contains(r, testSecret) {
			t.Fatalf("the secret is in a record: %s", r)
		}
	}
	running, done, failed := parts[0], parts[1], parts[2]
	if running.Output != "" || !strings.HasPrefix(running.Input, `{"command":"echo [REDACTED:T] é`) ||
		len(running.Input) > proto.MaxOpenCodeInput || len(running.Input) < proto.MaxOpenCodeInput-1 || !running.Truncated {
		t.Fatalf("running: %+v (%d bytes of input)", running, len(running.Input))
	}
	if !strings.HasPrefix(done.Output, "[REDACTED:T] éé") || len(done.Output) > proto.MaxOpenCodeOutput ||
		len(done.Output) < proto.MaxOpenCodeOutput-1 || !done.Truncated || done.Error != "" {
		t.Fatalf("completed: %d bytes of output, truncated %v, error %q", len(done.Output), done.Truncated, done.Error)
	}
	if failed.Error != "failed: [REDACTED:T]" || failed.Output != "" || failed.Input != `{"x":1}` || failed.Truncated {
		t.Fatalf("error: %+v", failed)
	}
}

// A child's message text is redacted and cut at 2 KiB, its text parts
// joined in the order they came, each at its newest text.
func TestOpenCodeChildMessageText(t *testing.T) {
	a, sink := childRun(t, ocSession, 0, nil)
	onBus(t, a, ocSessionCreated("ses_kid", ocSession))
	text := func(id, s string) string {
		b, _ := json.Marshal(map[string]any{"type": "message.part.updated", "properties": map[string]any{"sessionID": "ses_kid",
			"part": map[string]any{"id": id, "type": "text", "sessionID": "ses_kid", "messageID": "msg_1", "text": s}}})
		return string(b)
	}
	done := `{"type":"message.updated","properties":{"sessionID":"ses_kid","info":{"id":"msg_1","sessionID":"ses_kid","role":"assistant","cost":0.25,"tokens":{"input":1,"output":2,"reasoning":3,"cache":{"read":4,"write":5}},"time":{"created":1,"completed":9}}}}`
	onBus(t, a, text("prt_a", ""))
	onBus(t, a, text("prt_b", "second"))
	onBus(t, a, text("prt_a", "first "+testSecret))
	onBus(t, a, strings.Replace(done, `,"completed":9`, "", 1))
	onBus(t, a, done)
	onBus(t, a, done)
	onBus(t, a, text("prt_c", strings.Repeat("x", 3000)))
	onBus(t, a, strings.ReplaceAll(done, "msg_1", "msg_2"))
	got := sink.typed(proto.EvOpenCodeMessage)
	if len(got) != 2 {
		t.Fatalf("messages: %q", got)
	}
	if want := `{"sessionID":"ses_kid","messageID":"msg_1","role":"assistant","text":"first [REDACTED:T]\nsecond","cost":0.25,"tokens":{"input":1,"output":2,"reasoning":3,"cacheRead":4,"cacheWrite":5},"completed":9}`; got[0] != want {
		t.Fatalf("message:\n%s\nwant\n%s", got[0], want)
	}
	var m proto.OpenCodeMessage
	_ = json.Unmarshal([]byte(got[1]), &m)
	if m.MessageID != "msg_2" || m.Text != "" || m.Truncated {
		t.Fatalf("a message with no text parts of its own: %+v", m)
	}
	onBus(t, a, strings.ReplaceAll(text("prt_c", strings.Repeat("x", 3000)), "msg_1", "msg_3"))
	onBus(t, a, strings.ReplaceAll(done, "msg_1", "msg_3"))
	_ = json.Unmarshal([]byte(sink.typed(proto.EvOpenCodeMessage)[2]), &m)
	if len(m.Text) != proto.MaxOpenCodeText || !m.Truncated {
		t.Fatalf("a long text: %d bytes, truncated %v", len(m.Text), m.Truncated)
	}
}

func ocText(session, msg, part, text string) string {
	b, _ := json.Marshal(map[string]any{"type": "message.part.updated", "properties": map[string]any{"sessionID": session,
		"part": map[string]any{"id": part, "type": "text", "sessionID": session, "messageID": msg, "text": text}}})
	return string(b)
}

// heapNow is the live heap after a full collection.
func heapNow() uint64 {
	var ms runtime.MemStats
	runtime.GC()
	runtime.GC()
	runtime.ReadMemStats(&ms)
	return ms.HeapAlloc
}

// A 16 MiB text update of a child message that never completes keeps a
// few KiB, not the update: the tracker's heap grows by under 64 KiB, and
// the text it keeps is under 8 KiB. When the message does complete, its
// record has the text cut at MaxOpenCodeText, redacted, truncated.
func TestOpenCodeChildUnfinishedTextBounded(t *testing.T) {
	a, sink := childRun(t, ocSession, 0, nil)
	onBus(t, a, ocSessionCreated("ses_kid", ocSession))
	before := heapNow()
	func() {
		big := "head " + testSecret + " " + strings.Repeat("x", 16<<20)
		onBus(t, a, ocText("ses_kid", "msg_big", "prt_1", big))
	}()
	grew := int64(heapNow()) - int64(before)
	if kept := keptText(a); grew > 64<<10 || kept >= 8<<10 {
		t.Fatalf("after a 16 MiB text update: heap grew %d bytes, %d bytes of text kept", grew, kept)
	}
	onBus(t, a, ocText("ses_kid", "msg_big", "prt_2", "later part"))
	onBus(t, a, `{"type":"message.updated","properties":{"sessionID":"ses_kid","info":{"id":"msg_big","sessionID":"ses_kid","role":"assistant","cost":0,"tokens":{"cache":{}},"time":{"created":1,"completed":9}}}}`)
	got := joinedTyped(a, sink, proto.EvOpenCodeMessage)
	want := proto.OpenCodeMessage{SessionID: "ses_kid", MessageID: "msg_big", Role: "assistant", Completed: 9, Truncated: true,
		Text: ("head [REDACTED:T] " + strings.Repeat("x", 16<<20))[:proto.MaxOpenCodeText]}
	wb, _ := json.Marshal(want)
	if len(got) != 1 || got[0] != string(wb) {
		t.Fatalf("messages: %d, first %.200q", len(got), got)
	}
}

// keptText is the text the tracker holds for messages not yet written.
func keptText(a *ACP) int {
	a.kids.mu.Lock()
	defer a.kids.mu.Unlock()
	n := 0
	for _, m := range a.kids.texts {
		for _, s := range m.text {
			n += len(s)
		}
	}
	return n
}

// Many text parts of one unfinished message keep no more in all than one
// long part does.
func TestOpenCodeChildManyTextPartsBounded(t *testing.T) {
	a, _ := childRun(t, ocSession, 0, nil)
	onBus(t, a, ocSessionCreated("ses_kid", ocSession))
	for i := range 1000 {
		onBus(t, a, ocText("ses_kid", "msg_many", "prt_"+strconv.Itoa(i), strings.Repeat("y", 4<<10)))
	}
	if n := keptText(a); n >= 8<<10 {
		t.Fatalf("1000 parts of 4 KiB: %d bytes of text kept", n)
	}
}

// Parts dropped where they are kept make the message truncated even when
// redaction brings what was kept under MaxOpenCodeText.
func TestOpenCodeChildDroppedPartsTruncated(t *testing.T) {
	a, sink := childRun(t, ocSession, 0, nil)
	onBus(t, a, ocSessionCreated("ses_kid", ocSession))
	secrets := strings.Repeat(testSecret, maxKeptText/len(testSecret)+1) // redacted: 12 of every 18 bytes
	onBus(t, a, ocText("ses_kid", "msg_d", "prt_1", secrets[:maxKeptText]))
	onBus(t, a, ocText("ses_kid", "msg_d", "prt_2", "dropped"))
	onBus(t, a, `{"type":"message.updated","properties":{"sessionID":"ses_kid","info":{"id":"msg_d","sessionID":"ses_kid","role":"assistant","cost":0,"tokens":{"cache":{}},"time":{"created":1,"completed":9}}}}`)
	var m proto.OpenCodeMessage
	got := joinedTyped(a, sink, proto.EvOpenCodeMessage)
	if len(got) != 1 || json.Unmarshal([]byte(got[0]), &m) != nil {
		t.Fatalf("messages: %q", got)
	}
	if len(m.Text) >= proto.MaxOpenCodeText || strings.Contains(m.Text, "dropped") || !m.Truncated {
		t.Fatalf("%d bytes of text, truncated %v: %.80q", len(m.Text), m.Truncated, m.Text)
	}
}

// A long text part is cut where it is kept after it is redacted, never
// before: with earlier secrets redacted to shorter markers, a secret
// straddling the keep limit in the raw text lands inside the record's
// 2 KiB, and is redacted there, not half kept.
func TestOpenCodeChildKeptTextCutAfterRedaction(t *testing.T) {
	a, sink := childRun(t, ocSession, 0, nil)
	onBus(t, a, ocSessionCreated("ses_kid", ocSession))
	head := strings.Repeat(testSecret, 50)
	filler := strings.Repeat("z", maxKeptText-9-len(head))
	raw := head + filler + testSecret + strings.Repeat("w", 4<<10) // the third secret straddles byte maxKeptText
	onBus(t, a, ocText("ses_kid", "msg_s", "prt_1", raw))
	onBus(t, a, `{"type":"message.updated","properties":{"sessionID":"ses_kid","info":{"id":"msg_s","sessionID":"ses_kid","role":"assistant","cost":0,"tokens":{"cache":{}},"time":{"created":1,"completed":9}}}}`)
	got := joinedTyped(a, sink, proto.EvOpenCodeMessage)
	var m proto.OpenCodeMessage
	if len(got) != 1 || json.Unmarshal([]byte(got[0]), &m) != nil {
		t.Fatalf("messages: %q", got)
	}
	want := (strings.Repeat("[REDACTED:T]", 50) + filler + "[REDACTED:T]" + strings.Repeat("w", 4<<10))[:proto.MaxOpenCodeText]
	if m.Text != want || !m.Truncated {
		t.Fatalf("text %q\nwant %q", m.Text, want)
	}
}

// A child's user message keeps no text: what came before its role was
// known is dropped as it is, and what comes after is not kept.
func TestOpenCodeChildUserTextDropped(t *testing.T) {
	a, sink := childRun(t, ocSession, 0, nil)
	onBus(t, a, ocSessionCreated("ses_kid", ocSession))
	onBus(t, a, ocText("ses_kid", "msg_user", "prt_1", "the task's prompt"))
	onBus(t, a, `{"type":"message.updated","properties":{"sessionID":"ses_kid","info":{"id":"msg_user","sessionID":"ses_kid","role":"user","time":{"created":1}}}}`)
	onBus(t, a, ocText("ses_kid", "msg_user", "prt_2", "more of it"))
	a.kids.mu.Lock()
	held := len(a.kids.texts)
	a.kids.mu.Unlock()
	if held != 0 {
		t.Fatalf("text kept for %d messages", held)
	}
	if got := joined(a, sink)[1:]; len(got) != 0 {
		t.Fatalf("records: %q", got)
	}
}

// A child's pending followed by another status within pendingHold is
// written as that status alone; a pending still the newest after
// pendingHold is written then, and the running after it too. A repeated
// pending holds no second timer.
func TestOpenCodeChildPendingCollapses(t *testing.T) {
	clk := newTestClock()
	a, sink := childRun(t, ocSession, 0, clk)
	onBus(t, a, ocSessionCreated("ses_kid", ocSession))
	onBus(t, a, ocTool("ses_kid", "c1", "bash", "pending", nil))
	onBus(t, a, ocTool("ses_kid", "c1", "bash", "pending", nil))
	onBus(t, a, ocTool("ses_kid", "c1", "bash", "running", map[string]any{"input": map[string]any{"command": "ls"}, "output": "partial"}))
	clk.mu.Lock()
	due, delays := len(clk.due), slices.Clone(clk.delays)
	clk.mu.Unlock()
	if due != 0 || !slices.Equal(delays, []time.Duration{pendingHold}) {
		t.Fatalf("timers due %d, set %v", due, delays)
	}
	onBus(t, a, ocTool("ses_kid", "c2", "bash", "pending", nil))
	clk.fire(t)
	a.bg.Wait()
	onBus(t, a, ocTool("ses_kid", "c2", "bash", "pending", nil))
	onBus(t, a, ocTool("ses_kid", "c2", "bash", "running", nil))
	var got []string
	for _, d := range sink.typed(proto.EvOpenCodePart) {
		var p proto.OpenCodePart
		_ = json.Unmarshal([]byte(d), &p)
		got = append(got, p.CallID+" "+p.Status+" "+p.Input+" "+p.Output)
	}
	wantRecords(t, got, []string{`c1 running {"command":"ls"} `, "c2 pending  ", "c2 running  "})
}

// The Run ending with pendings still held stops their timers: none is
// left to fire after Run returns, and none is written.
func TestOpenCodeChildPendingStoppedAtRunEnd(t *testing.T) {
	clk := newTestClock()
	a, sink := childRun(t, ocSession, 0, clk)
	onBus(t, a, ocSessionCreated("ses_kid", ocSession))
	for _, c := range []string{"c1", "c2", "c3"} {
		onBus(t, a, ocTool("ses_kid", c, "bash", "pending", nil))
	}
	clk.mu.Lock()
	due := len(clk.due)
	clk.mu.Unlock()
	if due != 3 {
		t.Fatalf("%d timers held before the Run ended, want 3", due)
	}
	a.exited(func() {})
	clk.mu.Lock()
	due = len(clk.due)
	clk.mu.Unlock()
	a.kids.mu.Lock()
	held := len(a.kids.pending)
	a.kids.mu.Unlock()
	if due != 0 || held != 0 {
		t.Fatalf("after the Run ended: %d timers due, %d pendings held", due, held)
	}
	if got := sink.typed(proto.EvOpenCodePart); len(got) != 0 {
		t.Fatalf("parts written: %q", got)
	}
}

// stallDeadline bounds a wait that a correct relay ends at once: generous,
// as it only catches a deadlock.
const stallDeadline = 10 * time.Second

func within(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(stallDeadline):
		t.Fatalf("%s: not within %v", what, stallDeadline)
	}
}

// A sink write held up (a held pending written from its timer, its
// output file stalled) does not hold the bus reader: an event of another
// session, and an event of the Run's tree, are each handled while the
// write waits, and the tree's record is written after the held one.
func TestOpenCodeChildSlowSinkHoldsNoBus(t *testing.T) {
	clk := newTestClock()
	a, sink := childRun(t, ocSession, 0, clk)
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	free := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(free) // registered after childRun's: runs first, so its join ends
	sink.gate = func(rec string) chan struct{} {
		if strings.Contains(rec, `"callID":"c1"`) && strings.Contains(rec, `"status":"pending"`) {
			close(entered)
			return release
		}
		return nil
	}
	onBus(t, a, ocSessionCreated("ses_kid", ocSession))
	onBus(t, a, ocTool("ses_kid", "c1", "bash", "pending", nil))
	clk.fire(t) // spawns pendingDue
	within(t, entered, "the held pending's write")

	handled := make(chan struct{})
	go func() {
		defer close(handled)
		onBus(t, a, ocTool("ses_stranger", "x1", "bash", "running", nil))
		onBus(t, a, ocTool("ses_kid", "c2", "bash", "running", nil))
	}()
	within(t, handled, "onBus while a sink write is held")
	if got := sink.typed(proto.EvOpenCodePart); len(got) != 1 {
		t.Fatalf("written while the pending's write is held: %q", got)
	}
	free()
	var got []string
	for _, d := range joinedTyped(a, sink, proto.EvOpenCodePart) {
		var p proto.OpenCodePart
		_ = json.Unmarshal([]byte(d), &p)
		got = append(got, p.SessionID+" "+p.CallID+" "+p.Status)
	}
	wantRecords(t, got, []string{"ses_kid c1 pending", "ses_kid c2 running"})
}

func joinedTyped(a *ACP, sink *childSink, typ string) []string {
	a.bg.Wait()
	return sink.typed(typ)
}

// Past maxChildParts opencode.part records, one lux.warning, then records
// with the status alone (and the call's identity and times).
func TestOpenCodeChildPartsCapped(t *testing.T) {
	a, sink := childRun(t, ocSession, 0, nil)
	onBus(t, a, ocSessionCreated("ses_kid", ocSession))
	state := map[string]any{"title": "t", "input": map[string]any{"command": "ls"}, "output": "out", "time": map[string]any{"start": 5, "end": 6}}
	for i := range maxChildParts + 2 {
		onBus(t, a, ocTool("ses_kid", "c"+strconv.Itoa(i), "bash", "completed", state))
	}
	recs := sink.records()[1:]
	if len(recs) != maxChildParts+3 {
		t.Fatalf("%d records", len(recs))
	}
	full := `opencode.part {"sessionID":"ses_kid","messageID":"msg_ses_kid","callID":"c1999","tool":"bash","status":"completed","title":"t","input":"{\"command\":\"ls\"}","output":"out","time":{"start":5,"end":6}}`
	wantRecords(t, recs[maxChildParts-1:], []string{
		full,
		`lux.warning {"message":"opencode: the Run's subagent sessions have made 2000 tool-call records; the next ones carry their status alone"}`,
		`opencode.part {"sessionID":"ses_kid","messageID":"msg_ses_kid","callID":"c2000","tool":"bash","status":"completed","time":{"start":5,"end":6}}`,
		`opencode.part {"sessionID":"ses_kid","messageID":"msg_ses_kid","callID":"c2001","tool":"bash","status":"completed","time":{"start":5,"end":6}}`,
	})
}

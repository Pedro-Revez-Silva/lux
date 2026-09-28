package shim

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marcioapm/lux/internal/proto"
)

func TestRedactEncodings(t *testing.T) {
	r := NewRedactor(map[string]string{"TOKEN": "s3cr3t-value", "SHORT": "ab"})
	for _, in := range []string{
		"s3cr3t-value",
		base64.StdEncoding.EncodeToString([]byte("s3cr3t-value")),
		"s3cr3t%2Dvalue", // not an encoding we produce; must stay
	} {
		out := r.Redact("x " + in + " y")
		if in == "s3cr3t%2Dvalue" {
			continue
		}
		if strings.Contains(out, in) {
			t.Errorf("%q not redacted: %q", in, out)
		}
	}
	if got := r.Redact("ab"); got != "ab" {
		t.Errorf("short values are not redacted, got %q", got)
	}
}

func readRecords(t *testing.T, path string) []proto.Record {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []proto.Record
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 16<<20) // as the runner reads them
	for sc.Scan() {
		var r proto.Record
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// A secret split across two writes is still redacted whole.
func TestOutputRedactsAcrossWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.jsonl")
	o, err := OpenOutput(path, NewRedactor(map[string]string{"K": "supersecretvalue"}))
	if err != nil {
		t.Fatal(err)
	}
	o.Write("stdout", []byte("token=supers"))
	o.Write("stdout", []byte("ecretvalue done\n"))
	o.Event("x", map[string]string{"v": "supersecretvalue"})
	o.Close()
	b, _ := os.ReadFile(path)
	if strings.Contains(string(b), "supersecretvalue") {
		t.Fatalf("secret leaked: %s", b)
	}
	recs := readRecords(t, path)
	if len(recs) != 2 || recs[0].Seq != 1 || recs[1].Seq != 2 {
		t.Fatalf("records: %+v", recs)
	}
}

// A partial line is flushed after a short quiet period, and sequence
// numbers continue when the file is reopened (a runner restart).
func TestOutputFlushesAndContinues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.jsonl")
	o, _ := OpenOutput(path, NewRedactor(nil))
	o.Write("stdout", []byte("no newline"))
	time.Sleep(3 * flushAfter)
	if recs := readRecords(t, path); len(recs) != 1 || recs[0].Data != "no newline" {
		t.Fatalf("not flushed: %+v", recs)
	}
	o.Close()
	o2, _ := OpenOutput(path, NewRedactor(nil))
	o2.Write("stderr", []byte("more\n"))
	o2.Close()
	recs := readRecords(t, path)
	if recs[len(recs)-1].Seq != 2 || recs[1].Ch != "stderr" {
		t.Fatalf("seq did not continue: %+v", recs)
	}
}

// EndLine ends a message streamed in pieces on its own line, and adds
// nothing after text that already ended one.
func TestOutputEndLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.jsonl")
	o, _ := OpenOutput(path, NewRedactor(nil))
	o.Write("stdout", []byte("hel"))
	o.Write("stdout", []byte("lo"))
	o.EndLine("stdout")
	o.Write("stdout", []byte("done\n"))
	o.EndLine("stdout")
	o.EndLine("stderr") // nothing written there: nothing to end
	o.Close()
	var text string
	for _, r := range readRecords(t, path) {
		text += r.Data
	}
	if text != "hello\ndone\n" {
		t.Fatalf("got %q", text)
	}
}

// An input ack repeats what was delivered, capped, and redacts secrets in
// it like all output.
func TestInputAck(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.jsonl")
	red := NewRedactor(map[string]string{"TOKEN": "s3cr3t-value"})
	out, err := OpenOutput(path, red)
	if err != nil {
		t.Fatal(err)
	}
	k := &sink{s: &Shim{out: out, red: red}}
	k.InputAck(proto.Input{RequestID: "prompt", Text: "use s3cr3t-value please"}, nil)
	k.InputAck(proto.Input{RequestID: "big", Text: strings.Repeat("x", maxAckedText+10)}, nil)
	k.InputAck(proto.Input{RequestID: "raw", Raw: []byte("raw bytes\n")}, nil)
	// A secret straddling the cut: redacted whole before cutting.
	k.InputAck(proto.Input{RequestID: "edge", Text: strings.Repeat("y", maxAckedText-5) + "s3cr3t-value"}, nil)
	k.InputAck(proto.Input{}, nil) // no request id: nothing to ack
	out.Close()

	type ack struct {
		RequestID string `json:"requestId"`
		Text      string `json:"text"`
		Truncated bool   `json:"truncated"`
	}
	var acks []ack
	for _, r := range readRecords(t, path) {
		var ev struct {
			Type string `json:"type"`
			Data ack    `json:"data"`
		}
		if r.Ch == "event" && json.Unmarshal(r.Event, &ev) == nil && ev.Type == proto.EvInputAck {
			acks = append(acks, ev.Data)
		}
	}
	if len(acks) != 4 {
		t.Fatalf("got %d acks: %+v", len(acks), acks)
	}
	if acks[0].RequestID != "prompt" || acks[0].Text != "use [REDACTED:TOKEN] please" || acks[0].Truncated {
		t.Errorf("prompt ack: %+v", acks[0])
	}
	if len(acks[1].Text) != maxAckedText || !acks[1].Truncated {
		t.Errorf("big ack: %d bytes, truncated=%v", len(acks[1].Text), acks[1].Truncated)
	}
	if acks[2].Text != "raw bytes\n" {
		t.Errorf("raw ack: %+v", acks[2])
	}
	if strings.Contains(acks[3].Text, "s3cr") || strings.Contains(acks[3].Text, "[REDA") || !acks[3].Truncated {
		t.Errorf("edge ack keeps part of a secret: …%q", acks[3].Text[len(acks[3].Text)-30:])
	}
}

func outputText(t *testing.T, path string) (recs []proto.Record, text string) {
	t.Helper()
	for _, r := range readRecords(t, path) {
		if r.Ch != "event" {
			recs = append(recs, r)
			text += r.Data
		}
	}
	return recs, text
}

// A line past the size limit is cut before a secret it holds whole, not
// through it.
func TestOutputSizeCutKeepsSecretWhole(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.jsonl")
	o, _ := OpenOutput(path, NewRedactor(map[string]string{"K": "supersecretvalue"}))
	pad := strings.Repeat("x", 70000-8)
	o.Write("stdout", []byte(pad+"supersecretvalue tail"))
	o.Close()
	b, _ := os.ReadFile(path)
	if strings.Contains(string(b), "supers") || strings.Contains(string(b), "ecretvalue") {
		t.Fatal("part of the secret leaked")
	}
	if _, text := outputText(t, path); text != pad+"[REDACTED:K] tail" {
		t.Fatalf("got …%q", text[len(text)-40:])
	}
}

// The quiet-period flush holds back a tail that may start a secret, so a
// secret written in two pieces 100+ ms apart is still redacted whole.
func TestOutputTimerHoldsSecretPrefix(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.jsonl")
	o, _ := OpenOutput(path, NewRedactor(map[string]string{"K": "supersecretvalue"}))
	o.Write("stdout", []byte("token=supers"))
	time.Sleep(3 * flushAfter)
	if _, text := outputText(t, path); text != "token=" {
		t.Fatalf("after the quiet period: %q", text)
	}
	o.Write("stdout", []byte("ecretvalue\n"))
	o.Close()
	b, _ := os.ReadFile(path)
	if strings.Contains(string(b), "supers") || strings.Contains(string(b), "ecretvalue") {
		t.Fatalf("part of the secret leaked: %s", b)
	}
	if _, text := outputText(t, path); text != "token=[REDACTED:K]\n" {
		t.Fatalf("got %q", text)
	}
}

// A held tail is not held forever: it goes out after maxHold.
func TestOutputHoldIsBounded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.jsonl")
	o, _ := OpenOutput(path, NewRedactor(map[string]string{"K": "supersecretvalue"}))
	defer o.Close()
	o.Write("stdout", []byte("token=supers"))
	time.Sleep(maxHold + 5*flushAfter)
	if _, text := outputText(t, path); text != "token=supers" {
		t.Fatalf("held past maxHold: %q", text)
	}
}

// A multi-line secret written line by line is redacted whole: newline
// flushes hold back lines that may start it too.
func TestOutputMultiLineSecret(t *testing.T) {
	key := "-----BEGIN KEY-----\nMIIEvQIBADANBg\nkqhkiG9w0BAQEF\n-----END KEY-----\n"
	path := filepath.Join(t.TempDir(), "out.jsonl")
	o, _ := OpenOutput(path, NewRedactor(map[string]string{"KEY": key}))
	o.Write("stdout", []byte("loading\n"))
	for _, l := range strings.SplitAfter(key, "\n") {
		o.Write("stdout", []byte(l))
		time.Sleep(2 * flushAfter)
	}
	o.Write("stdout", []byte("done\n"))
	o.Close()
	b, _ := os.ReadFile(path)
	for _, l := range strings.Split(key, "\n") {
		if l != "" && strings.Contains(string(b), l) {
			t.Fatalf("%q leaked: %s", l, b)
		}
	}
	if _, text := outputText(t, path); text != "loading\n[REDACTED:KEY]done\n" {
		t.Fatalf("got %q", text)
	}
}

// Ordinary lines and partial lines go out without waiting for anything.
func TestOutputPlainIsPrompt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.jsonl")
	o, _ := OpenOutput(path, NewRedactor(map[string]string{"K": "supersecretvalue", "KEY": "-----BEGIN KEY-----\nabc"}))
	defer o.Close()
	o.Write("stdout", []byte("line one\nline two\n"))
	if _, text := outputText(t, path); text != "line one\nline two\n" {
		t.Fatalf("lines not flushed at once: %q", text)
	}
	o.Write("stdout", []byte("50% done"))
	time.Sleep(3 * flushAfter)
	if _, text := outputText(t, path); text != "line one\nline two\n50% done" {
		t.Fatalf("partial line not flushed after the quiet period: %q", text)
	}
}

// A value with & < > in an event is redacted in the form json.Marshal
// writes it.
func TestOutputEventHTMLEscapedSecret(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.jsonl")
	o, _ := OpenOutput(path, NewRedactor(map[string]string{"K": "p&ss<w>rd"}))
	o.Event("x", map[string]string{"v": "p&ss<w>rd"})
	o.Close()
	b, _ := os.ReadFile(path)
	if strings.Contains(string(b), "ss") {
		t.Fatalf("secret leaked: %s", b)
	}
	recs := readRecords(t, path)
	var ev struct {
		Data map[string]string `json:"data"`
	}
	if len(recs) != 1 || json.Unmarshal(recs[0].Event, &ev) != nil || ev.Data["v"] != "[REDACTED:K]" {
		t.Fatalf("records: %s", b)
	}
}

// Streamed text (an agent's reply in chunks) is held like stdout: a secret
// split across chunks is redacted whole, and the events keep their shape.
func TestOutputStreamSplitSecret(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.jsonl")
	o, _ := OpenOutput(path, NewRedactor(map[string]string{"K": "supersecretvalue"}))
	chunk := func(text string) {
		o.Stream("acp.agent_message_chunk", "m1", text, func(t string) any {
			return map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]string{"type": "text", "text": t}}
		})
	}
	for _, c := range []string{"the key is su", "pers", "ecretv", "alue, ok"} {
		chunk(c)
		time.Sleep(2 * flushAfter)
	}
	o.Event("acp.turn_end", map[string]string{"stopReason": "end_turn"})
	o.Close()
	b, _ := os.ReadFile(path)
	for _, part := range []string{"su\"", "pers", "ecretv", "alue"} {
		if strings.Contains(string(b), part) {
			t.Fatalf("%q leaked: %s", part, b)
		}
	}
	var text string
	recs := readRecords(t, path)
	for _, r := range recs[:len(recs)-1] {
		var ev struct {
			Type string `json:"type"`
			Data struct {
				Content struct{ Type, Text string } `json:"content"`
			} `json:"data"`
		}
		if json.Unmarshal(r.Event, &ev) != nil || ev.Type != "acp.agent_message_chunk" || ev.Data.Content.Type != "text" {
			t.Fatalf("record: %s", r.Event)
		}
		text += ev.Data.Content.Text
	}
	if text != "the key is [REDACTED:K], ok" || !strings.Contains(string(recs[len(recs)-1].Event), "turn_end") {
		t.Fatalf("got %q: %s", text, b)
	}
}

// A boundary event (a turn's end) releases everything pending first, held
// tails included, so records stay in the order they were written.
func TestOutputBoundaryReleasesHeld(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.jsonl")
	o, _ := OpenOutput(path, NewRedactor(map[string]string{"K": "supersecretvalue"}))
	o.Write("stdout", []byte("token=supers"))
	o.Stream("x.delta", "", "a su", func(t string) any { return map[string]string{"delta": t} })
	o.Event("x.turn_end", nil)
	o.Close()
	recs := readRecords(t, path)
	// "a " is safe at once; the held "su" and "token=supers" wait for the
	// event.
	if len(recs) != 4 || !strings.Contains(string(recs[0].Event), `"a "`) || recs[1].Data != "token=supers" ||
		!strings.Contains(string(recs[2].Event), `"su"`) || !strings.Contains(string(recs[3].Event), "x.turn_end") {
		t.Fatalf("records: %+v", recs)
	}
}

// Any other event mid-reply (an input ack, say) releases pending output
// only up to a safe cut: the start of a secret stays held, and the secret
// is redacted whole when the rest arrives.
func TestOutputEventMidStreamHoldsPrefix(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.jsonl")
	o, _ := OpenOutput(path, NewRedactor(map[string]string{"K": "supersecretvalue"}))
	wrap := func(t string) any { return map[string]string{"delta": t} }
	o.Write("stdout", []byte("token=supers"))
	o.Stream("x.delta", "", "a su", wrap)
	o.Event(proto.EvInputAck, map[string]string{"requestId": "r1"})
	o.Event(proto.EvActivity, map[string]string{"activity": "busy"})
	o.Write("stdout", []byte("ecretvalue\n"))
	o.Stream("x.delta", "", "persecretvalue.", wrap)
	o.Close()
	b, _ := os.ReadFile(path)
	if strings.Contains(string(b), "supers") || strings.Contains(string(b), `"su"`) || strings.Contains(string(b), "ecretvalue") {
		t.Fatalf("part of the secret leaked: %s", b)
	}
	var out, deltas string
	for _, r := range readRecords(t, path) {
		var ev struct {
			Data struct{ Delta string } `json:"data"`
		}
		out += r.Data
		if json.Unmarshal(r.Event, &ev) == nil {
			deltas += ev.Data.Delta
		}
	}
	if out != "token=[REDACTED:K]\n" || deltas != "a [REDACTED:K]." {
		t.Fatalf("stdout %q, deltas %q", out, deltas)
	}
}

// Interleaved messages are held apart: another message's chunk does not
// release one's held tail.
func TestOutputStreamInterleaved(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.jsonl")
	o, _ := OpenOutput(path, NewRedactor(map[string]string{"K": "supersecretvalue"}))
	wrap := func(key string) func(string) any {
		return func(t string) any { return map[string]string{"item": key, "delta": t} }
	}
	o.Stream("T", "A", "key=supers", wrap("A"))
	o.Stream("T", "B", "other", wrap("B"))
	o.Stream("T", "A", "ecretvalue\n", wrap("A"))
	o.Close()
	b, _ := os.ReadFile(path)
	if strings.Contains(string(b), "supers") || strings.Contains(string(b), "ecretvalue") {
		t.Fatalf("part of the secret leaked: %s", b)
	}
	text := map[string]string{}
	for _, r := range readRecords(t, path) {
		var ev struct {
			Data struct{ Item, Delta string } `json:"data"`
		}
		_ = json.Unmarshal(r.Event, &ev)
		text[ev.Data.Item] += ev.Data.Delta
	}
	if text["A"] != "key=[REDACTED:K]\n" || text["B"] != "other" {
		t.Fatalf("got %q", text)
	}
}

// Past maxStreams messages in flight, the oldest is released whole.
func TestOutputStreamCap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.jsonl")
	o, _ := OpenOutput(path, NewRedactor(map[string]string{"K": "supersecretvalue"}))
	defer o.Close()
	for i := range maxStreams + 1 {
		o.Stream("T", fmt.Sprint(i), "su", func(t string) any { return map[string]string{"delta": t} })
	}
	o.mu.Lock()
	n, first := o.streams, o.order[0].id
	o.mu.Unlock()
	if recs := readRecords(t, path); n != maxStreams || len(recs) != 1 || first != "event:T\x001" {
		t.Fatalf("%d buffers, first %q, records %+v", n, first, recs)
	}
}

// A chunk with empty text is written as it came when nothing is held.
func TestOutputStreamEmptyChunk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.jsonl")
	o, _ := OpenOutput(path, NewRedactor(map[string]string{"K": "supersecretvalue"}))
	o.Stream("T", "A", "", func(t string) any { return map[string]string{"delta": t} })
	o.Close()
	if recs := readRecords(t, path); len(recs) != 1 || string(recs[0].Event) != `{"data":{"delta":""},"type":"T"}` {
		t.Fatalf("records: %+v", recs)
	}
}

// A buffer covered by values wall to wall has no safe cut: it is still
// released, redacted as it is, at the size limit and after maxHold.
func TestOutputCoveredBufferIsBounded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.jsonl")
	o, _ := OpenOutput(path, NewRedactor(map[string]string{"K": "abab"}))
	defer o.Close()
	// "abab…": every cut is inside a match, and every tail a prefix.
	o.Write("stdout", []byte(strings.Repeat("ab", flushBytes/2+10)))
	recs, _ := outputText(t, path)
	if len(recs) != 1 || recs[0].Data != "[REDACTED:K]" {
		t.Fatalf("not released at the size limit: %+v", recs)
	}
	// A trickle never lets the quiet timer fire.
	start := time.Now()
	for time.Since(start) < maxHold+5*flushAfter {
		o.Write("stdout", []byte("ab"))
		time.Sleep(flushAfter / 2)
	}
	if recs, _ := outputText(t, path); len(recs) < 2 {
		t.Fatalf("held past maxHold: %+v", recs)
	}
}

// No release splits a character, on any path.
func TestOutputKeepsCharactersWhole(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.jsonl")
	o, _ := OpenOutput(path, NewRedactor(map[string]string{"K": "supersecretvalue"}))
	e := []byte("é")
	o.Write("stdout", append([]byte("caf"), e[0])) // timer path
	time.Sleep(3 * flushAfter)
	o.Write("stdout", e[1:])
	o.Write("stdout", append([]byte(" x"), e[0])) // event path
	o.Event("x", nil)
	o.Write("stdout", e[1:])
	big := append([]byte(strings.Repeat("y", flushBytes-1)), e[0]) // size path
	o.Write("stdout", big)
	o.Write("stdout", append(e[1:], '\n'))
	o.Close()
	b, _ := os.ReadFile(path)
	if strings.Contains(string(b), "\\ufffd") || strings.Contains(string(b), "�") {
		t.Fatalf("a character was split")
	}
	if _, text := outputText(t, path); text != "café xé"+strings.Repeat("y", flushBytes-1)+"é\n" {
		t.Fatalf("got …%q", text[max(0, len(text)-20):])
	}
}

func BenchmarkOutputLines(b *testing.B) {
	secrets := map[string]string{}
	for i := range 50 {
		secrets[fmt.Sprintf("S%d", i)] = fmt.Sprintf("secret-%02d-%s", i, strings.Repeat("q", 24))
	}
	secrets["PEM"] = "-----BEGIN PRIVATE KEY-----\n" + strings.Repeat(strings.Repeat("M", 64)+"\n", 50) + "-----END PRIVATE KEY-----\n"
	line := []byte(strings.Repeat("l", 79) + "\n")
	o, _ := OpenOutput(filepath.Join(b.TempDir(), "out.jsonl"), NewRedactor(secrets))
	defer o.Close()
	b.SetBytes(int64(1000 * len(line)))
	for b.Loop() {
		for range 1000 {
			o.Write("stdout", line)
		}
	}
}

// Two writers on one channel — the beforeStop hook beside a workload still
// writing — each keep their own partial line: nothing is spliced into
// another's record.
func TestOutputSecondWriterKeepsItsOwnLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.jsonl")
	o, _ := OpenOutput(path, NewRedactor(nil))
	o.Write("stdout", []byte("progress 4"))
	o.WriteAs("beforeStop:stdout", "stdout", []byte("hook-ran\n"))
	o.Write("stdout", []byte("0%\n"))
	o.Close()
	var lines []string
	for _, r := range readRecords(t, path) {
		if r.Ch != "stdout" {
			t.Fatalf("a record on %q, not stdout", r.Ch)
		}
		lines = append(lines, r.Data)
	}
	if strings.Join(lines, "|") != "hook-ran\n|progress 40%\n" {
		t.Fatalf("got %q", lines)
	}
}

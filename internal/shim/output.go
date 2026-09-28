package shim

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/marcioapm/lux/internal/proto"
)

// Output writes a placement's records to its output file: one JSON line
// each, sequenced, redacted. The file is the record: the runner tails it
// to relay live output and uploads it when the placement ends.
//
// stdout and stderr are buffered per channel up to a newline (or 64 KiB,
// or 100 ms of quiet), and streamed text events (Stream) per type and
// key, so a secret split across two writes is still redacted whole. No
// release but a forced one cuts inside a secret, or before a tail that may
// be the start of one (a value may span lines); such a tail waits for more
// bytes, up to maxHold or flushBytes, or until a boundary event (a turn's
// end, see boundary) or Close.
type Output struct {
	mu sync.Mutex
	// lastByte per stream channel: EndLine uses it to know whether the
	// channel is mid-line.
	lastByte map[string]byte
	f        *os.File
	w        *bufio.Writer
	seq      int64
	red      *Redactor
	bufs     map[string]*chanBuf
	order    []*chanBuf // bufs, in creation order: releases are in a stable order
	streams  int        // stream buffers in bufs
	closed   bool
}

type chanBuf struct {
	id string
	// ch is the channel its records go out on; id alone when empty. A
	// second writer on a channel (the beforeStop hook beside the workload)
	// holds its own partial line, so the two never splice into one record.
	ch   string
	data []byte
	// Streamed text (Stream): released as a typ event, built by wrap from
	// the text.
	typ   string
	wrap  func(text string) any
	timer *time.Timer
	// since the buffer last made progress: once it is maxHold old, it is
	// released whole.
	since time.Time
}

const (
	flushBytes = 64 << 10
	flushAfter = 100 * time.Millisecond
	maxHold    = 2 * time.Second
	// maxStreams bounds the stream buffers held at once (messages in
	// flight); past it the oldest is released whole.
	maxStreams = 64
)

// OpenOutput opens (or continues) an output file.
func OpenOutput(path string, red *Redactor) (*Output, error) {
	var seq int64
	if f, err := os.Open(path); err == nil {
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64<<10), 16<<20)
		for sc.Scan() {
			var r struct {
				Seq int64 `json:"seq"`
			}
			if json.Unmarshal(sc.Bytes(), &r) == nil && r.Seq > seq {
				seq = r.Seq
			}
		}
		f.Close()
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	return &Output{f: f, w: bufio.NewWriterSize(f, 64<<10), seq: seq, red: red, bufs: map[string]*chanBuf{}, lastByte: map[string]byte{}}, nil
}

func (o *Output) Seq() int64 {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.seq
}

// Write buffers bytes for a stream channel (stdout | stderr).
func (o *Output) Write(ch string, p []byte) { o.WriteAs(ch, ch, p) }

// WriteAs buffers bytes on channel ch under its own buffer id, so a second
// writer's partial line is held apart from the first's.
func (o *Output) WriteAs(id, ch string, p []byte) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return
	}
	b := o.buf(id)
	if b.ch == "" && id != ch {
		b.ch = ch
	}
	if len(p) > 0 {
		o.lastByte[id] = p[len(p)-1]
	}
	o.add(b, p)
	// Flush complete lines now, keeping a partial last line.
	if i := bytes.LastIndexByte(b.data, '\n'); i >= 0 {
		o.flushTo(b, i+1)
	}
	o.bound(b)
	o.w.Flush()
}

// Stream writes streamed text (an agent's reply, token by token) as typ
// events, each carrying wrap(text) for the text released so far. Text is
// held like stdout, per typ and key (one message: interleaved messages are
// held apart), so a secret split across chunks is redacted whole; events
// may be coalesced.
func (o *Output) Stream(typ, key, text string, wrap func(text string) any) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return
	}
	id := "event:" + typ + "\x00" + key
	b := o.bufs[id]
	if text == "" && (b == nil || len(b.data) == 0) {
		// Nothing to hold: the event as it came.
		o.event(typ, wrap(""))
		o.w.Flush()
		return
	}
	if b == nil && o.streams == maxStreams {
		for _, old := range o.order {
			if old.wrap != nil {
				o.release(old, true)
				o.drop(old)
				break
			}
		}
	}
	b = o.buf(id)
	b.typ, b.wrap = typ, wrap
	o.add(b, []byte(text))
	o.flushTo(b, len(b.data))
	o.bound(b)
	o.w.Flush()
}

// buf must be called with mu held.
func (o *Output) buf(id string) *chanBuf {
	b := o.bufs[id]
	if b == nil {
		b = &chanBuf{id: id}
		o.bufs[id] = b
		o.order = append(o.order, b)
		if strings.HasPrefix(id, "event:") {
			o.streams++
		}
	}
	return b
}

// drop forgets an empty stream buffer: message keys do not recur for long.
// Must be called with mu held.
func (o *Output) drop(b *chanBuf) {
	if b.wrap == nil || len(b.data) > 0 || o.bufs[b.id] != b {
		return
	}
	if b.timer != nil {
		b.timer.Stop()
	}
	delete(o.bufs, b.id)
	o.order = slices.DeleteFunc(o.order, func(x *chanBuf) bool { return x == b })
	o.streams--
}

// add must be called with mu held.
func (o *Output) add(b *chanBuf, p []byte) {
	if len(b.data) == 0 {
		b.since = time.Now()
	}
	b.data = append(b.data, p...)
}

// bound releases what is held too long or too much, and arms the quiet
// timer for the rest. A trickle of writes keeps pushing the timer back, so
// the age is checked here too. Must be called with mu held.
func (o *Output) bound(b *chanBuf) {
	if len(b.data) >= flushBytes {
		o.flushTo(b, len(b.data))
	}
	// Covered wall to wall by values (or a prefix of one) that far: no
	// safe cut, so redact it as it is.
	if len(b.data) >= flushBytes || len(b.data) > 0 && time.Since(b.since) >= maxHold {
		o.release(b, false)
	}
	if len(b.data) == 0 || o.bufs[b.id] != b {
		return
	}
	if b.timer == nil {
		id := b.id
		b.timer = time.AfterFunc(flushAfter, func() { o.flushChan(id) })
	} else {
		b.timer.Reset(flushAfter)
	}
}

func (o *Output) flushChan(id string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	b := o.bufs[id]
	if o.closed || b == nil || len(b.data) == 0 {
		return
	}
	if time.Since(b.since) >= maxHold {
		n := len(b.data)
		o.release(b, false)
		if len(b.data) == n {
			// Only a partial character, that old: it will not complete.
			o.release(b, true)
		}
	} else {
		o.flushTo(b, len(b.data))
	}
	if len(b.data) > 0 {
		// Held back as a possible secret: released when it is due.
		b.timer.Reset(max(flushAfter, maxHold-time.Since(b.since)))
	}
	o.drop(b)
	o.w.Flush()
}

// flushTo records the channel's buffer up to end, or up to the last point
// before it where the cut splits neither a secret nor a character. Must be
// called with mu held.
func (o *Output) flushTo(b *chanBuf, end int) {
	end = min(end, completeRunes(b.data))
	for {
		c := utf8Boundary(b.data, o.red.Cut(b.data, end))
		if c == end {
			break
		}
		end = c
	}
	o.emit(b, end)
}

// release records the channel's buffer whole, redacted as it is; all but a
// last partial character, unless all. Must be called with mu held.
func (o *Output) release(b *chanBuf, all bool) {
	end := len(b.data)
	if !all {
		end = completeRunes(b.data)
	}
	o.emit(b, end)
}

// emit records b.data[:end]. Must be called with mu held.
func (o *Output) emit(b *chanBuf, end int) {
	if end <= 0 {
		return
	}
	if b.wrap != nil {
		o.event(b.typ, b.wrap(string(b.data[:end])))
	} else {
		ch := b.id
		if b.ch != "" {
			ch = b.ch
		}
		o.record(ch, b.data[:end], nil)
	}
	b.data = append(b.data[:0], b.data[end:]...)
	b.since = time.Now()
	o.drop(b)
}

// EndLine ends the channel's current line, if it is mid-line: a message
// streamed in pieces then reads as one line, whoever wrote the pieces.
func (o *Output) EndLine(ch string) {
	o.mu.Lock()
	mid := o.lastByte[ch] != 0 && o.lastByte[ch] != '\n'
	o.mu.Unlock()
	if mid {
		o.Write(ch, []byte("\n"))
	}
}

// Event writes a structured event record. Pending output is released
// first so records stay in order: all of it at a boundary (an event that
// ends any text in progress), else up to a safe cut, so an event mid-reply
// does not release the start of a secret.
func (o *Output) Event(typ string, data any) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return
	}
	all := boundary(typ, data)
	for _, b := range slices.Clone(o.order) {
		if all {
			o.release(b, false)
		} else {
			o.flushTo(b, len(b.data))
		}
	}
	o.event(typ, data)
	o.w.Flush()
}

// boundary reports whether an event ends any text in progress: a turn's
// end, a whole Claude message, the agent going idle, the init process's
// exit.
func boundary(typ string, data any) bool {
	switch {
	case strings.HasSuffix(typ, ".turn_end"), typ == "codex.turn/completed", typ == "claude.result":
		return true
	case typ == "claude.assistant":
		// Claude Code streams one message at a time, and sends it whole
		// once done: its streamed text ends here. (Codex items run in
		// parallel, so an item's completion is no boundary for the others.)
		return true
	case typ == proto.EvActivity:
		d, _ := data.(map[string]string)
		return d["activity"] == "idle"
	case typ == proto.EvInit:
		d, _ := data.(map[string]any)
		return d["phase"] == "done"
	}
	return false
}

// event records an event, redacted as marshalled: values are matched in
// their JSON-escaped forms too (see Redactor.Set). Must be called with mu
// held.
func (o *Output) event(typ string, data any) {
	raw, err := json.Marshal(data)
	if err != nil {
		raw = []byte(`{}`)
	}
	ev, _ := json.Marshal(map[string]json.RawMessage{"type": mustJSON(typ), "data": raw})
	o.record("event", nil, json.RawMessage(o.red.Redact(string(ev))))
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

// record must be called with mu held.
func (o *Output) record(ch string, data []byte, event json.RawMessage) {
	o.seq++
	r := proto.Record{Seq: o.seq, Time: time.Now().UnixMilli(), Ch: ch}
	if event != nil {
		if !json.Valid(event) {
			event = mustJSON(string(event))
		}
		r.Event = event
	} else {
		r.Data = o.red.Redact(strings.ToValidUTF8(string(data), "�"))
	}
	b, _ := json.Marshal(r)
	o.w.Write(b)
	o.w.WriteByte('\n')
}

// Close flushes everything and closes the file.
func (o *Output) Close() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return nil
	}
	for _, b := range slices.Clone(o.order) {
		if b.timer != nil {
			b.timer.Stop()
		}
		o.release(b, true)
	}
	o.closed = true
	o.w.Flush()
	o.f.Sync()
	return o.f.Close()
}

// completeRunes is len(b) less a last character not yet written whole.
func completeRunes(b []byte) int {
	for i := len(b) - 1; i >= 0 && i >= len(b)-utf8.UTFMax; i-- {
		if utf8.RuneStart(b[i]) {
			if utf8.FullRune(b[i:]) {
				return len(b)
			}
			return i
		}
	}
	return len(b)
}

// utf8Boundary moves i back to the start of the character it is in.
func utf8Boundary(b []byte, i int) int {
	for i > 0 && i < len(b) && !utf8.RuneStart(b[i]) {
		i--
	}
	return i
}

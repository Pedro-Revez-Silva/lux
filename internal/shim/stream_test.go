package shim

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"

	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/vterm"
)

// ptyTerminal is a terminal on a fresh PTY pair: what is written to the
// returned tty is what a workload writes. Its output is read as the
// output file reads it, into read (when not nil).
func ptyTerminal(t *testing.T, rows, cols, history int, read func([]byte)) (*terminal, *os.File, chan struct{}) {
	t.Helper()
	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Skipf("no pty: %v", err)
	}
	if err := pty.Setsize(ptmx, winsize(rows, cols)); err != nil {
		t.Fatal(err)
	}
	term := newTerminal(ptmx, rows, cols)
	term.screen = vterm.New(rows, cols, history)
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 32<<10)
		for {
			n, err := term.Read(buf)
			if n > 0 && read != nil {
				read(buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() {
		tty.Close()
		ptmx.Close()
		<-done
	})
	return term, tty, done
}

// screenOf returns the terminal's emulator state as a snapshot, under its
// lock.
func screenOf(term *terminal) []byte {
	term.mu.Lock()
	defer term.mu.Unlock()
	return term.screen.Snapshot(0)
}

// waitScreen waits until the emulator shows want: all written before it
// has been read.
func waitScreen(t *testing.T, term *terminal, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if bytes.Contains(screenOf(term), []byte(want)) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("the terminal never showed %q", want)
}

// replay is a fresh emulator fed a snapshot and the output after it.
func replay(rows, cols, history int, chunks ...[]byte) *vterm.Terminal {
	v := vterm.New(rows, cols, history)
	for _, c := range chunks {
		v.Write(c)
	}
	return v
}

// liveUntil reads sub until what it read contains want.
func liveUntil(t *testing.T, sub *subscriber, want string) []byte {
	t.Helper()
	type res struct {
		b   []byte
		err error
	}
	var live []byte
	for !bytes.Contains(live, []byte(want)) {
		ch := make(chan res, 1)
		go func() { b, err := sub.next(); ch <- res{b, err} }()
		select {
		case r := <-ch:
			if r.err != nil {
				t.Fatalf("live output %q, then %v", live, r.err)
			}
			live = append(live, r.b...)
		case <-time.After(10 * time.Second):
			t.Fatalf("live output %q", live)
		}
	}
	return live
}

// attachOverPipe runs the shim's attach stream on term, as the runner
// opens it, and returns the runner's end of it.
func attachOverPipe(t *testing.T, term *terminal, restore bool) (net.Conn, *json.Decoder) {
	t.Helper()
	s := &Shim{term: term}
	runnerSide, shimSide := net.Pipe()
	t.Cleanup(func() { runnerSide.Close() })
	go func() {
		defer shimSide.Close()
		s.handleStream(bufio.NewScanner(shimSide), json.NewEncoder(shimSide), proto.StreamOpen{Kind: "attach", Restore: restore})
	}()
	return runnerSide, json.NewDecoder(runnerSide)
}

// readFrame decodes the next StreamData from the runner's end of a stream.
func readFrame(conn net.Conn, dec *json.Decoder) (proto.StreamData, error) {
	var d proto.StreamData
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	err := dec.Decode(&d)
	return d, err
}

func subscriberCount(term *terminal) int {
	term.mu.Lock()
	defer term.mu.Unlock()
	return len(term.subs)
}

// waitJoined waits until term has one subscriber: the stream has joined.
func waitJoined(t *testing.T, term *terminal) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for subscriberCount(term) != 1 {
		if time.Now().After(deadline) {
			t.Fatal("the stream never joined")
		}
		time.Sleep(time.Millisecond)
	}
}

// writePastBudget writes lines of c to tty until more than a restore
// subscriber's budget, and 1 MiB more, has been written.
func writePastBudget(tty *os.File, c string) {
	line := strings.Repeat(c, 79) + "\r\n"
	for written := 0; written <= restoreBudget+(1<<20); written += len(line) {
		io.WriteString(tty, line)
	}
}

func isJoined(term *terminal, sub *subscriber) bool {
	term.mu.Lock()
	defer term.mu.Unlock()
	return term.subs[sub]
}

func emulatorSize(term *terminal) (rows, cols int) {
	term.mu.Lock()
	defer term.mu.Unlock()
	return term.screen.(*vterm.Terminal).Size()
}

func screenText(v *vterm.Terminal) string {
	rows, cols := v.Size()
	var b strings.Builder
	for y := range rows {
		var l strings.Builder
		for x := range cols {
			l.WriteString(v.Text(v.Cell(x, y)))
		}
		b.WriteString(strings.TrimRight(l.String(), " "))
		b.WriteByte('\n')
	}
	return b.String()
}

func TestAttachRestoresPlainScreen(t *testing.T) {
	term, tty, _ := ptyTerminal(t, 6, 40, historyLines, nil)
	fmt.Fprint(tty, "\x1b]2;my title\x07$ echo hi\r\nhi\r\n$ ")
	waitScreen(t, term, "$ echo hi")

	snap, sub := term.join(true, 0, 0)
	defer term.leave(sub)
	v := replay(6, 40, historyLines, snap)
	if got := screenText(v); got != "$ echo hi\nhi\n$\n\n\n\n" {
		t.Fatalf("restored screen %q", got)
	}
	if x, y, _ := v.Cursor(); x != 2 || y != 2 {
		t.Fatalf("cursor %d,%d", x, y)
	}
	if v.Title() != "my title" {
		t.Fatalf("title %q", v.Title())
	}
	// Without restore, no snapshot: output from now on, as always.
	if snap, sub := term.join(false, 0, 0); snap != nil {
		t.Fatalf("a snapshot without restore: %q", snap)
	} else {
		term.leave(sub)
	}
}

func TestAttachRestoreScrollbackBounded(t *testing.T) {
	term, tty, _ := ptyTerminal(t, 5, 20, 100, nil)
	for i := range 1000 {
		fmt.Fprintf(tty, "line %d\r\n", i)
	}
	fmt.Fprint(tty, "end")
	waitScreen(t, term, "end")
	snap, sub := term.join(true, 0, 0)
	defer term.leave(sub)
	v := replay(5, 20, 100000, snap)
	if n := v.HistoryLen(); n != 100 {
		t.Fatalf("history %d lines, want the limit, 100", n)
	}
	first := lineText(v, v.HistoryLine(0))
	// 1000 lines then "end": 996–999 and "end" on screen, 100 before.
	if first != "line 896" {
		t.Fatalf("oldest history line %q", first)
	}
}

func TestAttachRestoresAltScreen(t *testing.T) {
	term, tty, _ := ptyTerminal(t, 8, 40, historyLines, nil)
	fmt.Fprint(tty, "$ make\r\nbuilt ok\r\n$ top\r\n")
	fmt.Fprint(tty, "\x1b[?1049h\x1b[?1h\x1b=\x1b[?1000h\x1b[?1006h\x1b[2J\x1b[H\x1b[7m PID  CPU \x1b[m\x1b[3;1H  42  99%")
	waitScreen(t, term, "  42  99%")

	snap, sub := term.join(true, 0, 0)
	defer term.leave(sub)
	v := replay(8, 40, historyLines, snap)
	if !v.AltScreen() || !v.Mode(1049) || !v.Mode(1) || !v.Mode(1000) || !v.Mode(1006) {
		t.Fatalf("alt screen %v, modes 1049 %v 1 %v 1000 %v 1006 %v", v.AltScreen(), v.Mode(1049), v.Mode(1), v.Mode(1000), v.Mode(1006))
	}
	if got := screenText(v); !strings.HasPrefix(got, " PID  CPU\n\n  42  99%\n") {
		t.Fatalf("alt screen %q", got)
	}
	if c := v.Cell(1, 0); c.Style.Attr&vterm.AttrReverse == 0 {
		t.Fatalf("header not reverse: %+v", c)
	}

	// The program exits the full screen: the client, fed only the live
	// output after its snapshot, is back on the main screen.
	fmt.Fprint(tty, "\x1b[?1000l\x1b[?1006l\x1b[?1l\x1b>\x1b[?1049l$ ")
	live := liveUntil(t, sub, "1049l$ ")
	v.Write(live)
	if v.AltScreen() || v.Mode(1000) || v.Mode(1) {
		t.Fatal("still on the alternate screen, or its modes")
	}
	if got := screenText(v); !strings.HasPrefix(got, "$ make\nbuilt ok\n$ top\n$\n") {
		t.Fatalf("main screen after leaving %q", got)
	}
	if !bytes.Equal(v.Snapshot(0), screenOf(term)) {
		t.Fatal("the client's terminal and the shim's differ")
	}
}

func TestAttachRestoreResizesFirst(t *testing.T) {
	term, tty, _ := ptyTerminal(t, 24, 80, historyLines, nil)
	fmt.Fprint(tty, "hello")
	waitScreen(t, term, "hello")
	snap, sub := term.join(true, 10, 30)
	defer term.leave(sub)
	ws, err := pty.GetsizeFull(term.ptmx)
	if err != nil {
		t.Fatal(err)
	}
	if ws.Rows != 10 || ws.Cols != 30 {
		t.Fatalf("pty %dx%d, want 10x30", ws.Rows, ws.Cols)
	}
	v := replay(10, 30, historyLines, snap)
	if !bytes.Equal(v.Snapshot(0), screenOf(term)) {
		t.Fatal("snapshot not of the new size")
	}
	term.resize(12, 50)
	if r, c := emulatorSize(term); r != 12 || c != 50 {
		t.Fatalf("emulator %dx%d after a resize, want 12x50", r, c)
	}
}

// A size over the emulator's limits is clamped before the PTY is resized,
// on join and in-band, so the PTY and the emulator agree.
func TestResizeClampedForPTYAndEmulator(t *testing.T) {
	term, _, _ := ptyTerminal(t, 24, 80, historyLines, nil)
	check := func(what string) {
		t.Helper()
		ws, err := pty.GetsizeFull(term.ptmx)
		if err != nil {
			t.Fatal(err)
		}
		r, c := emulatorSize(term)
		if int(ws.Rows) != vterm.MaxRows || int(ws.Cols) != vterm.MaxCols || r != vterm.MaxRows || c != vterm.MaxCols {
			t.Fatalf("%s: pty %dx%d, emulator %dx%d, want both %dx%d", what, ws.Rows, ws.Cols, r, c, vterm.MaxRows, vterm.MaxCols)
		}
	}
	_, sub := term.join(true, 70000, 70000)
	term.leave(sub)
	check("join")
	term.resize(24, 80)
	term.resize(1000, 100000)
	check("in-band")
}

// Attaches during a fast writer: each one's snapshot and the live output
// after it, replayed, are exactly the shim's terminal at the end — no
// output lost or repeated between them.
func TestAttachRestoreNoGapNoDuplicate(t *testing.T) {
	// A history whose full snapshot (at most ~70 bytes a line) stays under
	// snapshotLimit however long the writer runs, so every snapshot is
	// whole and comparable byte for byte with the shim's.
	const rows, cols, history = 8, 40, 20000
	term, tty, _ := ptyTerminal(t, rows, cols, history, nil)

	type attached struct {
		snap   []byte
		sub    *subscriber
		live   bytes.Buffer
		behind bool
		done   chan struct{}
	}
	var mu sync.Mutex
	var all []*attached
	attach := func() {
		a := &attached{done: make(chan struct{})}
		a.snap, a.sub = term.join(true, 0, 0)
		go func() {
			defer close(a.done)
			for {
				b, err := a.sub.next()
				if err != nil {
					a.behind = errors.Is(err, errFellBehind)
					return
				}
				a.live.Write(b)
			}
		}()
		mu.Lock()
		all = append(all, a)
		mu.Unlock()
	}

	// The writer goes on until every attach has happened, so all of them
	// land while output is flowing.
	const attaches = 40
	attachesDone := make(chan struct{})
	writerDone := make(chan struct{})
	written := 0
	go func() {
		defer close(writerDone)
		for i := 0; ; i++ {
			select {
			case <-attachesDone:
				fmt.Fprint(tty, "THE END")
				return
			default:
			}
			// Colours and cursor movement too, so a byte out of place shows.
			fmt.Fprintf(tty, "\x1b[3%dm%05d\x1b[m %s\r\n", i%8, i, strings.Repeat("x", i%30))
			if i%500 == 0 {
				fmt.Fprint(tty, "\x1b[2;3H\x1b[K*\x1b[8;1H")
			}
			written = i
		}
	}()
	for i := range attaches {
		attach()
		time.Sleep(time.Duration(i%3) * time.Millisecond)
	}
	close(attachesDone)
	<-writerDone
	waitScreen(t, term, "THE END")
	// Everything read has been queued to every subscriber still joined:
	// leaving now ends each one after what it was sent.
	for _, a := range all {
		term.leave(a.sub)
	}
	final := screenOf(term)
	if len(final) > snapshotLimit {
		t.Fatalf("the full terminal is %d bytes, over snapshotLimit: snapshots would leave history out", len(final))
	}
	ended := 0
	for i, a := range all {
		<-a.done
		if a.behind {
			// Ended rather than continued with a gap: a client attaches again.
			ended++
			continue
		}
		v := replay(rows, cols, history, a.snap, a.live.Bytes())
		if got := v.Snapshot(0); !bytes.Equal(got, final) {
			t.Fatalf("attach %d of %d (snapshot %d bytes, live %d): replayed terminal differs from the shim's", i, len(all), len(a.snap), a.live.Len())
		}
	}
	if ended == len(all) {
		t.Fatalf("all %d attaches fell behind: nothing was compared", ended)
	}
	if written < 100 {
		t.Fatalf("only %d lines written during the attaches", written)
	}
	t.Logf("%d attaches, %d ended as fallen behind", len(all), ended)
}

// A subscriber that never reads holds up neither the workload's output
// nor the reader that feeds the output file and the emulator, plain or
// restore, past a restore subscriber's budget.
func TestSlowSubscriberNeverBlocks(t *testing.T) {
	for _, restore := range []bool{false, true} {
		t.Run(fmt.Sprint("restore=", restore), func(t *testing.T) {
			var n int64
			var nmu sync.Mutex
			term, tty, _ := ptyTerminal(t, 24, 80, historyLines, func(b []byte) {
				nmu.Lock()
				n += int64(len(b))
				nmu.Unlock()
			})
			_, sub := term.join(restore, 0, 0)
			defer term.leave(sub)
			line := strings.Repeat("y", 79) + "\n"
			total := 0
			done := make(chan struct{})
			go func() {
				defer close(done)
				for total <= restoreBudget+(1<<20) {
					io.WriteString(tty, line)
					total += len(line) + 1 // ONLCR: \n goes out as \r\n
				}
				io.WriteString(tty, "DONE")
			}()
			select {
			case <-done:
			case <-time.After(60 * time.Second):
				t.Fatal("the workload's writes blocked")
			}
			waitScreen(t, term, "DONE")
			nmu.Lock()
			defer nmu.Unlock()
			if n != int64(total+4) {
				t.Fatalf("output read %d bytes, want %d", n, total+4)
			}
		})
	}
}

// A restore attach whose client does not read while more than its budget
// is written is ended with the fell-behind error once its snapshot is
// sent, not continued with a gap.
func TestRestoreAttachFallenBehindIsEnded(t *testing.T) {
	term, tty, _ := ptyTerminal(t, 24, 80, historyLines, nil)
	fmt.Fprint(tty, "before")
	waitScreen(t, term, "before")
	runnerSide, dec := attachOverPipe(t, term, true)
	waitJoined(t, term)
	// The stream's sender is blocked on its first snapshot frame (net.Pipe
	// is unbuffered) while the workload writes past the budget.
	writePastBudget(tty, "z")
	fmt.Fprint(tty, "DONE")
	waitScreen(t, term, "DONE")
	if joined := subscriberCount(term); joined != 0 {
		t.Fatalf("%d subscribers still joined", joined)
	}

	var out []byte
	for {
		d, err := readFrame(runnerSide, dec)
		if err != nil {
			t.Fatalf("the stream ended without an error, after %d bytes: %v", len(out), err)
		}
		if d.Error != "" {
			if d.Error != "attach fell behind; attach again" {
				t.Fatalf("error %q", d.Error)
			}
			break
		}
		out = append(out, d.Data...)
	}
	// What came before the error is the snapshot only: nothing written
	// after the join, which would be a screen with a gap.
	if bytes.Contains(out, []byte("zzz")) || !bytes.Contains(out, []byte("before")) {
		t.Fatalf("stream before the error: %q", out)
	}
	// Then the stream is closed: nothing more, not even from the client.
	if d, err := readFrame(runnerSide, dec); !errors.Is(err, io.EOF) {
		t.Fatalf("after the error: %+v, %v; want the stream closed", d, err)
	}
}

// The most 1-byte reads a restore subscriber may hold: each is charged
// entryCost on top of its byte.
const maxTinyEntries = restoreBudget / (entryCost + 1)

// A restore subscriber holds maxTinyEntries 1-byte reads, not
// restoreBudget of them; reading them frees their whole charge.
func TestRestoreSubscriberChargesEachRead(t *testing.T) {
	s := newSubscriber(true)
	for round := range 2 {
		for i := range maxTinyEntries {
			if !s.offer([]byte{'x'}) {
				t.Fatalf("round %d: ended after %d 1-byte reads, want %d", round, i, maxTinyEntries)
			}
		}
		if round == 0 {
			for range maxTinyEntries {
				if _, err := s.next(); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	if s.offer([]byte{'x'}) {
		t.Fatalf("a read past %d accepted", maxTinyEntries)
	}
	if _, err := s.next(); !errors.Is(err, errFellBehind) {
		t.Fatalf("next after the overflow: %v", err)
	}
}

// A stalled restore attach on a workload writing a byte at a time holds
// at most maxTinyEntries reads at any moment, and is ended.
func TestStalledRestoreAttachTinyWrites(t *testing.T) {
	term, tty, _ := ptyTerminal(t, 24, 80, historyLines, nil)
	snap, sub := term.join(true, 0, 0)
	defer term.leave(sub)
	if snap == nil || !sub.strict {
		t.Fatal("not a strict restore subscriber")
	}
	stop := make(chan struct{})
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		b := []byte("x")
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := tty.Write(b); err != nil {
				return
			}
		}
	}()
	defer func() { close(stop); <-writerDone }()
	deadline := time.Now().Add(60 * time.Second)
	for {
		sub.mu.Lock()
		n, err := len(sub.queue), sub.err
		sub.mu.Unlock()
		if n > maxTinyEntries {
			t.Fatalf("%d reads queued, over %d", n, maxTinyEntries)
		}
		if err != nil {
			if !errors.Is(err, errFellBehind) {
				t.Fatalf("ended with %v", err)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("not ended after 60s, %d reads queued", n)
		}
		time.Sleep(time.Millisecond)
	}
}

// A restore attach that gets no snapshot (the marker comes first) is as
// lossless as one that does: past its budget it is ended, not continued
// with a gap.
func TestRestoreAttachWithoutSnapshotFallenBehindIsEnded(t *testing.T) {
	term, tty, _ := ptyTerminal(t, 24, 80, historyLines, nil)
	term.mu.Lock()
	term.screen = nil
	term.mu.Unlock()
	snap, sub := term.join(true, 0, 0)
	defer term.leave(sub)
	if snap != nil {
		t.Fatalf("a snapshot without an emulator: %q", snap)
	}
	writePastBudget(tty, "q")
	deadline := time.Now().Add(30 * time.Second)
	for isJoined(term, sub) {
		if time.Now().After(deadline) {
			t.Fatal("still joined after its budget was written")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if b, err := sub.next(); !errors.Is(err, errFellBehind) {
		t.Fatalf("next: %d bytes, %v; want errFellBehind", len(b), err)
	}
}

// A restore subscriber holds the 4 MiB the API documents, counting each
// read as 64 bytes more than its length: 128 reads of 32 KiB less 64,
// or 64527 reads of 1 byte, and not one byte more.
func TestRestoreSubscriberHoldsFourMiB(t *testing.T) {
	for _, size := range []int{32<<10 - 64, 1} {
		s := newSubscriber(true)
		chunk := make([]byte, size)
		n := (4 << 20) / (size + 64)
		for i := range n {
			if !s.offer(chunk) {
				t.Fatalf("%d-byte reads: ended after %d of %d", size, i, n)
			}
		}
		if s.offer([]byte{1}) {
			t.Fatalf("%d-byte reads: one byte over 4 MiB accepted", size)
		}
	}
}

// After leave, a restore subscriber's reader gets what was queued, in
// order, then errLeft.
func TestRestoreSubscriberDrainsAfterLeave(t *testing.T) {
	term := &terminal{subs: map[*subscriber]bool{}}
	sub := newSubscriber(true)
	term.subs[sub] = true
	sub.offer([]byte("a"))
	sub.offer([]byte("b"))
	term.leave(sub)
	for _, want := range []string{"a", "b"} {
		if b, err := sub.next(); err != nil || string(b) != want {
			t.Fatalf("next: %q, %v; want %q", b, err, want)
		}
	}
	if b, err := sub.next(); !errors.Is(err, errLeft) {
		t.Fatalf("next after the queue: %q, %v; want errLeft", b, err)
	}
}

// A plain attach is as before: one that does not read misses what
// overflows its queue of reads and goes on with later output, never ended.
func TestPlainAttachMissesOverflow(t *testing.T) {
	term, tty, _ := ptyTerminal(t, 24, 80, historyLines, nil)
	snap, sub := term.join(false, 0, 0)
	defer term.leave(sub)
	if snap != nil || sub.strict {
		t.Fatalf("plain join: snapshot %q, strict %v", snap, sub.strict)
	}
	writePastBudget(tty, "p")
	fmt.Fprint(tty, "DONE")
	waitScreen(t, term, "DONE")
	if !isJoined(term, sub) {
		t.Fatal("a plain subscriber was removed")
	}
	if n := len(sub.ch); n != plainQueue {
		t.Fatalf("%d reads queued, want the queue full (%d)", n, plainQueue)
	}
	for range plainQueue {
		<-sub.ch
	}
	fmt.Fprint(tty, "MORE")
	if live := liveUntil(t, sub, "MORE"); !bytes.HasSuffix(live, []byte("MORE")) {
		t.Fatalf("after the overflow %q", live)
	}
}

type failingScreen struct{ calls int }

func (f *failingScreen) Write(p []byte) {
	f.calls++
	if f.calls > 1 {
		panic("emulator bug")
	}
}
func (f *failingScreen) Resize(rows, cols int) {}
func (f *failingScreen) Snapshot(int) []byte   { return []byte("snap") }

// An emulator that fails is dropped: output and attach go on, without
// restore.
func TestEmulatorFailureKeepsStream(t *testing.T) {
	var got bytes.Buffer
	var gmu sync.Mutex
	term, tty, _ := ptyTerminal(t, 24, 80, historyLines, func(b []byte) {
		gmu.Lock()
		got.Write(b)
		gmu.Unlock()
	})
	term.mu.Lock()
	term.screen = &failingScreen{}
	term.mu.Unlock()
	_, sub := term.join(false, 0, 0)
	defer term.leave(sub)

	io.WriteString(tty, "one")
	waitOutput := func(want string) {
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			gmu.Lock()
			ok := strings.Contains(got.String(), want)
			gmu.Unlock()
			if ok {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatalf("output never had %q", want)
	}
	waitOutput("one")
	io.WriteString(tty, "two") // the emulator panics on this one
	waitOutput("two")
	io.WriteString(tty, "three")
	waitOutput("three")

	live := liveUntil(t, sub, "three")
	if string(live) != "onetwothree" {
		t.Fatalf("subscriber got %q", live)
	}
	snap, sub2 := term.join(true, 0, 0)
	term.leave(sub2)
	if snap != nil {
		t.Fatalf("a snapshot from a dropped emulator: %q", snap)
	}
	term.resize(10, 10) // no emulator: just the PTY
}

// The shim's attach stream sends the snapshot as stdout StreamData in
// frames of exactly streamChunk (the last one shorter), then live output.
func TestAttachStreamSendsSnapshotFrames(t *testing.T) {
	term, tty, _ := ptyTerminal(t, 30, 200, historyLines, nil)
	// History and a screen whose snapshot is over two chunks.
	for i := range 500 {
		fmt.Fprintf(tty, "\x1b[3%dm%s\x1b[m\r\n", i%8, strings.Repeat(fmt.Sprint(i%10), 199))
	}
	fmt.Fprint(tty, "ready")
	waitScreen(t, term, "ready")
	term.mu.Lock()
	snapLen := len(term.screen.Snapshot(snapshotLimit))
	term.mu.Unlock()
	if snapLen <= 2*streamChunk {
		t.Fatalf("a snapshot of %d bytes: not over two chunks", snapLen)
	}
	runnerSide, dec := attachOverPipe(t, term, true)
	var frames []proto.StreamData
	var out []byte
	marker := -1 // index in frames of the Restored marker
	for !bytes.Contains(out, []byte("after")) {
		d, err := readFrame(runnerSide, dec)
		if err != nil {
			t.Fatalf("after %d frames: %v", len(frames), err)
		}
		if d.Restored {
			if marker >= 0 || len(d.Data) > 0 || d.Channel != "" {
				t.Fatalf("marker %+v (an earlier one at frame %d)", d, marker)
			}
			if len(out) != snapLen {
				t.Fatalf("the marker after %d bytes, the snapshot is %d", len(out), snapLen)
			}
			marker = len(frames)
			frames = append(frames, d)
			// Live output only now, so it cannot land before the marker
			// by being written early.
			fmt.Fprint(tty, " after")
			continue
		}
		if d.Channel != "stdout" || d.Error != "" {
			t.Fatalf("frame %+v", d)
		}
		if len(d.Data) > streamChunk {
			t.Fatalf("a frame of %d bytes", len(d.Data))
		}
		frames = append(frames, d)
		out = append(out, d.Data...)
	}
	if marker < 0 {
		t.Fatal("no end-of-snapshot marker")
	}
	// The snapshot's frames, before the marker: all full but the last.
	if marker < 3 {
		t.Fatalf("the snapshot in %d frames", marker)
	}
	for i, f := range frames[:marker-1] {
		if len(f.Data) != streamChunk {
			t.Fatalf("snapshot frame %d of %d bytes, want %d", i, len(f.Data), streamChunk)
		}
	}
	v := replay(30, 200, historyLines, out)
	if !bytes.Equal(v.Snapshot(0), screenOf(term)) {
		t.Fatal("the stream does not reproduce the terminal")
	}
}

// fillOverLimit resizes term to the emulator's largest size, writes first,
// then gives every cell of the screen then shown its own truecolour pen:
// a screen part over snapshotLimit.
func fillOverLimit(t *testing.T, term *terminal, tty *os.File, first string) {
	t.Helper()
	term.resize(vterm.MaxRows, vterm.MaxCols)
	w := bufio.NewWriterSize(tty, 64<<10)
	fmt.Fprint(w, first)
	for y := range vterm.MaxRows {
		fmt.Fprintf(w, "\x1b[%dH", y+1)
		for x := range vterm.MaxCols - 1 {
			fmt.Fprintf(w, "\x1b[1;3;9;38;2;%d;%d;%d;48;2;%d;%d;%dmx", x%256, y%256, (x+y)%256, y%256, x%256, (x*y)%256)
		}
	}
	fmt.Fprint(w, "\x1b[m\x1b[HREADY")
	ferr := w.Flush()
	// Polled cheaply: a whole snapshot of this screen per poll would hold
	// the lock the reader needs.
	deadline := time.Now().Add(30 * time.Second)
	for {
		term.mu.Lock()
		v := term.screen.(*vterm.Terminal)
		x, y, _ := v.Cursor()
		done := x == 5 && y == 0 && v.Text(v.Cell(4, 0)) == "Y"
		term.mu.Unlock()
		if done {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the terminal never showed READY: cursor %d,%d, flush %v", x, y, ferr)
		}
		time.Sleep(10 * time.Millisecond)
	}
	term.mu.Lock()
	defer term.mu.Unlock()
	if want := first != ""; term.screen.(*vterm.Terminal).AltScreen() != want {
		t.Fatalf("alternate screen shown: %v, want %v", !want, want)
	}
}

// A restore attach that gets no snapshot (the emulator was dropped, or the
// screen part, main or alternate, is over the limit) still gets the
// marker, first, then live output; a plain attach gets no marker.
func TestRestoreMarkerWithoutSnapshot(t *testing.T) {
	for _, tc := range []struct {
		name    string
		restore bool
		setup   func(*testing.T, *terminal, *os.File)
	}{
		{"emulator dropped", true, func(_ *testing.T, term *terminal, _ *os.File) {
			term.mu.Lock()
			term.screen = nil
			term.mu.Unlock()
		}},
		{"screen over the limit", true, func(t *testing.T, term *terminal, tty *os.File) {
			fillOverLimit(t, term, tty, "")
		}},
		{"alternate screen over the limit", true, func(t *testing.T, term *terminal, tty *os.File) {
			fillOverLimit(t, term, tty, "main\r\n\x1b[?1049h")
		}},
		{"plain attach", false, func(*testing.T, *terminal, *os.File) {}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			term, tty, _ := ptyTerminal(t, 24, 80, historyLines, nil)
			tc.setup(t, term, tty)
			runnerSide, dec := attachOverPipe(t, term, tc.restore)
			read := func() proto.StreamData {
				d, err := readFrame(runnerSide, dec)
				if err != nil {
					t.Fatal(err)
				}
				return d
			}
			if tc.restore {
				if d := read(); !d.Restored || len(d.Data) > 0 {
					t.Fatalf("first frame: restored %v, %d bytes of data; want the marker", d.Restored, len(d.Data))
				}
			} else {
				// Joined once the stream has subscribed; then live output.
				deadline := time.Now().Add(10 * time.Second)
				for subscriberCount(term) != 1 && time.Now().Before(deadline) {
					time.Sleep(time.Millisecond)
				}
			}
			fmt.Fprint(tty, "LIVE")
			var out []byte
			for !bytes.Contains(out, []byte("LIVE")) {
				d := read()
				if d.Restored {
					t.Fatalf("a marker %+v", d)
				}
				out = append(out, d.Data...)
			}
		})
	}
}

// join's snapshot is within snapshotLimit even when the whole terminal
// is not, and replays to the shim's screen with the newest history.
func TestJoinSnapshotWithinLimit(t *testing.T) {
	const rows, cols = 50, 200
	term, tty, _ := ptyTerminal(t, rows, cols, historyLines, nil)
	var line strings.Builder
	for x := range cols - 1 {
		fmt.Fprintf(&line, "\x1b[38;2;%d;%d;%d;48;2;%d;%d;%dm%c", x, 255-x, x/2, 255-x/2, x, x, 'a'+x%26)
	}
	line.WriteString("\x1b[m")
	w := bufio.NewWriterSize(tty, 64<<10)
	for i := range historyLines + rows {
		fmt.Fprintf(w, "%05d%s\r\n", i, line.String())
	}
	fmt.Fprint(w, "ready")
	w.Flush()
	waitScreen(t, term, "ready")
	full := screenOf(term)
	if len(full) <= snapshotLimit {
		t.Fatalf("the whole terminal is %d bytes, within the limit: nothing to test", len(full))
	}

	snap, sub := term.join(true, 0, 0)
	term.leave(sub)
	if len(snap) > snapshotLimit {
		t.Fatalf("a snapshot of %d bytes, over the limit %d", len(snap), snapshotLimit)
	}
	v := replay(rows, cols, historyLines, snap)
	term.mu.Lock()
	shim := term.screen.(*vterm.Terminal)
	wantScreen, wantHist := screenText(shim), shim.HistoryLen()
	wantNewest := lineText(shim, shim.HistoryLine(wantHist-1))
	term.mu.Unlock()
	if got := screenText(v); got != wantScreen {
		t.Fatalf("replayed screen differs:\n%q\nwant\n%q", got, wantScreen)
	}
	if n := v.HistoryLen(); n == 0 || n >= wantHist {
		t.Fatalf("replayed history %d lines of %d", n, wantHist)
	}
	if got := lineText(v, v.HistoryLine(v.HistoryLen()-1)); got != wantNewest {
		t.Fatalf("newest history line %q, want %q", got, wantNewest)
	}
}

func lineText(v *vterm.Terminal, l []vterm.Cell) string {
	var s strings.Builder
	for _, c := range l {
		s.WriteString(v.Text(c))
	}
	return s.String()
}

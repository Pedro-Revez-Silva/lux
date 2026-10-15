package shim

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"

	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/vterm"
)

// Interactive streams: exec and attach.
//
// The runner opens a connection on the shim socket per stream and sends
// ShimStream with a StreamOpen first. From then on the connection carries
// only that stream, as proto.StreamData JSON lines: stdin, EOF or a resize
// from the runner; output and finally one with ExitCode or Error from the
// shim. Closing the connection ends the stream (an exec'd process is
// killed; an attach just detaches).
//
// Streams are for a person at a terminal. They are not recorded in the
// Run's output (an attached workload's terminal output is, as always).

// streamConn serializes messages to one stream's connection.
type streamConn struct {
	mu  sync.Mutex
	enc *json.Encoder
}

func (c *streamConn) send(d proto.StreamData) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.enc.Encode(d)
}

func (c *streamConn) fail(msg string) { _ = c.send(proto.StreamData{Error: msg}) }

func (s *Shim) handleStream(sc *bufio.Scanner, enc *json.Encoder, open proto.StreamOpen) {
	out := &streamConn{enc: enc}
	switch open.Kind {
	case "exec":
		s.execStream(sc, out, open)
	case "attach":
		s.attachStream(sc, out, open)
	default:
		out.fail("unknown stream kind " + open.Kind)
	}
}

// execStream runs a command in the container as the workload user, with
// its environment, under a PTY if asked.
func (s *Shim) execStream(sc *bufio.Scanner, out *streamConn, open proto.StreamOpen) {
	s.mu.Lock()
	env := s.env
	s.mu.Unlock()
	if env == nil {
		out.fail("the workload has not started yet")
		return
	}
	if len(open.Command) == 0 {
		out.fail("no command")
		return
	}
	cmd := s.command(open.Command, env)
	type output struct {
		ch string
		r  io.Reader
	}
	var stdin io.WriteCloser
	var ptmx *os.File
	var outputs []output
	var exited chan syscall.WaitStatus
	var err error
	if open.TTY {
		// A PTY makes the command a session leader: Setsid, not Setpgid.
		cmd.SysProcAttr.Setpgid = false
		exited, err = s.startTracked(func() error {
			var e error
			ptmx, e = pty.StartWithSize(cmd, winsize(open.Rows, open.Cols))
			return e
		}, &cmd.Process)
		stdin, outputs = ptmx, []output{{"stdout", ptmx}}
	} else {
		in, e1 := cmd.StdinPipe()
		o, e2 := cmd.StdoutPipe()
		e, e3 := cmd.StderrPipe()
		if err = errors.Join(e1, e2, e3); err == nil {
			exited, err = s.startTracked(cmd.Start, &cmd.Process)
		}
		stdin, outputs = in, []output{{"stdout", o}, {"stderr", e}}
	}
	if err != nil {
		out.fail(err.Error())
		return
	}

	go func() {
		readStreamInput(sc, func(d proto.StreamData) {
			switch {
			case d.Rows > 0 && ptmx != nil:
				_ = pty.Setsize(ptmx, winsize(d.Rows, d.Cols))
			case d.EOF && ptmx == nil:
				_ = stdin.Close()
			case d.EOF:
				_, _ = stdin.Write([]byte{4}) // ^D
			case len(d.Data) > 0:
				_, _ = stdin.Write(d.Data)
			}
		})
		// The client went away: so does the command (it leads its own
		// process group, or session under a PTY).
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}()

	readers := make([]io.Reader, len(outputs))
	for i, o := range outputs {
		readers[i] = o.r
	}
	drain := copyOutputs(readers, func(i int, b []byte) { _ = out.send(proto.StreamData{Channel: outputs[i].ch, Data: b}) })
	ws := <-exited
	drain()
	_ = stdin.Close()
	code := exitCode(ws)
	_ = out.send(proto.StreamData{ExitCode: &code})
}

// attachStream joins the workload's terminal (a generic workload started
// with workload.tty): its output from now on, and input into it. With
// open.Restore, the output starts with a snapshot of the terminal's screen
// (at open.Rows×open.Cols, when given).
func (s *Shim) attachStream(sc *bufio.Scanner, out *streamConn, open proto.StreamOpen) {
	s.mu.Lock()
	term := s.term
	s.mu.Unlock()
	if term == nil {
		out.fail("the workload has no terminal: attach needs workload.tty (generic workloads)")
		return
	}
	snapshot, sub := term.join(open.Restore, open.Rows, open.Cols)
	defer term.leave(sub)
	behind := make(chan struct{})
	go func() {
		for b := range slices.Chunk(snapshot, streamChunk) {
			_ = out.send(proto.StreamData{Channel: "stdout", Data: b})
		}
		if open.Restore {
			// Where live output starts, with or without a snapshot.
			_ = out.send(proto.StreamData{Restored: true})
		}
		for {
			b, err := sub.next()
			if errors.Is(err, errFellBehind) {
				out.fail("attach fell behind; attach again")
				close(behind)
				return
			}
			if err != nil {
				return
			}
			_ = out.send(proto.StreamData{Channel: "stdout", Data: b})
		}
	}()
	inputDone := make(chan struct{})
	go func() {
		defer close(inputDone)
		readStreamInput(sc, func(d proto.StreamData) {
			switch {
			case d.Rows > 0:
				term.resize(d.Rows, d.Cols)
			case len(d.Data) > 0:
				_, _ = term.ptmx.Write(d.Data)
			}
		})
	}()
	select {
	case <-inputDone: // detached
	case <-behind:
	case <-term.gone: // the workload exited
		_ = out.send(proto.StreamData{Error: "the workload exited"})
	}
}

// streamChunk: the most output one StreamData carries, as a PTY read does.
const streamChunk = 32 << 10

// historyLines is how much main-screen scrollback a terminal keeps for
// attach's restore, as tmux's default history-limit.
const historyLines = 2000

// snapshotLimit bounds a snapshot (its oldest history lines go first):
// 128 frames of streamChunk, half of what luxd buffers for a stream
// before it drops it as fallen behind.
const snapshotLimit = 128 * streamChunk

// screenState is a terminal emulator fed the workload's output: what an
// attach with restore starts from.
type screenState interface {
	Write(p []byte)
	Resize(rows, cols int)
	Snapshot(limit int) []byte
}

// terminal is a workload's PTY, read for the output file and copied to
// whoever is attached. It never holds up the workload or its output file:
// a plain attach that cannot keep up misses output; a restore attach that
// cannot keep up is ended (see subscriber).
//
// What is read is also fed to an emulator (screen) under the same lock as
// the copy to subscribers, so a snapshot taken under it and a subscriber
// joined with it see every byte exactly once. An emulator that fails is
// dropped: attach goes on without restore.
type terminal struct {
	ptmx     *os.File
	mu       sync.Mutex
	subs     map[*subscriber]bool
	screen   screenState
	gone     chan struct{}
	goneOnce sync.Once
}

func newTerminal(ptmx *os.File, rows, cols int) *terminal {
	return &terminal{ptmx: ptmx, subs: map[*subscriber]bool{}, gone: make(chan struct{}),
		screen: vterm.New(rows, cols, historyLines)}
}

var (
	errFellBehind = errors.New("attach fell behind")
	errLeft       = errors.New("attach left")
)

// plainQueue is how many reads a plain attach's queue holds; past it,
// reads are dropped for that subscriber.
const plainQueue = 256

// subscriber is one attach's copy of the terminal's output.
//
// A plain attach has a channel of plainQueue reads and misses what
// overflows it. A restore attach's client has a screen built from the
// snapshot (or, without one, from live output after the marker), which a
// gap would leave wrong for good, so its queue is bounded in bytes
// (restoreBudget, as much as the snapshot it may still be sending) and on
// overflow it is removed and ended with errFellBehind: the client attaches
// again rather than going on from a wrong screen.
type subscriber struct {
	ch chan []byte // plain

	strict bool
	mu     sync.Mutex
	queue  [][]byte
	queued int
	wake   chan struct{} // capacity 1
	err    error
}

// restoreBudget is the most a restore attach may have queued, counting
// each queued read as its length plus entryCost.
const restoreBudget = snapshotLimit

// entryCost is the heap a queued read takes besides its bytes: its slice
// header in queue (24), at most as much again of append's spare capacity,
// and the smallest size class of its copy (8). Charging it bounds a
// stalled queue's memory by restoreBudget however small the reads are,
// as when a workload writes a byte at a time.
const entryCost = 64

func newSubscriber(strict bool) *subscriber {
	if strict {
		return &subscriber{strict: true, wake: make(chan struct{}, 1)}
	}
	return &subscriber{ch: make(chan []byte, plainQueue)}
}

// offer queues b without blocking. It reports false when a strict
// subscriber has fallen behind; the caller removes it.
func (s *subscriber) offer(b []byte) bool {
	if !s.strict {
		select {
		case s.ch <- b:
		default:
		}
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.queued+len(b)+entryCost > restoreBudget {
		s.queue, s.queued = nil, 0
		s.endLocked(errFellBehind)
		return false
	}
	s.queue = append(s.queue, b)
	s.queued += len(b) + entryCost
	s.signal()
	return true
}

func (s *subscriber) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *subscriber) endLocked(err error) {
	if s.err == nil {
		s.err = err
	}
	s.signal()
}

// next blocks for the next output. Once a strict subscriber has left it
// returns what was queued before, then errLeft; one that fell behind
// returns errFellBehind (its queue is dropped).
func (s *subscriber) next() ([]byte, error) {
	if !s.strict {
		b, ok := <-s.ch
		if !ok {
			return nil, errLeft
		}
		return b, nil
	}
	for {
		s.mu.Lock()
		if len(s.queue) > 0 {
			b := s.queue[0]
			s.queue[0] = nil
			s.queue = s.queue[1:]
			s.queued -= len(b) + entryCost
			s.mu.Unlock()
			return b, nil
		}
		if s.err != nil {
			err := s.err
			s.mu.Unlock()
			return nil, err
		}
		s.mu.Unlock()
		<-s.wake
	}
}

// join subscribes to the terminal's output. With restore, it first
// resizes to rows×cols (if given) and returns the snapshot the output
// continues from; nil if the terminal has no emulator or the snapshot is
// over snapshotLimit. Every restore subscriber is strict (see subscriber),
// snapshot or not: its client may build a screen from live output alone.
func (t *terminal) join(restore bool, rows, cols int) ([]byte, *subscriber) {
	t.mu.Lock()
	defer t.mu.Unlock()
	var snap []byte
	if restore {
		if rows > 0 && cols > 0 {
			t.resizeLocked(rows, cols)
		}
		t.withScreen(func(s screenState) { snap = s.Snapshot(snapshotLimit) })
	}
	sub := newSubscriber(restore)
	t.subs[sub] = true
	return snap, sub
}

func (t *terminal) leave(sub *subscriber) {
	t.mu.Lock()
	delete(t.subs, sub)
	t.mu.Unlock()
	if !sub.strict {
		close(sub.ch)
		return
	}
	sub.mu.Lock()
	sub.endLocked(errLeft)
	sub.mu.Unlock()
}

// resize resizes the PTY and the emulator.
func (t *terminal) resize(rows, cols int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.resizeLocked(rows, cols)
}

// resizeLocked resizes the PTY and the emulator to the same size: within
// the emulator's limits, so the two always agree.
func (t *terminal) resizeLocked(rows, cols int) {
	ws := winsize(min(rows, vterm.MaxRows), min(cols, vterm.MaxCols))
	_ = pty.Setsize(t.ptmx, ws)
	t.withScreen(func(s screenState) { s.Resize(int(ws.Rows), int(ws.Cols)) })
}

// withScreen calls fn with the emulator, if there still is one; one that
// panics is dropped. Called with mu held.
func (t *terminal) withScreen(fn func(screenState)) {
	if t.screen == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			t.screen = nil
			fmt.Fprintln(os.Stderr, "lux-shim: terminal emulator failed, attach restore is off:", r)
		}
	}()
	fn(t.screen)
}

// Read reads the PTY, feeds the emulator and copies what it read to
// everyone attached.
func (t *terminal) Read(p []byte) (int, error) {
	n, err := t.ptmx.Read(p)
	if n > 0 {
		t.mu.Lock()
		t.withScreen(func(s screenState) { s.Write(p[:n]) })
		if len(t.subs) > 0 {
			b := append([]byte{}, p[:n]...)
			for sub := range t.subs {
				if !sub.offer(b) {
					delete(t.subs, sub)
				}
			}
		}
		t.mu.Unlock()
	}
	if err != nil {
		t.goneOnce.Do(func() { close(t.gone) })
	}
	return n, err
}

func readStreamInput(sc *bufio.Scanner, fn func(proto.StreamData)) {
	for sc.Scan() {
		var d proto.StreamData
		if json.Unmarshal(sc.Bytes(), &d) == nil {
			fn(d)
		}
	}
}

func winsize(rows, cols int) *pty.Winsize {
	if rows <= 0 || cols <= 0 {
		rows, cols = 24, 80
	}
	return &pty.Winsize{Rows: uint16(rows), Cols: uint16(cols)}
}

// copyOutputs copies each reader to sink (by its index) as it is written,
// and returns drain: called once the process has exited, it waits for what
// it wrote to be read — no longer than 2s, since a background child can
// hold a pipe open — then closes the readers. The reaper collects the
// process, so cmd.Wait (which would close the pipes' parent ends) is never
// called.
func copyOutputs(readers []io.Reader, sink func(i int, b []byte)) (drain func()) {
	var wg sync.WaitGroup
	for i, r := range readers {
		wg.Add(1)
		go func() { defer wg.Done(); copyTo(r, func(b []byte) { sink(i, b) }) }()
	}
	return func() {
		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
		for _, r := range readers {
			if c, ok := r.(io.Closer); ok {
				_ = c.Close()
			}
		}
	}
}

// startTracked starts a process with the reaper told to hand its exit to
// the returned channel. The lock is held across the start, so the reaper
// cannot reap the process before it is registered.
func (s *Shim) startTracked(start func() error, proc **os.Process) (chan syscall.WaitStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := start(); err != nil {
		return nil, err
	}
	ch := make(chan syscall.WaitStatus, 1)
	s.streams[(*proc).Pid] = ch
	return ch, nil
}

// exitCode is a process's exit code, 128+signal if a signal ended it.
func exitCode(ws syscall.WaitStatus) int {
	if ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return ws.ExitStatus()
}

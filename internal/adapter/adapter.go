// Package adapter is how the shim starts, resumes, steers and stops a
// workload. lux itself is agent-agnostic: a Run is a process. An adapter
// only translates between that process's own stdio protocol and lux's four
// verbs — deliver input, interrupt, stop, and report what it learned
// (session id, idle/busy, structured events).
//
//	generic      a plain process: stdin gets raw input, stdout/stderr are
//	             output, SIGINT interrupts. Works for anything.
//	acp          any Agent Client Protocol agent (JSON-RPC over stdio).
//	claude-code  Claude Code's stream-json mode.
//	codex        Codex's app-server (JSON-RPC over stdio).
//	opencode     OpenCode over ACP, with its own default command.
package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"slices"
	"sync"
	"syscall"

	"github.com/marcioapm/lux/internal/proto"
)

// Sink receives everything an adapter produces. Implemented by the shim,
// which redacts, sequences and writes records.
type Sink interface {
	Stdout(p []byte)
	// EndMessage ends a message the agent wrote to stdout (in one piece or
	// streamed in several) on its own line.
	EndMessage()
	Stderr(p []byte)
	// Event writes a structured event record (ch=event).
	Event(typ string, data any)
	// Stream writes streamed text (a reply token by token) as typ events,
	// each wrap(text) for the text released so far. The sink may hold text
	// back and coalesce chunks, so a secret split across them is redacted
	// whole; key names the message (chunks with one key may be joined).
	Stream(typ, key, text string, wrap func(text string) any)
	Session(id string)
	Activity(idle bool)
	// InputAccepted: the agent has taken an input; d says when it reads it
	// and whether InputConsumed follows. The workload's first prompt is
	// reported too, with request id "prompt".
	InputAccepted(in proto.Input, d Delivery)
	// InputConsumed: the agent's model step has the input in context.
	InputConsumed(requestID string)
	// InputFailed: the input was not delivered, or the agent dropped it.
	InputFailed(in proto.Input, err error)
}

// Delivery is when an accepted input reaches the agent's model.
type Delivery struct {
	Lands   string // LandsNextStep | LandsNextTurn
	Receipt bool   // an InputConsumed follows
}

const (
	// LandsNextStep: read at the agent's next model step, possibly inside
	// the running turn.
	LandsNextStep = "next_step"
	// LandsNextTurn: read only when the running turn has ended.
	LandsNextTurn = "next_turn"
)

// Process is the workload process the shim started for an adapter.
type Process struct {
	Cmd    *exec.Cmd
	Stdin  io.WriteCloser
	Stdout io.ReadCloser
	Stderr io.ReadCloser
}

func (p *Process) Signal(sig syscall.Signal) error {
	if p.Cmd.Process == nil {
		return nil
	}
	// The workload runs in its own process group so a signal reaches its
	// children too.
	return syscall.Kill(-p.Cmd.Process.Pid, sig)
}

// Adapter drives one workload process.
type Adapter interface {
	// Command returns the argv to exec: first start or resume.
	Command(cfg proto.ShimConfig) ([]string, error)
	// Attach begins speaking the protocol on the started process. It
	// returns when the process's stdout closes.
	Run(ctx context.Context, p *Process, cfg proto.ShimConfig, sink Sink) error
	// Deliver hands input to the workload, queueing it if the workload
	// cannot take it now. Must not block on the workload.
	Deliver(in proto.Input)
	Interrupt() error
	// Stop asks the workload to wind down gracefully. The shim SIGKILLs
	// after the grace period.
	Stop() error
}

// CredentialFiles is implemented by adapters whose agent reads secrets
// from files rather than the environment: a key, or a config holding MCP
// headers. Given the Run's config (with MCP resolved), its secret values
// and the workload user's home, it returns the files to write (path →
// contents). The shim writes them like file secrets: on the secrets tmpfs,
// readable only by the workload user, never snapshotted. A path elsewhere
// is linked to its file there; a path under proto.ShimSecretsDir is the
// file itself.
type CredentialFiles interface {
	CredentialFiles(cfg proto.ShimConfig, secrets map[string]string, home string) map[string][]byte
}

// Environment is implemented by adapters whose agent needs variables in
// its environment (Codex reads MCP header values from variables it is
// told the names of). They are the workload's only, not init's or exec's.
type Environment interface {
	Environment(cfg proto.ShimConfig) map[string]string
}

func New(name string) (Adapter, error) {
	switch name {
	case "generic", "":
		return &Generic{}, nil
	case "acp":
		return NewACP(), nil
	case "opencode":
		return NewOpenCode(), nil
	case "claude-code":
		return NewClaude(), nil
	case "codex":
		return NewCodex(), nil
	}
	return nil, fmt.Errorf("unknown adapter %q", name)
}

// ---- generic --------------------------------------------------------------

// Generic runs a command as-is. Input is raw bytes (or text plus a newline)
// written to its stdin; interrupt is SIGINT; stop is SIGTERM.
type Generic struct {
	mu    sync.Mutex
	proc  *Process
	queue []proto.Input
	sink  Sink
}

func (g *Generic) Command(cfg proto.ShimConfig) ([]string, error) { return command(cfg) }

func (g *Generic) Run(ctx context.Context, p *Process, cfg proto.ShimConfig, sink Sink) error {
	g.mu.Lock()
	g.proc, g.sink = p, sink
	queued := g.queue
	g.queue = nil
	g.mu.Unlock()
	for _, in := range queued {
		g.Deliver(in)
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); pump(p.Stdout, sink.Stdout) }()
	go func() { defer wg.Done(); pump(p.Stderr, sink.Stderr) }()
	wg.Wait()
	return nil
}

func (g *Generic) Deliver(in proto.Input) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.proc == nil {
		g.queue = append(g.queue, in)
		return
	}
	if in.Interrupt {
		_ = g.proc.Signal(syscall.SIGINT)
	}
	data := in.Raw
	if len(data) == 0 && in.Text != "" {
		data = []byte(in.Text + "\n")
	}
	var err error
	if len(data) > 0 && g.proc.Stdin != nil {
		_, err = g.proc.Stdin.Write(data)
	}
	if err != nil {
		g.sink.InputFailed(in, err)
		return
	}
	// Whatever reads stdin reads it when it next reads; lux cannot tell when.
	g.sink.InputAccepted(in, Delivery{Lands: LandsNextStep})
}

func (g *Generic) Interrupt() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.proc == nil {
		return nil
	}
	return g.proc.Signal(syscall.SIGINT)
}

func (g *Generic) Stop() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.proc == nil {
		return nil
	}
	return g.proc.Signal(syscall.SIGTERM)
}

func pump(r io.Reader, out func([]byte)) {
	buf := make([]byte, 32<<10)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			b := make([]byte, n)
			copy(b, buf[:n])
			out(b)
		}
		if err != nil {
			return
		}
	}
}

// ---- helpers --------------------------------------------------------------

// inputLedger reports each input's phases in order, once each: accepted,
// then consumed (only with a receipt), or failed. Agents can report the
// consumption before the adapter has seen the call that accepted it return
// (both arrive on one stream, handled by two goroutines), so a consumption
// seen early is held until accepted.
//
// An input's payload is kept only while the adapter may send it again:
// until it is accepted without a receipt, consumed, or failed. After that
// its entry is a tombstone (request id and flags) that keeps a repeated
// phase from being reported twice. open holds the inputs accepted with a
// receipt and not yet read, so a turn's end does not walk the history.
type inputLedger struct {
	mu   sync.Mutex
	m    map[string]*inputState
	open map[string]*inputState
	seq  int
	// closed: the agent has exited (close); what is accepted from here on
	// cannot be read.
	closed error
}

type inputState struct {
	in                       proto.Input
	accepted, consumed, done bool
	receipt                  bool
	// early: consumed was seen before accepted.
	early bool
	// tag is the adapter's own note on the input (the Codex turn it was
	// steered into).
	tag string
	// seq: the order inputs were first tracked or accepted in, which is
	// the order they came in when the adapter tracks them on arrival.
	seq int
}

func (l *inputLedger) state(id string) *inputState {
	if l.m == nil {
		l.m = map[string]*inputState{}
	}
	st := l.m[id]
	if st == nil {
		st = &inputState{}
		l.m[id] = st
	}
	return st
}

// settled drops an input's payload once nothing will send it again.
func (l *inputLedger) settled(st *inputState) {
	st.in = proto.Input{RequestID: st.in.RequestID}
	delete(l.open, st.in.RequestID)
}

// track registers an input before it is sent, so its consumption is known
// to be ours.
func (l *inputLedger) track(in proto.Input) {
	if in.RequestID == "" {
		return
	}
	l.mu.Lock()
	st := l.state(in.RequestID)
	if !st.done && !st.accepted {
		st.in = in
	}
	if st.seq == 0 {
		l.seq++
		st.seq = l.seq
	}
	l.mu.Unlock()
}

func (l *inputLedger) accept(sink Sink, in proto.Input, d Delivery, tag string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if in.RequestID == "" {
		sink.InputAccepted(in, d)
		return
	}
	st := l.state(in.RequestID)
	if st.done {
		return
	}
	if st.seq == 0 {
		l.seq++
		st.seq = l.seq
	}
	if st.accepted {
		// Sent again (carried into the next turn): only its turn changes.
		st.tag = tag
		return
	}
	st.in, st.accepted, st.receipt, st.tag = in, true, d.Receipt, tag
	st.in.RequestID = in.RequestID
	sink.InputAccepted(in, d)
	switch {
	case !st.receipt:
		l.settled(st)
	case st.early:
		st.consumed, st.done = true, true
		l.settled(st)
		sink.InputConsumed(in.RequestID)
	case l.closed != nil:
		st.done = true
		l.settled(st)
		sink.InputFailed(in, l.closed)
	default:
		if l.open == nil {
			l.open = map[string]*inputState{}
		}
		l.open[in.RequestID] = st
	}
}

// consume reports that the agent read the input with this id, if it is one
// the adapter sent and has not reported read yet. It returns whether the id
// was known.
func (l *inputLedger) consume(sink Sink, id string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	st := l.m[id]
	if id == "" || st == nil || st.done {
		return st != nil
	}
	if !st.accepted {
		st.early = true
		return true
	}
	if st.receipt {
		st.consumed, st.done = true, true
		l.settled(st)
		sink.InputConsumed(id)
	}
	return true
}

// pending reports whether the input is known and neither read nor failed
// nor accepted without a receipt: something may still happen to it.
func (l *inputLedger) pending(id string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	st := l.m[id]
	return st != nil && !st.done && !(st.accepted && !st.receipt)
}

// unreadOne reports whether the input is accepted and not yet read.
func (l *inputLedger) unreadOne(id string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	st := l.m[id]
	return st != nil && st.accepted && !st.done
}

func (l *inputLedger) fail(sink Sink, in proto.Input, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if in.RequestID != "" {
		st := l.state(in.RequestID)
		if st.done {
			return
		}
		st.done = true
		st.in.RequestID = in.RequestID
		l.settled(st)
	}
	sink.InputFailed(in, err)
}

// close fails every input accepted with a receipt and not read (the agent
// has exited: nothing reads it now), drops the payloads still held, and
// makes a later acceptance with a receipt fail at once.
func (l *inputLedger) close(sink Sink, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.closed = err
	for _, st := range l.sortedLocked(func(st *inputState) bool { return true }) {
		st.done = true
		sink.InputFailed(st.in, err)
	}
	for _, st := range l.m {
		l.settled(st)
	}
}

// order is the input's seq, 0 if unknown.
func (l *inputLedger) order(id string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	if st := l.m[id]; st != nil {
		return st.seq
	}
	return 0
}

// forget drops an input the adapter will send again (as a new turn): its
// phases start over.
func (l *inputLedger) forget(id string) {
	l.mu.Lock()
	delete(l.m, id)
	delete(l.open, id)
	l.mu.Unlock()
}

// unread returns the inputs accepted with a receipt, not yet read, whose
// tag is tag, in the order they came (seq).
func (l *inputLedger) unread(tag string) []proto.Input {
	l.mu.Lock()
	defer l.mu.Unlock()
	return inputsOf(l.sortedLocked(func(st *inputState) bool { return st.tag == tag }))
}

// claim is unread, retagging what it returns to to under the same lock:
// of two callers claiming one tag, only one gets each input.
func (l *inputLedger) claim(tag, to string) []proto.Input {
	l.mu.Lock()
	defer l.mu.Unlock()
	sts := l.sortedLocked(func(st *inputState) bool { return st.tag == tag })
	for _, st := range sts {
		st.tag = to
	}
	return inputsOf(sts)
}

// sortedLocked: the open inputs that match, by seq.
func (l *inputLedger) sortedLocked(match func(*inputState) bool) []*inputState {
	var sts []*inputState
	for _, st := range l.open {
		if match(st) {
			sts = append(sts, st)
		}
	}
	slices.SortFunc(sts, func(a, b *inputState) int { return a.seq - b.seq })
	return sts
}

func inputsOf(sts []*inputState) []proto.Input {
	out := make([]proto.Input, len(sts))
	for i, st := range sts {
		out[i] = st.in
	}
	return out
}

var (
	errStoppedUnread = errors.New("the Run stopped before the agent read it")
	errExitedUnread  = errors.New("the agent exited before it read it")
)

// unreadWhy is why an input the agent never read fails once it has exited.
func unreadWhy(stopped bool) error {
	if stopped {
		return errStoppedUnread
	}
	return errExitedUnread
}

// lineWriter serializes JSON lines to a process's stdin.
type lineWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lineWriter) set(w io.Writer) {
	l.mu.Lock()
	l.w = w
	l.mu.Unlock()
}

func (l *lineWriter) send(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.w == nil {
		return fmt.Errorf("process stdin is closed")
	}
	_, err = l.w.Write(append(b, '\n'))
	return err
}

// close closes stdin; later sends fail. Protocol agents exit when their
// client goes away.
func (l *lineWriter) close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if c, ok := l.w.(io.Closer); ok {
		_ = c.Close()
	}
	l.w = nil
}

// workdir is where an agent session is rooted: the spec's workdir, or the
// workload user's home.
func workdir(cfg proto.ShimConfig) string {
	if cfg.Workdir != "" {
		return cfg.Workdir
	}
	if h := os.Getenv("HOME"); h != "" {
		return h
	}
	return "/root"
}

// textInput is the one-text-block message both ACP and Codex take.
func textInput(s string) []map[string]string {
	return []map[string]string{{"type": "text", "text": s}}
}

// withUsage adds an agent's usage report, as it sent it, to a turn_end
// event's data, if it sent one.
func withUsage(data map[string]any, usage json.RawMessage) map[string]any {
	if len(usage) > 0 && string(usage) != "null" {
		data["usage"] = usage
	}
	return data
}

// command picks the argv: the resume command on resume if the spec has one,
// else the spec's command (spec.Normalize fills each adapter's default).
func command(cfg proto.ShimConfig) ([]string, error) {
	if cfg.Resume && len(cfg.ResumeCommand) > 0 {
		return cfg.ResumeCommand, nil
	}
	if len(cfg.Command) == 0 {
		return nil, fmt.Errorf("%s: no command", cfg.Adapter)
	}
	return cfg.Command, nil
}

// streamEvent writes a raw JSON event whose string at one of paths (the
// first present) is streamed text through sink.Stream, and reports whether
// it was one. The event keeps its shape. Chunks with the same scope that
// differ only in that text (and in the volatile top-level fields) are one
// message, and may be joined, the last one's fields kept.
func streamEvent(sink Sink, typ, scope string, raw []byte, paths [][]string, volatile ...string) bool {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber() // numbers as they were written
	var root map[string]any
	if dec.Decode(&root) != nil {
		return false
	}
	for _, path := range paths {
		m := root
		for _, k := range path[:len(path)-1] {
			if m, _ = m[k].(map[string]any); m == nil {
				break
			}
		}
		leaf := path[len(path)-1]
		text, ok := m[leaf].(string)
		if !ok {
			continue
		}
		m[leaf] = ""
		kept := map[string]any{}
		for _, k := range volatile {
			if v, ok := root[k]; ok {
				kept[k] = v
				delete(root, k)
			}
		}
		key, _ := json.Marshal(root)
		maps.Copy(root, kept)
		sink.Stream(typ, scope+string(key), text, func(t string) any {
			m[leaf] = t
			return root
		})
		return true
	}
	return false
}

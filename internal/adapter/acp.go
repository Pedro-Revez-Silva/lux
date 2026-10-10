package adapter

import (
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/spec"
)

// ACP speaks the Agent Client Protocol: JSON-RPC 2.0, newline-delimited, on
// the agent's stdin/stdout. One adapter for every ACP agent.
//
// ACP has no mid-turn message, so input that arrives during a turn is
// queued and sent as the next session/prompt when the turn ends; with
// interrupt it cancels the turn (session/cancel) first.
//
// OpenCode (NewOpenCode) takes input during a turn at the agent's next
// step: lux sends it through OpenCode's HTTP server (opencode.go), which
// says when it was read, or else as a second session/prompt, which
// OpenCode joins to the running loop. Either way the turn ends once, when
// every prompt of it has resolved.
//
// Permission requests are answered by policy, since nobody is watching:
// the first allow_once option (the Run is already sandboxed; its limits are
// the container's). See docs/adapters.md.
type ACP struct {
	rpc     rpcConn
	mu      sync.Mutex
	sink    Sink
	session string
	busy    bool
	stopped bool
	// queue: inputs for the next turns; queueBytes their inputSize, held
	// under maxPendingSteerBytes.
	queue      []proto.Input
	queueBytes int
	ready      chan struct{}
	// loading: session/load replays the conversation as updates; they are
	// events, not new output.
	loading bool
	// images: the agent advertised promptCapabilities.image.
	images bool
	inputs inputLedger

	// OpenCode only. bus: OpenCode's server, followed for activity; lux
	// steers through it and reads receipts from it only when lux built the
	// command (steerBus). ocUser, ocPassword: its Basic auth, from the
	// workload's environment (WorkloadEnv).
	opencode           bool
	bus                *opencodeBus
	ocUser, ocPassword string
	// inflight: session/prompt calls of the running turn not yet
	// resolved; the turn ends when it drops to 0. turnEnd is the
	// turn-starting prompt's result, which acp.turn_end reports (joined
	// prompts resolve with the same stopReason and usage).
	inflight int
	turnEnd  map[string]any
	// busTurn: the ACP turn has ended but the Run stays busy until lux
	// knows every steer of it was read, or runs in a loop OpenCode started
	// outside ACP (a steer that reached it as the turn ended, or one sent
	// again after an interrupt), followed on the bus (settle).
	busTurn bool
	// reserved: steers whose prompt_async has not returned; a turn cannot
	// be settled until they have.
	reserved int
	// endHeld: the ACP turn's acp.turn_end, held while lux cannot yet tell
	// that no loop runs (a steer's prompt_async not returned, or a steer
	// unread). extraLoop: after the ACP turn ended, OpenCode ran another
	// loop (status busy then, a step answering a steer admittedLate, or one
	// sent again), which gets a turn end of its own. Both are reported only
	// once OpenCode is idle (busTurnEnded). admittedLate: message ids of
	// steers whose prompt_async returned after the ACP turn ended.
	endHeld      map[string]any
	extraLoop    bool
	admittedLate map[string]bool
	// cancelGen counts the session/cancels sent.
	cancelGen int
	// steers: inputs for the running turn, in the order they came, which
	// steerLoop delivers; steerKick wakes it. steerBytes: their size (text
	// and image data, inputSize), bounded with their count
	// (maxPendingSteers).
	steers     []proto.Input
	steerBytes int
	steerKick  chan struct{}
	// settleMu serializes settle. suspect: steers seen stored and
	// unanswered with no loop running, by message id, and when first seen.
	// resent: request ids settle sent again since the ACP turn ended.
	// settleErrs: settle's looks in a row OpenCode did not answer.
	// recheckStop: stops settle's next look while one is scheduled; nil
	// otherwise.
	settleMu    sync.Mutex
	suspect     map[string]time.Time
	resent      map[string]bool
	settleErrs  int
	settleEvery time.Duration
	recheckStop func() bool
	clock       settleClock
	ctx         context.Context
	// closed: Run is returning; no goroutine starts after it.
	closed bool
	// bg: the adapter's own goroutines, joined when Run returns.
	bg sync.WaitGroup

	// ocBusy (under mu): OpenCode's bus reports a loop running for the
	// session (session.status busy or retry), whoever started it; cleared
	// by idle and whenever the stream is down. The Run's activity is busy
	// while ocBusy or lux's own turn is. actMu orders activity reports;
	// shown and shownIdle (under actMu) are the last one reported; ocGen
	// (under actMu) counts the bus status events handled and the stream's
	// connects and disconnects. Under actMu too: ocUp, the stream is
	// connected; statusSeq numbers the status reads dispatched, of which
	// only the newest applies; statusCancel cancels the one in flight;
	// statusReads counts those not yet applied or discarded.
	ocBusy    bool
	actMu     sync.Mutex
	shown     bool
	shownIdle bool
	ocGen     int
	ocUp      bool
	// ocObserved (under actMu) is the latest status on this stream,
	// before the refusal mask. A successful read can lift that mask even
	// when a newer event has superseded the read's status.
	ocObserved   bool
	statusSeq    int
	statusCancel context.CancelFunc
	statusReads  int
	// statusRead, if set (by a test, before Run), gets the error of each
	// status read once it is applied or discarded, under actMu; busHandled
	// gets each bus event once onBus has handled it; statusGot gets each
	// status read's error as its response is in, before actMu.
	statusRead func(error)
	busHandled func(busEvent)
	statusGot  func(error)
	// compactionTimeout bounds reading a compaction's summary.
	compactionTimeout time.Duration
	// compactionMu runs one compaction's summary read at a time, so each
	// sees lastSummary, the summary the one before it reported.
	compactionMu sync.Mutex
	lastSummary  string
}

func NewACP() *ACP { return &ACP{ready: make(chan struct{})} }

// NewOpenCode is the ACP adapter for OpenCode.
func NewOpenCode() *ACP {
	return &ACP{ready: make(chan struct{}), opencode: true, steerKick: make(chan struct{}, 1),
		suspect: map[string]time.Time{}, resent: map[string]bool{}, admittedLate: map[string]bool{}, settleEvery: time.Second, clock: wallClock{},
		compactionTimeout: 10 * time.Second}
}

// settleClock is the time settle reads and schedules its next look by.
type settleClock interface {
	now() time.Time
	// afterFunc runs f after d; stop reports whether it stopped f first.
	afterFunc(d time.Duration, f func()) (stop func() bool)
}

type wallClock struct{}

// settle's bounds: a steer sent again and still stored, unread and idle
// for settleGiveUp fails; GET errors back off up to settleMaxBackoff.
const (
	settleGiveUp     = 2 * time.Minute
	settleMaxBackoff = 30 * time.Second
)

// uncertainNoStep: a steer stored and not seen answered when the loop that
// could have read it ended normally. It may have been read (by a step
// answering a message lux did not send), so lux does not send it again.
const uncertainNoStep = "uncertain: OpenCode stored it and its loop ended with no step lux saw answer it; it may have been read, so it is not sent again"

// uncertainCancelled: a steer stored and not seen answered when its loop
// was interrupted, after a model step stored after it (which had it in
// context).
const uncertainCancelled = "uncertain: it may have been read before the interrupt; not sent again"

// Steers waiting for the one in flight: past either limit, a new one
// fails at once.
const (
	maxPendingSteers      = 256
	maxPendingSteerBytes  = 32 << 20
	errPendingSteersLimit = "too many inputs wait for the agent (256 inputs or 32 MiB)"
)

func (wallClock) now() time.Time { return time.Now() }
func (wallClock) afterFunc(d time.Duration, f func()) func() bool {
	return time.AfterFunc(d, f).Stop
}

// Command: for OpenCode, `opencode acp` also serves its HTTP API on a
// loopback port (--port), which lux steers through. A command lux did not
// build (no "acp" argument, a --port of its own, or a resume command) is
// run as given, and steers go over ACP only; if it has a --port of its own,
// the server there is followed on 127.0.0.1 for the Run's activity alone
// (an observer bus).
func (a *ACP) Command(cfg proto.ShimConfig) ([]string, error) {
	argv, err := command(cfg)
	if err != nil || !a.opencode {
		return argv, err
	}
	if (cfg.Resume && len(cfg.ResumeCommand) > 0) ||
		!slices.Contains(argv, "acp") || slices.ContainsFunc(argv, func(s string) bool { return strings.HasPrefix(s, "--port") || strings.HasPrefix(s, "--hostname") }) {
		if port, ok := ownPort(argv); ok {
			a.bus = newOpencodeObserver(port, workdir(cfg), a.ocUser, a.ocPassword)
		}
		return argv, nil
	}
	port, perr := freeLoopbackPort()
	if perr != nil {
		return argv, nil
	}
	a.bus = newOpencodeBus(port, workdir(cfg))
	return append(slices.Clone(argv), "--port", strconv.Itoa(port), "--hostname", "127.0.0.1"), nil
}

// ownPort is the value of a command's last --port N or --port=N argument,
// if it is a TCP port. A last --port with no value or a bad one is no port:
// an earlier --port does not stand in for it.
func ownPort(argv []string) (int, bool) {
	val := ""
	for i, s := range argv {
		switch {
		case s == "--port":
			val = ""
			if i+1 < len(argv) {
				val = argv[i+1]
			}
		case strings.HasPrefix(s, "--port="):
			val = strings.TrimPrefix(s, "--port=")
		}
	}
	n, err := strconv.Atoi(val)
	return n, err == nil && n > 0 && n < 1<<16
}

// OpenCode's server takes HTTP Basic auth when OPENCODE_SERVER_PASSWORD is
// set in its environment, as user OPENCODE_SERVER_USERNAME, by default
// "opencode" (OpenCode's server config and `opencode attach --password`).
const (
	ocPasswordEnv = "OPENCODE_SERVER_PASSWORD"
	ocUsernameEnv = "OPENCODE_SERVER_USERNAME"
	ocDefaultUser = "opencode"
)

// WorkloadEnv keeps the credentials of OpenCode's server from the
// workload's environment (KEY=VALUE, the last of a key standing), for an
// observer bus Command creates.
func (a *ACP) WorkloadEnv(env []string) {
	if !a.opencode {
		return
	}
	a.ocUser, a.ocPassword = ocDefaultUser, ""
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		switch k {
		case ocPasswordEnv:
			a.ocPassword = v
		case ocUsernameEnv:
			a.ocUser = cmp.Or(v, ocDefaultUser)
		}
	}
}

// steerBus is OpenCode's server when lux steers through it and reads
// receipts from it: only the one lux started (Command). nil for an
// observer bus, which only the Run's activity follows.
func (a *ACP) steerBus() *opencodeBus {
	if a.bus == nil || a.bus.observer {
		return nil
	}
	return a.bus
}

// observerGaveUp reports, once, an observer bus whose server refused it
// refusalLimit times in a row (err, the last refusal): it sends nothing
// more, and the Run's activity is lux's own turns'.
func (a *ACP) observerGaveUp(err error) {
	a.sink.Event(proto.EvWarning, map[string]any{"message": fmt.Sprintf(
		"opencode: its server refused lux %d times in a row; lux no longer follows its activity: %v", refusalLimit, err)})
}

func (a *ACP) Run(ctx context.Context, p *Process, cfg proto.ShimConfig, sink Sink) error {
	ctx, cancel := context.WithCancel(ctx)
	a.mu.Lock()
	a.sink, a.ctx = sink, ctx
	a.mu.Unlock()
	defer a.exited(cancel)
	a.rpc.attach(p.Stdin)

	go pump(p.Stderr, sink.Stderr)
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		a.rpc.readLoop(p.Stdout, a.handleRequest, a.handleNotification, sink.Stdout)
	}()

	if a.opencode {
		a.spawn(func() { a.steerLoop(ctx) })
	}
	if a.bus != nil {
		a.bg.Add(1)
		go func() {
			defer a.bg.Done()
			err := a.bus.follow(ctx, a.onBus, func() {
				a.streamUp()
				a.settle()
			}, a.streamDown)
			if err != nil {
				a.observerGaveUp(err)
			}
		}()
	}
	err := a.handshake(cfg)
	cfg.PromptAttachments = nil // a.queue holds them while they are needed
	if err != nil {
		sink.Event(proto.EvWarning, map[string]any{"message": "acp: " + err.Error()})
		_ = p.Signal(syscall.SIGTERM)
	} else {
		close(a.ready)
		a.drain()
	}
	<-readDone
	return err
}

func (a *ACP) handshake(cfg proto.ShimConfig) error {
	res, err := a.rpc.call("initialize", map[string]any{
		"protocolVersion": 1,
		// Terminal and file-system access stay inside the agent: lux does
		// not proxy them.
		"clientCapabilities": map[string]any{"fs": map[string]bool{"readTextFile": false, "writeTextFile": false}, "terminal": false},
		"clientInfo":         map[string]string{"name": "lux", "version": "1"},
	})
	if err != nil {
		return err
	}
	var init struct {
		AgentCapabilities struct {
			LoadSession     bool `json:"loadSession"`
			MCPCapabilities struct {
				HTTP bool `json:"http"`
			} `json:"mcpCapabilities"`
			PromptCapabilities struct {
				Image bool `json:"image"`
			} `json:"promptCapabilities"`
		} `json:"agentCapabilities"`
	}
	_ = json.Unmarshal(res, &init)
	a.mu.Lock()
	a.images = init.AgentCapabilities.PromptCapabilities.Image
	a.mu.Unlock()
	cwd := workdir(cfg)
	mcp := acpMCPServers(cfg.MCP)
	if len(mcp) > 0 && !init.AgentCapabilities.MCPCapabilities.HTTP {
		// ACP lets a client send only transports the agent advertises.
		a.sink.Event(proto.EvWarning, map[string]any{"message": "the agent does not support HTTP MCP servers: workload.mcpServers are not given to it"})
		mcp = []any{}
	}
	if cfg.Resume && cfg.SessionID != "" && init.AgentCapabilities.LoadSession {
		a.setLoading(true)
		_, err := a.rpc.call("session/load", map[string]any{"sessionId": cfg.SessionID, "cwd": cwd, "mcpServers": mcp})
		a.setLoading(false)
		if err == nil {
			a.seedLastSummary(cfg.SessionID)
			a.setSession(cfg.SessionID)
			return nil
		}
		a.sink.Event(proto.EvWarning, map[string]any{"message": "session/load failed, starting a new session: " + err.Error()})
	}
	res, err = a.rpc.call("session/new", map[string]any{"cwd": cwd, "mcpServers": mcp})
	if err != nil {
		return err
	}
	var ns struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(res, &ns); err != nil || ns.SessionID == "" {
		return fmt.Errorf("session/new returned no sessionId")
	}
	a.setSession(ns.SessionID)
	if !cfg.Resume && (cfg.Prompt != "" || len(cfg.PromptAttachments) > 0) {
		a.mu.Lock()
		a.queue = append([]proto.Input{{RequestID: "prompt", Text: cfg.Prompt, Attachments: cfg.PromptAttachments}}, a.queue...)
		a.queueBytes += inputSize(a.queue[0])
		a.mu.Unlock()
	}
	return nil
}

// acpMCPServers are the servers as ACP HTTP McpServer entries.
func acpMCPServers(servers []proto.MCPServer) []any {
	out := []any{}
	for _, s := range servers {
		headers := []map[string]string{}
		for _, h := range s.Headers {
			headers = append(headers, map[string]string{"name": h.Name, "value": h.Value})
		}
		out = append(out, map[string]any{"type": "http", "name": s.Name, "url": s.URL, "headers": headers})
	}
	return out
}

func (a *ACP) setLoading(v bool) {
	a.mu.Lock()
	a.loading = v
	a.mu.Unlock()
}

func (a *ACP) setSession(id string) {
	a.mu.Lock()
	a.session = id
	a.mu.Unlock()
	a.sink.Session(id)
	a.activity(true)
	// A stream that connected before the session was known read no status.
	if a.bus != nil {
		a.requestStatus()
	}
}

// drain sends the next queued input as a prompt, if the agent is idle.
func (a *ACP) drain() {
	a.mu.Lock()
	select {
	case <-a.ready:
	default:
		a.mu.Unlock()
		return
	}
	if a.busy || len(a.queue) == 0 || a.stopped {
		a.mu.Unlock()
		return
	}
	in := a.queue[0]
	a.queue[0] = proto.Input{}
	a.queue = a.queue[1:]
	a.queueBytes -= inputSize(in)
	if len(in.Attachments) > 0 && !a.images {
		// Known only since initialize: the input fails, the next is sent.
		a.mu.Unlock()
		a.inputs.fail(a.sink, in, errNoImages)
		a.drain()
		// The queue that kept the Run busy may now be empty with no turn
		// started; no turn end will report that.
		if a.bus != nil {
			a.showCombined()
		}
		return
	}
	a.busy, a.inflight, a.turnEnd = true, 1, nil
	session := a.session
	a.mu.Unlock()

	a.activity(false)
	wait, err := a.rpc.start("session/prompt", map[string]any{"sessionId": session, "prompt": inputContent(dialectACP, in)})
	if err != nil {
		a.inputs.fail(a.sink, in, err)
		a.promptDone(nil, err, true)
		return
	}
	// A prompt that starts a turn is read by its first step; lux has no
	// signal for that on ACP.
	lands := LandsNextTurn
	if a.opencode {
		lands = LandsNextStep
	}
	a.inputs.accept(a.sink, in, Delivery{Lands: lands}, "")
	a.spawn(func() {
		res, err := wait()
		a.promptDone(res, err, true)
	})
}

// spawn runs f on a goroutine Run joins before it returns; none once Run
// is returning. It reports whether f was started.
func (a *ACP) spawn(f func()) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return false
	}
	a.bg.Add(1)
	go func() {
		defer a.bg.Done()
		f()
	}()
	return true
}

// promptDone is a session/prompt of the running turn resolving; the last
// one ends the turn. first: the prompt that started the turn, whose result
// the turn's acp.turn_end carries.
func (a *ACP) promptDone(res json.RawMessage, err error, first bool) {
	var pr struct {
		StopReason string          `json:"stopReason"`
		Usage      json.RawMessage `json:"usage"`
	}
	_ = json.Unmarshal(res, &pr)
	data := withUsage(map[string]any{"stopReason": pr.StopReason}, pr.Usage)
	if err != nil {
		data["error"] = err.Error()
	}
	a.mu.Lock()
	a.inflight--
	if first || a.turnEnd == nil {
		a.turnEnd = data
	}
	if a.inflight > 0 {
		a.mu.Unlock()
		return
	}
	data = a.turnEnd
	a.mu.Unlock()
	a.endTurn(data)
}

// exited ends the adapter's own work once the agent has exited: its
// goroutines are stopped and joined, and each input it accepted and the
// agent never read fails.
func (a *ACP) exited(cancel context.CancelFunc) {
	cancel()
	a.mu.Lock()
	a.closed = true
	stopped := a.stopped
	if a.recheckStop != nil && a.recheckStop() {
		a.bg.Done()
	}
	a.mu.Unlock()
	a.bg.Wait()
	why := unreadWhy(stopped)
	a.mu.Lock()
	undelivered := a.steers
	a.steers, a.steerBytes = nil, 0
	a.mu.Unlock()
	a.reportHeldEnd()
	for _, in := range undelivered {
		a.inputs.fail(a.sink, in, why)
	}
	a.inputs.close(a.sink, why)
}

// runCtx is the Run's context: HTTP calls to OpenCode end with it.
func (a *ACP) runCtx() context.Context {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ctx == nil {
		return context.Background()
	}
	return a.ctx
}

// endTurn ends the running turn: acp.turn_end, then idle unless more input
// waits. With OpenCode's server, a turn with a steer whose prompt_async has
// not returned, or one not known read, holds its turn end (endHeld) and is
// settled: that steer may run in a loop after this one.
func (a *ACP) endTurn(data map[string]any) {
	if a.steerBus() != nil {
		a.receipts(a.runCtx())
		a.mu.Lock()
		if a.reserved > 0 || len(a.inputs.unread("bus")) > 0 {
			a.busTurn, a.endHeld, a.extraLoop = true, data, false
			a.mu.Unlock()
			a.settle()
			return
		}
		a.mu.Unlock()
	}
	a.reportTurnEnd(data)
	a.mu.Lock()
	a.busy = false
	idle := len(a.queue) == 0
	a.mu.Unlock()
	if idle {
		a.activity(true)
	}
	a.drain()
}

// reportTurnEnd writes acp.turn_end. Replies stream in chunks without line
// breaks: the turn's text ends on a line of its own first.
func (a *ACP) reportTurnEnd(data map[string]any) {
	a.sink.EndMessage()
	a.sink.Event("acp.turn_end", data)
}

// reportHeldEnd reports the ACP turn's end if endTurn held it.
func (a *ACP) reportHeldEnd() {
	a.mu.Lock()
	held := a.endHeld
	a.endHeld = nil
	a.mu.Unlock()
	if held != nil {
		a.reportTurnEnd(held)
	}
}

// receipts consumes the steers OpenCode's stored messages show read
// (bus.answered for each assistant step's parent). It returns the ids of
// the user messages stored, the newest assistant step's id time in unix ms
// (0 if none), and false if OpenCode did not answer.
func (a *ACP) receipts(ctx context.Context) (map[string]bool, int64, bool) {
	oldest := a.bus.oldest()
	if oldest == "" {
		return nil, 0, true
	}
	a.mu.Lock()
	session := a.session
	a.mu.Unlock()
	msgs, err := a.bus.messagesSince(ctx, session, oldest)
	if err != nil {
		return nil, 0, false
	}
	stored, lastStep := map[string]bool{}, int64(0)
	for _, m := range msgs {
		a.bus.observe(m.Info.ID)
		switch {
		case m.Info.Role == "user":
			stored[m.Info.ID] = true
		case m.Info.Role == "assistant":
			if v, ok := idValue(m.Info.ID); ok {
				lastStep = max(lastStep, v>>12)
			}
			if m.Info.ParentID != "" {
				a.read(m.Info.ParentID)
			}
		}
	}
	return stored, lastStep, true
}

// read consumes the steers a model step answering parent has read. The
// step ran in a loop after the ACP turn's only if parent's prompt_async
// returned after that turn ended (admittedLate): a step of the ACP turn's
// own loop can be seen late, since stdout, the bus and stored messages are
// read separately.
func (a *ACP) read(parent string) {
	ids := a.bus.answered(parent)
	if len(ids) == 0 {
		return
	}
	a.mu.Lock()
	after := a.busTurn && a.admittedLate[parent]
	delete(a.admittedLate, parent)
	a.mu.Unlock()
	if after {
		a.anotherLoop()
	}
	for _, id := range ids {
		a.inputs.consume(a.sink, id)
	}
}

// anotherLoop notes that OpenCode runs a loop after the ACP turn's. Both
// ends wait for busTurnEnded: none is reported while a loop may run.
func (a *ACP) anotherLoop() {
	a.mu.Lock()
	if a.busTurn {
		a.extraLoop = true
	}
	a.mu.Unlock()
}

// settle decides, once the ACP turn has ended (busTurn), whether OpenCode
// still has work of lux's. It reads OpenCode's stored messages and session
// status rather than trusting bus events, which a dropped stream loses:
//
//   - a steer an assistant step answered is consumed;
//   - while a prompt_async is in flight the Run stays busy (its return
//     settles again), and while OpenCode runs a loop, until session.idle;
//   - a steer not stored yet is still on its way, unless OpenCode has run
//     no loop for settleGiveUp since: then it fails;
//   - a steer stored and unanswered while no loop runs was dropped by a
//     loop cancelled after it was sent, when seen so on two looks
//     settleEvery apart, and no assistant step was stored after it: it is
//     sent again, so it starts the next loop, or fails if the Run is
//     stopping. That copy, again stored and unanswered with no loop for
//     settleGiveUp, fails: OpenCode will not read it. With a step stored
//     after it, that step may have read it: it fails as uncertain
//     (uncertainCancelled) and is not sent again;
//   - a steer stored and unanswered for 3×settleEvery after a loop that
//     ended normally fails as uncertain (uncertainNoStep): that loop may
//     have read it, so it is never sent again;
//   - with none of these, the Run's work has ended (busTurnEnded).
//
// While it waits it looks again every settleEvery (backing off while
// OpenCode's GETs fail), so a lost session.idle cannot leave the Run busy.
// Also run when the event stream (re)connects; before the ACP turn has
// ended it only consumes.
func (a *ACP) settle() {
	if a.steerBus() == nil {
		return
	}
	a.settleMu.Lock()
	defer a.settleMu.Unlock()
	ctx := a.runCtx()
	if ctx.Err() != nil {
		return
	}
	a.mu.Lock()
	bt, session, reserved := a.busTurn, a.session, a.reserved
	a.mu.Unlock()
	if !bt {
		a.receipts(ctx)
		return
	}
	if reserved > 0 {
		return
	}
	busy, err := a.bus.sessionBusy(ctx, session)
	// Read after the status: a message stored and unanswered here was
	// stored while no loop ran.
	stored, lastStep, ok := a.receipts(ctx)
	a.mu.Lock()
	if err != nil || !ok {
		a.settleErrs++
	} else {
		a.settleErrs = 0
	}
	if busy {
		clear(a.suspect)
	}
	a.mu.Unlock()
	if err != nil || !ok || busy {
		// A loop may run while OpenCode does not answer: no end yet.
		if busy {
			a.anotherLoop()
		}
		a.recheck()
		return
	}
	// OpenCode runs no loop now and every prompt_async has returned: the
	// ACP turn's end, if held, is reported; a loop started after this
	// (a steer sent again) ends with busTurnEnded.
	a.reportHeldEnd()
	now, waiting := a.clock.now(), false
	for _, in := range a.inputs.unread("bus") {
		msgID, gen := a.bus.messageOf(in.RequestID)
		a.mu.Lock()
		since, seen := a.suspect[msgID]
		if !seen {
			a.suspect[msgID] = now
		}
		cancelled := a.cancelGen > gen
		// A copy dropped by an interrupt is sent again; that copy, dropped
		// without one, is given settleGiveUp to be read.
		giveUp := !cancelled && a.resent[in.RequestID]
		a.mu.Unlock()
		if !stored[msgID] {
			// Accepted (204) and never stored, with no loop running.
			if seen && now.Sub(since) >= settleGiveUp {
				a.failSteer(in, errors.New("OpenCode accepted it and never stored it"))
				continue
			}
			waiting = true
			continue
		}
		wait := 3 * a.settleEvery
		switch {
		case cancelled:
			wait = a.settleEvery
		case giveUp:
			wait = settleGiveUp
		}
		if !seen || now.Sub(since) < wait {
			waiting = true
			continue
		}
		a.mu.Lock()
		delete(a.suspect, msgID)
		a.mu.Unlock()
		// A step stored at or after the steer (ids in ms: two generators
		// do not order within one) had it in context, whatever its parent.
		steerMs, _ := idValue(msgID)
		followed := lastStep >= steerMs>>12
		switch {
		case giveUp:
			a.failSteer(in, errors.New("OpenCode stored it and never read it, also when sent again"))
		case !cancelled:
			a.failSteer(in, errors.New(uncertainNoStep))
		case followed:
			a.failSteer(in, errors.New(uncertainCancelled))
		case a.carry(ctx, session, in):
			waiting = true
		}
	}
	if waiting {
		a.recheck()
		return
	}
	a.busTurnEnded()
}

// failSteer fails a steer sent over HTTP and stops following its message
// ids on the bus.
func (a *ACP) failSteer(in proto.Input, err error) {
	a.bus.untrackRequest(in.RequestID)
	a.inputs.fail(a.sink, in, err)
}

// recheck settles again in settleEvery, unless a look is already due;
// after GET errors in a row, in twice as long each, up to settleMaxBackoff.
func (a *ACP) recheck() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.recheckStop != nil || a.closed || a.ctx == nil || a.ctx.Err() != nil {
		return
	}
	d := a.settleEvery
	for range a.settleErrs {
		d = min(2*d, settleMaxBackoff)
	}
	a.bg.Add(1)
	a.recheckStop = a.clock.afterFunc(d, func() {
		defer a.bg.Done()
		a.mu.Lock()
		a.recheckStop = nil
		a.mu.Unlock()
		a.settle()
	})
}

// carry sends a steer OpenCode dropped unread (its loop was cancelled)
// again, under the same request id and a new message id, so it starts the
// next loop; the dropped copy stays in the conversation, unanswered. When
// the Run is stopping, or OpenCode refuses it, it fails instead. It reports
// whether it was sent.
func (a *ACP) carry(ctx context.Context, session string, in proto.Input) bool {
	a.bus.untrackRequest(in.RequestID)
	a.mu.Lock()
	stopped, gen := a.stopped, a.cancelGen
	a.mu.Unlock()
	if stopped {
		a.inputs.fail(a.sink, in, errStoppedUnread)
		return false
	}
	msgID := a.bus.messageID(time.Now())
	a.bus.track(msgID, in.RequestID, gen)
	a.mu.Lock()
	a.resent[in.RequestID] = true
	// A step answering this copy runs in a loop after the ACP turn's, even
	// one seen before prompt_async returns.
	a.admittedLate[msgID] = true
	a.mu.Unlock()
	if err := a.bus.promptAsync(ctx, session, msgID, in); err != nil {
		// Refused or undialled, nothing ran: no loop of its own.
		a.bus.untrack(msgID)
		a.mu.Lock()
		delete(a.admittedLate, msgID)
		a.mu.Unlock()
		a.inputs.fail(a.sink, in, fmt.Errorf("sending it again after the turn was cancelled: %w", err))
		return false
	}
	a.anotherLoop()
	return true
}

// busTurnEnded ends the Run's work after its ACP turn, once no loop runs:
// the ACP turn's end if it was held, then one for the loops OpenCode ran
// after it if any did. Only settle calls it (under settleMu).
func (a *ACP) busTurnEnded() {
	a.mu.Lock()
	held, extra := a.endHeld, a.extraLoop
	a.busTurn, a.busy, a.endHeld, a.extraLoop = false, false, nil, false
	clear(a.resent)
	clear(a.admittedLate)
	idle := len(a.queue) == 0
	a.mu.Unlock()
	if held != nil {
		a.reportTurnEnd(held)
	}
	if extra {
		a.reportTurnEnd(map[string]any{"stopReason": "end_turn", "source": "opencode-bus"})
	}
	if idle {
		a.activity(true)
	}
	a.drain()
}

// steerLoop delivers steers one at a time, in the order they came, until
// the Run ends.
func (a *ACP) steerLoop(ctx context.Context) {
	for {
		a.mu.Lock()
		if len(a.steers) > 0 && ctx.Err() == nil {
			in := a.steers[0]
			// Clear the slot: the backing array outlives the steer.
			a.steers[0] = proto.Input{}
			a.steers = a.steers[1:]
			a.steerBytes -= inputSize(in)
			if len(a.steers) == 0 {
				a.steers = nil
			}
			a.mu.Unlock()
			a.steer(in)
			continue
		}
		a.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-a.steerKick:
		}
	}
}

// steer delivers input to OpenCode's running turn, read at its next step:
// through the HTTP server, with a receipt, or else as a second
// session/prompt, which OpenCode joins to the running loop (without one).
func (a *ACP) steer(in proto.Input) {
	ctx := a.runCtx()
	a.inputs.track(in)
	a.mu.Lock()
	session, busy := a.session, a.busy && !a.busTurn
	a.mu.Unlock()
	if !busy {
		// The turn ended meanwhile: the input starts the next one.
		a.queueInput(in)
		return
	}
	if a.steerBus() != nil {
		if a.bus.waitConnected(ctx, 5*time.Second) {
			a.steerHTTP(ctx, session, in)
			return
		}
		msg := "opencode: its event stream is not connected; steering over ACP, without a receipt"
		if err := a.bus.err(); err != nil {
			msg += ": " + err.Error()
		}
		a.sink.Event(proto.EvWarning, map[string]any{"message": msg})
	}
	a.steerACP(session, in)
}

// steerHTTP sends a steer through prompt_async under a message id lux
// chooses, with a receipt. One OpenCode certainly did not take goes as a
// second session/prompt, or to the next turn.
func (a *ACP) steerHTTP(ctx context.Context, session string, in proto.Input) {
	a.bus.seed(ctx, session)
	msgID := a.bus.messageID(time.Now())
	// Reserved against the running turn: it is not settled until the
	// request returns.
	a.mu.Lock()
	if !a.busy || a.busTurn || a.stopped {
		a.mu.Unlock()
		a.queueInput(in)
		return
	}
	a.reserved++
	a.bus.track(msgID, in.RequestID, a.cancelGen)
	a.mu.Unlock()
	err := a.bus.promptAsync(ctx, session, msgID, in)
	if err == nil {
		// Already answered (untracked): the ACP turn's own loop read it.
		tracked, _ := a.bus.messageOf(in.RequestID)
		a.mu.Lock()
		if a.busTurn && tracked == msgID {
			a.admittedLate[msgID] = true
		}
		a.mu.Unlock()
		a.inputs.accept(a.sink, in, Delivery{Lands: LandsNextStep, Receipt: true}, "bus")
	} else {
		a.bus.untrack(msgID)
	}
	a.mu.Lock()
	a.reserved--
	settle := a.busTurn && a.reserved == 0
	a.mu.Unlock()
	switch {
	case err == nil:
	case !errors.Is(err, errNotSent):
		a.inputs.fail(a.sink, in, err)
	case settle || len(a.inputs.unread("bus")) > 0:
		// The turn ended while OpenCode refused it, or a steer sent over
		// HTTP is unread (see steerACP): the next turn.
		a.queueInput(in)
	default:
		a.sink.Event(proto.EvWarning, map[string]any{"message": "opencode: steering over ACP instead: " + err.Error()})
		a.steerACP(session, in)
	}
	if settle {
		a.settle()
	}
}

// steerACP sends a steer as a second session/prompt, which OpenCode joins
// to the running loop; no receipt. Not while a steer sent over HTTP is
// unread: OpenCode stores the prompt under an id of its own, so a step
// answering it would read that steer without lux knowing. It waits for the
// next turn instead.
func (a *ACP) steerACP(session string, in proto.Input) {
	unreadHTTP := a.steerBus() != nil && len(a.inputs.unread("bus")) > 0
	a.mu.Lock()
	if !a.busy || a.busTurn || a.stopped || unreadHTTP {
		a.mu.Unlock()
		a.queueInput(in)
		return
	}
	if len(in.Attachments) > 0 && !a.images {
		a.mu.Unlock()
		a.inputs.fail(a.sink, in, errNoImages)
		return
	}
	a.inflight++
	a.mu.Unlock()
	wait, err := a.rpc.start("session/prompt", map[string]any{"sessionId": session, "prompt": inputContent(dialectACP, in)})
	if err != nil {
		a.inputs.fail(a.sink, in, err)
		a.promptDone(nil, err, false)
		return
	}
	a.inputs.accept(a.sink, in, Delivery{Lands: LandsNextStep}, "")
	a.spawn(func() {
		res, err := wait()
		a.promptDone(res, err, false)
	})
}

// queueInput holds input for the next turn.
func (a *ACP) queueInput(in proto.Input) {
	a.inputs.forget(in.RequestID)
	a.mu.Lock()
	a.queue = append(a.queue, in)
	a.queueBytes += inputSize(in)
	a.mu.Unlock()
	a.drain()
}

// onBus follows OpenCode's bus: an assistant step answering a steer lux
// sent has read it and the steers sent before it (bus.answered);
// session.idle after the ACP turn has ended settles the Run's work;
// session.status is the Run's activity (setOpenCodeBusy); session.compacted
// of the Run's session is reported as lux.compacted (compacted).
func (a *ACP) onBus(ev busEvent) {
	if a.busHandled != nil {
		defer a.busHandled(ev)
	}
	a.mu.Lock()
	session, bt := a.session, a.busTurn
	a.mu.Unlock()
	p := ev.Properties
	switch ev.Type {
	case "message.updated":
		if a.steerBus() == nil {
			return
		}
		if p.Info.SessionID == session {
			a.bus.observe(p.Info.ID)
		}
		if p.Info.Role == "assistant" && p.Info.SessionID == session && p.Info.ParentID != "" {
			a.read(p.Info.ParentID)
		}
	case "session.status":
		if busy, known := busyStatus(p.Status.Type); known && session != "" && p.SessionID == session {
			a.setOpenCodeBusy(busy)
		}
	case "session.idle":
		if session != "" && p.SessionID == session {
			a.setOpenCodeBusy(false)
		}
		if p.SessionID == session && bt {
			a.settle()
		}
	case "session.compacted":
		if session != "" && p.SessionID == session {
			a.compacted(session)
		}
	}
}

// compacted reports a compaction of the session as lux.compacted, with the
// summary read from OpenCode's stored messages (session.compacted carries
// none), off the bus goroutine and within compactionTimeout. Unread, the
// record goes without it, and a warning says why. A session.compacted
// whose summary was already reported is a duplicate: no record. One that
// comes as the Run ends (no read can start) is reported without a summary,
// on the bus goroutine, which the Run's end waits for.
func (a *ACP) compacted(session string) {
	started := a.spawn(func() {
		a.compactionMu.Lock()
		defer a.compactionMu.Unlock()
		ctx, cancel := context.WithTimeout(a.runCtx(), a.compactionTimeout)
		defer cancel()
		summary, trigger, id, err := a.bus.compaction(ctx, session, a.lastSummary)
		if errors.Is(err, errCompactionReported) {
			a.sink.Event(proto.EvWarning, map[string]any{"message": fmt.Sprintf(
				"opencode: session %s was compacted again, with %s", session, err)})
			return
		}
		if id != "" {
			a.lastSummary = id
		}
		if err != nil && a.runCtx().Err() != nil {
			err = errRunEndedBeforeSummary
		}
		reportCompacted(a.sink, "opencode: session", proto.Compaction{SessionID: session, Trigger: trigger, Summary: summary}, err)
	})
	if !started {
		reportCompacted(a.sink, "opencode: session", proto.Compaction{SessionID: session}, errRunEndedBeforeSummary)
	}
}

var errRunEndedBeforeSummary = errors.New("the Run ended before it was read")

// seedLastSummary sets lastSummary to the newest summary a resumed session
// already holds, so the Run's first compaction does not report an earlier
// Run's. Runs before setSession, so no compaction read races it. Unread,
// lastSummary stays empty.
func (a *ACP) seedLastSummary(session string) {
	if a.bus == nil {
		return
	}
	ctx, cancel := context.WithTimeout(a.runCtx(), a.compactionTimeout)
	defer cancel()
	id, err := a.bus.newestSummary(ctx, session)
	if err != nil {
		a.sink.Event(proto.EvWarning, map[string]any{"message": fmt.Sprintf(
			"opencode: the resumed session %s's earlier summaries could not be read; its first compaction may report one of them: %v", session, err)})
		return
	}
	a.compactionMu.Lock()
	a.lastSummary = id
	a.compactionMu.Unlock()
}

// setOpenCodeBusy records whether OpenCode runs a loop for the session and
// reports the Run's activity if that changes it.
func (a *ACP) setOpenCodeBusy(busy bool) {
	a.actMu.Lock()
	defer a.actMu.Unlock()
	a.ocGen++
	a.ocObserved = busy
	a.applyOpenCodeBusyLocked(busy)
}

// applyOpenCodeBusyLocked sets ocBusy and reports the combined activity.
// Lux's side is idle only with no turn of its own and nothing queued for
// one, so a turn about to start does not flash idle. A server refusing an
// observer is never shown busy. Under actMu.
func (a *ACP) applyOpenCodeBusyLocked(busy bool) {
	busy = busy && !a.bus.refusing()
	a.mu.Lock()
	a.ocBusy = busy
	idle := !a.busy && !busy && len(a.queue) == 0
	a.mu.Unlock()
	a.showLocked(idle)
}

// streamUp is the event stream (re)connecting: events from while it was
// down are not replayed, so OpenCode's status is read, off the stream's
// goroutine, which goes on to read events at once.
func (a *ACP) streamUp() {
	a.actMu.Lock()
	defer a.actMu.Unlock()
	a.ocGen++
	a.ocUp = true
	a.requestStatusLocked()
}

// streamDown is an established event stream ending: OpenCode's side counts
// as idle, and a status read in flight is cancelled and never applied.
func (a *ACP) streamDown() {
	a.actMu.Lock()
	defer a.actMu.Unlock()
	a.ocGen++
	a.ocUp = false
	a.ocObserved = false
	if a.statusCancel != nil {
		a.statusCancel()
		a.statusCancel = nil
	}
	a.applyOpenCodeBusyLocked(false)
}

// requestStatus reads OpenCode's status for the session on a joined
// goroutine, while the event stream is up: on (re)connect (streamUp), once
// the session is known, and when lux's turn ends while the bus last said
// busy. An error reads as not busy. Only the newest read applies, and only
// if no bus status event, connect or disconnect came after its dispatch
// (ocGen); a newer read cancels the one in flight.
func (a *ACP) requestStatus() {
	a.actMu.Lock()
	defer a.actMu.Unlock()
	a.requestStatusLocked()
}

// requestStatusLocked is requestStatus under actMu.
func (a *ACP) requestStatusLocked() {
	a.mu.Lock()
	session := a.session
	a.mu.Unlock()
	if session == "" || !a.ocUp || a.bus.givenUp() != nil {
		return
	}
	if a.statusCancel != nil {
		a.statusCancel()
	}
	ctx, cancel := context.WithCancel(a.runCtx())
	a.statusSeq++
	gen, seq := a.ocGen, a.statusSeq
	a.statusCancel = cancel
	a.statusReads++
	ok := a.spawn(func() {
		defer cancel()
		busy, err := a.bus.sessionBusy(ctx, session)
		if a.statusGot != nil {
			a.statusGot(err)
		}
		a.actMu.Lock()
		defer a.actMu.Unlock()
		a.statusReads--
		// Only a current read's outcome counts, its auth outcome too: one
		// a newer read superseded, or whose stream ended, says nothing of
		// the server now. A bus event since its dispatch (ocGen) outdates
		// its status, not whether the server accepted lux.
		current := ctx.Err() == nil && a.statusSeq == seq
		if a.statusSeq == seq {
			a.statusCancel = nil
		}
		if current {
			refused := a.bus.observer && errors.Is(err, errRefused)
			switch {
			case refused:
				a.bus.refused(err)
			case err == nil:
				a.bus.accepted()
			}
			if a.ocGen == gen {
				a.ocObserved = err == nil && busy
			}
			if refused || err == nil || a.ocGen == gen {
				a.applyOpenCodeBusyLocked(a.ocObserved)
			}
		}
		if a.statusRead != nil {
			a.statusRead(err)
		}
	})
	if !ok {
		a.statusReads--
		a.statusCancel = nil
		cancel()
	}
}

// activity reports what lux's own turns say of the Run's activity. With
// OpenCode's server it publishes the combined activity as it is now, not
// the caller's idle, which another turn may have made stale; an idle
// request that finds OpenCode busy re-reads its status, in case its idle
// event raced ahead of the ACP result.
func (a *ACP) activity(idle bool) {
	if a.bus == nil {
		a.sink.Activity(idle)
		return
	}
	a.actMu.Lock()
	a.mu.Lock()
	combinedIdle := !a.busy && !a.ocBusy && len(a.queue) == 0
	reread := idle && a.ocBusy && !a.busy
	a.mu.Unlock()
	a.showLocked(combinedIdle)
	a.actMu.Unlock()
	if reread {
		a.requestStatus()
	}
}

// showCombined reports the combined activity as it is now (OpenCode's
// server only).
func (a *ACP) showCombined() {
	a.actMu.Lock()
	defer a.actMu.Unlock()
	a.mu.Lock()
	idle := !a.busy && !a.ocBusy && len(a.queue) == 0
	a.mu.Unlock()
	a.showLocked(idle)
}

// showLocked reports idle unless it is what was last reported. Under actMu.
func (a *ACP) showLocked(idle bool) {
	if a.shown && a.shownIdle == idle {
		return
	}
	a.shown, a.shownIdle = true, idle
	a.sink.Activity(idle)
}

func (a *ACP) handleNotification(m rpcMsg) {
	if m.Method != "session/update" {
		a.sink.Event("acp."+m.Method, json.RawMessage(m.Params))
		return
	}
	var p struct {
		SessionID string          `json:"sessionId"`
		Update    json.RawMessage `json:"update"`
	}
	_ = json.Unmarshal(m.Params, &p)
	var u struct {
		Kind    string `json:"sessionUpdate"`
		Content struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	_ = json.Unmarshal(p.Update, &u)
	// The agent's reply text is also written to stdout, so `lux logs`
	// reads like a conversation without an event viewer.
	a.mu.Lock()
	loading := a.loading
	a.mu.Unlock()
	if u.Kind == "agent_message_chunk" && u.Content.Type == "text" && !loading && u.Content.Text != "" {
		a.sink.Stdout([]byte(u.Content.Text))
	}
	// Chunked text (agent_message_chunk, agent_thought_chunk, …) is
	// streamed, so a secret split across chunks is redacted whole.
	if strings.HasSuffix(u.Kind, "_chunk") && u.Content.Type == "text" &&
		streamEvent(a.sink, "acp."+u.Kind, p.SessionID+"\x00", p.Update, [][]string{{"content", "text"}}) {
		return
	}
	if u.Kind == "user_message_chunk" && u.Content.Type == "image" {
		a.sink.Event("acp."+u.Kind, userImageMeta(p.Update))
		return
	}
	a.sink.Event("acp."+u.Kind, json.RawMessage(p.Update))
}

// userImageMeta is a user_message_chunk image block (session/load replays
// the user's turns) as records keep it: its size and sha256 in place of
// its data, as for an input's images.
func userImageMeta(update json.RawMessage) map[string]any {
	var u map[string]any
	_ = json.Unmarshal(update, &u)
	if c, ok := u["content"].(map[string]any); ok {
		if d, ok := c["data"].(string); ok {
			b, _ := base64.StdEncoding.DecodeString(d)
			m := spec.MetaOf(spec.Attachment{}, b)
			c["size"], c["sha256"] = m.Size, m.SHA256
			delete(c, "data")
		}
	}
	return u
}

// handleRequest answers agent → client requests. Unattended: permission
// requests are allowed once; anything else is refused.
func (a *ACP) handleRequest(m rpcMsg) {
	if m.Method != "session/request_permission" {
		_ = a.rpc.replyError(m.ID, -32601, "method not supported by lux: "+m.Method)
		return
	}
	var p struct {
		Options []struct {
			OptionID string `json:"optionId"`
			Kind     string `json:"kind"`
		} `json:"options"`
	}
	_ = json.Unmarshal(m.Params, &p)
	choice := ""
	for _, pref := range []string{"allow_once", "allow_always"} {
		for _, o := range p.Options {
			if o.Kind == pref && choice == "" {
				choice = o.OptionID
			}
		}
	}
	if choice == "" && len(p.Options) > 0 {
		choice = p.Options[0].OptionID
	}
	_ = a.rpc.reply(m.ID, map[string]any{"outcome": map[string]string{"outcome": "selected", "optionId": choice}})
	a.sink.Event("acp.permission", map[string]any{"request": json.RawMessage(m.Params), "selected": choice})
}

func (a *ACP) Deliver(in proto.Input) {
	a.mu.Lock()
	steerHTTPPath := a.opencode && a.busy && !a.busTurn && !a.stopped && !a.closed && !in.Interrupt && in.HasContent()
	// An input bound for session/prompt is refused before anything is
	// queued or cancelled: an interrupt carrying images the agent cannot
	// take must not end the running turn. (A steer to OpenCode may still go
	// as prompt_async file parts; steerACP checks it if it falls back.)
	if len(in.Attachments) > 0 && !a.images && a.handshakeDone() && !steerHTTPPath {
		a.mu.Unlock()
		a.inputs.fail(a.sink, in, errNoImages)
		return
	}
	// Past the queue's budget: refused before an interrupt cancels anything.
	if !steerHTTPPath && in.HasContent() && a.queueBytes+inputSize(in) > maxPendingSteerBytes {
		a.mu.Unlock()
		a.inputs.fail(a.sink, in, errors.New(errPendingSteersLimit))
		return
	}
	if in.Interrupt && a.busy {
		// Put it first, then cancel the running turn; drain sends it when
		// the cancelled prompt returns. An interrupt alone sends nothing:
		// steers the turn left unread start the next one (settle).
		if in.HasContent() {
			a.queue = append([]proto.Input{in}, a.queue...)
			a.queueBytes += inputSize(in)
		}
		session := a.session
		a.cancelGen++
		a.mu.Unlock()
		err := a.rpc.notify("session/cancel", map[string]any{"sessionId": session})
		if !in.HasContent() {
			if err != nil {
				a.inputs.fail(a.sink, in, err)
			} else {
				a.inputs.accept(a.sink, in, Delivery{Lands: LandsNextTurn}, "interrupt")
			}
		}
		return
	}
	if steerHTTPPath {
		if len(a.steers) >= maxPendingSteers || a.steerBytes+inputSize(in) > maxPendingSteerBytes {
			a.mu.Unlock()
			a.inputs.fail(a.sink, in, errors.New(errPendingSteersLimit))
			return
		}
		a.steers = append(a.steers, in)
		a.steerBytes += inputSize(in)
		a.mu.Unlock()
		select {
		case a.steerKick <- struct{}{}:
		default:
		}
		return
	}
	a.queue = append(a.queue, in)
	a.queueBytes += inputSize(in)
	a.mu.Unlock()
	a.drain()
}

// handshakeDone: initialize has answered (a.images is known). Under a.mu.
func (a *ACP) handshakeDone() bool {
	select {
	case <-a.ready:
		return true
	default:
		return false
	}
}

func (a *ACP) Interrupt() error {
	a.mu.Lock()
	session, busy := a.session, a.busy
	if busy {
		a.cancelGen++
	}
	a.mu.Unlock()
	if !busy {
		return nil
	}
	return a.rpc.notify("session/cancel", map[string]any{"sessionId": session})
}

// Stop cancels the current turn, then closes stdin: an ACP agent exits
// when its client goes away.
func (a *ACP) Stop() error {
	_ = a.Interrupt()
	a.mu.Lock()
	a.stopped = true
	a.mu.Unlock()
	a.rpc.lw.close()
	return nil
}

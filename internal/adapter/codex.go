package adapter

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/marcioapm/lux/internal/proto"
)

// Codex drives `codex app-server`: JSON-RPC 2.0, newline-delimited, on
// stdio (docs/agent-protocols.md). A thread is the conversation (persisted
// as a rollout under ~/.codex); a turn is one exchange. Mid-turn input is
// native (turn/steer); interrupt is turn/interrupt; a resume on another
// host is thread/resume with the stored thread id.
type Codex struct {
	rpc    rpcConn
	mu     sync.Mutex
	sink   Sink
	thread string
	turn   string // in-progress turn id, "" when idle
	// usage is the last thread/tokenUsage/updated of the turn in progress,
	// as Codex sent it, and usageTurn the turnId it named: codex.turn_end
	// carries it. Cleared when a turn starts, so it never leaks into the next.
	usage     json.RawMessage
	usageTurn string
	queue     []proto.Input
	// early: inputs that arrived while turn/start was in flight; steered
	// into the turn once its id is known.
	early   []proto.Input
	ready   bool
	stopped bool
	// receipt: this Codex emits a steer's userMessage item when the model
	// reads it (0.155 on); older ones emit it on admission.
	receipt bool
	inputs  inputLedger
	// carried: steers an ended turn left unread, in the order they came;
	// they start the next turn, ahead of queue.
	carried []proto.Input
	// onSteer, if set, runs in a turn/steer's goroutine at "accepted"
	// (before lux checks whether its turn has ended) and "done" (a test
	// seam for interleavings).
	onSteer func(stage, requestID string)
	// rollout: thread.path, where Codex writes each compaction's summary.
	rollout string
	// rolloutBase: the compacted entries the rollout held at thread
	// start or resume, which are not this Run's.
	rolloutBase int
	// baseErr: why rolloutBase is unknown; no entry is then taken.
	baseErr error
	// reported: the contextCompaction items reported, by id.
	reported map[string]bool
	// ctx ends with Run; bg joins the summary reads waiting on the rollout.
	ctx context.Context
	bg  sync.WaitGroup
	// rolloutMu guards rolloutOff, how far the rollout has been read, and
	// compacted, the compacted entries read so far.
	rolloutMu  sync.Mutex
	rolloutOff int64
	compacted  []compactedEntry
	// compactionWait bounds waiting for a compaction's rollout entry.
	compactionWait time.Duration
}

func NewCodex() *Codex { return &Codex{compactionWait: 10 * time.Second} }

// CredentialFiles: Codex reads its key from ~/.codex/auth.json (what
// `codex login --with-api-key` writes), not from OPENAI_API_KEY in the
// environment. An OPENAI_API_KEY secret becomes that file.
func (c *Codex) CredentialFiles(_ proto.ShimConfig, secrets map[string]string, home string) map[string][]byte {
	key, ok := secrets["OPENAI_API_KEY"]
	if !ok {
		return nil
	}
	b, _ := json.Marshal(map[string]string{"auth_mode": "apikey", "OPENAI_API_KEY": key})
	return map[string][]byte{filepath.Join(home, ".codex", "auth.json"): b}
}

// codexMCPEnv names the variable holding MCP server n's header m. Codex
// reads header values from variables (env_http_headers), so none is in
// argv, which the lux.workload event records.
func codexMCPEnv(n, m int) string { return fmt.Sprintf("LUX_MCP_%d_%d", n, m) }

// Environment: the MCP servers' header values.
func (c *Codex) Environment(cfg proto.ShimConfig) map[string]string {
	env := map[string]string{}
	for n, s := range cfg.MCP {
		for m, h := range s.Headers {
			env[codexMCPEnv(n, m)] = h.Value
		}
	}
	return env
}

// Command runs the spec's command (default "codex") with app-server, and
// the MCP servers as config overrides (TOML values).
func (c *Codex) Command(cfg proto.ShimConfig) ([]string, error) {
	if cfg.Resume && len(cfg.ResumeCommand) > 0 {
		return cfg.ResumeCommand, nil
	}
	argv, err := command(cfg)
	if err != nil {
		return nil, err
	}
	argv = append(slices.Clone(argv), "app-server")
	for n, s := range cfg.MCPServers {
		key := "mcp_servers." + s.Name // spec names are TOML bare keys
		argv = append(argv, "-c", key+".url="+tomlString(cfg.MCPURL(s)))
		if len(s.Headers) > 0 {
			var kv []string
			for m, h := range s.Headers {
				kv = append(kv, tomlString(h.Name)+"="+tomlString(codexMCPEnv(n, m)))
			}
			argv = append(argv, "-c", key+".env_http_headers={"+strings.Join(kv, ",")+"}")
		}
	}
	return argv, nil
}

// tomlString is s as a TOML basic string: a JSON string is one, for
// strings without DEL (url.Parse refuses control characters; header names
// are tokens).
func tomlString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func (c *Codex) call(method string, params any) (json.RawMessage, error) {
	return c.rpc.call(method, params)
}

func (c *Codex) Run(ctx context.Context, p *Process, cfg proto.ShimConfig, sink Sink) error {
	ctx, cancel := context.WithCancel(ctx)
	c.mu.Lock()
	c.sink, c.ctx = sink, ctx
	c.mu.Unlock()
	c.rpc.attach(p.Stdin)
	go pump(p.Stderr, sink.Stderr)
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.rpc.readLoop(p.Stdout, c.handleRequest, func(m rpcMsg) { c.handleNotification(m, sink) }, sink.Stdout)
	}()

	err := c.handshake(cfg, sink)
	cfg.PromptAttachments = nil // the queued prompt holds their paths
	if err != nil {
		sink.Event(proto.EvWarning, map[string]any{"message": "codex: " + err.Error()})
		_ = p.Signal(syscall.SIGTERM)
	} else {
		c.mu.Lock()
		c.ready = true
		c.mu.Unlock()
		c.drain()
	}
	<-done
	cancel()
	c.bg.Wait()
	c.mu.Lock()
	why := unreadWhy(c.stopped)
	c.mu.Unlock()
	c.inputs.close(sink, why)
	return nil
}

func (c *Codex) handshake(cfg proto.ShimConfig, sink Sink) error {
	init, err := c.call("initialize", map[string]any{"clientInfo": map[string]string{"name": "lux", "version": "1"}})
	if err != nil {
		return err
	}
	var ir struct {
		UserAgent string `json:"userAgent"`
	}
	_ = json.Unmarshal(init, &ir)
	c.mu.Lock()
	c.receipt = codexReceipt(ir.UserAgent)
	c.mu.Unlock()
	cwd := workdir(cfg)
	var res json.RawMessage
	resumed := false
	if cfg.Resume && cfg.SessionID != "" {
		res, err = c.call("thread/resume", map[string]any{"threadId": cfg.SessionID})
		if err != nil {
			sink.Event(proto.EvWarning, map[string]any{"message": "thread/resume failed, starting a new thread: " + err.Error()})
		}
		resumed = res != nil
	}
	if res == nil {
		res, err = c.call("thread/start", map[string]any{"cwd": cwd})
		if err != nil {
			return err
		}
		if !cfg.Resume && (cfg.Prompt != "" || len(cfg.PromptAttachments) > 0) {
			c.mu.Lock()
			c.queue = append([]proto.Input{withoutWrittenData(proto.Input{RequestID: "prompt", Text: cfg.Prompt, Attachments: cfg.PromptAttachments})}, c.queue...)
			c.mu.Unlock()
		}
	}
	var r struct {
		Thread struct {
			ID   string `json:"id"`
			Path string `json:"path"`
		} `json:"thread"`
	}
	if err := json.Unmarshal(res, &r); err != nil || r.Thread.ID == "" {
		return errors.New("no thread id")
	}
	c.mu.Lock()
	c.thread, c.rollout = r.Thread.ID, r.Thread.Path
	c.mu.Unlock()
	if r.Thread.Path != "" {
		// A resumed thread's earlier compactions are not this Run's. A new
		// thread's rollout may not exist yet: it holds none. Any other
		// failure leaves the base unknown, so no entry is guessed at.
		prior, err := c.compactedEntries()
		if errors.Is(err, os.ErrNotExist) && !resumed {
			err = nil
		}
		if err != nil {
			sink.Event(proto.EvWarning, map[string]any{"message": "codex: the thread's rollout could not be read; compactions are reported without a summary: " + err.Error()})
		}
		c.mu.Lock()
		c.rolloutBase, c.baseErr = len(prior), err
		c.mu.Unlock()
	}
	sink.Session(r.Thread.ID)
	sink.Activity(true)
	return nil
}

// codexReceipt reports whether this Codex, named in initialize's
// userAgent ("<client>/<version> …"), emits a steer's userMessage item
// when the model reads it: from 0.155 on. Codex 0.144 emits it when the
// steer is admitted, still mid-tool, so there it is no receipt.
func codexReceipt(userAgent string) bool {
	first, _, _ := strings.Cut(userAgent, " ")
	_, ver, ok := strings.Cut(first, "/")
	if !ok {
		return false
	}
	var major, minor int
	if _, err := fmt.Sscanf(ver, "%d.%d", &major, &minor); err != nil {
		return false
	}
	return major > 0 || minor >= 155
}

// delivery is how an input Codex took reaches its model.
func (c *Codex) delivery() Delivery {
	c.mu.Lock()
	defer c.mu.Unlock()
	return Delivery{Lands: LandsNextStep, Receipt: c.receipt}
}

// userInput is turn/start's and turn/steer's params for an input: its
// images (files the shim wrote), then its text; its request id goes as
// clientUserMessageId, which Codex echoes as the userMessage item's
// clientId.
func userInput(params map[string]any, in proto.Input) map[string]any {
	params["input"] = inputContent(dialectCodex, in)
	if in.RequestID != "" {
		params["clientUserMessageId"] = in.RequestID
	}
	return params
}

// withoutWrittenData drops the bytes of the images the shim wrote to
// files: Codex is given their paths, so a held or carried input keeps
// only those.
func withoutWrittenData(in proto.Input) proto.Input {
	if len(in.Attachments) == 0 {
		return in
	}
	atts := slices.Clone(in.Attachments)
	for i := range atts {
		if atts[i].Path != "" {
			atts[i].Data = ""
		}
	}
	in.Attachments = atts
	return in
}

func (c *Codex) drain() {
	c.mu.Lock()
	if !c.ready || c.stopped || c.turn != "" || len(c.carried)+len(c.queue) == 0 {
		c.mu.Unlock()
		return
	}
	c.queue = append(c.carried, c.queue...)
	c.carried = nil
	in := c.queue[0]
	c.queue = c.queue[1:]
	// Input queued behind it (steers carried over from an interrupted
	// turn) is steered into the turn it starts, not held for the next.
	for len(c.queue) > 0 && !c.queue[0].Interrupt {
		c.early = append(c.early, c.queue[0])
		c.queue = c.queue[1:]
	}
	c.turn = "starting"
	c.usage, c.usageTurn = nil, ""
	thread := c.thread
	sink := c.sink
	c.mu.Unlock()
	sink.Activity(false)
	c.inputs.track(in)
	go func() {
		res, err := c.call("turn/start", userInput(map[string]any{"threadId": thread}, in))
		var r struct {
			Turn struct {
				ID string `json:"id"`
			} `json:"turn"`
		}
		_ = json.Unmarshal(res, &r)
		c.mu.Lock()
		if err != nil {
			c.turn = ""
			// Held for this turn: they start the next one instead.
			c.queue = append(c.queue, c.early...)
			c.early = nil
		} else if c.turn == "starting" {
			c.turn = r.Turn.ID
		}
		c.mu.Unlock()
		if err != nil {
			c.inputs.fail(sink, in, err)
			sink.Activity(true)
			c.drain()
			return
		}
		c.inputs.accept(sink, in, c.delivery(), r.Turn.ID)
		c.flushEarly()
	}()
}

// flushEarly delivers the inputs held while the turn was starting, now
// that its id is known: steered into it, or queued if it already ended.
func (c *Codex) flushEarly() {
	c.mu.Lock()
	if c.turn == "starting" {
		c.mu.Unlock()
		return
	}
	early := c.early
	c.early = nil
	c.mu.Unlock()
	for _, in := range early {
		c.Deliver(in)
	}
}

// handleRequest answers server requests (approvals): unattended, so
// approve; the sandbox is the container.
func (c *Codex) handleRequest(m rpcMsg) {
	_ = c.rpc.reply(m.ID, map[string]string{"decision": "approved"})
	c.sink.Event("codex.request."+m.Method, json.RawMessage(m.Params))
}

func (c *Codex) handleNotification(m rpcMsg, sink Sink) {
	// compaction: a contextCompaction item that completed, reported once
	// its codex.item/completed is relayed.
	var compaction *string
	defer func() {
		if compaction != nil {
			c.compactionCompleted(*compaction)
		}
	}()
	switch m.Method {
	case "turn/started":
		var p struct {
			Turn struct {
				ID string `json:"id"`
			} `json:"turn"`
		}
		_ = json.Unmarshal(m.Params, &p)
		c.mu.Lock()
		// A turn we did not know of (not the one turn/start just named):
		// usage so far belongs to an earlier one.
		if p.Turn.ID != c.turn {
			c.usage, c.usageTurn = nil, ""
		}
		c.turn = p.Turn.ID
		c.mu.Unlock()
	case "thread/tokenUsage/updated":
		var p struct {
			TurnID     string          `json:"turnId"`
			TokenUsage json.RawMessage `json:"tokenUsage"`
		}
		_ = json.Unmarshal(m.Params, &p)
		c.mu.Lock()
		// Only the turn in progress's; while it is "starting" its id is
		// not known yet, so take it.
		if c.turn != "" && (p.TurnID == "" || c.turn == "starting" || p.TurnID == c.turn) {
			c.usage, c.usageTurn = p.TokenUsage, p.TurnID
		}
		c.mu.Unlock()
	case "turn/completed":
		var p struct {
			Turn struct {
				ID     string `json:"id"`
				Status string `json:"status"`
			} `json:"turn"`
		}
		_ = json.Unmarshal(m.Params, &p)
		c.mu.Lock()
		c.turn = ""
		usage := c.usage
		if c.usageTurn != "" && p.Turn.ID != "" && c.usageTurn != p.Turn.ID {
			usage = nil
		}
		c.usage, c.usageTurn = nil, ""
		idle := len(c.queue)+len(c.carried) == 0
		c.mu.Unlock()
		// The turn's end, with the agent's usage as it reported it (the
		// counterpart of acp.turn_end and claude.turn_end).
		// Sent before idle and before the next turn starts.
		sink.Event("codex.turn_end", withUsage(map[string]any{"status": p.Turn.Status}, usage))
		if c.carryUnread(p.Turn.ID) {
			idle = false
		}
		if idle {
			sink.Activity(true)
		}
		defer c.drain() // after the turn/completed event below
	case "item/started":
		// A userMessage item carrying a clientId is an input of ours
		// entering the model's context (from Codex 0.155 on; see
		// codexReceipt).
		var p struct {
			Item struct {
				Type     string  `json:"type"`
				ClientID *string `json:"clientId"`
			} `json:"item"`
		}
		_ = json.Unmarshal(m.Params, &p)
		if p.Item.Type == "userMessage" && p.Item.ClientID != nil {
			c.inputs.consume(sink, *p.Item.ClientID)
		}
	case "item/completed":
		var p struct {
			Item struct {
				ID   string `json:"id"`
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"item"`
		}
		_ = json.Unmarshal(m.Params, &p)
		if p.Item.Type == "agentMessage" && p.Item.Text != "" {
			sink.Stdout([]byte(p.Item.Text))
			sink.EndMessage()
		}
		if p.Item.Type == "contextCompaction" {
			compaction = &p.Item.ID
		}
	}
	// Streamed text (item/agentMessage/delta, item/reasoning/textDelta,
	// command output, …), so a secret split across deltas is redacted whole.
	if strings.HasSuffix(strings.ToLower(m.Method), "delta") &&
		streamEvent(sink, "codex."+m.Method, "", m.Params, [][]string{{"delta"}}) {
		return
	}
	sink.Event("codex."+m.Method, json.RawMessage(m.Params))
}

// compactionCompleted reports a compaction once, as its item completes,
// with the summary read off the rollout (the app-server's item carries
// none). Codex writes one compacted entry per compaction, in order: the
// nth item reported in this Run is the nth entry after rolloutBase.
//
// Called on the reader goroutine right after the item/completed relay:
// when the rollout already holds the entry (Codex writes it in the same
// millisecond), the record directly follows that relay. Otherwise the
// entry is waited for off the reader goroutine, and the record comes later.
func (c *Codex) compactionCompleted(id string) {
	c.mu.Lock()
	if c.reported == nil {
		c.reported = map[string]bool{}
	}
	if c.reported[id] {
		c.mu.Unlock()
		return
	}
	c.reported[id] = true
	n := c.rolloutBase + len(c.reported) - 1
	thread, sink, ctx, baseErr := c.thread, c.sink, c.ctx, c.baseErr
	c.mu.Unlock()
	report := func(summary string, err error) {
		reportCompacted(sink, "codex: thread", proto.Compaction{SessionID: thread, Summary: summary}, err)
	}
	if baseErr != nil {
		report("", fmt.Errorf("the rollout's earlier entries are unknown: %w", baseErr))
		return
	}
	if summary, done, err := c.rolloutEntry(n); done {
		report(summary, err)
		return
	}
	c.bg.Add(1)
	go func() {
		defer c.bg.Done()
		report(c.rolloutSummary(ctx, n))
	}()
}

// errRemoteCompaction: Codex wrote the compaction with an empty message.
// For OpenAI's own provider it compacts remotely: the model gets an
// encrypted compaction item and no text is exposed.
var errRemoteCompaction = errors.New("Codex compacted remotely and exposes no summary text")

// rolloutEntry is the message of the rollout's nth compacted entry as the
// rollout holds it now; done is false while the entry is not written yet.
//
//	{"timestamp", "type":"compacted", "payload":{"message", "replacement_history", …}}
func (c *Codex) rolloutEntry(n int) (summary string, done bool, err error) {
	if c.rolloutFile() == "" {
		return "", true, errors.New("Codex named no rollout file for the thread")
	}
	entries, err := c.compactedEntries()
	switch {
	case err != nil:
		return "", true, err
	case len(entries) <= n:
		return "", false, nil
	case entries[n].err != nil:
		return "", true, entries[n].err
	case entries[n].message == "":
		return "", true, errRemoteCompaction
	}
	return entries[n].message, true, nil
}

// rolloutSummary waits up to compactionWait for the rollout's nth
// compacted entry, reading it once more when the wait or the Run ends.
func (c *Codex) rolloutSummary(ctx context.Context, n int) (string, error) {
	wait, cancel := context.WithTimeout(ctx, c.compactionWait)
	defer cancel()
	for {
		if summary, done, err := c.rolloutEntry(n); done {
			return summary, err
		}
		if wait.Err() != nil {
			if ctx.Err() != nil {
				return "", fmt.Errorf("the Run ended before Codex wrote its compacted entry to %s", c.rolloutFile())
			}
			return "", fmt.Errorf("no compacted entry in %s after %s", c.rolloutFile(), c.compactionWait)
		}
		select {
		case <-wait.Done():
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func (c *Codex) rolloutFile() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.rollout
}

// compactedEntry is a compacted entry of the rollout: its message, or why
// it could not be read.
type compactedEntry struct {
	message string
	err     error
}

// rolloutLineMax: a rollout line longer than this is skipped, not held.
const rolloutLineMax = 16 << 20

// compactedEntries reads the rollout's whole lines not read yet, and
// returns every compacted entry in it so far. The rollout is under the
// agent's home, which the agent can write: it is opened without following
// a symlink or blocking on a FIFO, and read only if it is a regular file.
func (c *Codex) compactedEntries() ([]compactedEntry, error) {
	c.rolloutMu.Lock()
	defer c.rolloutMu.Unlock()
	path := c.rolloutFile()
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil {
		return nil, err
	} else if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("the rollout %s is not a regular file (%s)", path, fi.Mode().Type())
	}
	if _, err := f.Seek(c.rolloutOff, io.SeekStart); err != nil {
		return nil, err
	}
	r := bufio.NewReaderSize(f, 64<<10)
	for {
		line, n, long, err := readLine(r, rolloutLineMax)
		if err != nil {
			// A line without its newline is still being written.
			return slices.Clone(c.compacted), nil
		}
		c.rolloutOff += n
		if long {
			// Its type is near the start of the line ({"timestamp","type",…}):
			// a compacted entry still counts, so later ones keep their place.
			if bytes.Contains(line[:min(len(line), 256)], []byte(`"type":"compacted"`)) {
				c.compacted = append(c.compacted, compactedEntry{err: fmt.Errorf(
					"its rollout entry is longer than %d bytes", rolloutLineMax)})
			}
			continue
		}
		if !bytes.Contains(line, []byte(`"type":"compacted"`)) {
			continue
		}
		var e struct {
			Type    string `json:"type"`
			Payload struct {
				Message string `json:"message"`
			} `json:"payload"`
		}
		if json.Unmarshal(line, &e) == nil && e.Type == "compacted" {
			c.compacted = append(c.compacted, compactedEntry{message: e.Payload.Message})
		}
	}
}

// readLine reads a line through its '\n'. n is its whole length; a line
// longer than max bytes is read and dropped past its first max bytes, and
// long is true. An error (io.EOF) means no whole line is left.
func readLine(r *bufio.Reader, max int) (line []byte, n int64, long bool, err error) {
	for {
		chunk, err := r.ReadSlice('\n')
		n += int64(len(chunk))
		if room := max - len(line); room > 0 {
			line = append(line, chunk[:min(room, len(chunk))]...)
		}
		switch {
		case err == nil:
			return line, n, n > int64(max), nil
		case !errors.Is(err, bufio.ErrBufferFull):
			return nil, n, false, err
		}
	}
}

// Deliver steers a running turn natively (turn/steer), or starts a turn.
// Input that arrives while turn/start is in flight waits for its turn id,
// then is steered into that turn.
func (c *Codex) Deliver(in proto.Input) {
	in = withoutWrittenData(in)
	c.mu.Lock()
	turn, thread, ready := c.turn, c.thread, c.ready
	if ready && turn == "starting" {
		c.early = append(c.early, in)
		c.mu.Unlock()
		return
	}
	if ready && turn != "" && in.Interrupt && in.HasContent() {
		// Queued under the lock that saw the turn running, so its
		// turn/completed starts it whether that comes before or after
		// the result of turn/interrupt.
		c.queue = append([]proto.Input{in}, c.queue...)
	}
	c.mu.Unlock()
	if !ready || turn == "" {
		c.mu.Lock()
		c.queue = append(c.queue, in)
		c.mu.Unlock()
		c.drain()
		return
	}
	if in.Interrupt {
		_, err := c.call("turn/interrupt", map[string]any{"threadId": thread, "turnId": turn})
		if !in.HasContent() {
			// An interrupt alone: steers the turn left unread start the
			// next one (carryUnread).
			if err != nil {
				c.inputs.fail(c.sink, in, err)
			} else {
				// Nothing for the model to read: no receipt.
				c.inputs.accept(c.sink, in, Delivery{Lands: LandsNextStep}, "")
			}
			return
		}
		return
	}
	// Tracked on arrival: carried inputs keep the order they came in.
	c.inputs.track(in)
	go func() {
		res, err := c.call("turn/steer", userInput(map[string]any{"threadId": thread, "expectedTurnId": turn}, in))
		if err != nil {
			if !codexTurnGone(err) {
				c.inputs.fail(c.sink, in, err)
				return
			}
			// The turn ended meanwhile: send it as the next turn.
			c.mu.Lock()
			c.queue = append(c.queue, in)
			c.mu.Unlock()
			c.drain()
			return
		}
		var r struct {
			TurnID string `json:"turnId"`
		}
		if json.Unmarshal(res, &r) != nil || r.TurnID == "" {
			r.TurnID = turn
		}
		c.inputs.accept(c.sink, in, c.delivery(), r.TurnID)
		if c.onSteer != nil {
			c.onSteer("accepted", in.RequestID)
			defer c.onSteer("done", in.RequestID)
		}
		// A turn that ended between the steer's result and here leaves it
		// unread; turn/completed may already have run its check.
		c.mu.Lock()
		gone := c.turn != r.TurnID
		c.mu.Unlock()
		if gone {
			if c.carryUnread(r.TurnID) {
				c.drain()
			}
		}
	}()
}

// codexTurnGone reports whether a turn/steer error means the turn it
// named is no longer the active one ("no active turn to steer", "expected
// active turn id `X` but found `Y`"): the input then starts a turn of its
// own. Any other refusal is the input's failure.
func codexTurnGone(err error) bool {
	s := err.Error()
	return strings.Contains(s, "no active turn") || strings.Contains(s, "expected active turn id")
}

// carryUnread handles the inputs steered into a turn that ended without
// the model reading them (Codex drops a turn's pending steers when it is
// interrupted): they start the next turn, in the order they came, under
// the same request ids, unless the Run is stopping, where they fail. Each
// is claimed from the ended turn atomically, so the turn/completed handler
// and a late turn/steer result cannot both carry it. It reports whether
// any were carried.
func (c *Codex) carryUnread(turn string) bool {
	if turn == "" {
		return false
	}
	unread := c.inputs.claim(turn, "carried")
	if len(unread) == 0 {
		return false
	}
	c.mu.Lock()
	stopped := c.stopped
	var now []proto.Input
	if !stopped {
		c.carried = append(c.carried, unread...)
		slices.SortStableFunc(c.carried, func(a, b proto.Input) int { return c.inputs.order(a.RequestID) - c.inputs.order(b.RequestID) })
		if c.turn != "" {
			// The next turn has started already: steered into it.
			now, c.carried = c.carried, nil
		}
	}
	c.mu.Unlock()
	if stopped {
		for _, in := range unread {
			c.inputs.fail(c.sink, in, errStoppedUnread)
		}
		return false
	}
	for _, in := range now {
		c.Deliver(in)
	}
	return true
}

func (c *Codex) Interrupt() error {
	c.mu.Lock()
	turn, thread := c.turn, c.thread
	c.mu.Unlock()
	if turn == "" || turn == "starting" {
		return nil
	}
	_, err := c.call("turn/interrupt", map[string]any{"threadId": thread, "turnId": turn})
	return err
}

// Stop interrupts the turn (so the rollout is flushed) and closes stdin.
func (c *Codex) Stop() error {
	c.mu.Lock()
	c.stopped = true
	c.mu.Unlock()
	_ = c.Interrupt()
	c.rpc.lw.close()
	return nil
}

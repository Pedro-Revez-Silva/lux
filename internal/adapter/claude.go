package adapter

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"sync"
	"syscall"

	"github.com/marcioapm/lux/internal/ids"
	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/spec"
)

// Claude drives Claude Code in stream-json mode (docs/agent-protocols.md):
//
//	claude -p --input-format stream-json --output-format stream-json --verbose
//
// A user message is one JSON line on stdin, with a uuid lux derives from
// its request id. Claude Code takes a line sent mid-turn itself: it reads
// it at the next tool boundary of the running turn, or, when the model's
// step ends the turn without a tool call, runs it as the next turn. With
// msg_lifecycle_v1 it reports each line by uuid in command_lifecycle
// frames: queued (accepted), started (read by the model: consumed), then
// completed, or cancelled/discarded/refused (failed). Interrupt is a
// control_request on stdin. Stop is SIGINT, which ends the turn cleanly;
// the transcript under ~/.claude is what a resume (--resume <session-id>)
// continues from.
type Claude struct {
	lw        lineWriter
	mu        sync.Mutex
	proc      *Process
	sink      Sink
	queue     []proto.Input
	sessionID string
	inputs    inputLedger
	// sent: lines written, by uuid, not yet ended (completed, cancelled,
	// discarded or refused); sentSize: their payload (inputSize), bounded
	// by maxPendingSteerBytes. running: a turn has started and its result
	// has not come. The Run is idle when neither holds anything.
	sent     map[string]proto.Input
	sentSize int
	running  bool
	// lifecycle: Claude Code reports command_lifecycle (msg_lifecycle_v1);
	// known from the first frame or from system/init. Without it the Run
	// is busy from each line to a result (inTurn counts them), which a
	// line folded into the running turn leaves unbalanced.
	lifecycle, known bool
	inTurn           int
	// stopping: Stop was called; lines cancelled from here on fail.
	stopping bool
	// resent: how often each request id's line was written again.
	resent map[string]int
}

func NewClaude() *Claude { return &Claude{} }

func (c *Claude) Command(cfg proto.ShimConfig) ([]string, error) {
	if cfg.Resume && len(cfg.ResumeCommand) > 0 {
		return cfg.ResumeCommand, nil
	}
	base, err := command(cfg)
	if err != nil {
		return nil, err
	}
	argv := append(slices.Clone(base), "-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose")
	if cfg.Resume && cfg.SessionID != "" {
		argv = append(argv, "--resume", cfg.SessionID)
	}
	// Added to the user's own MCP config (no --strict-mcp-config).
	if len(cfg.MCPServers) > 0 {
		argv = append(argv, "--mcp-config", claudeMCPConfig)
	}
	return argv, nil
}

// claudeMCPConfig is the --mcp-config file: on the secrets tmpfs, as it
// holds header values.
var claudeMCPConfig = filepath.Join(proto.ShimSecretsDir, spec.ReservedSecretPrefix+"claude-mcp.json")

// CredentialFiles: the MCP servers, as Claude Code's --mcp-config reads them.
func (c *Claude) CredentialFiles(cfg proto.ShimConfig, _ map[string]string, _ string) map[string][]byte {
	if len(cfg.MCP) == 0 {
		return nil
	}
	servers := map[string]any{}
	for _, s := range cfg.MCP {
		headers := map[string]string{}
		for _, h := range s.Headers {
			headers[h.Name] = h.Value
		}
		servers[s.Name] = map[string]any{"type": "http", "url": s.URL, "headers": headers}
	}
	b, _ := json.Marshal(map[string]any{"mcpServers": servers})
	return map[string][]byte{claudeMCPConfig: b}
}

func (c *Claude) Run(ctx context.Context, p *Process, cfg proto.ShimConfig, sink Sink) error {
	c.lw.set(p.Stdin)
	c.mu.Lock()
	c.proc, c.sink = p, sink
	queued := c.queue
	c.queue = nil
	c.mu.Unlock()

	go pump(p.Stderr, sink.Stderr)
	if !cfg.Resume && (cfg.Prompt != "" || len(cfg.PromptAttachments) > 0) {
		c.send(proto.Input{RequestID: "prompt", Text: cfg.Prompt, Attachments: cfg.PromptAttachments})
	}
	cfg.PromptAttachments = nil // c.sent holds them while they are needed
	for _, in := range queued {
		c.Deliver(in)
	}
	if cfg.Resume && cfg.Prompt == "" && len(queued) == 0 {
		sink.Activity(true)
	}

	sc := bufio.NewScanner(p.Stdout)
	sc.Buffer(make([]byte, 64<<10), 64<<20)
	// compaction: a compact_boundary whose summary line has not come yet.
	var compaction *claudeCompaction
	for sc.Scan() {
		line := sc.Bytes()
		var m struct {
			Type      string `json:"type"`
			Subtype   string `json:"subtype"`
			SessionID string `json:"session_id"`
			UUID      string `json:"uuid"`
			// Content: a list of blocks, or a string (a synthetic message,
			// such as a compaction's summary).
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
			IsSynthetic     bool            `json:"isSynthetic"`
			CompactMetadata json.RawMessage `json:"compact_metadata"`
			Result          string          `json:"result"`
			Usage           json.RawMessage `json:"usage"`
			Capabilities    []string        `json:"capabilities"`
			CommandUUID     string          `json:"command_uuid"`
			State           string          `json:"state"`
		}
		if err := json.Unmarshal(line, &m); err != nil || m.Type == "" {
			sink.Stdout(append(append([]byte{}, line...), '\n'))
			continue
		}
		if m.SessionID != "" && m.SessionID != c.sessionID {
			c.sessionID = m.SessionID
			sink.Session(m.SessionID)
		}
		if compaction != nil && compaction.reportWith(sink, m.Type, m.UUID, m.IsSynthetic, m.Message.Content) {
			compaction = nil
		}
		switch m.Type {
		case "assistant":
			var blocks []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			_ = json.Unmarshal(m.Message.Content, &blocks)
			for _, b := range blocks {
				if b.Type == "text" && b.Text != "" {
					sink.Stdout([]byte(b.Text))
					sink.EndMessage()
				}
			}
		case "system":
			switch m.Subtype {
			case "init":
				c.init(slices.Contains(m.Capabilities, "msg_lifecycle_v1"))
			case "compact_boundary":
				// Reported once the line after it, the summary, is read.
				compaction = newClaudeCompaction(m.SessionID, m.CompactMetadata)
			}
		case "command_lifecycle":
			c.lifecycleFrame(m.CommandUUID, m.State)
		case "result":
			c.mu.Lock()
			c.running = false
			if !c.lifecycle && c.inTurn > 0 {
				c.inTurn--
			}
			c.mu.Unlock()
			// The turn's end, with the agent's usage as it reported it (the
			// whole result line is claude.result, below).
			sink.Event("claude.turn_end", withUsage(map[string]any{}, m.Usage))
			c.maybeIdle()
		}
		// With --include-partial-messages, text arrives as deltas: streamed,
		// so a secret split across them is redacted whole.
		// Each line has its own uuid: not part of what makes chunks one.
		if m.Type == "stream_event" && streamEvent(sink, "claude.stream_event", "", line,
			[][]string{{"event", "delta", "text"}, {"event", "delta", "thinking"}, {"event", "delta", "partial_json"}}, "uuid") {
			continue
		}
		sink.Event("claude."+m.Type, json.RawMessage(append([]byte{}, line...)))
	}
	if compaction != nil {
		compaction.report(sink, "")
	}
	c.exited()
	return nil
}

// claudeCompaction is a compact_boundary waiting for its summary: Claude
// Code (2.1.207) writes, right after the boundary, the summary it gave its
// model as a synthetic user line with string content, whose uuid is the
// boundary's compact_metadata.preserved_segment.anchor_uuid. The same text
// is the transcript's isCompactSummary message.
type claudeCompaction struct {
	c      proto.Compaction
	anchor string
}

func newClaudeCompaction(session string, meta json.RawMessage) *claudeCompaction {
	var md struct {
		Trigger          string `json:"trigger"`
		PreTokens        *int64 `json:"pre_tokens"`
		PostTokens       *int64 `json:"post_tokens"`
		PreservedSegment struct {
			AnchorUUID string `json:"anchor_uuid"`
		} `json:"preserved_segment"`
	}
	_ = json.Unmarshal(meta, &md)
	return &claudeCompaction{anchor: md.PreservedSegment.AnchorUUID,
		c: proto.Compaction{SessionID: session, Trigger: md.Trigger, PreTokens: md.PreTokens, PostTokens: md.PostTokens}}
}

// reportWith takes the line after the boundary: it reports the compaction,
// with the line's text if it is the summary, and returns true; a line of
// another kind before it (system, stream_event) waits for the next one.
func (cc *claudeCompaction) reportWith(sink Sink, typ, uuid string, synthetic bool, content json.RawMessage) bool {
	var text string
	isText := json.Unmarshal(content, &text) == nil
	switch {
	case typ == "user" && isText && (uuid == cc.anchor || cc.anchor == "" && synthetic):
		cc.report(sink, text)
	case typ == "user" || typ == "assistant" || typ == "result":
		cc.report(sink, "")
	default:
		return false
	}
	return true
}

// report writes the record before its warning, and before the line that
// ended the wait is relayed: the record is the next thing a client sees
// after the boundary, bar lines of other types Claude wrote in between.
func (cc *claudeCompaction) report(sink Sink, summary string) {
	cc.c.Summary = summary
	sink.Compacted(cc.c)
	if summary == "" {
		sink.Event(proto.EvWarning, map[string]any{"message": "claude: compacted, but no summary line followed its compact_boundary"})
	}
}

// exited fails every line written that Claude Code never started, once
// its stdout has ended (the process exited or its output broke off):
// nothing reads them now. SIGINT (Stop) ends with an aborted result and
// exit, without a lifecycle frame for queued lines. A line already read is
// left alone.
func (c *Claude) exited() {
	c.mu.Lock()
	why := unreadWhy(c.stopping)
	sent := c.sent
	c.sent = nil
	c.sentSize = 0
	sink := c.sink
	c.mu.Unlock()
	for _, in := range sent {
		if c.inputs.pending(in.RequestID) {
			c.inputs.fail(sink, in, why)
		}
	}
	c.inputs.close(sink, why)
	c.mu.Lock()
	c.resent = nil
	c.mu.Unlock()
}

// claudeUUID is the uuid a request id's line carries: derived from it
// (SHA-1, RFC 4122 version 5 layout), so the same input always has the
// same uuid.
func claudeUUID(requestID string) string {
	h := sha1.Sum([]byte("lux-input:" + requestID))
	h[6] = h[6]&0x0f | 0x50
	h[8] = h[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", h[0:4], h[4:6], h[6:8], h[8:10], h[10:16])
}

func (c *Claude) send(in proto.Input) {
	if in.RequestID == "" {
		in.RequestID = ids.New("in")
	}
	uuid := claudeUUID(in.RequestID)
	c.mu.Lock()
	keep := !c.known || c.lifecycle
	if keep && c.sentSize+inputSize(in) > maxPendingSteerBytes {
		sink := c.sink
		c.mu.Unlock()
		c.inputs.fail(sink, in, errors.New(errPendingSteersLimit))
		return
	}
	c.mu.Unlock()
	c.inputs.track(in)
	c.mu.Lock()
	// Without lifecycle frames nothing ever ends a line: keep none.
	if keep {
		c.putSent(uuid, in)
	}
	c.mu.Unlock()
	err := c.writeLine(uuid, in)
	c.mu.Lock()
	if err != nil {
		c.dropSent(uuid)
	} else if !c.lifecycle {
		c.inTurn++
	}
	known, lifecycle := c.known, c.lifecycle
	sink := c.sink
	c.mu.Unlock()
	if err != nil {
		c.inputs.fail(sink, in, err)
		return
	}
	sink.Activity(false)
	if known && !lifecycle {
		c.inputs.accept(sink, in, Delivery{Lands: LandsNextStep}, "")
	}
}

// putSent and dropSent change c.sent and keep sentSize with it. Under c.mu.
func (c *Claude) putSent(uuid string, in proto.Input) {
	c.dropSent(uuid)
	if c.sent == nil {
		c.sent = map[string]proto.Input{}
	}
	c.sent[uuid] = in
	c.sentSize += inputSize(in)
}

func (c *Claude) dropSent(uuid string) {
	if old, ok := c.sent[uuid]; ok {
		c.sentSize -= inputSize(old)
		delete(c.sent, uuid)
	}
}

// init learns from system/init whether Claude Code reports
// command_lifecycle; without it, lines written are accepted as written.
func (c *Claude) init(lifecycle bool) {
	c.mu.Lock()
	if c.known {
		c.mu.Unlock()
		return
	}
	c.known, c.lifecycle = true, lifecycle
	var waiting []proto.Input
	if !lifecycle {
		for _, in := range c.sent {
			waiting = append(waiting, in)
		}
		c.sent = nil
		c.sentSize = 0
		c.inTurn = len(waiting)
	}
	c.mu.Unlock()
	for _, in := range waiting {
		c.inputs.accept(c.sink, in, Delivery{Lands: LandsNextStep}, "")
	}
}

// lifecycleFrame applies a command_lifecycle frame for one of lux's lines.
func (c *Claude) lifecycleFrame(uuid, state string) {
	c.mu.Lock()
	c.known, c.lifecycle = true, true
	in, ok := c.sent[uuid]
	switch state {
	case "started":
		c.running = true
		// Read: never written again, so its payload goes.
		if ok {
			c.putSent(uuid, proto.Input{RequestID: in.RequestID})
		}
	case "completed", "cancelled", "discarded", "refused":
		c.dropSent(uuid)
	}
	c.mu.Unlock()
	if !ok {
		return
	}
	switch state {
	case "queued":
		c.inputs.accept(c.sink, in, Delivery{Lands: LandsNextStep, Receipt: true}, "")
	case "started":
		// A line can start without a queued frame first.
		c.inputs.accept(c.sink, in, Delivery{Lands: LandsNextStep, Receipt: true}, "")
		c.inputs.consume(c.sink, in.RequestID)
	case "cancelled", "discarded":
		// Claude Code cancels the lines still queued when a turn is
		// interrupted (interrupt_cancel_queued_v1). One not yet read is
		// written again, so it starts the next turn; only a stopping Run
		// fails it.
		if !c.resend(in) {
			c.inputs.fail(c.sink, in, fmt.Errorf("claude: the message was %s", state))
		}
	case "refused":
		c.inputs.fail(c.sink, in, fmt.Errorf("claude: the message was %s", state))
	}
	if !c.inputs.pending(in.RequestID) {
		c.mu.Lock()
		delete(c.resent, in.RequestID)
		c.mu.Unlock()
	}
	if state != "queued" && state != "started" {
		c.maybeIdle()
	}
}

// resend writes an accepted, unread line again, under a new uuid mapped to
// the same request id; false if it must fail instead (the Run is stopping,
// it was read already, or it was cancelled three times).
func (c *Claude) resend(in proto.Input) bool {
	c.mu.Lock()
	if c.resent == nil {
		c.resent = map[string]int{}
	}
	n := c.resent[in.RequestID] + 1
	ok := !c.stopping && n <= 3
	if ok {
		c.resent[in.RequestID] = n
	}
	c.mu.Unlock()
	if !ok || !c.inputs.unreadOne(in.RequestID) {
		return false
	}
	uuid := claudeUUID(fmt.Sprintf("%s#%d", in.RequestID, n))
	c.mu.Lock()
	c.putSent(uuid, in)
	c.mu.Unlock()
	if err := c.writeLine(uuid, in); err != nil {
		c.mu.Lock()
		c.dropSent(uuid)
		c.mu.Unlock()
		return false
	}
	return true
}

// writeLine writes a user line: the input's images, then its text. Never
// "priority": "now" aborts the running turn at its next tool boundary,
// "later" holds the line until the turn ends.
func (c *Claude) writeLine(uuid string, in proto.Input) error {
	return c.lw.send(map[string]any{
		"type":               "user",
		"message":            map[string]any{"role": "user", "content": inputContent(dialectClaude, in)},
		"parent_tool_use_id": nil,
		"uuid":               uuid,
	})
}

// maybeIdle reports idle once no turn runs and no line waits: with
// lifecycle frames, every line has ended; without, every line sent has had
// a result.
func (c *Claude) maybeIdle() {
	c.mu.Lock()
	idle := !c.running && len(c.sent) == 0
	if !c.lifecycle {
		idle = c.inTurn == 0
	}
	sink := c.sink
	c.mu.Unlock()
	if idle {
		sink.Activity(true)
	}
}

// Deliver writes the message now: Claude Code queues mid-turn messages
// natively. With interrupt, the running turn is interrupted first.
func (c *Claude) Deliver(in proto.Input) {
	c.mu.Lock()
	if c.proc == nil {
		c.queue = append(c.queue, in)
		c.mu.Unlock()
		return
	}
	busy := c.running || len(c.sent) > 0 || c.inTurn > 0
	c.mu.Unlock()
	if in.Interrupt && busy {
		_ = c.Interrupt()
	}
	if in.HasContent() {
		c.send(in)
	} else if in.RequestID != "" {
		// An interrupt alone: nothing for the agent to read.
		c.inputs.accept(c.sink, in, Delivery{Lands: LandsNextStep}, "")
	}
}

func (c *Claude) Interrupt() error {
	return c.lw.send(map[string]any{
		"type":       "control_request",
		"request_id": ids.New("int"),
		"request":    map[string]string{"subtype": "interrupt"},
	})
}

// Stop sends SIGINT: Claude Code ends the turn cleanly and records it, so
// a resume continues from a consistent transcript. (SIGTERM would leave the
// turn unfinished.)
func (c *Claude) Stop() error {
	c.mu.Lock()
	c.stopping = true
	p := c.proc
	c.mu.Unlock()
	if p == nil {
		return nil
	}
	if err := p.Signal(syscall.SIGINT); err != nil {
		return fmt.Errorf("claude: %w", err)
	}
	return nil
}

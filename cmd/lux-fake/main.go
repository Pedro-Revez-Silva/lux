// Command lux-fake is a scripted coding agent for tests. It speaks the
// three agent protocols lux has adapters for, so steering, stop and resume
// on another host are tested without a model:
//
//	lux-fake [--no-mcp-http]                   ACP (JSON-RPC over stdio);
//	                                           --no-mcp-http: without the
//	                                           HTTP MCP capability
//	lux-fake -p --input-format stream-json …   Claude Code's stream-json
//	lux-fake app-server                        Codex's app-server
//	lux-fake plain                             a line-oriented generic workload
//	lux-fake serve <port> [text]               a small web server (serve.go)
//
// It keeps its conversation in a transcript under
// $HOME/.lux-fake/<session>.jsonl and resumes from it.
//
// Each prompt is a script, one command per line:
//
//	echo <text>            reply with text
//	write <file> <text>    write a line to a file (relative to cwd)
//	append <file> <text>   append a line
//	read <file>            reply with the file's contents
//	sleep <seconds>        take a while (cancellable)
//	print-secret <NAME>    reply with an environment variable
//	cat-file <path>        reply with a file's contents (absolute path)
//	commit <message>       git add -A and commit in the working directory;
//	                       reply "committed <sha>"
//	if-exists <path> <cmd...>      run the rest of the line only if path
//	unless-exists <path> <cmd...>  exists (does not exist); relative to cwd
//	stderr <text>          write to stderr
//	history                reply with every prompt so far in this session
//	exit <code>            exit the process
//	ask                    request a permission (ACP only); reply with the outcome
//	cd <path>              change the working directory for the lines that
//	                       follow (and the rest of the session); reply "cwd <path>"
//	mcp-call <server> <tool> <text>
//	                       call a tool on an MCP server the client configured
//	                       (streamable HTTP), with {"text": <text>}; report it
//	                       as the protocol's tool events and reply with the
//	                       result's text
//
// Anything else is echoed back as "you said: …".
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

func main() {
	switch {
	case len(os.Args) > 1 && os.Args[1] == "plain":
		plain()
	case len(os.Args) > 1 && os.Args[1] == "serve":
		serve(os.Args[2:])
	case slices.Contains(os.Args, "stream-json"):
		streamJSON()
	case slices.Contains(os.Args, "app-server"):
		appServer()
	default:
		acp()
	}
}

// ---- the protocol-neutral core ---------------------------------------------

type entry struct {
	Role string `json:"role"`
	Text string `json:"text"`
}

// agent is the conversation and its turns. Each protocol front end sets
// emit (how a reply goes out) and ask (how a permission request goes out)
// and turns its messages into prompts and cancels.
type agent struct {
	mu      sync.Mutex
	outMu   sync.Mutex
	out     *json.Encoder
	session string
	cwd     string
	cancel  chan struct{} // the running turn's; nil when idle
	steer   []string      // extra prompts for the running turn
	emit    func(text string)
	ask     func() string
	// mcp: the MCP servers the client gave, by name. tool reports a tool
	// call in the protocol's own events: started (result and err empty),
	// then done.
	mcp  map[string]mcpServer
	tool func(call toolCall)
}

func newAgent() *agent {
	a := &agent{out: json.NewEncoder(os.Stdout), cwd: "."}
	if wd, err := os.Getwd(); err == nil {
		a.cwd = wd
	}
	a.ask = func() string { return "not supported" }
	a.tool = func(toolCall) {}
	return a
}

func (a *agent) send(v any) {
	a.outMu.Lock()
	defer a.outMu.Unlock()
	_ = a.out.Encode(v)
}

func transcriptDir() string { return filepath.Join(os.Getenv("HOME"), ".lux-fake") }

func (a *agent) transcript() string { return filepath.Join(transcriptDir(), a.session+".jsonl") }

func (a *agent) newSession() {
	a.session = fmt.Sprintf("fake-%d", time.Now().UnixNano())
	a.record("system", "session started")
}

// loadSession continues a conversation from its transcript.
func (a *agent) loadSession(id string) error {
	if _, err := os.Stat(filepath.Join(transcriptDir(), id+".jsonl")); err != nil {
		return fmt.Errorf("no conversation found with session id %s", id)
	}
	a.session = id
	a.record("system", "session resumed")
	return nil
}

func (a *agent) record(role, text string) {
	_ = os.MkdirAll(transcriptDir(), 0o755)
	f, err := os.OpenFile(a.transcript(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	b, _ := json.Marshal(entry{role, text})
	f.Write(append(b, '\n'))
}

func (a *agent) history() []entry {
	b, err := os.ReadFile(a.transcript())
	if err != nil {
		return nil
	}
	var out []entry
	for _, l := range strings.Split(string(b), "\n") {
		var e entry
		if json.Unmarshal([]byte(l), &e) == nil && e.Role != "" {
			out = append(out, e)
		}
	}
	return out
}

func (a *agent) say(s string) {
	a.emit(s)
	a.record("agent", s)
}

// startTurn marks a turn running and returns its cancel channel, or false
// if one is already running.
func (a *agent) startTurn() (chan struct{}, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cancel != nil {
		return nil, false
	}
	a.cancel = make(chan struct{})
	return a.cancel, true
}

// cancelTurn cancels the running turn, if any.
func (a *agent) cancelTurn() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cancel != nil {
		select {
		case <-a.cancel:
		default:
			close(a.cancel)
		}
	}
}

// addSteer adds a prompt to the running turn; false if none is running.
func (a *agent) addSteer(text string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cancel == nil {
		return false
	}
	a.steer = append(a.steer, text)
	return true
}

// runTurn runs a prompt, then any prompts steered into the turn, and ends
// the turn. Steers are taken and the turn ended under one lock, so a steer
// is either run in this turn or refused (addSteer false): never lost.
func (a *agent) runTurn(prompt string, c chan struct{}) (cancelled bool) {
	for text := prompt; ; {
		a.record("user", text)
		if a.runScript(text, c) {
			cancelled = true
		}
		a.mu.Lock()
		if len(a.steer) == 0 || cancelled {
			// Steers accepted into a cancelled turn are still part of the
			// conversation; record them, not run them.
			for _, t := range a.steer {
				a.record("user", t)
			}
			a.steer, a.cancel = nil, nil
			a.mu.Unlock()
			return cancelled
		}
		text, a.steer = a.steer[0], a.steer[1:]
		a.mu.Unlock()
	}
}

func (a *agent) path(p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(a.cwd, p)
}

// commit commits everything in the working directory, as an agent would.
func (a *agent) commit(message string) string {
	git := func(args ...string) (string, error) {
		c := exec.Command("git", append([]string{"-c", "user.name=lux-fake", "-c", "user.email=lux-fake@localhost"}, args...)...)
		c.Dir = a.cwd
		out, err := c.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("git %s: %v: %s", args[0], err, strings.TrimSpace(string(out)))
		}
		return strings.TrimSpace(string(out)), nil
	}
	if _, err := git("add", "-A"); err != nil {
		return "error: " + err.Error()
	}
	if _, err := git("commit", "-q", "-m", message); err != nil {
		return "error: " + err.Error()
	}
	sha, err := git("rev-parse", "HEAD")
	if err != nil {
		return "error: " + err.Error()
	}
	return "committed " + sha
}

// runScript runs one prompt; true if it was cancelled.
func (a *agent) runScript(script string, cancel chan struct{}) bool {
	for _, line := range strings.Split(script, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if a.runLine(line, cancel) {
			return true
		}
		select {
		case <-cancel:
			a.say("cancelled")
			return true
		default:
		}
	}
	return false
}

// runLine runs one script command; true if it was cancelled.
func (a *agent) runLine(line string, cancel chan struct{}) bool {
	cmd, rest, _ := strings.Cut(line, " ")
	switch cmd {
	case "if-exists", "unless-exists":
		path, then, _ := strings.Cut(rest, " ")
		_, err := os.Stat(a.path(path))
		if then = strings.TrimSpace(then); (err == nil) == (cmd == "if-exists") && then != "" {
			return a.runLine(then, cancel)
		}
	case "echo":
		a.say(rest)
	case "write", "append":
		file, text, _ := strings.Cut(rest, " ")
		flag := os.O_CREATE | os.O_WRONLY | os.O_TRUNC
		if cmd == "append" {
			flag = os.O_CREATE | os.O_WRONLY | os.O_APPEND
		}
		_ = os.MkdirAll(filepath.Dir(a.path(file)), 0o755)
		if err := appendFile(a.path(file), flag, text+"\n"); err != nil {
			a.say("error: " + err.Error())
			return false
		}
		a.say("wrote " + file)
	case "read", "cat-file":
		b, err := os.ReadFile(a.path(rest))
		if err != nil {
			a.say("error: " + err.Error())
			return false
		}
		a.say(strings.TrimRight(string(b), "\n"))
	case "sleep":
		secs, _ := strconv.ParseFloat(rest, 64)
		select {
		case <-time.After(time.Duration(secs * float64(time.Second))):
		case <-cancel:
			a.say("cancelled")
			return true
		}
	case "print-secret":
		a.say(os.Getenv(rest))
	case "commit":
		a.say(a.commit(rest))
	case "stderr":
		fmt.Fprintln(os.Stderr, rest)
	case "history":
		var parts []string
		for _, e := range a.history() {
			if e.Role == "user" {
				parts = append(parts, e.Text)
			}
		}
		a.say("history: " + strings.Join(parts, " | "))
	case "exit":
		code, _ := strconv.Atoi(rest)
		os.Exit(code)
	case "ask":
		a.say("permission: " + a.ask())
	case "cd":
		dir := filepath.Clean(a.path(rest))
		if fi, err := os.Stat(dir); err != nil {
			a.say("error: " + err.Error())
		} else if !fi.IsDir() {
			a.say("error: " + dir + " is not a directory")
		} else {
			a.cwd = dir
			a.say("cwd " + dir)
		}
	case "http":
		a.httpCall(rest)
	case "mcp-call":
		server, rest, _ := strings.Cut(rest, " ")
		tool, text, _ := strings.Cut(rest, " ")
		a.say(a.mcpCall(server, tool, text))
	default:
		a.say("you said: " + line)
	}
	return false
}

func appendFile(path string, flag int, text string) error {
	f, err := os.OpenFile(path, flag, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(text)
	return err
}

func scanner() *bufio.Scanner {
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	return sc
}

// textBlocks is the [{"type":"text","text":…}] list all three protocols use.
type textBlocks []struct {
	Text string `json:"text"`
}

func (t textBlocks) String() string {
	var b strings.Builder
	for _, x := range t {
		b.WriteString(x.Text)
	}
	return b.String()
}

// ---- ACP ----------------------------------------------------------------

type rpcMsg struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
}

// acp speaks the Agent Client Protocol. Replies stream as several chunks
// without line breaks, as real agents' do.
func acp() {
	a := newAgent()
	var pmu sync.Mutex
	pending := map[string]chan json.RawMessage{}
	nextID := 0
	rpc := func(v map[string]any) { v["jsonrpc"] = "2.0"; a.send(v) }
	update := func(kind, text string) {
		rpc(map[string]any{"method": "session/update", "params": map[string]any{"sessionId": a.session,
			"update": map[string]any{"sessionUpdate": kind, "content": map[string]string{"type": "text", "text": text}}}})
	}
	a.emit = func(s string) {
		for len(s) > 4 {
			update("agent_message_chunk", s[:4])
			s = s[4:]
		}
		update("agent_message_chunk", s+" ")
	}
	a.ask = func() string {
		pmu.Lock()
		nextID++
		id := strconv.Itoa(1000 + nextID)
		ch := make(chan json.RawMessage, 1)
		pending[id] = ch
		pmu.Unlock()
		rpc(map[string]any{"id": json.RawMessage(id), "method": "session/request_permission", "params": map[string]any{
			"sessionId": a.session,
			"options": []map[string]string{
				{"optionId": "no", "name": "Reject", "kind": "reject_once"},
				{"optionId": "yes", "name": "Allow", "kind": "allow_once"},
			},
		}})
		return string(<-ch)
	}
	// An MCP tool call is a tool_call, then a tool_call_update.
	a.tool = func(c toolCall) {
		u := map[string]any{"sessionUpdate": "tool_call", "toolCallId": c.ID, "title": c.Server + ": " + c.Tool,
			"kind": "other", "status": "in_progress", "rawInput": c.Args}
		if c.Done {
			status, text := "completed", c.Result
			if c.Err != "" {
				status, text = "failed", c.Err
			}
			u = map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": c.ID, "status": status,
				"content": []any{map[string]any{"type": "content", "content": map[string]string{"type": "text", "text": text}}}}
		}
		rpc(map[string]any{"method": "session/update", "params": map[string]any{"sessionId": a.session, "update": u}})
	}
	reply := func(id json.RawMessage, result any) { rpc(map[string]any{"id": id, "result": result}) }
	fail := func(id json.RawMessage, code int, msg string) {
		rpc(map[string]any{"id": id, "error": map[string]any{"code": code, "message": msg}})
	}
	for sc := scanner(); sc.Scan(); {
		var m rpcMsg
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		if m.Method == "" && len(m.ID) > 0 {
			pmu.Lock()
			ch := pending[string(m.ID)]
			delete(pending, string(m.ID))
			pmu.Unlock()
			if ch != nil {
				ch <- m.Result
			}
			continue
		}
		var p struct {
			SessionID  string          `json:"sessionId"`
			Cwd        string          `json:"cwd"`
			Prompt     textBlocks      `json:"prompt"`
			MCPServers json.RawMessage `json:"mcpServers"`
		}
		_ = json.Unmarshal(m.Params, &p)
		switch m.Method {
		case "initialize":
			reply(m.ID, map[string]any{"protocolVersion": 1, "agentCapabilities": map[string]any{"loadSession": true,
				"mcpCapabilities": map[string]bool{"http": !slices.Contains(os.Args, "--no-mcp-http"), "sse": false}},
				"agentInfo": map[string]string{"name": "lux-fake", "version": "1"}})
		case "session/new":
			a.cwd = p.Cwd
			a.setMCP(acpMCP(p.MCPServers))
			a.newSession()
			reply(m.ID, map[string]any{"sessionId": a.session})
		case "session/load":
			if err := a.loadSession(p.SessionID); err != nil {
				fail(m.ID, -32002, err.Error())
				continue
			}
			a.cwd = p.Cwd
			a.setMCP(acpMCP(p.MCPServers))
			for _, e := range a.history() {
				kind := "agent_message_chunk"
				if e.Role == "user" {
					kind = "user_message_chunk"
				}
				update(kind, e.Text+"\n")
			}
			reply(m.ID, map[string]any{})
		case "session/prompt":
			id, text := m.ID, p.Prompt.String()
			go func() {
				// ACP has no mid-turn message; a prompt during a turn waits.
				c, ok := a.startTurn()
				for !ok {
					time.Sleep(50 * time.Millisecond)
					c, ok = a.startTurn()
				}
				stop := "end_turn"
				if a.runTurn(text, c) {
					stop = "cancelled"
				}
				// Usage as OpenCode reports it (lux passes it through).
				reply(id, map[string]any{"stopReason": stop, "usage": map[string]any{"inputTokens": 2, "outputTokens": 10, "totalTokens": 12}, "_meta": map[string]any{}})
			}()
		case "session/cancel":
			a.cancelTurn()
		default:
			if len(m.ID) > 0 {
				fail(m.ID, -32601, "unknown method "+m.Method)
			}
		}
	}
}

// ---- Claude Code stream-json --------------------------------------------

// streamJSON speaks Claude Code's stream-json protocol (the subset lux
// uses; see docs/agent-protocols.md):
//
//	lux-fake -p --input-format stream-json --output-format stream-json --verbose [--resume <id>]
//
// User messages queue and run one turn at a time, each ending with a
// "result" event. A control_request interrupt cancels the running turn.
// SIGINT ends the turn cleanly and exits 0, as the real CLI does in -p
// mode. When stdin closes, queued turns
// finish before it exits, as the real CLI does in -p mode.
func streamJSON() {
	a := newAgent()
	a.emit = func(s string) {
		a.send(map[string]any{"type": "assistant", "session_id": a.session,
			"message": map[string]any{"role": "assistant", "content": []map[string]string{{"type": "text", "text": s}}}})
	}
	// An MCP tool call is a tool_use in an assistant message, then its
	// tool_result in a user message; Claude Code names the tool
	// mcp__<server>__<tool>.
	a.tool = func(c toolCall) {
		if !c.Done {
			a.send(map[string]any{"type": "assistant", "session_id": a.session, "message": map[string]any{"role": "assistant",
				"content": []map[string]any{{"type": "tool_use", "id": c.ID, "name": "mcp__" + c.Server + "__" + c.Tool, "input": c.Args}}}})
			return
		}
		text := c.Result
		if c.Err != "" {
			text = c.Err
		}
		a.send(map[string]any{"type": "user", "session_id": a.session, "message": map[string]any{"role": "user",
			"content": []map[string]any{{"type": "tool_result", "tool_use_id": c.ID, "is_error": c.Err != "",
				"content": []map[string]string{{"type": "text", "text": text}}}}}})
	}
	a.setMCP(claudeMCP(os.Args))
	if i := slices.Index(os.Args, "--resume"); i >= 0 && i+1 < len(os.Args) {
		if err := a.loadSession(os.Args[i+1]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	} else {
		a.newSession()
	}
	a.send(map[string]any{"type": "system", "subtype": "init", "session_id": a.session, "cwd": a.cwd,
		"capabilities": []string{"interrupt_receipt_v1"}})

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT)
	go func() {
		<-sigs
		a.cancelTurn()
		time.Sleep(100 * time.Millisecond)
		os.Exit(0)
	}()
	turns := make(chan string, 64)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for text := range turns {
			c, _ := a.startTurn()
			reason := "completed"
			if a.runTurn(text, c) {
				reason = "aborted_streaming"
			}
			a.send(map[string]any{"type": "result", "subtype": "success", "session_id": a.session,
				"terminal_reason": reason, "queued_turn_count": len(turns),
				// Usage as Claude Code reports it on its result line.
				"usage": map[string]any{"input_tokens": 3, "output_tokens": 9}, "total_cost_usd": 0.0001})
		}
	}()
	for sc := scanner(); sc.Scan(); {
		var m struct {
			Type      string `json:"type"`
			RequestID string `json:"request_id"`
			Request   struct {
				Subtype string `json:"subtype"`
			} `json:"request"`
			Message struct {
				Content textBlocks `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		switch m.Type {
		case "user":
			turns <- m.Message.Content.String()
		case "control_request":
			if m.Request.Subtype == "interrupt" {
				a.cancelTurn()
			}
			a.send(map[string]any{"type": "control_response", "response": map[string]any{
				"subtype": "success", "request_id": m.RequestID, "response": map[string]any{"still_queued": []string{}}}})
		}
	}
	close(turns)
	<-done
}

// ---- Codex app-server -----------------------------------------------------

// appServer speaks Codex's app-server protocol (the subset lux uses; see
// docs/agent-protocols.md): JSON-RPC over stdio, with responses that carry
// no "jsonrpc" field, like the real server. A thread is the conversation; a
// turn runs a prompt. turn/steer adds to the running turn; turn/interrupt
// cancels it; thread/resume continues a thread in a new process.
func appServer() {
	a := newAgent()
	var turnMu sync.Mutex
	turnID, turnN := "", 0
	a.emit = func(s string) {
		turnMu.Lock()
		id := turnID
		turnMu.Unlock()
		a.send(map[string]any{"method": "item/completed", "params": map[string]any{"threadId": a.session, "turnId": id,
			"item": map[string]any{"type": "agentMessage", "id": fmt.Sprintf("msg-%d", time.Now().UnixNano()), "text": s}}})
	}
	// An MCP tool call is an mcpToolCall item, started then completed (the
	// shape of codex app-server's generated schema, ThreadItem).
	a.tool = func(c toolCall) {
		turnMu.Lock()
		id := turnID
		turnMu.Unlock()
		item := map[string]any{"type": "mcpToolCall", "id": c.ID, "server": c.Server, "tool": c.Tool,
			"arguments": c.Args, "status": "inProgress", "result": nil, "error": nil, "durationMs": nil}
		method := "item/started"
		if c.Done {
			method = "item/completed"
			item["status"], item["durationMs"] = "completed", time.Since(c.Started).Milliseconds()
			item["result"] = map[string]any{"content": []map[string]string{{"type": "text", "text": c.Result}}}
			if c.Err != "" {
				item["status"], item["result"] = "failed", nil
				item["error"] = map[string]string{"message": c.Err}
			}
		}
		a.send(map[string]any{"method": method, "params": map[string]any{"threadId": a.session, "turnId": id, "item": item}})
	}
	a.setMCP(codexMCP(os.Args))
	reply := func(id json.RawMessage, result any) { a.send(map[string]any{"id": id, "result": result}) }
	fail := func(id json.RawMessage, err error) {
		a.send(map[string]any{"id": id, "error": map[string]any{"code": -32600, "message": err.Error()}})
	}
	thread := func() map[string]any {
		return map[string]any{"thread": map[string]any{"id": a.session, "status": map[string]string{"type": "idle"}}}
	}
	for sc := scanner(); sc.Scan(); {
		var m rpcMsg
		if json.Unmarshal(sc.Bytes(), &m) != nil || m.Method == "" {
			continue
		}
		var p struct {
			Cwd            string     `json:"cwd"`
			ThreadID       string     `json:"threadId"`
			TurnID         string     `json:"turnId"`
			ExpectedTurnID string     `json:"expectedTurnId"`
			Input          textBlocks `json:"input"`
		}
		_ = json.Unmarshal(m.Params, &p)
		turnMu.Lock()
		current := turnID
		turnMu.Unlock()
		switch m.Method {
		case "initialize":
			reply(m.ID, map[string]any{"userAgent": "lux-fake/1"})
		case "thread/start":
			if p.Cwd != "" {
				a.cwd = p.Cwd
			}
			a.newSession()
			reply(m.ID, thread())
		case "thread/resume":
			if err := a.loadSession(p.ThreadID); err != nil {
				fail(m.ID, fmt.Errorf("no rollout found for thread id %s", p.ThreadID))
				continue
			}
			reply(m.ID, thread())
		case "turn/start":
			if p.ThreadID != a.session {
				fail(m.ID, fmt.Errorf("thread not found: %s", p.ThreadID))
				continue
			}
			c, ok := a.startTurn()
			if !ok {
				fail(m.ID, errors.New("a turn is already in progress"))
				continue
			}
			turnMu.Lock()
			turnN++
			turnID = fmt.Sprintf("turn-%d", turnN)
			id := turnID
			turnMu.Unlock()
			turn := map[string]any{"id": id, "status": "inProgress"}
			reply(m.ID, map[string]any{"turn": turn})
			a.send(map[string]any{"method": "turn/started", "params": map[string]any{"threadId": a.session, "turn": turn}})
			go func(text string) {
				status := "completed"
				if a.runTurn(text, c) {
					status = "interrupted"
				}
				turnMu.Lock()
				turnID = ""
				turnMu.Unlock()
				// Usage as Codex reports it: a notification before the turn ends.
				a.send(map[string]any{"method": "thread/tokenUsage/updated", "params": map[string]any{"threadId": a.session,
					"turnId": id, "tokenUsage": map[string]any{
						"last":               map[string]any{"inputTokens": 5, "outputTokens": 7, "cachedInputTokens": 0, "reasoningOutputTokens": 0, "totalTokens": 12},
						"total":              map[string]any{"inputTokens": 5, "outputTokens": 7, "cachedInputTokens": 0, "reasoningOutputTokens": 0, "totalTokens": 12},
						"modelContextWindow": 200000}}})
				a.send(map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": a.session,
					"turn": map[string]any{"id": id, "status": status}}})
			}(p.Input.String())
		case "turn/steer":
			if current == "" || p.ExpectedTurnID != current || !a.addSteer(p.Input.String()) {
				fail(m.ID, errors.New("expectedTurnId does not match the active turn"))
				continue
			}
			reply(m.ID, map[string]any{})
		case "turn/interrupt":
			if p.TurnID == current {
				a.cancelTurn()
			}
			reply(m.ID, map[string]any{})
		default:
			fail(m.ID, fmt.Errorf("unknown method %s", m.Method))
		}
	}
}

// ---- plain ----------------------------------------------------------------

// plain is a line-oriented workload for the generic adapter: each stdin
// line is a script line.
func plain() {
	for sc := bufio.NewScanner(os.Stdin); sc.Scan(); {
		line := strings.TrimSpace(sc.Text())
		cmd, rest, _ := strings.Cut(line, " ")
		switch cmd {
		case "echo":
			fmt.Println(rest)
		case "exit":
			code, _ := strconv.Atoi(rest)
			os.Exit(code)
		case "print-secret":
			fmt.Println(os.Getenv(rest))
		case "write":
			file, text, _ := strings.Cut(rest, " ")
			os.WriteFile(file, []byte(text), 0o644)
			fmt.Println("wrote", file)
		default:
			fmt.Println("you said:", line)
		}
	}
}

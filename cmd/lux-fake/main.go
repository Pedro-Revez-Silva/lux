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
//	sh <command>           run command with sh -c as the agent's shell tool,
//	                       reported as the protocol's shell tool events
//	                       (shell.go); a cancel kills it
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
	steer   []prompt      // extra prompts for the running turn
	emit    func(text string)
	// read reports a prompt entering the model's context, as the protocol
	// does (Codex's userMessage item, Claude Code's command_lifecycle
	// started): at the turn's start, and for a steer at the next step.
	read func(p prompt)
	// readAll, if set, reports the prompts steered in during one step
	// entering the next step's context together (OpenCode: one assistant
	// step, whose parent is the newest of them); else read, each.
	readAll func(ps []prompt)
	ask     func() string
	// finalStepEndsTurn: a prompt steered in during the turn's last step
	// (no tool call follows) is not read in this turn; runTurn leaves it in
	// carry, for the next (Claude Code). Otherwise the turn reads it at
	// its end and goes on (Codex, OpenCode).
	finalStepEndsTurn bool
	carry             []prompt
	// dropped reports the steers a cancelled turn never read.
	dropped func([]prompt)
	// mcp: the MCP servers the client gave, by name. tool reports a tool
	// call in the protocol's own events: started (result and err empty),
	// then done.
	mcp  map[string]mcpServer
	tool func(call toolCall)
	// shell reports a shell tool call the same way (shell.go).
	shell func(call shellCall)
}

// prompt is a user message and the client's id for it.
type prompt struct {
	text, id string
}

func newAgent() *agent {
	a := &agent{out: json.NewEncoder(os.Stdout), cwd: ".", read: func(prompt) {}}
	if wd, err := os.Getwd(); err == nil {
		a.cwd = wd
	}
	a.ask = func() string { return "not supported" }
	a.tool = func(toolCall) {}
	a.shell = func(shellCall) {}
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
func (a *agent) addSteer(p prompt) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cancel == nil {
		return false
	}
	a.steer = append(a.steer, p)
	return true
}

// join adds a prompt to the running turn and, under the same lock, calls
// then; false (and then not called) if no turn is running.
func (a *agent) join(p prompt, then func()) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cancel == nil {
		return false
	}
	a.steer = append(a.steer, p)
	then()
	return true
}

// takeSteers returns the prompts steered into the running turn so far.
func (a *agent) takeSteers() []prompt {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.steer
	a.steer = nil
	return s
}

// runTurn runs a prompt, and ends the turn. Prompts steered into it are
// read at its next step, after the script line running when they arrived
// (as a real agent reads them after the running tool call), and run then.
// Steers are taken and the turn ended under one lock, so a steer is either
// run in this turn or refused (addSteer false): never lost.
func (a *agent) runTurn(first prompt, c chan struct{}) (cancelled bool) {
	a.read(first)
	a.record("user", first.text)
	cancelled = a.runScript(first.text, c)
	for !cancelled {
		a.mu.Lock()
		if len(a.steer) == 0 || a.finalStepEndsTurn {
			a.carry = append(a.carry, a.steer...)
			a.steer, a.cancel = nil, nil
			a.mu.Unlock()
			return false
		}
		a.mu.Unlock()
		cancelled = a.runSteers(c)
	}
	// Steers accepted into a cancelled turn are dropped unread, as Codex
	// and OpenCode drop them (Claude Code reports them cancelled).
	a.mu.Lock()
	dropped := a.steer
	a.steer, a.cancel = nil, nil
	a.mu.Unlock()
	if a.dropped != nil {
		a.dropped(dropped)
	}
	return true
}

// runSteers reads and runs the prompts steered in so far; true if the turn
// was cancelled.
func (a *agent) runSteers(c chan struct{}) bool {
	steers := a.takeSteers()
	if a.readAll != nil {
		a.readAll(steers)
	}
	for _, p := range steers {
		if a.readAll == nil {
			a.read(p)
		}
		a.record("user", p.text)
		if a.runScript(p.text, c) {
			return true
		}
	}
	return false
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

// runScript runs one prompt; true if it was cancelled. Prompts steered in
// during a line are read and run after it: the next step.
func (a *agent) runScript(script string, cancel chan struct{}) bool {
	var lines []string
	for _, line := range strings.Split(script, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	for i, line := range lines {
		if a.runLine(line, cancel) {
			return true
		}
		select {
		case <-cancel:
			a.say("cancelled")
			return true
		default:
		}
		if i == len(lines)-1 && a.finalStepEndsTurn {
			break
		}
		if a.runSteers(cancel) {
			return true
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
	case "sh":
		return a.runShell(rest, cancel)
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
// without line breaks, as real agents' do. As OpenCode does, a
// session/prompt sent during a turn joins it: it is read at the turn's next
// step, and every prompt of the turn gets its result when the turn ends.
// With --port it also serves OpenCode's HTTP server (opencode.go).
func acp() {
	a := newAgent()
	oc := &opencodeServer{a: a}
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
	// A shell command is OpenCode's bash tool: a pending tool_call, an
	// in_progress update naming the command, then completed (or failed)
	// with its output (opencode-acp-legacy-1).
	a.shell = func(c shellCall) {
		up := func(u map[string]any) {
			u["toolCallId"] = c.ID
			rpc(map[string]any{"method": "session/update", "params": map[string]any{"sessionId": a.session, "update": u}})
		}
		if !c.Done {
			up(map[string]any{"sessionUpdate": "tool_call", "title": "bash", "kind": "execute", "status": "pending", "rawInput": map[string]any{}})
			up(map[string]any{"sessionUpdate": "tool_call_update", "status": "in_progress", "kind": "execute", "title": c.Command,
				"rawInput": map[string]any{"command": c.Command}})
			return
		}
		status := "completed"
		if c.Cancelled || c.ExitCode != 0 {
			status = "failed"
		}
		up(map[string]any{"sessionUpdate": "tool_call_update", "status": status, "title": c.Command,
			"content":   []any{map[string]any{"type": "content", "content": map[string]string{"type": "text", "text": c.Output}}},
			"rawOutput": map[string]any{"output": c.Output, "metadata": map[string]any{"output": c.Output, "exit": c.ExitCode}}})
	}
	reply := func(id json.RawMessage, result any) { rpc(map[string]any{"id": id, "result": result}) }
	fail := func(id json.RawMessage, code int, msg string) {
		rpc(map[string]any{"id": id, "error": map[string]any{"code": code, "message": msg}})
	}
	// waiters: the session/prompt ids joined to the running loop (guarded
	// by a.mu, through join).
	var waiters []json.RawMessage
	// Each model step reading a user message is an assistant message whose
	// parentID is that message.
	a.read = func(p prompt) { oc.step(p.id) }
	a.readAll = func(ps []prompt) {
		if len(ps) > 0 {
			oc.step(ps[len(ps)-1].id)
		}
	}
	// runLoop runs a loop for its first prompt and any joined to it, then
	// answers every prompt of it with the same result.
	runLoop := func(first prompt, ids []json.RawMessage) {
		c, ok := a.startTurn()
		for !ok {
			// A loop is ending; the prompt starts the next one.
			time.Sleep(20 * time.Millisecond)
			c, ok = a.startTurn()
		}
		oc.status(true)
		stop := "end_turn"
		if a.runTurn(first, c) {
			stop = "cancelled"
		}
		a.mu.Lock()
		ids = append(ids, waiters...)
		waiters = nil
		a.mu.Unlock()
		oc.status(false)
		for _, id := range ids {
			// Usage as OpenCode reports it (lux passes it through).
			reply(id, map[string]any{"stopReason": stop, "usage": map[string]any{"inputTokens": 2, "outputTokens": 10, "totalTokens": 12}, "_meta": map[string]any{}})
		}
	}
	oc.run = func(p prompt) { runLoop(p, nil) }
	oc.listen(os.Args)
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
			pr := prompt{text: p.Prompt.String(), id: oc.messageID()}
			oc.stored(pr)
			if a.join(pr, func() { waiters = append(waiters, m.ID) }) {
				continue
			}
			go runLoop(pr, []json.RawMessage{m.ID})
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
// A user message sent during a turn is read at the turn's next step, after
// the script line running when it came (a tool boundary), and the turn goes
// on: one "result". Sent during the turn's last line (its final step, no
// tool call after it), it runs as the next turn, as Claude Code does. A
// line with a "uuid" gets msg_lifecycle_v1 command_lifecycle frames:
// queued, started when read, completed (or cancelled with its turn).
// A control_request interrupt cancels the running turn.
// SIGINT ends the turn cleanly and exits 0, as the real CLI does in -p
// mode. When stdin closes, queued turns
// finish before it exits, as the real CLI does in -p mode.
func streamJSON() {
	a := newAgent()
	a.finalStepEndsTurn = true
	lifecycle := func(p prompt, state string) {
		if p.id != "" {
			a.send(map[string]any{"type": "command_lifecycle", "command_uuid": p.id, "state": state,
				"uuid": fmt.Sprintf("lc-%d", time.Now().UnixNano()), "session_id": a.session})
		}
	}
	var started []prompt // read in the running turn
	a.dropped = func(ps []prompt) {
		go func() {
			time.Sleep(20 * time.Millisecond) // after the result
			for _, p := range ps {
				lifecycle(p, "cancelled")
			}
		}()
	}
	a.read = func(p prompt) {
		started = append(started, p)
		lifecycle(p, "started")
	}
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
	// A shell command is the Bash tool: a tool_use, then its tool_result
	// (claude-line-uuid-2).
	a.shell = func(c shellCall) {
		if !c.Done {
			a.send(map[string]any{"type": "assistant", "session_id": a.session, "message": map[string]any{"role": "assistant",
				"content": []map[string]any{{"type": "tool_use", "id": c.ID, "name": "Bash", "input": map[string]string{"command": c.Command}}}}})
			return
		}
		out := c.Output
		if c.Cancelled {
			out = "Interrupted"
		}
		a.send(map[string]any{"type": "user", "session_id": a.session, "parent_tool_use_id": nil, "message": map[string]any{"role": "user",
			"content": []map[string]any{{"type": "tool_result", "tool_use_id": c.ID, "content": out, "is_error": c.Cancelled || c.ExitCode != 0}}}})
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
		"capabilities": []string{"interrupt_receipt_v1", "interrupt_cancel_queued_v1", "msg_lifecycle_v1"}})

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT)
	go func() {
		<-sigs
		a.cancelTurn()
		time.Sleep(100 * time.Millisecond)
		os.Exit(0)
	}()
	turns := make(chan prompt, 64)
	done := make(chan struct{})
	go func() {
		defer close(done)
		next := func() (prompt, bool) {
			a.mu.Lock()
			if len(a.carry) > 0 {
				p := a.carry[0]
				a.carry = a.carry[1:]
				a.mu.Unlock()
				return p, true
			}
			a.mu.Unlock()
			p, ok := <-turns
			return p, ok
		}
		for p, ok := next(); ok; p, ok = next() {
			c, _ := a.startTurn()
			started = nil
			reason := "completed"
			if a.runTurn(p, c) {
				reason = "aborted_streaming"
			}
			// Steers the turn read end with it, then its result, then the
			// prompt that started it (as claude 2.1.280 orders them).
			for _, s := range started[1:] {
				lifecycle(s, "completed")
			}
			a.mu.Lock()
			queued := len(turns) + len(a.carry)
			a.mu.Unlock()
			a.send(map[string]any{"type": "result", "subtype": "success", "session_id": a.session,
				"terminal_reason": reason, "queued_turn_count": queued, "num_turns": len(started),
				// Usage as Claude Code reports it on its result line.
				"usage": map[string]any{"input_tokens": 3, "output_tokens": 9}, "total_cost_usd": 0.0001})
			lifecycle(started[0], map[bool]string{true: "cancelled", false: "completed"}[reason != "completed"])
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
			UUID string `json:"uuid"`
		}
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		switch m.Type {
		case "user":
			p := prompt{text: m.Message.Content.String(), id: m.UUID}
			lifecycle(p, "queued")
			if !a.addSteer(p) {
				turns <- p
			}
		case "control_request":
			if m.Request.Subtype == "interrupt" {
				a.cancelTurn()
			}
			// interrupt_cancel_queued_v1: the lines queued behind the turn
			// are cancelled with it (command_lifecycle cancelled, after the
			// turn's result).
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
	// A shell command is a commandExecution item, started, then completed
	// with its aggregatedOutput (codex-appserver-1).
	a.shell = func(c shellCall) {
		turnMu.Lock()
		id := turnID
		turnMu.Unlock()
		item := map[string]any{"type": "commandExecution", "id": c.ID, "command": "/bin/sh -c '" + c.Command + "'",
			"cwd": a.cwd, "status": "inProgress", "aggregatedOutput": nil, "exitCode": nil}
		method := "item/started"
		if c.Done {
			method = "item/completed"
			item["status"], item["aggregatedOutput"], item["exitCode"] = "completed", c.Output, c.ExitCode
			if c.Cancelled || c.ExitCode != 0 {
				item["status"] = "failed"
			}
		}
		a.send(map[string]any{"method": method, "params": map[string]any{"threadId": a.session, "turnId": id, "item": item}})
	}
	// A user message entering the turn is a userMessage item, started and
	// completed, carrying the client's id (null without one). Codex 0.155
	// emits it when the model's next step reads it.
	a.read = func(p prompt) {
		turnMu.Lock()
		id := turnID
		turnMu.Unlock()
		var client any
		if p.id != "" {
			client = p.id
		}
		item := map[string]any{"type": "userMessage", "id": fmt.Sprintf("um-%d", time.Now().UnixNano()), "clientId": client,
			"content": []map[string]any{{"type": "text", "text": p.text, "text_elements": []any{}}}}
		for _, method := range []string{"item/started", "item/completed"} {
			a.send(map[string]any{"method": method, "params": map[string]any{"threadId": a.session, "turnId": id, "item": item}})
		}
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
			// Echoed as the userMessage item's clientId.
			ClientUserMessageID string `json:"clientUserMessageId"`
		}
		_ = json.Unmarshal(m.Params, &p)
		turnMu.Lock()
		current := turnID
		turnMu.Unlock()
		switch m.Method {
		case "initialize":
			// The real server names its version here; lux reads it.
			reply(m.ID, map[string]any{"userAgent": "lux/0.155.1 (lux-fake)"})
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
			go func(first prompt) {
				status := "completed"
				if a.runTurn(first, c) {
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
			}(prompt{p.Input.String(), p.ClientUserMessageID})
		case "turn/steer":
			// The real server's errors (codex 0.155.1).
			if current == "" {
				fail(m.ID, errors.New("no active turn to steer"))
				continue
			}
			if p.ExpectedTurnID != current {
				fail(m.ID, fmt.Errorf("expected active turn id `%s` but found `%s`", p.ExpectedTurnID, current))
				continue
			}
			if !a.addSteer(prompt{p.Input.String(), p.ClientUserMessageID}) {
				fail(m.ID, errors.New("no active turn to steer"))
				continue
			}
			reply(m.ID, map[string]any{"turnId": current})
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

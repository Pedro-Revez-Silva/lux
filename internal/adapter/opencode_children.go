package adapter

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/marcioapm/lux/internal/proto"
)

// OpenCode's task tool runs a subagent in a child session (session.created
// with info.parentID), which ACP never shows: lux follows it on the bus
// and relays it as opencode.* records (proto.EvOpenCodeSession and the
// rest), for every session whose chain of parents reaches the Run's. What
// the bus says (opencode 1.18.21, .taskdocs/probes and
// testdata/opencode-1.18.21-children*.ndjson):
//
//   - a session: session.created and session.updated, info a Session
//     {id, parentID, title, time{created}}; the bus does not replay them,
//     so a child made before the stream (re)connected, or before a resume,
//     is never known;
//   - a call of the task tool, in its caller's session: a tool part whose
//     state.metadata.sessionId (from running on) is the child it runs;
//   - a child's tool call: message.part.updated, part type tool, on every
//     change of state (pending with no input, running, completed or error)
//     and sometimes again unchanged;
//   - a child's reply: text parts (message.part.updated, whole, and
//     message.part.delta, which is never relayed), then its assistant
//     message.updated with info.time.completed, twice;
//   - a permission request: permission.asked {id, sessionID, permission,
//     patterns, tool{callID}}. OpenCode's ACP forwards it only for a
//     session ACP opened: a child's waits for an answer from nobody.
type ocChildren struct {
	mu sync.Mutex
	// parent: each session's parent session, from session.created and
	// session.updated, and from the task calls that run them.
	parent map[string]string
	// announced: sessions opencode.session was written for.
	announced map[string]bool
	// tasks and parts: the statuses written for each call, as "session
	// \x00callID\x00status"; a status is written once per call, also one
	// the bus repeats after a later one.
	tasks map[string]bool
	parts map[string]bool
	// pending: the stop of the timer that writes a call's held pending
	// ("session\x00callID"; pendingHold).
	pending map[string]func() bool
	// partsWritten: opencode.part records written; past maxChildParts they
	// carry the status alone.
	partsWritten int
	// texts: the text of each text part of a child's message not yet
	// written, by message id, then part id, in the order they came.
	texts map[string]*msgText
	// messages and permissions written, by id.
	messages    map[string]bool
	permissions map[string]bool
}

type msgText struct {
	order []string
	text  map[string]string
}

func newOcChildren() *ocChildren {
	return &ocChildren{parent: map[string]string{}, announced: map[string]bool{}, tasks: map[string]bool{},
		parts: map[string]bool{}, pending: map[string]func() bool{}, texts: map[string]*msgText{}, messages: map[string]bool{}, permissions: map[string]bool{}}
}

const (
	// maxSessionDepth bounds the walk up a session's parents.
	maxSessionDepth = 64
	// childPermissionTimeout bounds answering a child's permission request.
	childPermissionTimeout = 10 * time.Second
	// pendingHold: a tool call's pending is written only if no other
	// status of it comes within this long; a running before then is
	// written alone.
	pendingHold = time.Second
	// maxChildParts: opencode.part records a Run writes in full; after
	// them, one lux.warning, and the status alone.
	maxChildParts = 2000
)

// ofRun reports whether session is the Run's or descends from it. Under
// k.mu.
func (k *ocChildren) ofRun(session, run string) bool {
	for range maxSessionDepth {
		if session == "" || run == "" {
			return false
		}
		if session == run {
			return true
		}
		session = k.parent[session]
	}
	return false
}

// childEvent relays what a bus event says of the Run's child sessions;
// run is the Run's session.
func (a *ACP) childEvent(ev busEvent, run string) {
	k := a.kids
	if k == nil || run == "" {
		return
	}
	p := ev.Properties
	switch ev.Type {
	case "session.created", "session.updated":
		a.childSession(ev, run)
	case "message.part.updated":
		if p.Part == nil {
			return
		}
		switch p.Part.Type {
		case "tool":
			a.childTool(p.Part, run)
		case "text":
			a.childText(p.Part, run)
		}
	case "message.updated":
		a.childMessage(ev, run)
	case "permission.asked":
		a.childPermission(ev, run)
	}
}

// childSession learns a session's parent and writes opencode.session for
// a descendant of the Run's, once.
func (a *ACP) childSession(ev busEvent, run string) {
	k, info := a.kids, ev.Properties.Info
	if info.ID == "" || info.ParentID == "" {
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if _, known := k.parent[info.ID]; !known {
		k.parent[info.ID] = info.ParentID
	}
	if k.announced[info.ID] || info.ID == run || !k.ofRun(info.ID, run) {
		return
	}
	k.announced[info.ID] = true
	a.sink.Event(proto.EvOpenCodeSession, proto.OpenCodeSession{ID: info.ID, ParentID: k.parent[info.ID],
		Title: info.Title, CreatedAt: info.Time.Created})
}

// childTool writes opencode.task for a task call of the Run's tree that
// names its child, and opencode.part for a child's tool call, each on a
// change of status.
func (a *ACP) childTool(part *busPart, run string) {
	k := a.kids
	k.mu.Lock()
	defer k.mu.Unlock()
	child := part.State.Metadata.SessionID
	if part.Tool == "task" && child != "" && child != part.SessionID {
		if _, known := k.parent[child]; !known {
			k.parent[child] = part.SessionID
		}
	}
	if !k.ofRun(part.SessionID, run) || part.CallID == "" {
		return
	}
	key := part.SessionID + "\x00" + part.CallID
	if part.Tool == "task" && child != "" {
		if st := taskStatus(part.State.Status); st != "" && !k.tasks[key+"\x00"+st] {
			k.tasks[key+"\x00"+st] = true
			a.sink.Event(proto.EvOpenCodeTask, proto.OpenCodeTask{SessionID: part.SessionID, CallID: part.CallID,
				ChildSessionID: child, Model: part.State.Metadata.Model, Status: st})
		}
	}
	if part.SessionID == run {
		return
	}
	st := part.State.Status
	if st == "" || k.parts[key+"\x00"+st] {
		return
	}
	if st == "pending" {
		if k.pending[key] == nil {
			held := *part
			k.pending[key] = a.clock.afterFunc(pendingHold, func() {
				a.spawn(func() { a.pendingDue(key, &held) })
			})
		}
		return
	}
	if stop := k.pending[key]; stop != nil {
		stop()
		delete(k.pending, key)
	}
	k.parts[key+"\x00"+st] = true
	a.writePart(part)
}

// pendingDue writes a call's pending held for pendingHold, unless another
// status of it came meanwhile.
func (a *ACP) pendingDue(key string, part *busPart) {
	k := a.kids
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.pending[key] == nil {
		return
	}
	delete(k.pending, key)
	k.parts[key+"\x00pending"] = true
	a.writePart(part)
}

// taskStatus is opencode.task's status for a task call's state, "" for
// one it does not report (pending: no child yet).
func taskStatus(s string) string {
	switch s {
	case "running", "completed", "error":
		return s
	}
	return ""
}

// writePart writes opencode.part: input on every status, output only
// once completed and error only on error, each redacted and cut. Past
// maxChildParts, the status and times alone, after one lux.warning. Under
// k.mu.
func (a *ACP) writePart(part *busPart) {
	k := a.kids
	rec := proto.OpenCodePart{SessionID: part.SessionID, MessageID: part.MessageID, CallID: part.CallID, Tool: part.Tool,
		Status: part.State.Status, Time: proto.OpenCodeTime{Start: part.State.Time.Start, End: part.State.Time.End}}
	if k.partsWritten++; k.partsWritten > maxChildParts {
		if k.partsWritten == maxChildParts+1 {
			a.sink.Event(proto.EvWarning, map[string]any{"message": fmt.Sprintf(
				"opencode: the Run's subagent sessions have made %d tool-call records; the next ones carry their status alone", maxChildParts)})
		}
		a.sink.Event(proto.EvOpenCodePart, rec)
		return
	}
	rec.Title = part.State.Title
	var cut [3]bool
	if in := strings.TrimSpace(string(part.State.Input)); in != "" && in != "{}" && in != "null" {
		rec.Input, cut[0] = a.sink.Clip(in, proto.MaxOpenCodeInput)
	}
	switch rec.Status {
	case "completed":
		rec.Output, cut[1] = a.sink.Clip(part.State.Output, proto.MaxOpenCodeOutput)
	case "error":
		rec.Error, cut[2] = a.sink.Clip(part.State.Error, proto.MaxOpenCodeOutput)
	}
	rec.Truncated = cut[0] || cut[1] || cut[2]
	a.sink.Event(proto.EvOpenCodePart, rec)
}

// childText keeps the newest text of a child message's text part, which
// its message's completion writes.
func (a *ACP) childText(part *busPart, run string) {
	k := a.kids
	k.mu.Lock()
	defer k.mu.Unlock()
	if part.SessionID == run || !k.ofRun(part.SessionID, run) || k.messages[part.MessageID] {
		return
	}
	m := k.texts[part.MessageID]
	if m == nil {
		m = &msgText{text: map[string]string{}}
		k.texts[part.MessageID] = m
	}
	if _, ok := m.text[part.ID]; !ok {
		m.order = append(m.order, part.ID)
	}
	m.text[part.ID] = part.Text
}

// childMessage writes opencode.message for a child's assistant message
// once it is completed, once.
func (a *ACP) childMessage(ev busEvent, run string) {
	k, info := a.kids, ev.Properties.Info
	if info.Role != "assistant" || info.Time.Completed == 0 || info.SessionID == run {
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.messages[info.ID] || !k.ofRun(info.SessionID, run) {
		return
	}
	k.messages[info.ID] = true
	var text []string
	if m := k.texts[info.ID]; m != nil {
		for _, id := range m.order {
			if t := m.text[id]; t != "" {
				text = append(text, t)
			}
		}
	}
	delete(k.texts, info.ID)
	rec := proto.OpenCodeMessage{SessionID: info.SessionID, MessageID: info.ID, Role: info.Role, Cost: info.Cost,
		Completed: info.Time.Completed, Tokens: proto.OpenCodeTokens{Input: info.Tokens.Input, Output: info.Tokens.Output,
			Reasoning: info.Tokens.Reasoning, CacheRead: info.Tokens.Cache.Read, CacheWrite: info.Tokens.Cache.Write}}
	rec.Text, rec.Truncated = a.sink.Clip(strings.Join(text, "\n"), proto.MaxOpenCodeText)
	a.sink.Event(proto.EvOpenCodeMessage, rec)
}

// childPermission records a permission request of the Run's tree, once
// per id. The Run's own session's is answered over ACP (handleRequest):
// recorded as allowed, nothing sent. A child's, which nobody else
// answers, lux allows once over OpenCode's server, off the bus goroutine
// and within a.permissionTimeout, then records it and warns: OpenCode goes
// on with the child as it takes the answer, so records of what the child
// did next can come before these.
func (a *ACP) childPermission(ev busEvent, run string) {
	k, p := a.kids, ev.Properties
	k.mu.Lock()
	if p.ID == "" || k.permissions[p.ID] || !k.ofRun(p.SessionID, run) {
		k.mu.Unlock()
		return
	}
	k.permissions[p.ID] = true
	k.mu.Unlock()
	rec := proto.OpenCodePermission{SessionID: p.SessionID, PermissionID: p.ID, Permission: p.Permission,
		Patterns: p.Patterns, CallID: p.Tool.CallID, Answered: proto.PermissionAllowed}
	if rec.Patterns == nil {
		rec.Patterns = []string{}
	}
	if p.SessionID == run {
		a.sink.Event(proto.EvOpenCodePermission, rec)
		return
	}
	answer := func(err error) {
		msg := fmt.Sprintf("opencode: a subagent session %s asked for %s on %s; lux allowed it", rec.SessionID, rec.Permission, strings.Join(rec.Patterns, ", "))
		if err != nil {
			rec.Answered, rec.Error = proto.PermissionNone, err.Error()
			msg = fmt.Sprintf("opencode: a subagent session %s asked for %s on %s; lux could not answer it, and its task call may wait for it: %v",
				rec.SessionID, rec.Permission, strings.Join(rec.Patterns, ", "), err)
		}
		// Under k.mu, so no record of the tree comes between the two.
		k.mu.Lock()
		defer k.mu.Unlock()
		a.sink.Event(proto.EvOpenCodePermission, rec)
		a.sink.Event(proto.EvWarning, map[string]any{"message": msg})
	}
	if !a.spawn(func() {
		ctx, cancel := context.WithTimeout(a.runCtx(), a.permissionTimeout)
		defer cancel()
		answer(a.bus.allowPermission(ctx, rec.PermissionID))
	}) {
		answer(errors.New("the Run ended before lux could answer it"))
	}
}

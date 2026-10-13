package adapter

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/marcioapm/lux/internal/proto"
)

// OpenCode's task tool runs a subagent in a child session (session.created
// with info.parentID), which ACP never shows: lux follows it on the bus
// and relays it as opencode.* records (proto.EvOpenCodeSession and the
// rest), for every session whose chain of parents reaches the Run's. What
// the bus says (opencode 1.18.21, testdata/opencode-1.18.21-children-*
// .ndjson):
//
//   - a session: session.created and session.updated, info a Session
//     {id, parentID, title, time{created}}; the bus replays nothing, so a
//     child made before the stream (re)connected, or before a resume, is
//     unknown until a later session.updated or an update of the task call
//     running it names its parent, and its earlier events are lost;
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
	// texts: the text of each text part of a child's assistant message not
	// yet written, by message id, then part id, in the order they came;
	// redacted, and at most maxKeptText bytes of a message's (msgText.keep).
	texts map[string]*msgText
	// messages: messages done with, by id: written, or not an
	// assistant's (no text kept). permissions: written, by id.
	messages    map[string]bool
	permissions map[string]bool
	// out: records decided under mu, in order, not yet written; emitting:
	// a goroutine is writing them (emitKids). Records are clipped and
	// written outside mu, so a slow sink never holds the bus reader.
	out      []ocRecord
	emitting bool
}

// ocRecord makes a record to write: it clips (redacts) outside k.mu.
type ocRecord func() (typ string, data any)

func plainRecord(typ string, data any) ocRecord { return func() (string, any) { return typ, data } }

type msgText struct {
	order []string
	text  map[string]string
	// cut: parts whose newest text was cut or dropped where it is kept.
	cut map[string]bool
}

// keep bounds the parts' redacted text, joined as childMessage joins it,
// to maxKeptText bytes: the part crossing the bound keeps its prefix (on
// a rune boundary), the parts after it nothing.
func (m *msgText) keep() {
	n := 0
	for _, id := range m.order {
		t := m.text[id]
		if t == "" {
			continue
		}
		if n > 0 {
			n++ // the "\n" before it
		}
		if room := max(maxKeptText-n, 0); len(t) > room {
			for room > 0 && !utf8.RuneStart(t[room]) {
				room--
			}
			t = t[:room]
			m.text[id], m.cut[id] = t, true
		}
		n += len(t)
	}
}

// truncated reports whether a part's text was cut or dropped.
func (m *msgText) truncated() bool {
	for _, c := range m.cut {
		if c {
			return true
		}
	}
	return false
}

func newOcChildren() *ocChildren {
	return &ocChildren{parent: map[string]string{}, announced: map[string]bool{}, tasks: map[string]bool{},
		parts: map[string]bool{}, pending: map[string]func() bool{}, texts: map[string]*msgText{}, messages: map[string]bool{}, permissions: map[string]bool{}}
}

const (
	// maxSessionDepth: sessions ofRun visits, the Run's own included, so a
	// session at most maxSessionDepth-1 levels below the Run's is of the
	// Run; it also ends the walk on a cycle of parents.
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
	// maxKeptText: bytes of a child message's redacted text, its parts
	// joined, kept until it completes (keep). The record redacts the join
	// again and cuts it to MaxOpenCodeText; the 256 bytes over it are
	// slack for that second redaction shrinking the join, where a pattern
	// matches across a part boundary.
	maxKeptText = proto.MaxOpenCodeText + 256
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
	if _, known := k.parent[info.ID]; !known {
		k.parent[info.ID] = info.ParentID
	}
	if k.announced[info.ID] || info.ID == run || !k.ofRun(info.ID, run) {
		k.mu.Unlock()
		return
	}
	k.announced[info.ID] = true
	k.out = append(k.out, plainRecord(proto.EvOpenCodeSession, proto.OpenCodeSession{ID: info.ID, ParentID: k.parent[info.ID],
		Title: info.Title, CreatedAt: info.Time.Created}))
	k.mu.Unlock()
	a.emitKids()
}

// childTool writes opencode.task for a task call of the Run's tree that
// names its child, and opencode.part for a child's tool call, each on a
// change of status.
func (a *ACP) childTool(part *busPart, run string) {
	k := a.kids
	k.mu.Lock()
	defer a.emitKids()
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
			k.out = append(k.out, plainRecord(proto.EvOpenCodeTask, proto.OpenCodeTask{SessionID: part.SessionID, CallID: part.CallID,
				ChildSessionID: child, Model: part.State.Metadata.Model, Status: st}))
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
	a.queuePart(*part)
}

// pendingDue writes a call's pending held for pendingHold, unless another
// status of it came meanwhile.
func (a *ACP) pendingDue(key string, part *busPart) {
	k := a.kids
	k.mu.Lock()
	defer a.emitKids()
	defer k.mu.Unlock()
	if k.pending[key] == nil {
		return
	}
	delete(k.pending, key)
	k.parts[key+"\x00pending"] = true
	a.queuePart(*part)
}

// stopPending stops the timers of every held pending, as the Run ends: a
// pending not yet written is not.
func (k *ocChildren) stopPending() {
	if k == nil {
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	for key, stop := range k.pending {
		stop()
		delete(k.pending, key)
	}
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

// queuePart queues opencode.part: input on every status, output only
// once completed and error only on error, each redacted and cut as it is
// written. Past maxChildParts, the status and times alone, after one
// lux.warning. Under k.mu.
func (a *ACP) queuePart(part busPart) {
	k := a.kids
	rec := proto.OpenCodePart{SessionID: part.SessionID, MessageID: part.MessageID, CallID: part.CallID, Tool: part.Tool,
		Status: part.State.Status, Time: proto.OpenCodeTime{Start: part.State.Time.Start, End: part.State.Time.End}}
	if k.partsWritten++; k.partsWritten > maxChildParts {
		if k.partsWritten == maxChildParts+1 {
			k.out = append(k.out, plainRecord(proto.EvWarning, map[string]any{"message": fmt.Sprintf(
				"opencode: the Run's subagent sessions have made %d tool-call records; the next ones carry their status alone", maxChildParts)}))
		}
		k.out = append(k.out, plainRecord(proto.EvOpenCodePart, rec))
		return
	}
	rec.Title = part.State.Title
	k.out = append(k.out, func() (string, any) {
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
		return proto.EvOpenCodePart, rec
	})
}

// emitKids writes the queued records, in order, outside k.mu. One
// goroutine writes at a time; one that queues while another writes
// returns at once, and the writer takes its records too.
func (a *ACP) emitKids() {
	k := a.kids
	k.mu.Lock()
	if k.emitting {
		k.mu.Unlock()
		return
	}
	k.emitting = true
	for len(k.out) > 0 {
		batch := k.out
		k.out = nil
		k.mu.Unlock()
		for _, r := range batch {
			a.sink.Event(r())
		}
		k.mu.Lock()
	}
	k.emitting = false
	k.mu.Unlock()
}

// childText keeps the newest text of a child message's text part, which
// its message's completion writes: at most maxKeptText bytes of it, cut
// by sink.Clip (redacted first, so a secret is never half kept), outside
// k.mu.
func (a *ACP) childText(part *busPart, run string) {
	k := a.kids
	if part.SessionID == run {
		return
	}
	k.mu.Lock()
	ours := !k.messages[part.MessageID] && k.ofRun(part.SessionID, run)
	k.mu.Unlock()
	if !ours {
		return
	}
	// Every part redacted before keep charges it, so where the parts
	// split a text does not change what is kept of it.
	text, cut := a.sink.Clip(part.Text, maxKeptText)
	text = strings.Clone(text) // not a slice of the whole text
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.messages[part.MessageID] {
		return
	}
	m := k.texts[part.MessageID]
	if m == nil {
		m = &msgText{text: map[string]string{}, cut: map[string]bool{}}
		k.texts[part.MessageID] = m
	}
	if _, ok := m.text[part.ID]; !ok {
		m.order = append(m.order, part.ID)
	}
	m.text[part.ID], m.cut[part.ID] = text, cut
	m.keep()
}

// childMessage writes opencode.message for a child's assistant message
// once it is completed, once. A message of another role is done with as
// its role is known: its text is dropped, and no more kept.
func (a *ACP) childMessage(ev busEvent, run string) {
	k, info := a.kids, ev.Properties.Info
	if info.ID == "" || info.Role == "" || info.SessionID == run || (info.Role == "assistant" && info.Time.Completed == 0) {
		return
	}
	k.mu.Lock()
	if k.messages[info.ID] || !k.ofRun(info.SessionID, run) {
		k.mu.Unlock()
		return
	}
	k.messages[info.ID] = true
	m := k.texts[info.ID]
	delete(k.texts, info.ID)
	if info.Role != "assistant" {
		k.mu.Unlock()
		return
	}
	var text []string
	cut := false
	if m != nil {
		cut = m.truncated()
		for _, id := range m.order {
			if t := m.text[id]; t != "" {
				text = append(text, t)
			}
		}
	}
	rec := proto.OpenCodeMessage{SessionID: info.SessionID, MessageID: info.ID, Role: info.Role, Cost: info.Cost,
		Completed: info.Time.Completed, Tokens: proto.OpenCodeTokens{Input: info.Tokens.Input, Output: info.Tokens.Output,
			Reasoning: info.Tokens.Reasoning, CacheRead: info.Tokens.Cache.Read, CacheWrite: info.Tokens.Cache.Write}}
	k.out = append(k.out, func() (string, any) {
		rec.Text, rec.Truncated = a.sink.Clip(strings.Join(text, "\n"), proto.MaxOpenCodeText)
		// A part cut where it was kept is truncated whatever the
		// join's length.
		rec.Truncated = rec.Truncated || cut
		return proto.EvOpenCodeMessage, rec
	})
	k.mu.Unlock()
	a.emitKids()
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
	rec := proto.OpenCodePermission{SessionID: p.SessionID, PermissionID: p.ID, Permission: p.Permission,
		Patterns: p.Patterns, CallID: p.Tool.CallID, Answered: proto.PermissionAllowed}
	if rec.Patterns == nil {
		rec.Patterns = []string{}
	}
	if p.SessionID == run {
		k.out = append(k.out, plainRecord(proto.EvOpenCodePermission, rec))
		k.mu.Unlock()
		a.emitKids()
		return
	}
	k.mu.Unlock()
	answer := func(err error) {
		msg := fmt.Sprintf("opencode: a subagent session %s asked for %s on %s; lux allowed it", rec.SessionID, rec.Permission, strings.Join(rec.Patterns, ", "))
		if err != nil {
			rec.Answered, rec.Error = proto.PermissionNone, err.Error()
			msg = fmt.Sprintf("opencode: a subagent session %s asked for %s on %s; lux could not answer it, and its task call may wait for it: %v",
				rec.SessionID, rec.Permission, strings.Join(rec.Patterns, ", "), err)
		}
		// Queued together, so no record of the tree is written between
		// the two.
		k.mu.Lock()
		k.out = append(k.out, plainRecord(proto.EvOpenCodePermission, rec), plainRecord(proto.EvWarning, map[string]any{"message": msg}))
		k.mu.Unlock()
		a.emitKids()
	}
	if !a.spawn(func() {
		ctx, cancel := context.WithTimeout(a.runCtx(), a.permissionTimeout)
		defer cancel()
		answer(a.bus.allowPermission(ctx, rec.PermissionID))
	}) {
		answer(errors.New("the Run ended before lux could answer it"))
	}
}

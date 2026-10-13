package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// subagent runs a task tool call in a child session, as opencode 1.18.21
// reports one on its bus (internal/adapter/testdata/
// opencode-1.18.21-children-*.ndjson), in this order, and returns what the
// call answered:
//
//   - the parent's task part, pending;
//   - session.created for the child (parentID the parent);
//   - the task part running, its state.metadata.sessionId the child;
//   - a read tool call of the child: pending, running, completed;
//   - a bash tool call of the child, running; with ask, a permission.asked
//     for it, which nothing over ACP answers (OpenCode forwards a child's
//     to nobody): the call waits for POST /permission/{id}/reply, then
//     permission.replied; then the call completes, or fails if rejected;
//   - the child's text part and its assistant message, completed (twice,
//     as the bus repeats it), and session.idle for the child;
//   - the task part completed, with the child's answer.
//
// Ids are the parent's session and a counter: the same script gives the
// same ids in every Run of a session. It first waits (up to 5 s) for a
// client on GET /event, so a prompt sent as the Run starts, before lux's
// event stream has connected, does not publish to nobody.
func (o *opencodeServer) subagent(ask bool, cancel chan struct{}) string {
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		o.mu.Lock()
		n := len(o.subs)
		o.mu.Unlock()
		if n > 0 {
			break
		}
	}
	o.mu.Lock()
	o.kids++
	n := o.kids
	o.mu.Unlock()
	parent := o.a.session
	child := fmt.Sprintf("ses_%s_child%d", parent, n)
	taskCall, readCall, bashCall := fmt.Sprintf("toolu_task%d", n), fmt.Sprintf("toolu_read%d", n), fmt.Sprintf("toolu_bash%d", n)
	parentMsg, childMsg := o.messageID(), o.messageID()
	model := map[string]string{"modelID": "lux-fake", "providerID": "lux-fake"}
	meta := map[string]any{"sessionId": child, "parentSessionId": parent, "model": model}
	taskInput := map[string]any{"subagent_type": "general", "description": "fake subagent", "prompt": "read a.txt"}
	now := func() int64 { return time.Now().UnixMilli() }
	start := now()

	o.toolPart(parent, parentMsg, taskCall, "task", map[string]any{"status": "pending", "input": map[string]any{}, "raw": ""})
	o.publish("session.created", map[string]any{"sessionID": child, "info": map[string]any{"id": child, "parentID": parent,
		"title": "fake subagent (@general subagent)", "time": map[string]any{"created": start, "updated": start}}})
	o.toolPart(parent, parentMsg, taskCall, "task", map[string]any{"status": "running", "input": taskInput, "title": "fake subagent",
		"metadata": meta, "time": map[string]any{"start": start}})

	readInput := map[string]any{"filePath": "a.txt"}
	o.toolPart(child, childMsg, readCall, "read", map[string]any{"status": "pending", "input": map[string]any{}, "raw": ""})
	o.toolPart(child, childMsg, readCall, "read", map[string]any{"status": "running", "input": readInput, "time": map[string]any{"start": now()}})
	o.toolPart(child, childMsg, readCall, "read", map[string]any{"status": "completed", "input": readInput, "title": "a.txt",
		"output": "<path>a.txt</path>\n<content>\n1: hello\n</content>", "metadata": map[string]any{}, "time": map[string]any{"start": now(), "end": now()}})

	bashInput := map[string]any{"command": "cat /etc/hostname", "description": "Read the hostname"}
	o.toolPart(child, childMsg, bashCall, "bash", map[string]any{"status": "running", "input": bashInput, "time": map[string]any{"start": now()}})
	reply := "none asked"
	if ask {
		var ok bool
		if reply, ok = o.askPermission(child, childMsg, bashCall, n, cancel); !ok {
			return "cancelled"
		}
	}
	if reply == "reject" {
		o.toolPart(child, childMsg, bashCall, "bash", map[string]any{"status": "error", "input": bashInput,
			"error": "The user rejected permission to use this specific tool call.", "time": map[string]any{"start": now(), "end": now()}})
	} else {
		o.toolPart(child, childMsg, bashCall, "bash", map[string]any{"status": "completed", "input": bashInput, "title": "Read the hostname",
			"output": "fakehost\n", "metadata": map[string]any{}, "time": map[string]any{"start": now(), "end": now()}})
	}

	answer := "the child read a.txt: hello"
	o.publish("message.part.updated", map[string]any{"sessionID": child, "part": map[string]any{"id": fmt.Sprintf("prt_text%d", n),
		"type": "text", "sessionID": child, "messageID": childMsg, "text": answer, "time": map[string]any{"start": now(), "end": now()}}})
	info := map[string]any{"id": childMsg, "sessionID": child, "role": "assistant", "parentID": fmt.Sprintf("msg_user%d", n),
		"cost": 0.001, "tokens": map[string]any{"input": 10, "output": 5, "reasoning": 0, "cache": map[string]int{"read": 2, "write": 1}},
		"time": map[string]any{"created": start, "completed": now()}, "finish": "stop"}
	for range 2 {
		o.publish("message.updated", map[string]any{"sessionID": child, "info": info})
	}
	o.publish("session.idle", map[string]any{"sessionID": child})
	o.toolPart(parent, parentMsg, taskCall, "task", map[string]any{"status": "completed", "input": taskInput, "title": "fake subagent",
		"output":   fmt.Sprintf("<task id=%q state=\"completed\">\n<task_result>\n%s\n</task_result>\n</task>", child, answer),
		"metadata": meta, "time": map[string]any{"start": start, "end": now()}})
	return fmt.Sprintf("%s ran in %s; permission %s", taskCall, child, reply)
}

// toolPart publishes message.part.updated for a tool call.
func (o *opencodeServer) toolPart(session, msg, call, tool string, state map[string]any) {
	o.publish("message.part.updated", map[string]any{"sessionID": session, "part": map[string]any{"id": "prt_" + call,
		"type": "tool", "sessionID": session, "messageID": msg, "callID": call, "tool": tool, "state": state}})
}

// askPermission publishes permission.asked for a child's call and waits
// for its reply, or cancel (false).
func (o *opencodeServer) askPermission(child, msg, call string, n int, cancel chan struct{}) (string, bool) {
	id := fmt.Sprintf("per_fake%d", n)
	ch := make(chan string, 1)
	o.mu.Lock()
	if o.asked == nil {
		o.asked = map[string]chan string{}
	}
	o.asked[id] = ch
	o.mu.Unlock()
	o.publish("permission.asked", map[string]any{"id": id, "sessionID": child, "permission": "bash",
		"patterns": []string{"cat /etc/hostname"}, "always": []string{"cat *"}, "metadata": map[string]any{},
		"tool": map[string]string{"messageID": msg, "callID": call}})
	select {
	case r := <-ch:
		o.publish("permission.replied", map[string]any{"sessionID": child, "requestID": id, "reply": r})
		return r, true
	case <-cancel:
		o.mu.Lock()
		delete(o.asked, id)
		o.mu.Unlock()
		return "", false
	}
}

// permissionReply answers a pending permission: true, or 404 for one not
// pending, as opencode 1.18.21 does.
func (o *opencodeServer) permissionReply(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Reply string `json:"reply"`
	}
	if json.NewDecoder(r.Body).Decode(&body) != nil || (body.Reply != "once" && body.Reply != "always" && body.Reply != "reject") {
		http.Error(w, `{"name":"BadRequest","data":{"message":"Missing key\n  at [\"reply\"]","kind":"Payload"}}`, http.StatusBadRequest)
		return
	}
	id := r.PathValue("id")
	o.mu.Lock()
	ch := o.asked[id]
	delete(o.asked, id)
	o.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if ch == nil {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprintf(w, `{"_tag":"PermissionNotFoundError","requestID":%q,"message":"Permission request not found: %s"}`, id, id)
		return
	}
	ch <- body.Reply
	fmt.Fprint(w, "true")
}

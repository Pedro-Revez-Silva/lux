package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// runCompact compacts the conversation: its summary names every prompt so
// far, as a real agent's summary keeps what the user asked. remote: as
// Codex compacts for OpenAI's provider, with no summary text. The
// transcript is kept whole, so `history` still answers from it.
func (a *agent) runCompact(remote bool) {
	if a.compact == nil {
		a.say("compact: this protocol reports no compaction")
		return
	}
	if remote {
		a.compact("")
		return
	}
	var prompts []string
	for _, e := range a.history() {
		if e.Role == "user" && !strings.HasPrefix(strings.TrimSpace(e.Text), "compact") {
			prompts = append(prompts, e.Text)
		}
	}
	a.compact("Summary:\n1. Primary Request and Intent:\n   " + strings.Join(prompts, "\n   "))
}

// claudeCompact writes what claude 2.1.207 writes for a compaction
// (internal/adapter/testdata/claude-2.1.207-compact.jsonl): compact_boundary,
// the summary as a synthetic user line whose uuid is the boundary's
// preserved_segment.anchor_uuid, then the /compact command's output.
func claudeCompact(a *agent) func(summary string) {
	return func(summary string) {
		anchor := fmt.Sprintf("anchor-%d", time.Now().UnixNano())
		a.send(map[string]any{"type": "system", "subtype": "compact_boundary", "session_id": a.session,
			"uuid": fmt.Sprintf("boundary-%d", time.Now().UnixNano()),
			"compact_metadata": map[string]any{"trigger": "manual", "pre_tokens": 36663, "post_tokens": 972,
				"preserved_segment": map[string]string{"anchor_uuid": anchor}}})
		a.send(map[string]any{"type": "user", "session_id": a.session, "parent_tool_use_id": nil, "uuid": anchor,
			"isReplay": false, "isSynthetic": true, "message": map[string]any{"role": "user",
				"content": "This session is being continued from a previous conversation that ran out of context. " +
					"The summary below covers the earlier portion of the conversation.\n\n" + summary}})
		a.send(map[string]any{"type": "user", "session_id": a.session, "parent_tool_use_id": nil, "isReplay": true,
			"uuid":    fmt.Sprintf("replay-%d", time.Now().UnixNano()),
			"message": map[string]any{"role": "user", "content": "<local-command-stdout>Compacted </local-command-stdout>"}})
	}
}

// codexRollout is the fake thread's rollout file, as thread.path names it.
func (a *agent) codexRollout() string {
	return filepath.Join(transcriptDir(), a.session+".rollout.jsonl")
}

// codexCompact reports a compaction as codex 0.145.0 does
// (internal/adapter/testdata/codex-0.145.0-*.jsonl): a contextCompaction
// item, started, then completed once the rollout has its compacted entry
// (the summary, for a local compaction) and context_compacted event.
func codexCompact(a *agent, turn func() string) func(summary string) {
	return func(summary string) {
		item := map[string]any{"type": "contextCompaction", "id": fmt.Sprintf("cc-%d", time.Now().UnixNano())}
		params := func() map[string]any {
			return map[string]any{"threadId": a.session, "turnId": turn(), "item": item}
		}
		a.send(map[string]any{"method": "item/started", "params": params()})
		now := time.Now().UTC().Format(time.RFC3339Nano)
		for _, e := range []map[string]any{
			{"timestamp": now, "type": "compacted", "payload": map[string]any{"message": summary, "replacement_history": []any{}}},
			{"timestamp": now, "type": "event_msg", "payload": map[string]string{"type": "context_compacted"}},
		} {
			b, _ := json.Marshal(e)
			_ = appendFile(a.codexRollout(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, string(b)+"\n")
		}
		a.send(map[string]any{"method": "item/completed", "params": params()})
	}
}

// compact stores a compaction as opencode 1.18.35 does
// (internal/adapter/testdata/opencode-1.18.35-compaction.json): a user
// message with a compaction part, the summary as an assistant message with
// info.summary true answering it, then session.compacted on the bus.
func (o *opencodeServer) compact(summary string) {
	user := o.messageID()
	now := time.Now().UnixMilli()
	o.storeWhole(map[string]any{"id": user, "role": "user", "sessionID": o.a.session, "summary": map[string]any{"diffs": []any{}},
		"time": map[string]any{"created": now}},
		[]any{map[string]any{"type": "compaction", "auto": false, "messageID": user, "sessionID": o.a.session}})
	id := o.messageID()
	o.storeWhole(map[string]any{"id": id, "role": "assistant", "parentID": user, "sessionID": o.a.session, "mode": "compaction",
		"agent": "compaction", "summary": true, "finish": "stop", "time": map[string]any{"created": now, "completed": now}},
		[]any{map[string]any{"type": "step-start"}, map[string]any{"type": "text", "text": summary}, map[string]any{"type": "step-finish", "reason": "stop"}})
	o.publish("session.compacted", map[string]any{"sessionID": o.a.session})
}

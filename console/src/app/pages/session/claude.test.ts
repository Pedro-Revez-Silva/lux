import { expect, test } from "bun:test";
import type { OutputRecord } from "../../../api/index.ts";
import { fixture } from "./fixtures.ts";
import { agentOf, briefOf, sessionFromRecords, subagentTree } from "./projection.ts";
import type { MarkerItem, MessageItem, PlanItem, ThoughtItem, ToolItem } from "./types.ts";

const TASK = "toolu_01GJFr24BVyX5H76UdYzZuQa";

test("claude-code: an Agent tool_use is a subagent; lines with its parent_tool_use_id are its turns", () => {
  const s = sessionFromRecords("claude-code", fixture("claude-2.1.207-agent-subagent.jsonl"));
  expect([s.subagents, s.model, s.sessionId]).toEqual(["reported", "claude-haiku-4.5", "0e212c6c-b0a9-4e2d-b591-6e6608b3e932"]);
  const root = s.threads[s.root]!;
  expect(root.items.map((i) => (i.kind === "marker" ? `marker:${i.type}` : i.kind))).toEqual(["thought", "tool", "message", "marker:turn"]);
  expect((root.items[0] as ThoughtItem).text.startsWith("The user is asking me to use the Agent tool")).toBe(true);

  const call = root.items[1] as ToolItem;
  expect([call.name, call.title, call.status, call.child, call.cursor]).toEqual(["Agent", "Read /etc/hosts first line", "completed", TASK, "1.32"]);
  expect(call.output).toBe("The first line of `/etc/hosts` is:\n\n```\n##\n```");
  expect((root.items[2] as MessageItem).text).toBe("The first line of `/etc/hosts` is `##` (a comment line with two hash marks).");
  expect((root.items[3] as MarkerItem).text).toBe("turn ended · 2.8s · 1 tool call · 1 subagent");

  const child = s.threads[TASK]!;
  expect([child.title, child.status, child.parent, child.model, child.tokens]).toEqual(["Read /etc/hosts first line", "completed", s.root, "claude-haiku-4.5", 15143]);
  expect(agentOf(s, child)).toBe("Explore");
  expect(briefOf(s, child)).toEqual({ text: "Read the file /etc/hosts and report only its first line. Do not read anything else.", cut: false });
  const read = child.items[0] as ToolItem;
  expect(child.items).toHaveLength(1);
  expect([read.name, read.status, read.output, read.cursor]).toEqual(["Read", "completed", "1\t##", "1.36"]);
  expect(read.input).toEqual({ file_path: "/etc/hosts", limit: 1 });
  expect(subagentTree(s).map((x) => x.thread.id)).toEqual([TASK]);
});

test("claude-code: a compaction is one marker with its summary; the summary line is not an input", () => {
  const s = sessionFromRecords("claude-code", fixture("claude-2.1.207-compact.jsonl"));
  const root = s.threads[s.root]!;
  expect(root.items).toHaveLength(1);
  const m = root.items[0] as MarkerItem;
  expect([m.type, m.trigger, m.tokens]).toEqual(["compacted", "manual", { pre: 36663, post: 972 }]);
  expect(m.summary?.startsWith("This session is being continued from a previous conversation")).toBe(true);
  expect(s.compactions).toBe(1);
});

// Documented shape (Claude Code stream-json with --include-partial-messages,
// as claude.go relays it): deltas stream a reply, the assistant line ends it.
test("claude-code: streamed deltas show a caret until the whole message replaces them; TodoWrite is the plan", () => {
  let seq = 0;
  const ev = (type: string, data: Record<string, unknown>): OutputRecord => ({ cursor: `1.${++seq}`, epoch: 1, seq, t: 1000 * seq, ch: "event", event: { type, data } });
  const delta = (text: string) => ev("claude.stream_event", { type: "stream_event", event: { type: "content_block_delta", index: 0, delta: { type: "text_delta", text } }, parent_tool_use_id: null });
  const recs = [
    ev("claude.assistant", { type: "assistant", message: { content: [{ type: "tool_use", id: "t1", name: "TodoWrite", input: { todos: [{ content: "Read", status: "completed" }, { content: "Fix", status: "in_progress" }] } }] }, parent_tool_use_id: null }),
    delta("Fixed "),
    delta("it"),
  ];
  const mid = sessionFromRecords("claude-code", recs);
  const items = mid.threads[mid.root]!.items;
  expect((items[0] as PlanItem).steps.map((x) => x.status)).toEqual(["done", "now"]);
  expect([(items[1] as MessageItem).text, (items[1] as MessageItem).streaming]).toEqual(["Fixed it", true]);
  const done = sessionFromRecords("claude-code", [...recs, ev("claude.assistant", { type: "assistant", message: { content: [{ type: "text", text: "Fixed it." }] }, parent_tool_use_id: null })]);
  const after = done.threads[done.root]!.items;
  expect(after).toHaveLength(2);
  expect([(after[1] as MessageItem).text, (after[1] as MessageItem).streaming]).toEqual(["Fixed it.", false]);
  const thought = (thinking: string) => ev("claude.stream_event", { event: { type: "content_block_delta", index: 0, delta: { type: "thinking_delta", thinking } }, parent_tool_use_id: null });
  const steps = sessionFromRecords("claude-code", [
    thought("plan A"),
    ev("claude.assistant", { message: { content: [{ type: "thinking", thinking: "plan A" }, { type: "tool_use", id: "shell", name: "Bash", input: { command: "ls" } }] } }),
    ev("claude.user", { message: { content: [{ type: "tool_result", tool_use_id: "shell", content: "ok" }] } }),
    thought("plan B"),
    ev("claude.assistant", { message: { content: [{ type: "thinking", thinking: "plan B" }, { type: "text", text: "done" }] } }),
  ]).threads.root!.items;
  expect(steps.map((i) => i.kind)).toEqual(["thought", "tool", "thought", "message"]);
  expect(steps.filter((i) => i.kind === "thought").map((i) => i.text)).toEqual(["plan A", "plan B"]);
  expect((steps[1] as ToolItem).status).toBe("completed");
});

import { expect, test } from "bun:test";
import type { OutputRecord } from "../../../api/index.ts";
import { agentOf, briefOf, sessionFromRecords, SessionProjection, subagentTree } from "./projection.ts";
import { fixture } from "./fixtures.ts";
import { subagentCounts } from "./view.ts";
import type { MessageItem, NoteItem, ToolItem } from "./types.ts";

const CHILD = "ses_ec59a5ecfffeJV2xJIrK0QFXi1";

test("opencode: a task call's child session becomes a subagent with its own tool calls and messages", () => {
  const s = sessionFromRecords("opencode", fixture("opencode-1.18.21-task-child.jsonl"));
  expect(s.subagents).toBe("reported");
  const root = s.threads[s.root]!;
  const task = root.items.find((i) => i.kind === "tool" && i.name === "task") as ToolItem;
  expect([task.child, task.status, task.title, task.cursor]).toEqual([CHILD, "completed", "Nested subagent task", "1.11"]);
  expect(root.children).toEqual([CHILD]);

  const child = s.threads[CHILD]!;
  expect([child.title, child.agent, child.model, child.status, child.parent]).toEqual(["Nested subagent task", "general", "opencode/big-pickle", "completed", s.root]);
  expect(child.startedAt).toBe(1791981101360);
  expect(child.endedAt).toBe(1791981134425);
  const tools = child.items.filter((i): i is ToolItem => i.kind === "tool");
  expect(tools.map((t) => [t.name, t.status, t.title, t.output?.split("\n")[0]])).toEqual([
    ["bash", "completed", "wc -l notes.txt", "       3 notes.txt"],
    ["read", "completed", "private/var/tmp/impl-sess-lux/oc2/notes.txt", "<path>/private/var/tmp/impl-sess-lux/oc2/notes.txt</path>"],
  ]);
  expect((tools[0]!.input as { command: string }).command).toBe("wc -l notes.txt");
  expect([tools[0]!.t, tools[0]!.end]).toEqual([1791981113270, 1791981113360]);
  const msgs = child.items.filter((i): i is MessageItem => i.kind === "message");
  expect(msgs).toHaveLength(2);
  expect(msgs[1]!.text.endsWith("(b) First line of `notes.txt`: `alpha`")).toBe(true);
  expect(msgs.every((m) => !m.streaming)).toBe(true);
  expect(child.tokens).toBe(5486 + 342 + 1792 + 533 + 183 + 7168);

  // The brief is the task call's prompt, from the parent's ACP tool call.
  expect(briefOf(s, child)?.text.startsWith("You are a general subagent.")).toBe(true);
  expect(briefOf(s, child)?.cut).toBe(false);
  expect(subagentTree(s).map((x) => [x.thread.id, x.depth])).toEqual([[CHILD, 0]]);
  // The child's tool calls are not the root's.
  expect(root.items.filter((i) => i.kind === "tool").map((i) => (i as ToolItem).name)).toEqual(["task", "bash"]);
});

// The capture has no lux.session (it is the bus relay alone): the Run's
// session id is given the way the adapter writes it first.
const session = (id: string): OutputRecord => ({ cursor: "1.0", epoch: 1, seq: 0, t: 1791870692912, ch: "event", event: { type: "lux.session", data: { sessionId: id } } });

test("opencode: a subagent's permission lux could not answer is a warning in that subagent", () => {
  const s = sessionFromRecords("opencode", [session("ses_ecc2f11cfffeaAFEEcNfwHJCeC"), ...fixture("opencode-1.18.21-children-permission.jsonl")]);
  const child = s.threads["ses_ecc2f05bcffeO26eP2Q6hCnlL5"]!;
  expect([child.title, child.agent, child.model]).toEqual(["Read first line of /etc/hosts", "general", "llmproxy-anthropic/claude-haiku-4.5"]);
  const note = child.items.find((i) => i.kind === "note") as NoteItem;
  expect([note.tone, note.text]).toEqual(["warning", "lux could not answer: external_directory /etc/*"]);
  expect(note.detail).toContain("can't assign requested address");
  // The task call was not in this capture's ACP side: its row is made from opencode.task.
  const task = s.threads[s.root]!.items.find((i) => i.kind === "tool") as ToolItem;
  expect([task.name, task.child, task.status]).toEqual(["task", child.id, "completed"]);
  // The empty first message (tool calls only) adds no turn.
  expect(child.items.filter((i) => i.kind === "message").map((m) => (m as MessageItem).text)).toEqual(["The first line of /etc/hosts is:\n\n```\n##\n```"]);
});

// Hand-built from the documented shape (docs/adapters.md "OpenCode
// subagents"; lux's own TestOpenCodeChildRelayNested): no real capture has a
// grandchild, the recorded child had no task tool.
test("opencode: a nested task call's grandchild hangs under the child, and briefs come cut", () => {
  let seq = 0;
  const ev = (type: string, data: Record<string, unknown>): OutputRecord => ({ cursor: `1.${++seq}`, epoch: 1, seq, t: 1000 + seq, ch: "event", event: { type, data } });
  const cut = '{"description":"Count lines","prompt":"Run wc -l on every file under internal/ and report the total, then';
  const s = sessionFromRecords("opencode", [
    ev("lux.session", { sessionId: "ses_run" }),
    ev("acp.tool_call", { sessionUpdate: "tool_call", toolCallId: "c1", title: "task", status: "pending", rawInput: {} }),
    ev("opencode.task", { sessionID: "ses_run", callID: "c1", childSessionID: "ses_kid", status: "running" }),
    ev("opencode.session", { id: "ses_kid", parentID: "ses_run", title: "Review (@review subagent)", createdAt: 1001 }),
    ev("opencode.task", { sessionID: "ses_kid", callID: "c2", childSessionID: "ses_grandkid", status: "running" }),
    ev("opencode.part", { sessionID: "ses_kid", messageID: "m", callID: "c2", tool: "task", status: "running", input: cut, truncated: true, time: { start: 1005 } }),
    ev("opencode.part", { sessionID: "ses_grandkid", messageID: "m2", callID: "c3", tool: "bash", status: "running", input: '{"command":"wc -l"}', time: { start: 1006 } }),
    ev("opencode.session", { id: "ses_grandkid", parentID: "ses_kid", title: "Count lines (@explore subagent)", createdAt: 1004 }),
    ev("opencode.task", { sessionID: "ses_kid", callID: "c2", childSessionID: "ses_grandkid", status: "error" }),
  ]);
  expect(subagentTree(s).map((x) => [x.thread.id, x.depth])).toEqual([["ses_kid", 0], ["ses_grandkid", 1]]);
  const kid = s.threads.ses_kid!;
  const call = kid.items.find((i) => i.id === "tool:c2") as ToolItem;
  expect([call.child, call.status, call.truncated]).toEqual(["ses_grandkid", "error", true]);
  const grand = s.threads.ses_grandkid!;
  expect([grand.status, grand.parent, grand.title, agentOf(s, grand)]).toEqual(["error", "ses_kid", "Count lines", "explore"]);
  expect(briefOf(s, grand)).toEqual({ text: "Run wc -l on every file under internal/ and report the total, then", cut: true });
  expect(grand.items.map((i) => (i as ToolItem).name)).toEqual(["bash"]);
});

test("opencode: a parent named only by an orphan task is neutral until adopted", () => {
  let seq = 0;
  const ev = (type: string, data: Record<string, unknown>): OutputRecord => ({ cursor: `1.${++seq}`, epoch: 1, seq, t: 1000 + seq, ch: "event", event: { type, data } });
  const p = new SessionProjection("opencode");
  p.fold([ev("lux.session", { sessionId: "run" }), ev("opencode.task", { sessionID: "parent", callID: "c", childSessionID: "child", status: "running" })]);
  let s = p.snapshot();
  expect([s.threads.parent!.status, subagentCounts(s).running]).toEqual(["idle", 1]);
  p.fold([ev("opencode.session", { id: "parent", parentID: "run", title: "Parent" }), ev("opencode.task", { sessionID: "parent", callID: "c", childSessionID: "child", status: "completed" })]);
  s = p.snapshot();
  expect(subagentTree(s).map((x) => [x.thread.id, x.depth])).toEqual([["parent", 0], ["child", 1]]);
  expect(subagentCounts(s)).toEqual({ total: 2, running: 0 });
  // A child's session announced before its parent's (adopt): the parent is a placeholder, not running.
  const q = new SessionProjection("opencode");
  q.fold([ev("lux.session", { sessionId: "run" }), ev("opencode.session", { id: "kid", parentID: "dad", title: "Kid" }), ev("opencode.task", { sessionID: "dad", callID: "d", childSessionID: "kid", status: "completed" })]);
  expect(q.snapshot().threads.dad!.status).toBe("idle");
  expect(subagentCounts(q.snapshot())).toEqual({ total: 2, running: 0 });
});

test("opencode: message truncation is kept; the Run's own permission is not duplicated", () => {
  const s = sessionFromRecords("opencode", [session("run"), { cursor: "1.1", epoch: 1, seq: 1, t: 1, ch: "event", event: { type: "opencode.message", data: { sessionID: "kid", messageID: "m", text: "cut", truncated: true } } }, { cursor: "1.2", epoch: 1, seq: 2, t: 2, ch: "event", event: { type: "opencode.permission", data: { sessionID: "run", answered: "allow", permission: "bash" } } }]);
  expect((s.threads.kid!.items[0] as MessageItem).truncated).toBe(true);
  expect(s.threads.root!.items).toHaveLength(0);
});

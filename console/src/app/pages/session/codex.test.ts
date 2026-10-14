import { expect, test } from "bun:test";
import type { OutputRecord } from "../../../api/index.ts";
import { fixture } from "./fixtures.ts";
import { sessionFromRecords, subagentTree } from "./projection.ts";
import type { MarkerItem, MessageItem, PlanItem, ThoughtItem, ToolItem } from "./types.ts";

const CHILD = "01a133b0-0aa5-7b93-b598-a603ff0d58a5";

test("codex: a subAgentActivity names the child thread; its notifications are the subagent's turns", () => {
  const s = sessionFromRecords("codex", fixture("codex-0.144.1-subagent.jsonl"));
  expect(s.subagents).toBe("reported");
  const root = s.threads[s.root]!;
  expect(root.items.map((i) => (i.kind === "marker" ? `marker:${i.type}` : i.kind))).toEqual(["input", "message", "tool", "tool"]);
  expect((root.items[1] as MessageItem).text).toBe("I’ll delegate the read and wait for the result.\n");
  const spawn = root.items[2] as ToolItem;
  expect([spawn.name, spawn.title, spawn.child, spawn.status, spawn.cursor]).toEqual(["spawn_agent", "/root/read_hosts", CHILD, "completed", "1.26"]);
  const wait = root.items[3] as ToolItem;
  expect([wait.name, wait.status, wait.t, wait.end]).toEqual(["wait", "completed", 1791868539831, 1791868544372]);

  const child = s.threads[CHILD]!;
  expect([child.title, child.parent, child.status, child.tokens]).toEqual(["read_hosts", s.root, "completed", 16833]);
  expect(child.items.map((i) => (i.kind === "marker" ? `marker:${i.type}` : i.kind))).toEqual(["tool", "message", "marker:turn"]);
  const cmd = child.items[0] as ToolItem;
  expect([cmd.name, cmd.title, cmd.status, cmd.output, cmd.cursor]).toEqual(["shell", "/bin/zsh -lc 'head -n 1 /etc/hosts'", "completed", "##\n", "1.32"]);
  expect((child.items[1] as MessageItem).text).toBe("##");
  // The child's turn end is the child's: the root's turn is still in its wait.
  expect(root.items.some((i) => i.kind === "marker")).toBe(false);
  expect(subagentTree(s).map((x) => x.thread.id)).toEqual([CHILD]);
});

test("codex: an agent message streams by delta until its item completes", () => {
  const recs = fixture("codex-0.144.1-subagent.jsonl");
  const mid = sessionFromRecords("codex", recs.slice(0, 18));
  const m = mid.threads[mid.root]!.items.at(-1) as MessageItem;
  expect([m.text, m.streaming]).toEqual(["I’ll delegate the read and wait", true]);
});

// Documented shape (codex app-server generate-json-schema, 0.144.1): a
// fileChange item, reasoning summary deltas, a turn plan and a root turn end.
test("codex: file changes carry their diff; reasoning folds into a thought; the plan updates in place", () => {
  let seq = 0;
  const ev = (type: string, data: Record<string, unknown>): OutputRecord => ({ cursor: `1.${++seq}`, epoch: 1, seq, t: 1000 * seq, ch: "event", event: { type, data } });
  const th = "thr_root";
  const s = sessionFromRecords("codex", [
    ev("lux.session", { sessionId: th }),
    ev("codex.item/reasoning/summaryTextDelta", { threadId: th, itemId: "r1", delta: "Check the " }),
    ev("codex.item/reasoning/summaryTextDelta", { threadId: th, itemId: "r1", delta: "retry loop." }),
    ev("codex.turn/plan/updated", { threadId: th, turnId: "t1", plan: [{ step: "Read", status: "completed" }, { step: "Edit", status: "inProgress" }] }),
    ev("codex.item/started", { threadId: th, item: { type: "fileChange", id: "f1", status: "inProgress", changes: [{ path: "retry.go", kind: { type: "update" }, diff: "-a\n+b" }] } }),
    ev("codex.item/completed", { threadId: th, item: { type: "fileChange", id: "f1", status: "completed", changes: [{ path: "retry.go", kind: { type: "update" }, diff: "-a\n+b" }] } }),
    ev("codex.turn/plan/updated", { threadId: th, turnId: "t1", plan: [{ step: "Read", status: "completed" }, { step: "Edit", status: "completed" }] }),
    ev("codex.turn/completed", { threadId: th, turn: { id: "t1", status: "completed" } }),
    ev("codex.turn_end", { status: "completed" }),
  ]);
  const items = s.threads[s.root]!.items;
  expect(items.map((i) => i.kind)).toEqual(["thought", "plan", "tool", "marker"]);
  expect((items[0] as ThoughtItem).text).toBe("Check the retry loop.");
  expect((items[1] as PlanItem).steps.map((x) => x.status)).toEqual(["done", "done"]);
  const patch = items[2] as ToolItem;
  expect([patch.name, patch.title, patch.status, patch.diff]).toEqual(["patch", "retry.go", "completed", "@@ retry.go\n-a\n+b"]);
  expect((items[3] as MarkerItem).text).toBe("turn ended · 6s · 1 tool call");
});

test("codex: a child turn reopens its spawn row; nonzero exit fails; new turn gets a new plan", () => {
  let seq = 0;
  const ev = (type: string, data: Record<string, unknown>): OutputRecord => ({ cursor: `1.${++seq}`, epoch: 1, seq, t: 1000 * seq, ch: "event", event: { type, data } });
  const th = "root";
  const s = sessionFromRecords("codex", [
    ev("lux.session", { sessionId: th }),
    ev("codex.item/started", { threadId: th, item: { type: "subAgentActivity", id: "spawn", kind: "started", agentThreadId: "child", agentPath: "/root/child" } }),
    ev("codex.turn/completed", { threadId: "child", turn: { status: "completed" } }),
    ev("codex.turn/started", { threadId: "child" }),
    ev("codex.item/completed", { threadId: th, item: { type: "commandExecution", id: "cmd", command: "false", status: "completed", exitCode: 2 } }),
    ev("codex.turn/plan/updated", { threadId: th, turnId: "one", plan: [{ step: "one", status: "completed" }] }),
    ev("codex.turn/completed", { threadId: th, turn: { status: "completed" } }),
    ev("codex.turn/plan/updated", { threadId: th, turnId: "two", plan: [{ step: "two", status: "inProgress" }] }),
  ]);
  const root = s.threads.root!.items;
  expect([(root[0] as ToolItem).status, (root[0] as ToolItem).end, s.threads.child!.status]).toEqual(["running", undefined, "running"]);
  expect([(root[1] as ToolItem).status, (root[1] as ToolItem).error]).toEqual(["error", "exit 2"]);
  expect(root.filter((i): i is PlanItem => i.kind === "plan").map((p) => p.steps[0]!.text)).toEqual(["one", "two"]);
});

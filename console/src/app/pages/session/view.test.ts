import { expect, test } from "bun:test";
import { fixture } from "./fixtures.ts";
import { sessionFromRecords } from "./projection.ts";
import type { ToolItem } from "./types.ts";
import { agentColors, briefFor, hasSession, lastActivity, noSubagentsText, toolCounts, toolCutNote, toolSummary, usageText, visible } from "./view.ts";

test("only agent adapters get a Session tab", () => {
  expect(["acp", "opencode", "claude-code", "codex", "generic", "", undefined].map(hasSession)).toEqual([true, true, true, true, false, false, false]);
});

test("a folded call's summary is its title, else what its input says it did", () => {
  const s = sessionFromRecords("claude-code", fixture("claude-2.1.207-agent-subagent.jsonl"));
  const read = s.threads["toolu_01GJFr24BVyX5H76UdYzZuQa"]!.items[0] as ToolItem;
  expect(toolSummary(read)).toBe("/etc/hosts");
  expect(lastActivity(s.threads["toolu_01GJFr24BVyX5H76UdYzZuQa"]!)).toBe("Read /etc/hosts");
  expect(lastActivity(s.threads.root!)).toBe("The first line of /etc/hosts is ## (a comment line with two hash marks).");
  const oc = sessionFromRecords("opencode", fixture("opencode-1.18.21-task-child.jsonl"));
  expect(lastActivity(oc.threads["ses_ec59a5ecfffeJV2xJIrK0QFXi1"]!)).toMatch(/directly and read the file: \(a\) wc -l notes\.txt output: 3/);
});

test("Messages hides tool calls and thoughts; Everything shows all", () => {
  const s = sessionFromRecords("acp", fixture("acp-opencode-1.18.21-todo-task.jsonl"));
  const items = s.threads.root!.items;
  expect(visible(items, "messages").map((i) => i.kind)).toEqual(["input", "message", "marker"]);
  expect(visible(items, "all")).toBe(items);
  expect(toolCounts(s)).toEqual([
    { name: "task", count: 1 },
    { name: "bash", count: 1 },
  ]);
  expect(noSubagentsText("acp")).toBe("The acp adapter doesn't report subagents: ACP shows only the Run's own session.");
});

test("an OpenCode child's cut tool input and brief say lux cut them; its usage is summed", () => {
  const s = sessionFromRecords("opencode", fixture("opencode-1.18.21-task-child.jsonl"));
  const child = s.threads["ses_ec59a5ecfffeJV2xJIrK0QFXi1"]!;
  expect(usageText(child)).toBe("15.5k tokens");
  expect(briefFor(s, child)?.cutNote).toBeUndefined();
  const t = { ...(child.items[0] as ToolItem), truncated: true };
  expect(toolCutNote("opencode", child, t)).toBe("cut by lux: input at 300 bytes, output and error at 2 KiB");
  // One agent type, one colour; the slot map keeps it across renders.
  const slots = new Map<string, number>();
  expect([...agentColors(s, slots).values()]).toEqual(["var(--agent-1)"]);
  expect([...agentColors(s, slots).values()]).toEqual(["var(--agent-1)"]);
});

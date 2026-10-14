import { expect, test } from "bun:test";
import { fixture } from "./fixtures.ts";
import { sessionFromRecords, SessionProjection } from "./projection.ts";
import type { InputItem, MessageItem, NoteItem, OutputRecord, PlanItem, SessionItem, ThoughtItem, ToolItem } from "./types.ts";

const kinds = (items: SessionItem[]) => items.map((i) => (i.kind === "marker" ? `marker:${i.type}` : i.kind));
const record = (epoch: number, seq: number, type: string, data: Record<string, unknown> = {}): OutputRecord => ({ epoch, seq, cursor: `${epoch}.${seq}`, t: seq * 1000, ch: "event", event: { type, data } });
const chunk = (text: string, messageId?: string) => ({ sessionUpdate: "agent_message_chunk", content: { type: "text", text }, ...(messageId ? { messageId } : {}) });

// Plain chunks use lux-fake's session/update shape, without messageId.
test("plain ACP keeps turns and thoughts separated by tools", () => {
  const events: [string, Record<string, unknown>][] = [
    ["lux.input", { requestId: "prompt", text: "one" }],
    ["acp.agent_message_chunk", chunk("first ")], ["acp.agent_message_chunk", chunk("reply")],
    ["acp.turn_end", {}], ["lux.input", { requestId: "two", text: "two" }],
    ["acp.agent_message_chunk", chunk("second reply")], ["acp.turn_end", {}],
    ["acp.agent_thought_chunk", { ...chunk("plan A"), sessionUpdate: "agent_thought_chunk" }],
    ["acp.tool_call", { toolCallId: "call", title: "shell", status: "completed" }],
    ["acp.agent_thought_chunk", { ...chunk("plan B"), sessionUpdate: "agent_thought_chunk" }],
    ["acp.agent_thought_chunk", { ...chunk(" more"), sessionUpdate: "agent_thought_chunk" }],
  ];
  const items = sessionFromRecords("acp", events.map(([type, data], i) => record(1, i + 1, type, data))).threads.root!.items;
  expect(kinds(items)).toEqual(["input", "message", "marker:turn", "input", "message", "marker:turn", "thought", "tool", "thought"]);
  expect(items.filter((i) => i.kind === "message").map((i) => i.text)).toEqual(["first reply", "second reply"]);
  expect(items.filter((i) => i.kind === "thought").map((i) => i.text)).toEqual(["plan A", "plan B more"]);
  // Settled by idle with nothing after it: the next chunk is a new message.
  const after = sessionFromRecords("acp", [record(1, 1, "lux.input", { requestId: "prompt", text: "one" }), record(1, 2, "acp.agent_message_chunk", chunk("first")), record(1, 3, "lux.activity", { activity: "idle" }), record(1, 4, "acp.agent_message_chunk", chunk("unsolicited"))]).threads.root!.items;
  expect(after.filter((i) => i.kind === "message").map((i) => i.text)).toEqual(["first", "unsolicited"]);
});

test("ACP initial history is retained; later epoch replay stops at lux.session", () => {
  const p = new SessionProjection("opencode");
  p.fold([record(1, 1, "acp.agent_message_chunk", chunk("history", "m")), record(1, 2, "acp.tool_call", { toolCallId: "t", title: "shell", status: "completed" }), record(1, 3, "lux.session", { sessionId: "s" })]);
  p.fold([record(2, 1, "acp.agent_message_chunk", chunk("history", "m")), record(2, 2, "acp.tool_call", { toolCallId: "t", title: "shell", status: "in_progress" }), record(2, 3, "lux.warning", { message: "resuming" }), record(2, 4, "opencode.message", { sessionID: "child", role: "assistant", id: "child-message", text: "child history" }), record(2, 5, "lux.session", { sessionId: "s" }), record(2, 6, "lux.input", { requestId: "next", text: "next" }), record(2, 7, "acp.agent_message_chunk", chunk("genuine", "m2"))]);
  const s = p.snapshot();
  expect(s.threads.root!.items.filter((i) => i.kind === "message").map((i) => i.text)).toEqual(["history", "genuine"]);
  expect(s.threads.root!.items.find((i) => i.kind === "tool")?.status).toBe("completed");
  expect(s.threads.root!.items.some((i) => i.kind === "note" && i.text === "resuming")).toBe(true);
  expect(s.threads.child!.items.some((i) => i.kind === "message" && i.text === "child history")).toBe(true);
  p.fold([record(3, 1, "lux.session", { sessionId: "s" }), record(3, 2, "acp.agent_message_chunk", chunk("no replay", "m3"))]);
  expect(p.snapshot().threads.root!.items.at(-1)?.kind).toBe("message");
  expect((p.snapshot().threads.root!.items.at(-1) as MessageItem).text).toBe("no replay");
});

test("ACP permission wording follows selected option kind", () => {
  for (const kind of ["allow_once", "allow_always", "reject_once", "reject_always"]) {
    const items = sessionFromRecords("acp", [record(1, 1, "acp.permission", { selected: "choice", request: { toolCall: { title: "shell" }, options: [{ optionId: "choice", kind }] } })]).threads.root!.items;
    const note = items[0] as NoteItem;
    expect(note.tone).toBe(kind.startsWith("allow_") ? "permission" : "warning");
    expect(note.text).toBe(`lux ${kind.startsWith("allow_") ? "allowed" : "had to reject"} shell`);
  }
});

test("acp: an OpenCode turn over plain ACP reads as input, thought, plan, tools, reply, turn end", () => {
  const s = sessionFromRecords("acp", fixture("acp-opencode-1.18.21-todo-task.jsonl"));
  const root = s.threads[s.root]!;
  expect(kinds(root.items)).toEqual(["input", "thought", "plan", "tool", "thought", "tool", "message", "marker:turn"]);

  const input = root.items[0] as InputItem;
  expect(input.steer).toBe(false);
  expect(input.phase).toBe("accepted");
  expect(input.text.startsWith("First write a todo list")).toBe(true);

  expect((root.items[1] as ThoughtItem).text).toBe("Let me follow the instructions. First write a todo list with two items, then use task tool.");

  // The second todowrite ticks off the first plan; it does not add another.
  expect((root.items[2] as PlanItem).steps).toEqual([
    { text: "Run 'wc -l notes.txt' via a general subagent and get the count", status: "done" },
    { text: "Run 'ls' and mark todos done", status: "done" },
  ]);

  const task = root.items[3] as ToolItem;
  expect([task.name, task.title, task.status, task.cursor]).toEqual(["task", "Run wc -l notes.txt", "completed", "1.15"]);
  const ls = root.items[5] as ToolItem;
  expect([ls.name, ls.title, ls.status, ls.output]).toEqual(["bash", "ls", "completed", "notes.txt\n"]);
  expect((ls.input as { command: string }).command).toBe("ls");

  const reply = root.items[6] as MessageItem;
  expect(reply.text).toBe("`notes.txt` has 3 lines, and `ls` shows only `notes.txt` in the directory.");
  expect(reply.streaming).toBe(false);
  const end = root.items[7]!;
  expect(end.kind === "marker" && end.text).toBe("turn ended · 10.5s · 2 tool calls");

  expect(s.subagents).toBe("unsupported");
  expect(s.sessionId).toBe("ses_ec59d54c8ffeodNJkSlnW18KML");
  expect(s.activity).toBe("idle");
  expect([s.turns, s.records, s.cursor]).toEqual([1, 46, "1.46"]);
});

test("folding every fixture in batches gives the same session as folding at once", () => {
  const fixtures = [
    ["acp", "acp-opencode-1.18.21-todo-task.jsonl"],
    ["opencode", "opencode-1.18.21-task-child.jsonl"],
    ["opencode", "opencode-1.18.21-children-permission.jsonl"],
    ["claude-code", "claude-2.1.207-agent-subagent.jsonl"],
    ["claude-code", "claude-2.1.207-compact.jsonl"],
    ["codex", "codex-0.144.1-subagent.jsonl"],
  ] as const;
  for (const [adapter, file] of fixtures) {
    const recs = fixture(file);
    const whole = sessionFromRecords(adapter, recs);
    for (const n of [1, 3, 7]) {
      const p = new SessionProjection(adapter);
      for (let i = 0; i < recs.length; i += n) p.fold(recs.slice(i, i + n)).snapshot();
      const inBatches = p.snapshot();
      expect(inBatches.threads).toEqual(whole.threads);
      expect([inBatches.cursor, inBatches.turns]).toEqual([whole.cursor, whole.turns]);
    }
  }
});

test("a reply streams with a caret until the next item", () => {
  const recs = fixture("acp-opencode-1.18.21-todo-task.jsonl");
  const p = new SessionProjection("acp");
  const mid = p.fold(recs.slice(0, 37)).snapshot();
  const last = mid.threads[mid.root]!.items.at(-1) as MessageItem;
  expect([last.kind, last.streaming, last.text]).toEqual(["message", true, "`notes.txt` has 3 lines, and `"]);
});

test("a fold that changes nothing keeps every thread object", () => {
  const recs = fixture("acp-opencode-1.18.21-todo-task.jsonl");
  const p = new SessionProjection("acp");
  const a = p.fold(recs).snapshot();
  const b = p.fold([]).snapshot();
  expect(b.threads[b.root]).toBe(a.threads[a.root]!);
  expect(b.version).toBe(a.version);
});

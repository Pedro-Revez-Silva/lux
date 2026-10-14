// Claude Code (claude.* records): stream-json lines as Claude Code wrote
// them. A subagent's lines (the Task/Agent tool) carry parent_tool_use_id,
// the id of the tool_use that runs it: that id is the child thread's.
import { appendText, setPlan } from "./acp.ts";
import { ROOT, type Builder } from "./projection.ts";
import type { MessageItem, ToolItem } from "./types.ts";
import { arr, blocksText, num, obj, str, type Obj } from "./util.ts";

const SUBAGENT_TOOLS = new Set(["Task", "Agent"]);
const TODO_TOOLS = new Set(["TodoWrite"]);

function threadOf(d: Obj): string {
  return str(d.parent_tool_use_id) ?? ROOT;
}

// Which thread each tool_use lives in, for its tool_result.
function owners(b: Builder): Map<string, string> {
  let m = b.memo.get("claude:owners") as Map<string, string> | undefined;
  if (!m) {
    m = new Map();
    b.memo.set("claude:owners", m);
  }
  return m;
}

// Edit's old/new strings as a change the ToolCall can draw.
function editDiff(name: string, input: Obj): string | undefined {
  if (name === "Edit" && typeof input.old_string === "string" && typeof input.new_string === "string") {
    const lines = [`@@ ${str(input.file_path) ?? ""}`];
    for (const l of input.old_string.split("\n")) lines.push(`-${l}`);
    for (const l of input.new_string.split("\n")) lines.push(`+${l}`);
    return lines.join("\n");
  }
  if (name === "Write" && typeof input.content === "string") {
    return [`@@ ${str(input.file_path) ?? ""}`, ...input.content.split("\n").map((l) => `+${l}`)].join("\n");
  }
  return undefined;
}

export function foldClaude(b: Builder, type: string, d: Obj): boolean {
  switch (type) {
    case "claude.system":
      system(b, d);
      return true;
    case "claude.assistant":
      assistant(b, d);
      return true;
    case "claude.user":
      user(b, d);
      return true;
    case "claude.stream_event": {
      // Only with --include-partial-messages: a reply's text as it streams.
      const ev = obj(d.event);
      const delta = obj(ev?.delta);
      const thread = threadOf(d);
      if (str(delta?.type) === "text_delta" && str(delta?.text)) appendText(b, thread, "message", `stream:${thread}:${b.memo.get(`claude:stream:${thread}`) ?? 0}`, str(delta?.text)!);
      if (str(delta?.type) === "thinking_delta" && str(delta?.thinking)) appendText(b, thread, "thought", `stream:${thread}:${b.memo.get(`claude:stream:${thread}`) ?? 0}`, str(delta?.thinking)!);
      return true;
    }
  }
  return false;
}

function system(b: Builder, d: Obj) {
  switch (str(d.subtype)) {
    case "init":
      b.model = str(d.model) ?? b.model;
      return;
    case "task_started": {
      const id = str(d.tool_use_id);
      if (id && b.has(id)) b.setThread(id, { status: "running" });
      return;
    }
    case "task_notification": {
      const id = str(d.tool_use_id);
      if (!id || !b.has(id)) return;
      const usage = obj(d.usage);
      if (num(usage?.total_tokens)) b.setThread(id, { tokens: num(usage?.total_tokens) });
      const st = str(d.status);
      if (st === "completed" || st === "failed" || st === "error") b.endChild(id, st === "completed" ? "completed" : "error");
      return;
    }
  }
}

function assistant(b: Builder, d: Obj) {
  const thread = threadOf(d);
  const msg = obj(d.message);
  if (thread !== ROOT && str(msg?.model)) {
    const th = b.thread(thread);
    if (!th.model) b.setThread(thread, { model: str(msg?.model) });
  }
  const key = `claude:stream:${thread}`;
  const n = (b.memo.get(key) as number | undefined) ?? 0;
  const settledStream = b.item(thread, `message:stream:${thread}:${n}`) || b.item(thread, `thought:stream:${thread}:${n}`);
  for (const block of arr(msg?.content)) {
    const o = obj(block);
    if (!o) continue;
    switch (str(o.type)) {
      case "text": {
        const text = str(o.text) ?? "";
        if (!text) break;
        // The whole message replaces what streamed of it (deltas, if any).
        const streamed = b.item<MessageItem>(thread, `message:stream:${thread}:${n}`);
        if (streamed) {
          b.update<MessageItem>(thread, streamed.id, { text, streaming: false });
        } else {
          b.settle(thread);
          b.push(thread, { kind: "message", id: `message:${str(d.uuid) ?? b.at}`, t: b.t, text, streaming: false });
        }
        break;
      }
      case "thinking": {
        const text = str(o.thinking) ?? "";
        if (text && !b.item(thread, `thought:stream:${thread}:${n}`)) b.push(thread, { kind: "thought", id: `thought:${str(d.uuid) ?? b.at}`, t: b.t, end: b.t, text });
        break;
      }
      case "tool_use":
        toolUse(b, thread, o);
        break;
    }
  }
  if (settledStream) b.memo.set(key, n + 1);
}

function toolUse(b: Builder, thread: string, o: Obj) {
  const id = str(o.id) ?? b.at;
  const name = str(o.name) ?? "tool";
  const input = obj(o.input) ?? {};
  owners(b).set(id, thread);
  if (TODO_TOOLS.has(name)) {
    setPlan(b, thread, id, input.todos);
    return;
  }
  b.settle(thread);
  const item: ToolItem = { kind: "tool", id: `tool:${id}`, t: b.t, name, status: "running", input, cursor: b.at, diff: editDiff(name, input) };
  if (SUBAGENT_TOOLS.has(name)) {
    item.title = str(input.description);
    b.push(thread, item);
    b.thread(id, { title: str(input.description) ?? "Subagent", agent: str(input.subagent_type), brief: str(input.prompt), startedAt: b.t });
    b.linkCall(thread, item.id, id);
    return;
  }
  b.push(thread, item);
}

function user(b: Builder, d: Obj) {
  const msg = obj(d.message);
  // A string content is synthetic (a compaction's summary, a slash command's
  // replay), and the prompt text blocks are lux.input's or a subagent's brief.
  for (const block of arr(msg?.content)) {
    const o = obj(block);
    if (str(o?.type) !== "tool_result") continue;
    const useId = str(o?.tool_use_id);
    if (!useId) continue;
    const thread = owners(b).get(useId) ?? threadOf(d);
    const call = b.item<ToolItem>(thread, `tool:${useId}`);
    if (!call) continue;
    const isError = o?.is_error === true;
    const output = blocksText(o?.content);
    b.update<ToolItem>(thread, call.id, { status: isError ? "error" : "completed", output, error: isError ? output.split("\n")[0] : undefined, end: b.t });
    if (call.child) {
      const res = obj(d.tool_use_result);
      if (str(res?.resolvedModel)) b.setThread(call.child, { model: str(res?.resolvedModel) });
      if (num(res?.totalTokens)) b.setThread(call.child, { tokens: num(res?.totalTokens) });
      b.endChild(call.child, isError ? "error" : "completed");
    }
  }
}

// ACP (acp.* records): the generic adapter, and OpenCode's own session.
import type { Builder } from "./projection.ts";
import { ROOT } from "./projection.ts";
import type { MessageItem, PlanItem, ThoughtItem, ToolItem, ToolStatus } from "./types.ts";
import { arr, blocksText, obj, planSteps, str, type Obj } from "./util.ts";

export function acpStatus(s: string | undefined): ToolStatus | undefined {
  switch (s) {
    case "pending":
      return "pending";
    case "in_progress":
      return "running";
    case "completed":
      return "completed";
    case "failed":
      return "error";
  }
  return undefined;
}

// Appends streamed text to the thread's open item of that kind and key, or opens one.
export function appendText(b: Builder, thread: string, kind: "message" | "thought", key: string, text: string) {
  const id = `${kind}:${key}`;
  const prev = b.item<MessageItem | ThoughtItem>(thread, id);
  if (prev) {
    if (prev.kind === "message") b.update<MessageItem>(thread, id, { text: prev.text + text, streaming: true });
    else b.update<ThoughtItem>(thread, id, { text: prev.text + text, end: b.t });
    return;
  }
  if (kind === "message") {
    b.push(thread, { kind: "message", id, t: b.t, text, streaming: true });
  } else {
    b.settle(thread);
    b.push(thread, { kind: "thought", id, t: b.t, end: b.t, text });
  }
}

// A plan: one per turn, updated in place as the agent ticks it off.
export function setPlan(b: Builder, thread: string, key: string, list: unknown) {
  const steps = planSteps(list);
  if (steps.length === 0) return;
  const open = b.memo.get(`plan:${thread}`) as string | undefined;
  const turnEnded = (() => {
    const items = b.threads.get(thread)?.items ?? [];
    for (let i = items.length - 1; i >= 0; i--) {
      if (items[i]!.id === open) return false;
      if (items[i]!.kind === "marker" && (items[i] as { type: string }).type === "turn") return true;
    }
    return true;
  })();
  if (open && !turnEnded && b.item<PlanItem>(thread, open)) {
    b.update<PlanItem>(thread, open, { steps });
    return;
  }
  const id = `plan:${key}`;
  b.settle(thread);
  b.push(thread, { kind: "plan", id, t: b.t, steps });
  b.memo.set(`plan:${thread}`, id);
}

// ACP diff content ({type:"diff", path, oldText, newText}) as unified-ish lines.
function diffText(content: unknown): string | undefined {
  const out: string[] = [];
  for (const c of arr(content)) {
    const o = obj(c);
    if (str(o?.type) !== "diff") continue;
    out.push(`@@ ${str(o?.path) ?? ""}`);
    for (const l of (str(o?.oldText) ?? "").split("\n")) if (l !== "") out.push(`-${l}`);
    for (const l of (str(o?.newText) ?? "").split("\n")) if (l !== "") out.push(`+${l}`);
  }
  return out.length ? out.join("\n") : undefined;
}

const TODO = /^todo(write|_write)?$/i;

export function foldACP(b: Builder, type: string, d: Obj, thread = ROOT): boolean {
  switch (type) {
    case "acp.agent_message_chunk":
    case "acp.agent_thought_chunk": {
      const text = str(obj(d.content)?.text);
      if (text == null) return true;
      const kind = type === "acp.agent_message_chunk" ? "message" : "thought";
      const last = b.last(thread);
      // Without a messageId a chunk extends the thread's last item of its kind while that is still open, else starts at this record.
      const open = last?.kind === kind && (last.kind !== "message" || last.streaming);
      const key = str(d.messageId) ?? (open ? last.id.slice(kind.length + 1) : b.at);
      appendText(b, thread, kind, key, text);
      return true;
    }
    case "acp.tool_call":
    case "acp.tool_call_update":
      toolCall(b, thread, d, type === "acp.tool_call");
      return true;
    case "acp.plan":
      setPlan(b, thread, b.at, d.entries);
      return true;
    case "acp.permission": {
      const req = obj(d.request);
      const tc = obj(req?.toolCall);
      const what = str(tc?.title) ?? "a tool call";
      const selected = arr(req?.options).map(obj).find((option) => option?.optionId === d.selected);
      const allowed = str(selected?.kind)?.startsWith("allow_") === true;
      b.push(thread, { kind: "note", id: `perm:${b.at}`, t: b.t, tone: allowed ? "permission" : "warning", text: `lux ${allowed ? "allowed" : "had to reject"} ${what}`, detail: allowed ? "allowed by policy" : "no allow option: not allowed" });
      return true;
    }
  }
  return false;
}

function toolCall(b: Builder, thread: string, d: Obj, first: boolean) {
  const callId = str(d.toolCallId) ?? b.at;
  const id = `tool:${callId}`;
  const prev = b.item<ToolItem>(thread, id);
  const status = acpStatus(str(d.status));
  const rawInput = obj(d.rawInput);
  const name = prev?.name ?? str(d.title) ?? "tool";
  // A todo tool's list is the plan, not a tool row; its updates retitle it ("2 todos").
  const todos = (b.memo.get("todoCalls") as Set<string> | undefined) ?? new Set<string>();
  b.memo.set("todoCalls", todos);
  if (!prev && TODO.test(name)) todos.add(callId);
  if (todos.has(callId)) {
    if (rawInput && rawInput.todos) setPlan(b, thread, callId, rawInput.todos);
    return;
  }
  const patch: Partial<ToolItem> = {};
  if (status) patch.status = status;
  if (rawInput && Object.keys(rawInput).length > 0) patch.input = rawInput;
  if (!first || prev) {
    const title = str(d.title);
    if (title && title !== name) patch.title = title;
  }
  const out = str(obj(d.rawOutput)?.output) ?? (d.content ? blocksText(d.content) || undefined : undefined);
  if (out != null) patch.output = out;
  const diff = diffText(d.content);
  if (diff) patch.diff = diff;
  if (status === "error") patch.error = out ?? "failed";
  if (status === "completed" || status === "error") patch.end = b.t;
  if (prev) {
    b.update<ToolItem>(thread, id, patch);
    return;
  }
  b.settle(thread);
  b.push(thread, { kind: "tool", id, t: b.t, name, status: "pending", cursor: b.at, ...patch });
}

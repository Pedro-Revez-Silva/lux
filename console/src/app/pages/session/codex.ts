// Codex (codex.* records): app-server notifications as Codex sent them.
// Codex reports subagents (multi_agent, 0.144.1): a subAgentActivity item
// of the parent names the child's thread (agentThreadId), whose own
// notifications then come on the same stream under that threadId.
import { appendText, setPlan } from "./acp.ts";
import { turnEnd } from "./lux.ts";
import { ROOT, type Builder } from "./projection.ts";
import type { ToolItem, ToolStatus } from "./types.ts";
import { arr, num, obj, str, type Obj } from "./util.ts";

function rootThread(b: Builder): string | undefined {
  return (b.memo.get("codex:root") as string | undefined) ?? b.sessionId;
}

// The session thread for a Codex thread id: the Run's thread is the root.
function threadOf(b: Builder, threadId: string | undefined): string {
  if (!threadId || threadId === rootThread(b)) return ROOT;
  return threadId;
}

function status(s: string | undefined): ToolStatus {
  switch (s) {
    case "completed":
      return "completed";
    case "failed":
    case "declined":
      return "error";
    default:
      return "running";
  }
}

function tool(item: Obj): Omit<ToolItem, "id" | "t" | "cursor"> | null {
  const st = status(str(item.status));
  switch (str(item.type)) {
    case "commandExecution": {
      const exit = num(item.exitCode);
      return {
        kind: "tool",
        name: "shell",
        title: str(item.command),
        status: exit != null && exit !== 0 ? "error" : st,
        input: { command: str(item.command), cwd: str(item.cwd) },
        output: str(item.aggregatedOutput),
        error: exit != null && exit !== 0 ? `exit ${exit}` : undefined,
      };
    }
    case "fileChange": {
      const changes = arr(item.changes).map(obj).filter((c): c is Obj => !!c);
      return {
        kind: "tool",
        name: "patch",
        title: changes.map((c) => str(c.path)).filter(Boolean).join(", "),
        status: st,
        input: { files: changes.map((c) => str(c.path)) },
        diff: changes.map((c) => `@@ ${str(c.path) ?? ""}\n${str(c.diff) ?? ""}`).join("\n") || undefined,
      };
    }
    case "mcpToolCall": {
      const err = str(obj(item.error)?.message);
      return {
        kind: "tool",
        name: `${str(item.server) ?? "mcp"}:${str(item.tool) ?? "?"}`,
        status: err ? "error" : st,
        input: item.arguments,
        output: arr(obj(item.result)?.content)
          .map((c) => str(obj(c)?.text) ?? "")
          .join("\n") || undefined,
        error: err,
      };
    }
    case "dynamicToolCall":
      return { kind: "tool", name: str(item.tool) ?? "tool", status: item.success === false ? "error" : st, input: item.arguments };
    case "webSearch":
      return { kind: "tool", name: "web_search", title: str(item.query), status: "completed", input: { query: str(item.query) } };
    case "collabAgentToolCall":
      return { kind: "tool", name: str(item.tool) ?? "agent", title: str(item.prompt) ?? undefined, status: st, input: { receivers: item.receiverThreadIds, model: item.model } };
  }
  return null;
}

export function foldCodex(b: Builder, type: string, d: Obj): boolean {
  switch (type) {
    case "codex.thread/started": {
      const th = obj(d.thread);
      const id = str(th?.id);
      const parent = str(th?.parentThreadId);
      if (id && !parent && !b.memo.has("codex:root")) b.memo.set("codex:root", id);
      if (id && parent) {
        b.thread(id, { title: str(th?.agentNickname) ?? "Subagent", agent: str(th?.agentRole) ?? undefined });
        b.adopt(threadOf(b, parent), id);
      }
      return true;
    }
    case "codex.item/started":
    case "codex.item/completed":
      // Times are the records' (lux's clock), not Codex's startedAtMs, so a turn's items and lux's markers share one clock.
      item(b, threadOf(b, str(d.threadId)), obj(d.item) ?? {}, type === "codex.item/completed");
      return true;
    case "codex.item/agentMessage/delta":
      appendText(b, threadOf(b, str(d.threadId)), "message", str(d.itemId) ?? b.at, str(d.delta) ?? "");
      return true;
    case "codex.item/reasoning/summaryTextDelta":
    case "codex.item/reasoning/textDelta":
      appendText(b, threadOf(b, str(d.threadId)), "thought", str(d.itemId) ?? b.at, str(d.delta) ?? "");
      return true;
    case "codex.turn/plan/updated":
      setPlan(b, threadOf(b, str(d.threadId)), str(d.turnId) ?? b.at, d.plan);
      return true;
    case "codex.turn/completed": {
      const thread = threadOf(b, str(d.threadId));
      // A child's turn/completed also makes lux write codex.turn_end: turns end here, by thread.
      turnEnd(b, thread);
      if (thread !== ROOT) {
        const st = str(obj(d.turn)?.status);
        b.endChild(thread, st === "failed" ? "error" : "completed");
      }
      return true;
    }
    case "codex.turn/started": {
      const thread = threadOf(b, str(d.threadId));
      if (thread !== ROOT && b.has(thread)) {
        const child = b.thread(thread);
        b.setThread(thread, { status: "running", endedAt: undefined });
        if (child.parent && child.callId) b.update<ToolItem>(child.parent, child.callId, { status: "running", end: undefined });
      }
      return true;
    }
    case "codex.thread/tokenUsage/updated": {
      const thread = threadOf(b, str(d.threadId));
      const total = num(obj(obj(d.tokenUsage)?.total)?.totalTokens);
      if (thread !== ROOT && total != null && b.has(thread)) b.setThread(thread, { tokens: total });
      return true;
    }
  }
  // lux writes codex.turn_end for every thread's turn/completed; turn ends
  // are taken from codex.turn/completed above, by thread.
  return type === "codex.turn_end";
}

function item(b: Builder, thread: string, it: Obj, completed: boolean) {
  const id = str(it.id) ?? b.at;
  const t = b.t;
  switch (str(it.type)) {
    case "userMessage":
    case "contextCompaction":
      // lux.input and lux.compacted say these.
      return;
    case "agentMessage": {
      const text = str(it.text) ?? "";
      const key = `message:${id}`;
      if (b.item(thread, key)) {
        if (completed) b.update(thread, key, { text, streaming: false });
      } else if (text || !completed) {
        b.thread(thread);
        b.settle(thread);
        b.push(thread, { kind: "message", id: key, t, text, streaming: !completed });
      }
      return;
    }
    case "reasoning": {
      const text = [...arr(it.summary), ...arr(it.content)].filter((x) => typeof x === "string").join("\n");
      const key = `thought:${id}`;
      if (b.item(thread, key)) {
        if (completed && text) b.update(thread, key, { text, end: t });
      } else if (text) b.push(thread, { kind: "thought", id: key, t, end: t, text });
      return;
    }
    case "plan":
      return;
    case "subAgentActivity": {
      const child = str(it.agentThreadId);
      if (!child || str(it.kind) !== "started") return;
      const path = str(it.agentPath) ?? "";
      b.thread(child, { title: path.split("/").pop() || "Subagent", startedAt: t });
      // The spawn call itself is not an app-server item (0.144.1): this
      // row stands for it, so the stub has a place in the parent.
      const rowId = `tool:${id}`;
      if (!b.item(thread, rowId)) {
        b.settle(thread);
        b.push(thread, { kind: "tool", id: rowId, t, name: "spawn_agent", title: path, status: "running", cursor: b.at });
      }
      b.linkCall(thread, rowId, child);
      return;
    }
  }
  const tl = tool(it);
  if (!tl) return;
  const key = `tool:${id}`;
  const prev = b.item<ToolItem>(thread, key);
  const st = completed ? (tl.status === "running" ? "completed" : tl.status) : tl.status;
  if (prev) {
    b.update<ToolItem>(thread, key, { ...tl, status: st, end: completed ? t : undefined, input: tl.input ?? prev.input, title: tl.title ?? prev.title });
    return;
  }
  b.thread(thread);
  b.settle(thread);
  b.push(thread, { ...tl, id: key, t, cursor: b.at, status: st, end: completed ? t : undefined });
}

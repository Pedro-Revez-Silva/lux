// OpenCode subagents (opencode.* records; docs/adapters.md "OpenCode
// subagents"). A child's text does not stream: messages arrive whole on
// completion, tool calls on each change of status, cut by lux.
import { ROOT, type Builder } from "./projection.ts";
import type { ToolItem, ToolStatus } from "./types.ts";
import { num, obj, str, type Obj } from "./util.ts";

// The Run's own OpenCode session is the root thread; children keep their ids.
function threadOf(b: Builder, session: string | undefined): string | undefined {
  if (!session) return undefined;
  return session === b.sessionId ? ROOT : session;
}

// "Find callers (@explore subagent)" -> title and agent.
function splitTitle(title: string): { title: string; agent?: string } {
  const m = /^(.*?)\s*\(@([\w-]+) subagent\)\s*$/.exec(title);
  return m ? { title: m[1]!, agent: m[2] } : { title };
}

function modelName(m: unknown): string | undefined {
  const o = obj(m);
  const id = str(o?.modelID);
  if (!id) return undefined;
  return str(o?.providerID) ? `${str(o?.providerID)}/${id}` : id;
}

function partStatus(s: string | undefined): ToolStatus {
  switch (s) {
    case "completed":
    case "error":
    case "running":
      return s;
    default:
      return "pending";
  }
}

export function foldOpenCode(b: Builder, type: string, d: Obj): boolean {
  switch (type) {
    case "opencode.session": {
      const id = str(d.id);
      if (!id) return true;
      const t = splitTitle(str(d.title) ?? "");
      const parent = threadOf(b, str(d.parentID));
      const th = b.thread(id, { startedAt: num(d.createdAt) ?? b.t });
      b.setThread(id, { title: t.title || th.title, agent: th.agent ?? t.agent, startedAt: num(d.createdAt) ?? th.startedAt });
      if (parent) b.adopt(parent, id);
      return true;
    }
    case "opencode.task": {
      const parent = threadOf(b, str(d.sessionID));
      const child = str(d.childSessionID);
      const callId = str(d.callID);
      if (!parent || !child || !callId) return true;
      b.thread(parent, { status: "idle" });
      b.thread(child);
      const model = modelName(d.model);
      if (model) b.setThread(child, { model });
      // A nested task call is itself a child's tool call (opencode.part); one
      // that has not arrived yet gets a row now, so the stub has a place.
      if (!b.item<ToolItem>(parent, `tool:${callId}`))
        b.push(parent, { kind: "tool", id: `tool:${callId}`, t: b.t, name: "task", status: "running", cursor: b.at });
      b.linkCall(parent, `tool:${callId}`, child);
      const st = str(d.status);
      if (st === "completed" || st === "error") b.endChild(child, st);
      else b.setThread(child, { status: "running" });
      return true;
    }
    case "opencode.part": {
      const thread = threadOf(b, str(d.sessionID));
      const callId = str(d.callID);
      if (!thread || !callId) return true;
      const id = `tool:${callId}`;
      const time = obj(d.time);
      const status = partStatus(str(d.status));
      const patch: Partial<ToolItem> = { status };
      const input = str(d.input);
      if (input) {
        patch.inputText = input;
        try {
          patch.input = JSON.parse(input);
        } catch {
          // Cut at 300 bytes: kept as text only.
        }
      }
      if (str(d.title)) patch.title = str(d.title);
      if (d.output != null) patch.output = str(d.output);
      if (str(d.error)) patch.error = str(d.error);
      if (d.truncated === true) patch.truncated = true;
      if (num(time?.end)) patch.end = num(time?.end);
      if (b.item<ToolItem>(thread, id)) {
        b.update<ToolItem>(thread, id, patch);
      } else {
        b.thread(thread);
        b.push(thread, { kind: "tool", id, t: num(time?.start) ?? b.t, name: str(d.tool) ?? "tool", cursor: b.at, ...patch, status });
      }
      return true;
    }
    case "opencode.message": {
      const thread = threadOf(b, str(d.sessionID));
      const text = str(d.text) ?? "";
      if (!thread || thread === ROOT) return true;
      const tokens = obj(d.tokens);
      const th = b.thread(thread);
      const sum = (num(tokens?.input) ?? 0) + (num(tokens?.output) ?? 0) + (num(tokens?.reasoning) ?? 0) + (num(tokens?.cacheRead) ?? 0) + (num(tokens?.cacheWrite) ?? 0);
      b.setThread(thread, { cost: (th.cost ?? 0) + (num(d.cost) ?? 0), tokens: (th.tokens ?? 0) + sum });
      // A step that only called tools completes with no text.
      if (text.trim() !== "")
        b.push(thread, { kind: "message", id: `message:${str(d.messageID) ?? b.at}`, t: num(d.completed) ?? b.t, text, streaming: false, truncated: d.truncated === true });
      return true;
    }
    case "opencode.permission": {
      const thread = threadOf(b, str(d.sessionID)) ?? ROOT;
      const patterns = Array.isArray(d.patterns) ? (d.patterns as string[]).join(", ") : "";
      const allowed = str(d.answered) === "allow";
      // The Run's own session's requests are recorded by acp.permission too.
      if (thread === ROOT) return true;
      b.thread(thread);
      b.push(thread, {
        kind: "note",
        id: `perm:${str(d.permissionID) ?? b.at}`,
        t: b.t,
        tone: allowed ? "permission" : "warning",
        text: allowed ? `lux allowed once: ${str(d.permission) ?? "?"} ${patterns}`.trim() : `lux could not answer: ${str(d.permission) ?? "?"} ${patterns}`.trim(),
        detail: allowed ? "subagent permissions are answered by policy" : str(d.error),
      });
      return true;
    }
  }
  return false;
}

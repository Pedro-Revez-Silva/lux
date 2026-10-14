// Folds a Run's output records into a Session, incrementally: fold() takes
// each new batch onto the state the earlier ones built, so a long Run is
// never replayed per record. Items are replaced, never mutated, when they
// change, and snapshot() copies only the threads a fold touched, so React
// can skip everything else by identity.
import type { Event } from "../../../api/index.ts";
import type { Adapter, OutputRecord, Session, SessionItem, SessionThread, SubagentSupport, ToolItem } from "./types.ts";
import { foldACP } from "./acp.ts";
import { foldClaude } from "./claude.ts";
import { foldCodex } from "./codex.ts";
import { foldLux, foldLuxEvent } from "./lux.ts";
import { foldOpenCode } from "./opencode.ts";

export const ROOT = "root";

// What each adapter can say about subagents (docs/adapters.md; codex: the
// app-server's subAgentActivity item and the child thread's notifications).
export function subagentSupport(adapter: Adapter): SubagentSupport {
  return adapter === "opencode" || adapter === "claude-code" || adapter === "codex" ? "reported" : "unsupported";
}

export class Builder {
  readonly threads = new Map<string, SessionThread>();
  // Where each item sits in its thread: thread id -> item id -> index.
  private readonly index = new Map<string, Map<string, number>>();
  private touched = new Set<string>();
  // Adapter-private state the mappers keep between folds.
  readonly memo = new Map<string, unknown>();
  // Host id -> name, for lifecycle markers (the Run's placements know them).
  readonly hostNames = new Map<string, string>();
  sessionId?: string;
  model?: string;
  activity?: "busy" | "idle";
  turns = 0;
  compactions = 0;
  records = 0;
  cursor = "";
  version = 0;
  // The record being folded: its time and cursor.
  t = 0;
  at = "";

  constructor(readonly adapter: Adapter) {
    this.threads.set(ROOT, { id: ROOT, title: "Run session", status: "idle", items: [], children: [], tools: 0 });
    this.index.set(ROOT, new Map());
  }

  thread(id: string, init?: Partial<SessionThread>): SessionThread {
    let th = this.threads.get(id);
    if (!th) {
      th = { id, title: "Subagent", status: "running", items: [], children: [], tools: 0, startedAt: this.t, ...init };
      this.threads.set(id, th);
      this.index.set(id, new Map());
      this.touch(id);
    }
    return th;
  }

  has(id: string): boolean {
    return this.threads.has(id);
  }

  touch(id: string) {
    this.touched.add(id);
  }

  // Sets fields of a thread (status, title, …) as a new object.
  setThread(id: string, patch: Partial<SessionThread>) {
    const th = this.thread(id);
    Object.assign(th, patch);
    this.touch(id);
  }

  // Links child under parent once, in first-seen order.
  adopt(parent: string, child: string) {
    const p = this.thread(parent, { status: "idle" });
    const c = this.thread(child);
    if (c.parent && c.parent !== parent) return;
    if (!c.parent) {
      c.parent = parent;
      this.touch(child);
    }
    if (!p.children.includes(child)) {
      p.children = [...p.children, child];
      this.touch(parent);
    }
  }

  // A tool call of parent runs child: the call row gets its stub, the child
  // its place in the tree.
  linkCall(parent: string, callItemId: string, child: string) {
    this.adopt(parent, child);
    const call = this.item<ToolItem>(parent, callItemId);
    if (call && call.child !== child) this.update<ToolItem>(parent, callItemId, { child });
    if (!this.threads.get(child)!.callId) this.setThread(child, { callId: callItemId });
  }

  // Ends a child: its status, and its call row in the parent if still open.
  endChild(child: string, status: "completed" | "error") {
    const th = this.threads.get(child);
    if (!th) return;
    this.settle(child);
    this.setThread(child, { status, endedAt: th.endedAt ?? this.t });
    if (th.parent && th.callId) {
      const call = this.item<ToolItem>(th.parent, th.callId);
      if (call && (call.status === "running" || call.status === "pending"))
        this.update<ToolItem>(th.parent, th.callId, { status: status === "error" ? "error" : "completed", end: call.end ?? this.t });
    }
  }

  item<T extends SessionItem>(thread: string, id: string): T | undefined {
    const i = this.index.get(thread)?.get(id);
    return i == null ? undefined : (this.threads.get(thread)!.items[i] as T);
  }

  push(thread: string, item: SessionItem) {
    const th = this.thread(thread);
    this.index.get(thread)!.set(item.id, th.items.length);
    th.items.push(item);
    if (item.kind === "tool") th.tools++;
    this.touch(thread);
  }

  // Puts an item in time order: lux's lifecycle events arrive after the
  // records they sit between.
  insert(thread: string, item: SessionItem) {
    const th = this.thread(thread);
    let at = th.items.length;
    while (at > 0 && th.items[at - 1]!.t > item.t) at--;
    if (at === th.items.length) return this.push(thread, item);
    th.items.splice(at, 0, item);
    const idx = this.index.get(thread)!;
    th.items.forEach((x, i) => idx.set(x.id, i));
    this.touch(thread);
  }

  update<T extends SessionItem>(thread: string, id: string, patch: Partial<T>): T | undefined {
    const i = this.index.get(thread)?.get(id);
    if (i == null) return undefined;
    const th = this.threads.get(thread)!;
    const next = { ...th.items[i]!, ...patch } as T;
    th.items[i] = next;
    this.touch(thread);
    return next;
  }

  last(thread: string): SessionItem | undefined {
    const items = this.threads.get(thread)?.items;
    return items?.[items.length - 1];
  }

  // Ends whatever is streaming in a thread: a reply's caret stops at the
  // next item, or at the turn's end.
  settle(thread: string) {
    const l = this.last(thread);
    if (l?.kind === "message" && l.streaming) this.update(thread, l.id, { streaming: false });
  }

  // Tool calls since the thread's last turn marker, and when the turn began.
  turnStats(thread: string): { tools: number; subagents: number; start?: number } {
    const items = this.threads.get(thread)?.items ?? [];
    let tools = 0;
    let subagents = 0;
    let start: number | undefined;
    for (let i = items.length - 1; i >= 0; i--) {
      const x = items[i]!;
      if (x.kind === "marker" && x.type === "turn") break;
      if (x.kind === "tool") {
        tools++;
        if ((x as ToolItem).child) subagents++;
      }
      if (x.kind !== "marker") start = x.t;
    }
    return { tools, subagents, start };
  }

  snapshot(prev?: Session): Session {
    const threads: Record<string, SessionThread> = {};
    for (const [id, th] of this.threads) {
      const old = prev?.threads[id];
      threads[id] = old && !this.touched.has(id) ? old : { ...th, items: th.items.slice(), children: th.children.slice() };
    }
    const changed = this.touched.size > 0 || !prev;
    this.touched = new Set();
    if (changed) this.version++;
    return {
      adapter: this.adapter,
      subagents: subagentSupport(this.adapter),
      root: ROOT,
      threads,
      sessionId: this.sessionId,
      model: this.model,
      activity: this.activity,
      turns: this.turns,
      compactions: this.compactions,
      records: this.records,
      cursor: this.cursor,
      version: this.version,
    };
  }
}

// The data of an event record: {type, data}.
export function eventOf(r: OutputRecord): { type: string; data: Record<string, unknown> } | null {
  const ev = r.event as { type?: unknown; data?: unknown } | undefined;
  if (r.ch !== "event" || !ev || typeof ev.type !== "string") return null;
  const data = ev.data && typeof ev.data === "object" && !Array.isArray(ev.data) ? (ev.data as Record<string, unknown>) : {};
  return { type: ev.type, data };
}

export class SessionProjection {
  private readonly b: Builder;
  private last?: Session;
  private epoch?: number;
  private replaying = false;

  constructor(adapter: Adapter) {
    this.b = new Builder(adapter);
  }

  fold(records: OutputRecord[]): this {
    const b = this.b;
    for (const r of records) {
      b.records++;
      b.cursor = r.cursor;
      b.t = r.t;
      b.at = r.cursor;
      if (this.epoch !== r.epoch) {
        this.replaying = this.epoch != null;
        this.epoch = r.epoch;
      }
      const ev = eventOf(r);
      if (!ev) continue;
      if (ev.type === "lux.session") this.replaying = false;
      if (this.replaying && ev.type.startsWith("acp.")) continue;
      if (foldLux(b, ev.type, ev.data)) continue;
      switch (b.adapter) {
        case "acp":
          foldACP(b, ev.type, ev.data);
          break;
        case "opencode":
          if (!foldOpenCode(b, ev.type, ev.data)) foldACP(b, ev.type, ev.data);
          break;
        case "claude-code":
          foldClaude(b, ev.type, ev.data);
          break;
        case "codex":
          foldCodex(b, ev.type, ev.data);
          break;
      }
    }
    return this;
  }

  // A lux lifecycle event (the output stream's "lux" events).
  foldEvent(e: Event): this {
    this.b.t = Date.parse(e.time);
    foldLuxEvent(this.b, e);
    return this;
  }

  hosts(names: Record<string, string>): this {
    for (const [id, name] of Object.entries(names)) this.b.hostNames.set(id, name);
    return this;
  }

  snapshot(): Session {
    this.last = this.b.snapshot(this.last);
    return this.last;
  }
}

// The whole of a Run's records at once (tests, a settled Run).
export function sessionFromRecords(adapter: Adapter, records: OutputRecord[]): Session {
  return new SessionProjection(adapter).fold(records).snapshot();
}

// The tool call in the parent that runs a subagent.
export function callOf(s: Session, th: SessionThread): ToolItem | undefined {
  if (!th.parent || !th.callId) return undefined;
  const call = s.threads[th.parent]?.items.find((i) => i.id === th.callId);
  return call?.kind === "tool" ? call : undefined;
}

// What a subagent was briefed with: the task's prompt, from its call's input.
// cut: lux cut the input it came in (an OpenCode child's call, at 300 bytes).
export function briefOf(s: Session, th: SessionThread): { text: string; cut: boolean } | undefined {
  if (th.brief) return { text: th.brief, cut: th.briefTruncated === true };
  const call = callOf(s, th);
  if (!call) return undefined;
  const prompt = (call.input as { prompt?: unknown } | undefined)?.prompt;
  if (typeof prompt === "string") return { text: prompt, cut: false };
  if (call.inputText) {
    const m = /"prompt"\s*:\s*"((?:[^"\\]|\\.)*)/.exec(call.inputText);
    return { text: m ? m[1]!.replace(/\\n/g, "\n").replace(/\\"/g, '"') : call.inputText, cut: call.truncated === true };
  }
  return undefined;
}

// A subagent's type (opencode/claude subagent_type), from the thread or its call.
export function agentOf(s: Session, th: SessionThread): string | undefined {
  if (th.agent) return th.agent;
  const input = callOf(s, th)?.input as { subagent_type?: unknown } | undefined;
  return typeof input?.subagent_type === "string" ? input.subagent_type : undefined;
}

// Every subagent in tree order (depth first), with its depth; threads whose
// parent never arrived (OpenCode's bus does not replay) are listed at the top.
export function subagentTree(s: Session): { thread: SessionThread; depth: number }[] {
  const out: { thread: SessionThread; depth: number }[] = [];
  const seen = new Set<string>();
  const walk = (id: string, depth: number) => {
    for (const c of s.threads[id]?.children ?? []) {
      if (seen.has(c)) continue;
      seen.add(c);
      out.push({ thread: s.threads[c]!, depth });
      walk(c, depth + 1);
    }
  };
  walk(s.root, 0);
  for (const th of Object.values(s.threads)) {
    if (th.id === s.root || seen.has(th.id) || (th.parent && s.threads[th.parent])) continue;
    seen.add(th.id);
    out.push({ thread: th, depth: 0 });
    walk(th.id, 1);
  }
  return out;
}

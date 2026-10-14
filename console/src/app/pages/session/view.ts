// What the Session tab draws from a Session, kept apart from React so it is
// testable: summaries, cut notes, colours, the rail's figures.
import { agentColor, agentSlot } from "@lux/design-system";
import { agentOf, briefOf, subagentTree } from "./projection.ts";
import type { Session, SessionItem, SessionThread, ToolItem } from "./types.ts";

// The adapters whose Runs get a Session tab: the agent adapters.
export const AGENT_ADAPTERS = new Set(["acp", "opencode", "claude-code", "codex"]);

export function hasSession(adapter: string | undefined): boolean {
  return AGENT_ADAPTERS.has(adapter ?? "");
}

const SUMMARY_KEYS = ["command", "filePath", "file_path", "path", "pattern", "description", "url", "query", "prompt"];

/** One line for a folded tool call: its title, else the input field that says what it did. */
export function toolSummary(t: ToolItem): string {
  if (t.title) return t.title;
  const input = t.input && typeof t.input === "object" ? (t.input as Record<string, unknown>) : undefined;
  for (const k of SUMMARY_KEYS) {
    const v = input?.[k];
    if (typeof v === "string" && v) return v.split("\n")[0]!;
  }
  return t.inputText ?? "";
}

/** A task-like call reads as prose (its description); commands and paths in mono. */
export function summaryIsProse(t: ToolItem): boolean {
  return !!t.child || /^(task|agent)$/i.test(t.name);
}

export function toolInputText(t: ToolItem): string | undefined {
  if (t.input !== undefined && !(typeof t.input === "object" && t.input && Object.keys(t.input).length === 0)) return JSON.stringify(t.input, null, 2);
  return t.inputText;
}

// What lux cut on the way, per adapter (docs/adapters.md "OpenCode subagents").
export function toolCutNote(adapter: string, thread: SessionThread, t: ToolItem): string | undefined {
  if (!t.truncated) return undefined;
  if (adapter === "opencode" && thread.parent) return "cut by lux: input at 300 bytes, output and error at 2 KiB";
  return "cut by lux";
}

/** Which items a filter shows: Messages hides tool calls, thoughts, plans and notes. */
export function visible(items: SessionItem[], filter: "all" | "messages"): SessionItem[] {
  return filter === "all" ? items : items.filter((i) => i.kind === "input" || i.kind === "message" || i.kind === "marker");
}

/** The newest tool call or reply of a thread, as one line. */
export function lastActivity(th: SessionThread): string | undefined {
  for (let i = th.items.length - 1; i >= 0; i--) {
    const x = th.items[i]!;
    if (x.kind === "message" && x.text.trim())
      return x.text
        .replace(/^\s*([-*]|\d+\.)\s+/gm, "")
        .replace(/\s+/g, " ")
        .replace(/[`*]/g, "")
        .trim();
    if (x.kind === "tool") return `${x.name} ${toolSummary(x)}`.trim();
  }
  return undefined;
}

/** Each agent type's colour, in the order the Run first showed it. */
export function agentColors(s: Session, slots: Map<string, number>): Map<string, string> {
  const out = new Map<string, string>();
  for (const th of Object.values(s.threads)) {
    if (th.id === s.root) continue;
    out.set(th.id, agentColor(agentSlot(slots, agentOf(s, th))));
  }
  return out;
}

export function briefFor(s: Session, th: SessionThread): { text: string; cutNote?: string } | undefined {
  const b = briefOf(s, th);
  if (!b) return undefined;
  return { text: b.text, cutNote: b.cut ? "the task's input, cut at 300 bytes by lux" : undefined };
}

// What each adapter's subagent panel says about how its records arrive.
export function subagentFootnote(adapter: string): string | undefined {
  switch (adapter) {
    case "opencode":
      return "A subagent's text arrives when each message completes, not as it streams; tool calls arrive as their status changes, cut by lux.";
    case "claude-code":
      return "A subagent's text arrives when each message completes, not as it streams.";
    case "codex":
      return "Codex does not report what a subagent was briefed with: its spawn call is not on the app-server stream.";
  }
  return undefined;
}

export function noSubagentsText(adapter: string): string {
  return adapter === "acp" ? "The acp adapter doesn't report subagents: ACP shows only the Run's own session." : `The ${adapter} adapter doesn't report subagents.`;
}

/** Tool calls per name across every thread. */
export function toolCounts(s: Session): { name: string; count: number }[] {
  const m = new Map<string, number>();
  for (const th of Object.values(s.threads)) for (const i of th.items) if (i.kind === "tool") m.set(i.name, (m.get(i.name) ?? 0) + 1);
  return [...m].map(([name, count]) => ({ name, count }));
}

export function subagentCounts(s: Session): { total: number; running: number } {
  const rows = subagentTree(s);
  return { total: rows.length, running: rows.filter((r) => r.thread.status === "running").length };
}

/** "$0.31 · 61.4k tokens", what a subagent reported; nothing when it reported none. */
export function usageText(th: SessionThread): string | undefined {
  const parts: string[] = [];
  if (th.cost) parts.push(`$${th.cost < 0.01 ? th.cost.toFixed(4) : th.cost.toFixed(2)}`);
  if (th.tokens) parts.push(`${th.tokens >= 1000 ? `${(th.tokens / 1000).toFixed(1)}k` : th.tokens} tokens`);
  return parts.length ? parts.join(" · ") : undefined;
}

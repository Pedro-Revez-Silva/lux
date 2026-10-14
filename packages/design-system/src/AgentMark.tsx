import type { CSSProperties } from "react";

/** The colour of the nth agent type seen in a Run (0-based): --agent-1…5, then again. */
export function agentColor(slot: number): string {
  return `var(--agent-${(((slot % 5) + 5) % 5) + 1})`;
}

/**
 * Gives each agent type a slot in first-seen order, so a type keeps one
 * colour for the whole Run. Pass the same map on every render.
 */
export function agentSlot(slots: Map<string, number>, agent: string | undefined): number {
  const key = agent ?? "";
  let s = slots.get(key);
  if (s == null) {
    s = slots.size;
    slots.set(key, s);
  }
  return s;
}

export interface AgentMarkProps {
  color: string;
  /** Pulses while the subagent runs (a live state dot). */
  live?: boolean;
  /** Screen-reader text; the visible name sits beside the mark. */
  label?: string;
}

/** A dot in an agent's colour. Never alone: always beside the agent's name. */
export function AgentMark({ color, live, label }: AgentMarkProps) {
  return (
    <span className={live ? "agent-mark is-live" : "agent-mark"} style={{ "--agent": color } as CSSProperties} aria-hidden={label ? undefined : true} role={label ? "img" : undefined} aria-label={label} />
  );
}

import type { CSSProperties, KeyboardEvent } from "react";
import { AgentMark } from "./AgentMark.tsx";
import { Badge } from "./Badge.tsx";
import { formatDuration } from "./format.ts";
import { IconChevronRight } from "./icons.tsx";
import { plural } from "./plural.ts";

export type SubagentStatus = "running" | "completed" | "error" | "idle";

/** A subagent's status as a labelled badge. */
export function SubagentStatusBadge({ status }: { status: SubagentStatus }) {
  if (status === "running") return <Badge tone="info">running</Badge>;
  if (status === "error") return <Badge tone="danger">failed</Badge>;
  // idle: known only as a parent, its own status not reported yet.
  if (status === "idle") return <Badge>idle</Badge>;
  return <Badge tone="success">done</Badge>;
}

/** How long a subagent has run: to its end, or to now while it runs. */
export function subagentSpan(start?: number, end?: number, now?: number): string {
  if (start == null) return "";
  const e = end ?? now;
  return e != null ? formatDuration(Math.max(0, e - start) / 1000) : "";
}

export const agentLabel = (s: string | undefined) => (s ? s[0]!.toUpperCase() + s.slice(1) : "Subagent");

export interface SubagentSummary {
  id: string;
  title: string;
  /** The agent type (explore, general, Explore): named beside its colour. */
  agent?: string;
  model?: string;
  color: string;
  status: SubagentStatus;
  startedAt?: number;
  endedAt?: number;
  tools: number;
  /** Subagents it started itself. */
  subagents?: number;
}

export interface SubagentStubProps extends SubagentSummary {
  /** The newest tool call or reply, one line. */
  last?: string;
  selected?: boolean;
  now?: number;
  onOpen: () => void;
}

/** The row under a task call: the subagent's colour, name, status, last activity, counts, Open. */
export function SubagentStub({ title, agent, color, status, startedAt, endedAt, tools, subagents, last, selected, now, onOpen }: SubagentStubProps) {
  const key = (e: KeyboardEvent) => {
    if (e.key === "Enter" || e.key === " ") {
      e.preventDefault();
      onOpen();
    }
  };
  return (
    <div className={selected ? "turn subthread is-selected" : "turn subthread"} style={{ "--agent": color } as CSSProperties} role="button" tabIndex={0} aria-label={`Open subagent ${title}`} onClick={onOpen} onKeyDown={key}>
      <AgentMark color={color} live={status === "running"} />
      <span className="subthread-main">
        <span className="subthread-title">
          <span className="agent-name">{agentLabel(agent)}</span>
          <SubagentStatusBadge status={status} />
        </span>
        <span className="subthread-last">{last ?? (status === "running" ? "starting…" : title)}</span>
      </span>
      <span className="subthread-meta">
        {plural(tools, "tool")}
        {subagents ? ` · ${subagents} sub` : ""}
        {startedAt != null && ` · ${subagentSpan(startedAt, endedAt, now)}`}
      </span>
      <span className="subthread-open">
        Open <IconChevronRight size={12} />
      </span>
    </div>
  );
}

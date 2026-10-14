import type { CSSProperties } from "react";
import { AgentMark } from "./AgentMark.tsx";
import { plural } from "./plural.ts";
import { agentLabel, SubagentStatusBadge, subagentSpan, type SubagentSummary } from "./SubagentStub.tsx";

export interface SubagentTreeProps {
  rows: (SubagentSummary & { depth: number })[];
  selected?: string;
  now?: number;
  onOpen: (id: string) => void;
}

/** The Run's subagents, nested by parent, a guide per depth. */
export function SubagentTree({ rows, selected, now, onOpen }: SubagentTreeProps) {
  return (
    <div className="subtree" role="tree" aria-label="Subagents">
      {rows.map((r) => (
        <button
          key={r.id}
          type="button"
          role="treeitem"
          aria-level={r.depth + 1}
          aria-selected={selected === r.id}
          className={selected === r.id ? "subtree-row is-selected" : "subtree-row"}
          style={{ "--depth": r.depth, "--agent": r.color } as CSSProperties}
          onClick={() => onOpen(r.id)}
        >
          {Array.from({ length: r.depth }, (_, i) => (
            <span key={i} className="subtree-guide" style={{ left: `calc(var(--sp-4) + ${i} * var(--subagent-indent) + 3px)` }} />
          ))}
          <AgentMark color={r.color} live={r.status === "running"} />
          <span className="subtree-main">
            <span className="subtree-title">{r.title}</span>
            <span className="subtree-sub">
              <span className="agent-name">{agentLabel(r.agent)}</span>
              {r.model && ` · ${r.model.split("/").pop()}`}
            </span>
          </span>
          <span className="subtree-side">
            <SubagentStatusBadge status={r.status} />
            <span>
              {plural(r.tools, "tool")}
              {r.startedAt != null && ` · ${subagentSpan(r.startedAt, r.endedAt, now)}`}
            </span>
          </span>
        </button>
      ))}
    </div>
  );
}

import type { CSSProperties, ReactNode } from "react";
import { AgentMark } from "./AgentMark.tsx";
import { IconChevronLeft, IconClose, IconInfo } from "./icons.tsx";
import { IdChip } from "./IdChip.tsx";
import { Spinner } from "./Spinner.tsx";
import { plural } from "./plural.ts";
import { agentLabel, SubagentStatusBadge, subagentSpan, type SubagentSummary } from "./SubagentStub.tsx";

export interface SubagentPanelProps extends SubagentSummary {
  /** The parent subagent to go back to; none for the Run's own session. */
  back?: { label: string; onClick: () => void };
  onClose: () => void;
  brief?: { text: string; cutNote?: string };
  /** Cost and tokens, as the caller formats them. */
  usage?: string;
  now?: number;
  /** The adapter's caveat (a subagent's text does not stream, …). */
  footnote?: ReactNode;
  /** In a Dialog: no card frame of its own. */
  bare?: boolean;
  children: ReactNode;
}

/** One subagent in the rail's place: its colour, facts, brief and turns. Esc closes (the caller binds it). */
export function SubagentPanel({ id, title, agent, model, color, status, startedAt, endedAt, tools, back, onClose, brief, usage, now, footnote, bare, children }: SubagentPanelProps) {
  return (
    <section className={bare ? "subpanel is-bare" : "subpanel"} style={{ "--agent": color } as CSSProperties} aria-label={`Subagent ${title}`}>
      <div className="subpanel-head">
        <div className="subpanel-top">
          <span className="subpanel-kicker">Subagent</span>
          <button type="button" className="subpanel-close" aria-label="Close subagent" title="Close (Esc)" onClick={onClose}>
            <IconClose size={14} />
          </button>
        </div>
        <button type="button" className="subpanel-back" onClick={back?.onClick ?? onClose}>
          <IconChevronLeft size={12} /> {back?.label ?? "Run session"}
        </button>
        <h3 className="subpanel-title">{title}</h3>
        <div className="subpanel-facts">
          <AgentMark color={color} live={status === "running"} />
          <span className="agent-name">{agentLabel(agent)}</span>
          <SubagentStatusBadge status={status} />
          {model && <span className="mono">{model}</span>}
          <span>{plural(tools, "tool call")}</span>
          {startedAt != null && <span className="num">{subagentSpan(startedAt, endedAt, now)}</span>}
          {usage && <span>{usage}</span>}
          <IdChip value={id} truncate={18} />
        </div>
      </div>
      <div className="subpanel-scroll">
        {brief && (
          <div>
            <div className="toolcall-section-label">
              Briefed with {brief.cutNote && <span className="cut-note">· {brief.cutNote}</span>}
            </div>
            <div className="subpanel-brief">{brief.text}</div>
          </div>
        )}
        {children}
        {status === "running" && (
          <div className="activity-tail">
            <Spinner size={12} /> working…
          </div>
        )}
      </div>
      {footnote && (
        <div className="subpanel-foot">
          <IconInfo size={12} />
          <span>{footnote}</span>
        </div>
      )}
    </section>
  );
}

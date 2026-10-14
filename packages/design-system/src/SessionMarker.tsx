import { useState, type ReactNode } from "react";
import { formatClock } from "./format.ts";
import { IconCompress, IconInfo, IconMove, IconShield, IconWarning } from "./icons.tsx";

export type SessionMarkerType = "epoch" | "turn" | "compacted" | "warning" | "ended";

export interface SessionMarkerProps {
  type: SessionMarkerType;
  t?: number;
  children: ReactNode;
  /** A compaction's summary, behind a "summary" toggle. */
  summary?: string;
  summaryNote?: string;
}

const ICON: Partial<Record<SessionMarkerType, ReactNode>> = {
  epoch: <IconMove size={12} />,
  compacted: <IconCompress size={12} />,
  warning: <IconWarning size={12} />,
  ended: <IconInfo size={12} />,
};

/** What lux itself did between turns: a hairline with text (epochs, moves, turn ends, compactions). */
export function SessionMarker({ type, t, children, summary, summaryNote }: SessionMarkerProps) {
  const [open, setOpen] = useState(false);
  return (
    <div className="turn">
      {t != null && <span className="turn-time">{formatClock(t)}</span>}
      <div className={type === "turn" ? "marker is-turn" : "marker"} role="separator">
        <span className="marker-body">
          {ICON[type]}
          <span className="marker-text">{children}</span>
          {summary && (
            <button type="button" aria-expanded={open} onClick={() => setOpen(!open)}>
              {open ? "hide summary" : "summary"}
            </button>
          )}
        </span>
      </div>
      {open && summary && (
        <div className="marker-summary">
          {summary}
          {summaryNote && <div className="cut-note">{summaryNote}</div>}
        </div>
      )}
    </div>
  );
}

export interface NoteProps {
  tone?: "permission" | "warning";
  t?: number;
  children: ReactNode;
  detail?: ReactNode;
}

/** One iconed line: a permission lux answered by policy, or a warning. */
export function Note({ tone = "permission", t, children, detail }: NoteProps) {
  return (
    <div className={tone === "warning" ? "turn note is-warn" : "turn note"}>
      {t != null && <span className="turn-time">{formatClock(t)}</span>}
      {tone === "warning" ? <IconWarning size={13} /> : <IconShield size={13} />}
      <span>
        {children}
        {detail && <span className="note-detail"> · {detail}</span>}
      </span>
    </div>
  );
}

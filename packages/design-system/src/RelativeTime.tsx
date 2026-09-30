import type { ReactNode } from "react";
import { useNow } from "./clock.ts";
import { formatDuration, formatRelative, formatTimestampZone } from "./format.ts";
import { Tooltip } from "./Tooltip.tsx";

const DASH = <span className="muted">–</span>;

export interface RelativeTimeProps {
  at: string | number | Date | null | undefined;
  /** The clock to measure from; the shared 10s clock when omitted. */
  now?: number;
  /** What the Tooltip says before the exact time ("Created", "Terminated"). */
  label?: string;
}

/**
 * "3h ago", with the exact date, time and time zone in a Tooltip. Every
 * table shows times this way. An en dash when there is no time.
 */
export function RelativeTime({ at, now, label }: RelativeTimeProps) {
  const tick = useNow();
  if (at == null || at === "") return DASH;
  const exact = formatTimestampZone(at);
  return (
    <Tooltip content={label ? `${label} ${exact}` : exact}>
      <span className="rel-time" data-at={typeof at === "string" ? at : new Date(at).toISOString()}>
        {formatRelative(at, now ?? tick)}
      </span>
    </Tooltip>
  );
}

export interface DurationCellProps {
  /** Seconds; null for none (an en dash, with `missing` as its Tooltip). */
  seconds: number | null | undefined;
  /** Still counting: rendered in the foreground colour with a trailing "…" when `ellipsis`. */
  live?: boolean;
  ellipsis?: boolean;
  /** Warn tone (e.g. a slow placement). */
  warn?: boolean;
  /** What the Tooltip says (how it was measured). */
  tip?: ReactNode;
  /** Tooltip of the en dash. */
  missing?: ReactNode;
}

/** A duration in a table cell (formatDuration), its meaning in a Tooltip. */
export function DurationCell({ seconds, live, ellipsis, warn, tip, missing }: DurationCellProps) {
  if (seconds == null || !Number.isFinite(seconds)) {
    return missing ? (
      <Tooltip content={missing}>
        <span className="muted" tabIndex={0}>
          –
        </span>
      </Tooltip>
    ) : (
      DASH
    );
  }
  const text = `${formatDuration(seconds >= 1 ? Math.floor(seconds) : seconds)}${ellipsis ? "…" : ""}`;
  const cls = ["duration", live ? "is-live" : "", warn ? "is-warn" : ""].join(" ").trim();
  const body = (
    <span className={cls} tabIndex={tip ? 0 : undefined}>
      {text}
    </span>
  );
  return tip ? <Tooltip content={tip}>{body}</Tooltip> : body;
}

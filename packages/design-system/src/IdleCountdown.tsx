import { formatClock } from "./format.ts";

/** "7:42": minutes and seconds left (hours when there are any). */
export function formatCountdown(ms: number): string {
  const s = Math.max(0, Math.round(ms / 1000));
  const h = Math.floor(s / 3600);
  const m = Math.floor((s % 3600) / 60);
  const ss = String(s % 60).padStart(2, "0");
  return h > 0 ? `${h}:${String(m).padStart(2, "0")}:${ss}` : `${m}:${ss}`;
}

export interface IdleCountdownProps {
  /** When it goes idle without another request (RFC 3339); null: not counting (not ready, or never idle). */
  idleAt?: string | null;
  /** The idle period, in words ("10m"), shown under the figure. */
  idleAfter?: string;
  now?: number;
}

/**
 * How long until a ready server is idle: "Idle in 7:42 · 10m after the last
 * request". At zero, "Idle now" (its owner has been told). Nothing to count:
 * a dash.
 */
export function IdleCountdown({ idleAt, idleAfter, now = Date.now() }: IdleCountdownProps) {
  if (!idleAt) {
    return (
      <span className="idle-countdown is-off muted" title="Not counting: it is not ready, or never goes idle">
        —
      </span>
    );
  }
  const left = new Date(idleAt).getTime() - now;
  return (
    <span className={["idle-countdown", left <= 0 ? "is-due" : ""].join(" ").trim()} title={`idle at ${formatClock(idleAt)}`}>
      <span className="idle-countdown-figure mono">{left <= 0 ? "Idle now" : `Idle in ${formatCountdown(left)}`}</span>
      {idleAfter && <span className="idle-countdown-note muted">{idleAfter} after the last request</span>}
    </span>
  );
}

import { Fragment, useState } from "react";
import { formatClock, formatDuration, formatTimestamp } from "./format.ts";

export interface TimelineStage {
  key: string;
  label: string;
  /** Epoch ms. A stage with no start hasn't happened. */
  start?: number | null;
  /** Epoch ms. Missing end with a start means it is in progress. */
  end?: number | null;
  /** Hue: setup stages are neutral, the workload is teal, teardown is amber, failures red. */
  tone?: "neutral" | "accent" | "teal" | "amber" | "red";
  note?: string;
}

export interface TimelineProps {
  stages: TimelineStage[];
  /** Clock used for in-progress stages; defaults to now. */
  now?: number;
  /** Total width of the label column. */
  labelWidth?: number;
}

/** Horizontal waterfall of lifecycle stages. Each row is one stage; bars share one time axis. */
export function Timeline({ stages, now = Date.now(), labelWidth = 150 }: TimelineProps) {
  const [hoverKey, setHoverKey] = useState<string | null>(null);
  const starts = stages.map((s) => s.start).filter((v): v is number => v != null);
  const ends = stages.map((s) => s.end ?? (s.start != null ? now : null)).filter((v): v is number => v != null);
  if (starts.length === 0) return <div className="timeline timeline-empty muted">No lifecycle events yet.</div>;
  const t0 = Math.min(...starts);
  const t1 = Math.max(...ends, t0 + 1000);
  const total = t1 - t0;
  const pct = (t: number) => `${(((t - t0) / total) * 100).toFixed(3)}%`;
  const ticks = niceTicks(total, 5);

  return (
    <div className="timeline" style={{ ["--tl-label-w" as string]: `${labelWidth}px` }}>
      <div className="timeline-axis">
        <div className="timeline-axis-label muted">
          {formatTimestamp(t0)} → {formatDuration(total / 1000)}
        </div>
        <div className="timeline-axis-track">
          {ticks.map((t) => (
            <span key={t} className="timeline-tick mono" style={{ left: pct(t0 + t) }}>
              +{formatDuration(t / 1000)}
            </span>
          ))}
        </div>
      </div>
      {stages.map((s) => {
        const started = s.start != null;
        const end = s.end ?? (started ? now : null);
        const dur = started && end != null ? (end - s.start!) / 1000 : null;
        const inProgress = started && s.end == null;
        const tone = s.tone ?? "neutral";
        const active = hoverKey === s.key;
        return (
          <Fragment key={s.key}>
            <div
              className={["timeline-row", started ? "" : "is-pending", active ? "is-active" : ""].join(" ").trim()}
              onMouseEnter={() => setHoverKey(s.key)}
              onMouseLeave={() => setHoverKey(null)}
            >
              <div className="timeline-label">
                {s.label}
                {s.note && <span className="timeline-note muted"> · {s.note}</span>}
              </div>
              <div className="timeline-track">
                {ticks.map((t) => (
                  <span key={t} className="timeline-grid" style={{ left: pct(t0 + t) }} />
                ))}
                {started && (
                  <span
                    className={["timeline-bar", `tone-${tone}`, inProgress ? "is-live" : ""].join(" ").trim()}
                    style={{ left: pct(s.start!), width: `calc(${pct(end!)} - ${pct(s.start!)})` }}
                    title={`${s.label}: ${formatClock(s.start!)} → ${inProgress ? "now" : formatClock(end!)} (${formatDuration(dur)})`}
                  />
                )}
                <span className="timeline-dur mono" style={{ left: started ? `calc(${pct(end!)} + 6px)` : 0 }}>
                  {started ? (inProgress ? `${formatDuration(dur)}…` : formatDuration(dur)) : "not yet"}
                </span>
              </div>
            </div>
          </Fragment>
        );
      })}
    </div>
  );
}

/** Evenly spaced tick offsets (ms) rounded to a clean step. */
function niceTicks(total: number, target: number): number[] {
  const raw = total / target;
  const steps = [100, 250, 500, 1000, 2000, 5000, 10_000, 15_000, 30_000, 60_000, 120_000, 300_000, 600_000, 900_000, 1_800_000, 3_600_000, 7_200_000, 21_600_000, 43_200_000, 86_400_000];
  const step = steps.find((s) => s >= raw) ?? steps[steps.length - 1]!;
  const out: number[] = [];
  for (let t = step; t < total; t += step) out.push(t);
  return out;
}

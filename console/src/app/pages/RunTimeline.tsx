import { Badge, Card, formatBytes, formatDuration, Timeline, type TimelineStage } from "@lux/design-system";
import type { Placement, Run } from "../../api/index.ts";
import { HostLink } from "./common.tsx";

type Stamp = keyof Placement;

/** Each stage runs between two stamps; the label names what was happening. */
const STAGES: { key: string; label: string; from: Stamp; to: Stamp | Stamp[]; tone: TimelineStage["tone"] }[] = [
  { key: "accept", label: "Accepting", from: "assignedAt", to: "acceptedAt", tone: "neutral" },
  { key: "image", label: "Pulling image", from: "acceptedAt", to: "imageReadyAt", tone: "accent" },
  { key: "volumes", label: "Restoring volumes", from: "imageReadyAt", to: "volumesRestoredAt", tone: "accent" },
  { key: "container", label: "Starting container", from: "volumesRestoredAt", to: "containerStartedAt", tone: "accent" },
  { key: "workload", label: "Starting workload", from: "containerStartedAt", to: "workloadStartedAt", tone: "accent" },
  { key: "running", label: "Running", from: "workloadStartedAt", to: ["stopRequestedAt", "exitedAt"], tone: "teal" },
  { key: "stopping", label: "Stopping", from: "stopRequestedAt", to: "exitedAt", tone: "amber" },
  { key: "snapshot", label: "Snapshotting", from: "exitedAt", to: "snapshotDoneAt", tone: "neutral" },
  { key: "upload", label: "Uploading", from: "snapshotDoneAt", to: "uploadedAt", tone: "neutral" },
];

const PLACEMENT_DONE = new Set(["exited", "lost", "failed", "rejected"]);

function placementStages(p: Placement): TimelineStage[] {
  const at = (k: Stamp): number | null => {
    const v = p[k];
    if (typeof v !== "string") return null;
    const t = Date.parse(v);
    return Number.isFinite(t) ? t : null;
  };
  const done = PLACEMENT_DONE.has(p.state);
  const stages: TimelineStage[] = [];
  let openStage = false;
  for (const s of STAGES) {
    const start = at(s.from);
    const ends = Array.isArray(s.to) ? s.to : [s.to];
    const end = ends.map(at).find((t): t is number => t != null) ?? null;
    if (start == null) {
      // A stage that never started: hidden once the placement is over, or
      // when it does not apply (no stop was requested).
      if (done || s.key === "stopping") continue;
      stages.push({ key: s.key, label: s.label, start: null, tone: s.tone });
      continue;
    }
    if (s.key === "stopping" && p.stopRequestedAt == null) continue;
    const inProgress = end == null && !done && !openStage;
    if (end == null && !inProgress) continue;
    if (inProgress) openStage = true;
    const note = s.key === "running" || s.key === "stopping" ? exitNote(p) : s.key === "snapshot" && p.snapshotBytes != null ? formatBytes(p.snapshotBytes) : s.key === "stopping" ? p.stopReason : undefined;
    stages.push({ key: s.key, label: s.label, start, end: end != null && end <= start ? start + 50 : end, tone: s.tone, note });
  }
  return stages;
}

function exitNote(p: Placement): string | undefined {
  const parts: string[] = [];
  if (p.exitCode != null) parts.push(`exit ${p.exitCode}`);
  if (p.exitReason) parts.push(p.exitReason);
  if (p.stopReason) parts.push(`stop: ${p.stopReason}`);
  return parts.length ? parts.join(" · ") : undefined;
}

export function RunTimeline({ run, now }: { run: Run; now: number }) {
  const placements = [...(run.placements ?? [])].sort((a, b) => b.epoch - a.epoch);
  if (placements.length === 0) return <Card title="Placements">{<div className="muted">Not placed yet.</div>}</Card>;
  return (
    <div className="stack">
      {placements.map((p) => (
        <Card
          key={p.epoch}
          title={
            <span className="row">
              <span>Epoch {p.epoch}</span>
              <Badge outline>{p.state}</Badge>
              {p.epoch === run.epoch && <Badge tone="accent">current</Badge>}
            </span>
          }
          subtitle={
            <span className="row" style={{ gap: "var(--sp-2) var(--sp-3)" }}>
              <span>
                on <HostLink id={p.host} name={p.hostName} />
              </span>
              {p.cpuSeconds != null && <span>· CPU {formatDuration(p.cpuSeconds)}</span>}
              {p.peakMemoryBytes != null && <span>· peak mem {formatBytes(p.peakMemoryBytes)}</span>}
              {p.peakDiskBytes != null && <span>· peak disk {formatBytes(p.peakDiskBytes)}</span>}
              {p.peakPids != null && <span>· {p.peakPids} pids</span>}
              {(p.netRxBytes != null || p.netTxBytes != null) && <span>· net {formatBytes(p.netRxBytes)} in / {formatBytes(p.netTxBytes)} out</span>}
            </span>
          }
        >
          <Timeline stages={placementStages(p)} now={now} labelWidth={140} />
        </Card>
      ))}
    </div>
  );
}

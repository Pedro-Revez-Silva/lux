import type { TimelineStage } from "@lux/design-system";
import type { Host, HostTimeKey } from "../../api/index.ts";

const HOST_TIMES: { key: HostTimeKey; label: string; tone: TimelineStage["tone"] }[] = [
  { key: "created", label: "Created", tone: "neutral" },
  { key: "provisionRequested", label: "Provision requested", tone: "neutral" },
  { key: "provisioned", label: "Provisioned", tone: "accent" },
  { key: "registered", label: "Registered", tone: "accent" },
  { key: "firstPlacement", label: "First placement", tone: "teal" },
  { key: "lastPlacementEnded", label: "Last placement ended", tone: "teal" },
  { key: "drainRequested", label: "Drain requested", tone: "amber" },
  { key: "terminateRequested", label: "Terminate requested", tone: "amber" },
  { key: "lost", label: "Lost", tone: "red" },
  { key: "terminated", label: "Terminated", tone: "neutral" },
];

/**
 * Host times are instants; each stage runs from its stamp to the next one
 * that happened. An ended host's (terminated or lost) last stamp is where
 * it ended: a point, not a span, so the axis stops there. A live host's
 * last stage is still in progress.
 */
export function hostStages(h: Host): TimelineStage[] {
  const stamped = HOST_TIMES.map((t) => ({ ...t, at: h.times[t.key] ? Date.parse(h.times[t.key]!) : null })).filter((t) => t.at != null && Number.isFinite(t.at));
  stamped.sort((a, b) => a.at! - b.at!);
  const ended = h.state === "terminated" || h.state === "lost";
  return stamped.map((t, i) => {
    const next = stamped[i + 1];
    if (!next && ended) return { key: t.key, label: t.label, start: t.at, point: true, tone: t.tone };
    return { key: t.key, label: t.label, start: t.at, end: next ? next.at : null, tone: t.tone };
  });
}

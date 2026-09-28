// Run and host states → hue family + label. The hue is one of the --st-* token
// families in tokens.css; the pill never carries meaning by color alone (label
// and a dot are always present, and "live" states pulse).

export type StateHue = "neutral" | "blue" | "teal" | "green" | "amber" | "red" | "violet";

/** Run states luxd sets. A running run's activity (busy/idle) refines its pill. */
export type RunState =
  | "submitted"
  | "scheduled"
  | "provisioning"
  | "starting"
  | "running"
  | "stopping"
  | "stopped"
  | "resuming"
  | "succeeded"
  | "failed"
  | "cancelled"
  | "lost";

/** Host states luxd sets (draining is also a flag on a host in another state). */
export type HostState = "provisioning" | "ready" | "draining" | "lost" | "terminated";

export interface StateStyle {
  hue: StateHue;
  label: string;
  /** Animated dot: the thing is alive and changing. */
  live?: boolean;
}

const RUN_STATES: Record<RunState, StateStyle> = {
  submitted: { hue: "neutral", label: "Submitted" },
  scheduled: { hue: "blue", label: "Scheduled" },
  provisioning: { hue: "neutral", label: "Provisioning", live: true },
  starting: { hue: "blue", label: "Starting", live: true },
  running: { hue: "teal", label: "Running", live: true },
  stopping: { hue: "amber", label: "Stopping", live: true },
  stopped: { hue: "neutral", label: "Stopped" },
  resuming: { hue: "blue", label: "Resuming", live: true },
  succeeded: { hue: "green", label: "Succeeded" },
  failed: { hue: "red", label: "Failed" },
  cancelled: { hue: "neutral", label: "Cancelled" },
  lost: { hue: "red", label: "Lost" },
};

const HOST_STATES: Record<HostState, StateStyle> = {
  provisioning: { hue: "neutral", label: "Provisioning", live: true },
  ready: { hue: "green", label: "Ready" },
  draining: { hue: "amber", label: "Draining", live: true },
  lost: { hue: "red", label: "Lost" },
  terminated: { hue: "neutral", label: "Terminated" },
};

export const RUN_STATE_LIST = Object.keys(RUN_STATES) as RunState[];
export const HOST_STATE_LIST = Object.keys(HOST_STATES) as HostState[];

const UNKNOWN: StateStyle = { hue: "neutral", label: "Unknown" };

export function runStateStyle(state: string): StateStyle {
  return RUN_STATES[state as RunState] ?? { ...UNKNOWN, label: state };
}

export function hostStateStyle(state: string): StateStyle {
  return HOST_STATES[state as HostState] ?? { ...UNKNOWN, label: state };
}

/**
 * A cost's status as luxd reports it (GET /v1/runs/{id}/cost `status`).
 * "complete" is every source answered but some line may still change, which
 * reads as an estimate.
 */
export type CostStatus = "pending" | "complete" | "incomplete" | "final";

export interface CostStatusStyle {
  hue: StateHue;
  /** Badge tone of the same meaning. */
  tone: "neutral" | "info" | "warn" | "success";
  label: string;
  description: string;
}

const COST_STATUSES: Record<CostStatus, CostStatusStyle> = {
  pending: { hue: "neutral", tone: "neutral", label: "Pending", description: "No cost has been reported yet." },
  complete: { hue: "blue", tone: "info", label: "Estimate", description: "Every source has answered; some amounts may still change." },
  incomplete: { hue: "amber", tone: "warn", label: "Incomplete", description: "A source has not answered yet, or failed: the total is missing its part." },
  final: { hue: "green", tone: "success", label: "Final", description: "Every source has settled: these amounts will not change." },
};

export const COST_STATUS_LIST = Object.keys(COST_STATUSES) as CostStatus[];

export function costStatusStyle(status: string): CostStatusStyle {
  return COST_STATUSES[status as CostStatus] ?? { hue: "neutral", tone: "neutral", label: status, description: "" };
}

/* ---------- cost families ---------- */

/**
 * Categorical chart slot (--chart-N) per colour name a cost plugin may hint.
 * Slot 1 (blue) is compute's alone: blue hints take the nearest other slot.
 */
const HINT_SLOTS: Record<string, number> = {
  blue: 7, sky: 7, navy: 7, cyan: 3,
  orange: 2, coral: 2,
  teal: 3, mint: 3, emerald: 3,
  amber: 4, yellow: 4, gold: 4,
  pink: 5, magenta: 5, rose: 5,
  green: 6, lime: 6, olive: 6,
  violet: 7, purple: 7, indigo: 7,
  red: 8, crimson: 8,
};

/** Hue (degrees) of each slot, for the nearest match of a hex hint. */
const SLOT_HUES: [number, number][] = [
  [2, 17], [3, 158], [4, 41], [5, 337], [6, 120], [7, 249], [8, 0],
];

/** The slot compute always gets: the first, so it never changes colour. */
export const COMPUTE_SLOT = 1;

function hexHue(hex: string): number | null {
  const m = /^#?([0-9a-f]{6})$/i.exec(hex.trim());
  if (!m) return null;
  const n = parseInt(m[1]!, 16);
  const [r, g, b] = [(n >> 16) & 255, (n >> 8) & 255, n & 255].map((c) => c / 255) as [number, number, number];
  const max = Math.max(r, g, b);
  const d = max - Math.min(r, g, b);
  if (d === 0) return null; // grey: no hue to match
  const h = max === r ? ((g - b) / d) % 6 : max === g ? (b - r) / d + 2 : (r - g) / d + 4;
  return (h * 60 + 360) % 360;
}

/**
 * The chart slot (1..8) of a cost family: compute is fixed to slot 1; a
 * plugin's colour hint (a name, or #rrggbb matched by nearest hue) picks a
 * slot; otherwise the family's name picks one of slots 2..8, so a family
 * keeps its colour wherever it appears. Raw colour values are never used.
 */
export function familySlot(family: string, hint?: string | null): number {
  if (family === "compute") return COMPUTE_SLOT;
  const h = hint?.trim().toLowerCase();
  if (h) {
    const named = HINT_SLOTS[h];
    if (named) return named;
    const hue = hexHue(h);
    if (hue != null) {
      let best = SLOT_HUES[0]!;
      for (const s of SLOT_HUES) {
        const dist = (x: number) => Math.min(Math.abs(x - hue), 360 - Math.abs(x - hue));
        if (dist(s[1]) < dist(best[1])) best = s;
      }
      return best[0];
    }
  }
  let hash = 0;
  for (const c of family) hash = (hash * 31 + c.charCodeAt(0)) >>> 0;
  return 2 + (hash % 7);
}

/** familySlot as a CSS colour: var(--chart-N). */
export function familyColor(family: string, hint?: string | null): string {
  return `var(--chart-${familySlot(family, hint)})`;
}

/**
 * Colours for the families shown together (a chart, a breakdown): compute
 * and hinted families keep their slot; an unhinted family whose name-derived
 * slot is taken moves to the next free one, so two families on one chart
 * never share a colour while 8 slots last. Keyed by family.
 */
export function familyColors(families: readonly { family: string; color?: string | null }[]): Map<string, string> {
  const out = new Map<string, string>();
  const used = new Set<number>();
  const fixed = families.filter((f) => f.family === "compute" || f.color);
  const free = families.filter((f) => !(f.family === "compute" || f.color));
  for (const f of fixed) {
    if (out.has(f.family)) continue;
    const s = familySlot(f.family, f.color);
    used.add(s);
    out.set(f.family, `var(--chart-${s})`);
  }
  for (const f of free) {
    if (out.has(f.family)) continue;
    let s = familySlot(f.family);
    for (let k = 0; k < 7 && used.has(s); k++) s = 2 + ((s - 1) % 7);
    used.add(s);
    out.set(f.family, `var(--chart-${s})`);
  }
  return out;
}

export interface FamilyInfo {
  family: string;
  /** The plugin describe's displayName; compute is "Compute". */
  displayName?: string | null;
  /** The plugin describe's colour hint. */
  color?: string | null;
}

/**
 * Label and colour of each family shown together: displayName (the family
 * key when there is none) and familyColors() of the hints. Every view of
 * cost families resolves them here, so one family reads the same everywhere.
 */
export function familyDisplay(families: readonly FamilyInfo[]): Map<string, { label: string; color: string }> {
  const colors = familyColors(families);
  const out = new Map<string, { label: string; color: string }>();
  for (const f of families) {
    if (!out.has(f.family)) out.set(f.family, { label: f.displayName || (f.family === "compute" ? "Compute" : f.family), color: colors.get(f.family)! });
  }
  return out;
}

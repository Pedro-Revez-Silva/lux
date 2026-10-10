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
  | "terminated"
  | "lost";

/**
 * Host states luxd sets (draining is also a flag on a host in another
 * state), and launch_failed: a terminated host whose launch the provider
 * refused (its launch outcome), shown apart from one that ran and ended.
 */
export type HostState = "provisioning" | "ready" | "draining" | "lost" | "terminated" | "launch_failed";

export interface StateStyle {
  hue: StateHue;
  label: string;
  /** Animated dot: the thing is alive and changing. */
  live?: boolean;
  /** Drawn as an outline in its hue: an outcome rather than a state it held. */
  outline?: boolean;
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
  terminated: { hue: "neutral", label: "Terminated" },
  lost: { hue: "red", label: "Lost" },
};

const HOST_STATES: Record<HostState, StateStyle> = {
  provisioning: { hue: "neutral", label: "Provisioning", live: true },
  ready: { hue: "green", label: "Ready" },
  draining: { hue: "amber", label: "Draining", live: true },
  lost: { hue: "red", label: "Lost" },
  terminated: { hue: "neutral", label: "Terminated" },
  launch_failed: { hue: "red", label: "Launch failed", outline: true },
};

/** The state a host row shows: launch_failed for a terminated host whose launch failed, else its state. */
export function hostDisplayState(h: { state: string; launch?: { outcome?: string | null } | null }): string {
  return h.state === "terminated" && h.launch?.outcome === "failed" ? "launch_failed" : h.state;
}

/** States of a Run's server (a named port, optionally with a command lux starts). */
export type ServerState = "stopped" | "starting" | "ready" | "unreachable" | "exited";

const SERVER_STATES: Record<ServerState, StateStyle> = {
  stopped: { hue: "neutral", label: "Stopped" },
  starting: { hue: "blue", label: "Starting", live: true },
  ready: { hue: "green", label: "Ready" },
  unreachable: { hue: "amber", label: "Unreachable", live: true },
  exited: { hue: "red", label: "Exited" },
};

/**
 * A server's state as the tenant's server list shows it (GET /v1/servers):
 * derived from its process, its Run and an open wake.
 */
export type ServedState = "ready" | "waking" | "asleep" | "stopped" | "unreachable" | "exited" | "no answer";

const SERVED_STATES: Record<ServedState, StateStyle> = {
  ready: { hue: "green", label: "Ready" },
  waking: { hue: "blue", label: "Waking", live: true },
  asleep: { hue: "violet", label: "Asleep" },
  stopped: { hue: "neutral", label: "Stopped" },
  unreachable: { hue: "amber", label: "Unreachable", live: true },
  exited: { hue: "red", label: "Exited" },
  "no answer": { hue: "amber", label: "No answer", outline: true },
};

export const SERVED_STATE_LIST = Object.keys(SERVED_STATES) as ServedState[];

export function servedStateStyle(state: string): StateStyle {
  return SERVED_STATES[state as ServedState] ?? { ...UNKNOWN, label: state };
}

export const RUN_STATE_LIST = Object.keys(RUN_STATES) as RunState[];
export const HOST_STATE_LIST = Object.keys(HOST_STATES) as HostState[];
export const SERVER_STATE_LIST = Object.keys(SERVER_STATES) as ServerState[];

const UNKNOWN: StateStyle = { hue: "neutral", label: "Unknown" };

export function runStateStyle(state: string): StateStyle {
  return RUN_STATES[state as RunState] ?? { ...UNKNOWN, label: state };
}

export function hostStateStyle(state: string): StateStyle {
  return HOST_STATES[state as HostState] ?? { ...UNKNOWN, label: state };
}

export function serverStateStyle(state: string): StateStyle {
  return SERVER_STATES[state as ServerState] ?? { ...UNKNOWN, label: state };
}

/** A server whose process (or port) is up, or on its way up: stop and restart apply, start does not. */
export function isServerUp(state: string): boolean {
  return state === "starting" || state === "ready" || state === "unreachable";
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
 * The chart slot (1..8) of a cost family: lux's own families are fixed
 * (compute slot 1, block storage slot 3); a
 * plugin's colour hint (a name, or #rrggbb matched by nearest hue) picks a
 * slot; otherwise the family's name picks one of slots 2..8, so a family
 * keeps its colour wherever it appears. Raw colour values are never used.
 */
export function familySlot(family: string, hint?: string | null): number {
  if (family === "compute") return 1;
  if (family === "block-storage") return 3;
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

export function familyColor(family: string, hint?: string | null): string {
  return `var(--chart-${familySlot(family, hint)})`;
}

// Every chart slot but compute's.
const BAND_SLOTS: readonly number[] = [1, 2, 3, 4, 5, 6, 7, 8].filter((s) => s !== familySlot("compute"));

/**
 * The colour of the breakdown band at rank i (0-based; label values, API
 * keys, pools, tenants, Runs): never compute's slot, so compute's colour
 * means compute alone. Past BAND_SLOTS.length bands the slots repeat;
 * Other and "(none)" are not bands here (OTHER_BAND_COLOR, NONE_BAND_COLOR).
 */
export function bandColor(i: number): string {
  return `var(--chart-${BAND_SLOTS[i % BAND_SLOTS.length]})`;
}

/** Every value folded past the top ones: neutral, darker than (none)'s. */
export const OTHER_BAND_COLOR = "var(--st-neutral-fg)";
/** Cost with no value for the breakdown ("(none)"): the neutral dot. */
export const NONE_BAND_COLOR = "var(--st-neutral-dot)";

export interface FamilyInfo {
  family: string;
  displayName?: string | null;
  color?: string | null;
}

/**
 * Label and colour of each family shown together: displayName (the family
 * key when there is none) and familyColor() of its hint. Every view of
 * cost families resolves them here, so one family reads the same everywhere.
 */
export function familyDisplay(families: readonly FamilyInfo[]): Map<string, { label: string; color: string }> {
  const out = new Map<string, { label: string; color: string }>();
  for (const f of families) {
    if (!out.has(f.family)) out.set(f.family, { label: f.displayName || FAMILY_LABELS[f.family] || f.family, color: familyColor(f.family, f.color) });
  }
  return out;
}

// Labels of lux's own families, when luxd's displayName is absent.
const FAMILY_LABELS: Record<string, string> = { compute: "Compute", "block-storage": "Block storage" };

/* ---------- stored bytes by blob kind ---------- */

// Charted blob kinds; context is sampled but has no writer yet.
export type StorageKind = "volume" | "output" | "artifact";

/**
 * Label and chart slot of each kind. Fixed per kind, so a kind keeps its
 * colour whichever kinds a view shows; slot 1 is left to compute.
 */
const STORAGE_KINDS: Record<StorageKind, { label: string; slot: number }> = {
  volume: { label: "Snapshots", slot: 3 },
  output: { label: "Output", slot: 7 },
  artifact: { label: "Artifacts", slot: 4 },
};

/** Every kind, in stacking order (bottom first). */
export const STORAGE_KIND_LIST = Object.keys(STORAGE_KINDS) as StorageKind[];

/** A kind's label and colour; an unknown kind gets its key and the neutral dot. */
export function storageKindStyle(kind: string): { label: string; color: string } {
  const k = STORAGE_KINDS[kind as StorageKind];
  return k ? { label: k.label, color: `var(--chart-${k.slot})` } : { label: kind, color: "var(--st-neutral-dot)" };
}

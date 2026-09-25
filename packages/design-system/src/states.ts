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

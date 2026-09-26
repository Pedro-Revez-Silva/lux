// Mirrors of the luxd JSON types (internal/server/*.go). Times are RFC 3339
// strings; optional fields are omitted by the server when empty.

export interface ApiErrorBody {
  error: { code: string; message: string; details?: unknown };
}

export interface Resources {
  cpus: number;
  memory: number;
  disk: number;
  runs: number;
}

/** spec.Resources: what a Run asked for. */
export interface SpecResources {
  cpus?: number;
  memory?: number;
  disk?: number;
  pids?: number;
}

export interface RunSpec {
  name?: string;
  labels?: Record<string, string>;
  image: { ref?: string; build?: { containerfile: string; context?: string; args?: Record<string, string> } };
  workload: { adapter: string; command?: string[]; prompt?: string; workdir?: string; user?: string; tty?: boolean };
  resources: SpecResources;
  placement: { pool?: string; requires?: Record<string, string>; prefers?: Record<string, string> };
  [key: string]: unknown;
}

export interface SecretRef {
  name: string;
  fingerprint: string;
}

export interface Placement {
  epoch: number;
  /** Host id. */
  host: string;
  hostName: string;
  state: string;
  exitCode?: number;
  exitReason?: string;
  stopReason?: string;
  assignedAt: string;
  acceptedAt?: string;
  imageReadyAt?: string;
  volumesRestoredAt?: string;
  containerStartedAt?: string;
  workloadStartedAt?: string;
  stopRequestedAt?: string;
  exitedAt?: string;
  snapshotDoneAt?: string;
  uploadedAt?: string;
  peakMemoryBytes?: number;
  peakDiskBytes?: number;
  peakPids?: number;
  cpuSeconds?: number;
  netRxBytes?: number;
  netTxBytes?: number;
  snapshotBytes?: number;
}

export interface RunUsage {
  peakMemoryBytes: number;
  peakDiskBytes: number;
  peakPids: number;
  cpuSeconds: number;
  netRxBytes: number;
  netTxBytes: number;
  placements: number;
  queueSeconds?: number;
}

export interface Resumability {
  snapshot?: string;
  uploaded: boolean;
  /** Names of the hosts holding a local copy. */
  onHosts?: string[];
  secrets?: string[];
  secretsHeld: boolean;
  blockers?: string[];
}

export interface Run {
  id: string;
  /** The owning tenant's name. */
  tenant: string;
  name?: string;
  labels: Record<string, string>;
  state: string;
  stateReason?: string;
  activity?: string;
  exitCode?: number;
  epoch: number;
  sessionId?: string;
  snapshotId?: string;
  /** Name of the host holding the current placement. */
  host?: string;
  /** Id of that host: link with this, names can be reused. */
  hostId?: string;
  spec: RunSpec;
  image?: { containerfile: string; imageId: string };
  secrets: SecretRef[];
  createdAt: string;
  firstScheduledAt?: string;
  firstStartedAt?: string;
  finishedAt?: string;
  placements?: Placement[];
  usage?: RunUsage;
  resume?: Resumability;
}

export interface Event {
  id: number;
  epoch?: number;
  type: string;
  data: Record<string, unknown>;
  time: string;
}

export interface FeedEvent extends Event {
  runId: string;
  tenant: string;
}

export interface OutputRecord {
  cursor: string;
  epoch: number;
  seq: number;
  /** Unix ms. */
  t: number;
  ch: string;
  data?: string;
  event?: unknown;
}

export interface Snapshot {
  id: string;
  epoch: number;
  manifest: { snapshotId: string; runId: string; epoch: number; sessionId: string; volumes: { name: string; path: string; blobId: string; size: number; sha256: string }[] };
  available: boolean;
  onHost?: string;
  uploaded: boolean;
  createdAt: string;
}

export interface Artifact {
  id: string;
  epoch: number;
  path: string;
  contentType: string;
  size: number;
  sha256: string;
  available: boolean;
  createdAt: string;
}

export interface HostPlacement {
  runId: string;
  runName?: string;
  tenant: string;
  epoch: number;
  state: string;
  resources: SpecResources;
  since: string;
}

export type HostTimeKey = "provisionRequested" | "provisioned" | "registered" | "firstPlacement" | "lastPlacementEnded" | "drainRequested" | "terminateRequested" | "terminated" | "lost" | "created";

export interface Host {
  id: string;
  name: string;
  /** Owning tenant's name; empty for a platform host. */
  tenant?: string;
  pool: string;
  state: string;
  stateReason?: string;
  draining: boolean;
  labels: Record<string, string>;
  capacity: Resources;
  allocated: SpecResources;
  versions: Record<string, unknown>;
  platform: boolean;
  liveRuns: number;
  providerId?: string;
  lastHeartbeat?: string;
  times: Record<HostTimeKey, string | null>;
  placements?: HostPlacement[];
}

export interface Pool {
  name: string;
  tenant?: string;
  provider: string;
  template?: Record<string, unknown>;
  minHosts: number;
  maxHosts: number;
  warmHosts: number;
  shared: boolean;
  platform: boolean;
}

/** GET /v1/whoami: who the key belongs to. */
export interface WhoAmI {
  operator: boolean;
  /** Tenant name; "" for operators. */
  tenant: string;
  /** "" for operators. */
  tenantId: string;
  keyId?: string;
  /** A person signed in through luxd's console auth (no key). */
  email?: string;
  name?: string;
  scopes: string[];
  /** key: the console needs an API key; cloudflare-access: Access signs people in. */
  consoleAuth: "key" | "cloudflare-access";
}

export interface Tenant {
  id: string;
  name: string;
  retentionDays: number;
  maxConcurrentRuns?: number;
  maxHosts?: number;
  maxStorageBytes?: number;
  activeRuns: number;
  runs: number;
  hosts: number;
  storedBytes: number;
  createdAt: string;
}

export interface Percentiles {
  n: number;
  p50?: number;
  p95?: number;
  max?: number;
}

export interface Status {
  runs: Record<string, number>;
  busy: number;
  idle: number;
  queued: number;
  oldestQueuedAt?: string;
  startLatency: Percentiles;
  hosts: Record<string, number>;
  capacity: Resources;
  allocated: Resources;
}

export interface Sample {
  at: string;
  cpuCores?: number;
  memoryBytes?: number;
  diskBytes?: number;
  placements?: number;
  allocCpus?: number;
  allocMemory?: number;
  pids?: number;
  netRxRate?: number;
  netTxRate?: number;
  epoch?: number;
  runs?: Record<string, number>;
  busy?: number;
  idle?: number;
  queued?: number;
  started?: number;
  finished?: number;
  startP50?: number;
  startP95?: number;
  hosts?: Record<string, number>;
  capacityCpus?: number;
  capacityMemory?: number;
  allocatedCpus?: number;
  allocatedMemory?: number;
  /** Only for an operator reading the whole system. */
  control?: ControlSample;
}

/** The machine luxd runs on, and its Postgres. */
export interface ControlSample {
  cpuCores?: number;
  cpus?: number;
  memoryBytes?: number;
  memoryTotal?: number;
  databaseBytes?: number;
  databaseConnections?: number;
  disks?: DiskSample[];
}

export interface DiskSample {
  path: string;
  usedBytes: number;
  /** What an unprivileged process can still write. */
  freeBytes: number;
  totalBytes: number;
}

export interface History {
  from: string;
  to: string;
  /** Seconds per sample; 0 is raw. */
  resolution: number;
  samples: Sample[];
}

export interface ResumeRequest {
  secrets?: { name: string; value: string }[];
  input?: { text: string };
  fromSnapshot?: string;
  to?: string;
  /** Raise the Run's disk limit from now on: bytes, or a size ("40Gi"). */
  resources?: { disk?: number | string };
}

export interface MigrateRequest {
  to?: string;
  input?: { text: string };
}

export interface RunListParams {
  state?: string[];
  resumable?: boolean;
  host?: string;
  label?: string;
  before?: string;
  limit?: number;
}

export interface HostListParams {
  all?: boolean;
  pool?: string;
  state?: string;
}

/** Run states that never change again. */
export const TERMINAL_RUN_STATES = new Set(["succeeded", "failed", "cancelled"]);
/** What POST /input accepts. */
export const INPUT_RUN_STATES = new Set(["starting", "running"]);
/** What POST /resume accepts. */
export const RESUMABLE_RUN_STATES = new Set(["stopped", "lost", "failed"]);

/** Still changing: worth polling. */
export function isRunActive(state: string): boolean {
  return !TERMINAL_RUN_STATES.has(state) && state !== "stopped" && state !== "lost";
}

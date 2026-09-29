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
  workload: { adapter: string; command?: string[]; prompt?: string; workdir?: string; user?: string; tty?: boolean; servers?: SpecServer[] };
  resources: SpecResources;
  placement: { pool?: string; requires?: Record<string, string>; prefers?: Record<string, string> };
  [key: string]: unknown;
}

/** workload.servers[]: a server declared in the spec. */
export interface SpecServer {
  name: string;
  port: number;
  command?: string[];
  workdir?: string;
  env?: Record<string, string>;
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
  /** The Run's servers (named ports, optionally with a command lux starts). */
  servers?: Server[];
  /** In GET /v1/runs only (not GET /v1/runs/{id}); absent means unknown, not zero. */
  cost?: RunCostBrief;
}

export type ServerState = "stopped" | "starting" | "ready" | "unreachable" | "exited";

/** A Run's server: a named port, optionally with a command lux starts in the container. */
export interface Server {
  name: string;
  port: number;
  command?: string[] | null;
  workdir?: string;
  env?: Record<string, string>;
  /** Declared in the spec (auto-started on every start of the Run). */
  fromSpec: boolean;
  state: ServerState;
  /** When exited. */
  exitCode?: number;
  /** The last stderr line on exit, if any. */
  error?: string;
  /** When the state last changed. */
  since: string;
  /** When it last became ready. */
  readySince?: string | null;
  /** Why it is stopped. */
  stopReason?: "stopped" | "run stopped" | "migrated" | "host lost" | null;
  /** Placement epoch it stopped in (null if never started). */
  stoppedEpoch?: number | null;
  /** Placement epoch of the current state. */
  epoch: number;
  /** The preview URL; null when previews are not configured. */
  url?: string | null;
  /** When the preview last proxied a request for it. */
  lastRequestAt?: string | null;
}

/** POST /v1/runs/{id}/servers; PUT takes the same minus name. */
export interface ServerInput {
  name: string;
  port: number;
  command?: string[];
  workdir?: string;
  env?: Record<string, string>;
  /** Start it now; defaults to true when a command is set. */
  start?: boolean;
}

/** GET /v1/runs/{id}/servers/{name}/log: one line of the server's output. */
export interface ServerLogLine {
  /** Unix ms. */
  t: number;
  stream: "stdout" | "stderr";
  text: string;
}

/** POST /v1/runs/{id}/tickets: a single-use token for a browser's WebSocket or preview. */
export interface StreamTicket {
  ticket: string;
  kind: "exec" | "preview";
  runId: string;
  expiresAt: string;
}

/** RunCostBrief in internal/server/costs.go: GET /v1/runs/{id}/cost's status and totals. */
export interface RunCostBrief {
  status: CostStatus;
  /** Per currency, ordered by currency, each with its final and estimate parts; empty while pending. */
  totals: CostTotal[];
}

export interface MoneyTotal {
  currency: string;
  /** A decimal string; never converted between currencies. */
  amount: string;
}

/** pending: nothing reported yet; complete: all answered, some estimates; incomplete: a source is missing; final: settled. */
export type CostStatus = "pending" | "complete" | "incomplete" | "final";

/** CostLine in internal/server/costs.go. */
export interface CostLine {
  runId: string;
  /** compute, or a cost plugin's configured name. */
  source: string;
  family: string;
  /** Empty when the source gives none. */
  item: string;
  amount: string;
  currency: string;
  from: string;
  to: string;
  /** false: an estimate that may still change. */
  final: boolean;
  details: Record<string, unknown> | null;
  reportedAt: string;
}

/** CostTotal: per currency in totals, per family and currency in byFamily. */
export interface CostTotal {
  family?: string;
  displayName?: string;
  /** A plugin's colour hint; mapped to a chart token, never used raw. */
  color?: string;
  currency: string;
  /** final + estimate. */
  amount: string;
  final: string;
  estimate: string;
}

export interface CostSource {
  source: string;
  status: "ok" | "incomplete" | "final";
  answeredAt?: string;
  nextAt?: string;
}

/** GET /v1/runs/{id}/cost. */
export interface RunCost {
  runId: string;
  status: CostStatus;
  final: boolean;
  /** list: list prices, before discounts, credits and tax. */
  basis: string;
  totals: CostTotal[];
  byFamily: CostTotal[];
  lines: CostLine[];
  sources: CostSource[];
}

export interface CostSummaryRow {
  /** With interval: the bucket's start. */
  at?: string;
  /** With group: the group's value per group key ("(none)" when absent). */
  group?: Record<string, string>;
  currency: string;
  amount: string;
}

export interface HostAllocation {
  hostId: string;
  currency: string;
  allocated: string;
  unallocated: string;
}

/** GET /v1/costs. */
export interface CostSummary {
  from: string;
  to: string;
  basis: string;
  totals: CostSummaryRow[];
  series?: CostSummaryRow[];
  /** Operators without a tenant scope only. */
  unallocated?: CostSummaryRow[];
  /** Operators without a tenant scope, grouped by host. */
  hosts?: HostAllocation[];
  /** Grouped by family: each family's displayName and colour hint, as in a Run's byFamily. */
  families?: CostFamily[];
  /** Grouped by run: each Run's name (empty when it has none). */
  runs?: { id: string; name?: string }[];
}

export interface CostFamily {
  family: string;
  displayName?: string;
  color?: string;
}

export interface CostSummaryParams {
  /** Up to two: tenant (operators), pool, host, family, run, label:key. */
  group?: string[];
  family?: string;
  interval?: "hour" | "day";
  since?: string;
  from?: string;
  to?: string;
}

export interface HostCostHour {
  hour: string;
  currency: string;
  allocated: string;
  /** Operators only. */
  unallocated?: string;
}

export interface HostCostRate {
  from: string;
  to?: string;
  perHour: string;
  currency: string;
  /** static, or the provider's price source (e.g. ec2-pricing, ec2-spot-history). */
  source: string;
}

/** GET /v1/hosts/{id}/cost. */
export interface HostCost {
  hostId: string;
  from: string;
  to: string;
  basis: string;
  hours: HostCostHour[];
  /** Operators only. */
  rates?: HostCostRate[];
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
  instanceType?: string;
  zone?: string;
  market?: "on-demand" | "spot";
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
  /** Their photo's URL, from the identity provider (https). */
  picture?: string;
  scopes: string[];
  /** key: the console needs an API key; cloudflare-access: Access signs people in. */
  consoleAuth: "key" | "cloudflare-access";
  /** Where preview URLs are (https://<server>-<run suffix>.<previewDomain>); null when previews are off, or signed in to through Cloudflare Access. */
  previewDomain?: string | null;
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
  /** Hosts: the runner process itself (absent before the runner reported it). */
  runner?: ProcessSample;
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
}

/** The control host by what each figure is of: a luxd restart starts a new luxd series; a machine's and Postgres's go on. */
export interface Control {
  /** A series per machine luxd ran on, oldest first. */
  machines: MachineSeries[];
  /** lux's Postgres database: one series. */
  postgres: PostgresPoint[];
  /** A series per luxd process (a restart is a new one), oldest first. */
  luxd: LuxdSeries[];
}

export interface MachineSeries {
  hostname: string;
  samples: MachinePoint[];
}

export interface MachinePoint {
  at: string;
  cpuCores?: number;
  cpus?: number;
  memoryBytes?: number;
  memoryTotal?: number;
  disks?: DiskSample[];
}

export interface PostgresPoint {
  at: string;
  bytes?: number;
  connections?: number;
}

export interface LuxdSeries {
  /** The process's id, new at each start (rows from before process ids: its hostname). */
  instance: string;
  /** The machine it runs on. */
  hostname: string;
  samples: (ProcessSample & { at: string })[];
}

/** One of lux's own processes: luxd, or a host's runner (not the podman and conmon processes it starts). */
export interface ProcessSample {
  /** When the process started: a change is a restart. */
  started: string;
  /** Cores, a rate over the previous point of the same process. */
  cpuCores?: number;
  rssBytes?: number;
  /** The highest RSS since the previous sample (a rollup: in its interval). */
  peakRssBytes?: number;
  /** Go heap objects, live or not yet swept. */
  heapBytes?: number;
  goroutines?: number;
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
  /** Only for an operator reading the whole system. */
  control?: Control;
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
/** What the exec stream (a terminal) and a server's start/stop/restart need. */
export const EXEC_RUN_STATES = new Set(["running"]);

/** Still changing: worth polling. */
export function isRunActive(state: string): boolean {
  return !TERMINAL_RUN_STATES.has(state) && state !== "stopped" && state !== "lost";
}

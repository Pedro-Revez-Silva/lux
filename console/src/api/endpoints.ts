// Typed calls, one per endpoint. Lists are unwrapped from their envelope.
import { download, request } from "./client.ts";
import type { Artifact, CostSummary, CostSummaryParams, Event, History, Host, HostCost, HostListParams, LifecycleEvent, MigrateRequest, Page, PageParams, Pool, PoolCost, PoolMetrics, PoolStats, ResumeRequest, Run, RunCost, RunListParams, Server, ServerInput, ServerLogLine, Snapshot, Status, StreamTicket, Tenant, WhoAmI } from "./types.ts";

type Sig = AbortSignal | undefined;
/** Tenant scope of a list call: a tenant id or name, or undefined for all the key sees. */
type Scope = string | undefined;

export const api = {
  whoami: (signal?: Sig) => request<WhoAmI>("/whoami", { signal }),
  status: (tenant: Scope, signal?: Sig) => request<Status>("/status", { tenant, signal }),
  history: (tenant: Scope, since: string, signal?: Sig) => request<History>("/history", { tenant, query: { since }, signal }),

  runs: (tenant: Scope, p: RunListParams, signal?: Sig) =>
    request<{ runs: Run[] }>("/runs", { tenant, query: { state: p.state?.join(","), resumable: p.resumable, host: p.host, label: p.label, before: p.before, limit: p.limit }, signal }).then((r) => r.runs),
  /** A page of Runs in a sort's order (server-side, across every match). */
  runsPage: (tenant: Scope, p: RunListParams, signal?: Sig) =>
    request<{ runs: Run[]; next?: string; prev?: string; page?: string }>("/runs", { tenant, query: { state: p.state?.join(","), resumable: p.resumable, host: p.host, label: p.label, limit: p.limit, ...pageQuery(p) }, signal }).then(
      (r): Page<Run> => ({ rows: r.runs, next: r.next, prev: r.prev, page: r.page }),
    ),
  run: (id: string, signal?: Sig) => request<Run>(`/runs/${enc(id)}`, { signal }),
  runHistory: (id: string, since: string, signal?: Sig) => request<History>(`/runs/${enc(id)}/history`, { query: { since }, signal }),
  runEvents: (id: string, after = 0, signal?: Sig) => request<{ events: Event[] }>(`/runs/${enc(id)}/events`, { query: { after }, signal }).then((r) => r.events),
  snapshots: (id: string, signal?: Sig) => request<{ snapshots: Snapshot[] }>(`/runs/${enc(id)}/snapshots`, { signal }).then((r) => r.snapshots),
  artifacts: (id: string, signal?: Sig) => request<{ artifacts: Artifact[] }>(`/runs/${enc(id)}/artifacts`, { signal }).then((r) => r.artifacts),
  downloadArtifact: (a: Artifact) => download(`/artifacts/${enc(a.id)}`, a.path.split("/").pop() || a.id),
  runCost: (id: string, signal?: Sig) => request<RunCost>(`/runs/${enc(id)}/cost`, { signal }),
  costs: (tenant: Scope, p: CostSummaryParams, signal?: Sig) =>
    request<CostSummary>("/costs", { tenant, query: { group: p.group, family: p.family, interval: p.interval, since: p.since, from: p.from, to: p.to }, signal }),

  stopRun: (id: string) => request<Run>(`/runs/${enc(id)}/stop`, { method: "POST" }),
  cancelRun: (id: string) => request<Run>(`/runs/${enc(id)}/cancel`, { method: "POST" }),
  resumeRun: (id: string, body: ResumeRequest) => request<Run>(`/runs/${enc(id)}/resume`, { method: "POST", body }),
  migrateRun: (id: string, body: MigrateRequest) => request<Run>(`/runs/${enc(id)}/migrate`, { method: "POST", body }),
  inputRun: (id: string, text: string) => request<{ requestId: string }>(`/runs/${enc(id)}/input`, { method: "POST", body: { text } }),
  interruptRun: (id: string) => request<{ requestId: string }>(`/runs/${enc(id)}/input`, { method: "POST", body: { interrupt: true } }),

  /** A single-use ticket (60s) that lets a browser open the exec WebSocket or a preview without a header. */
  ticket: (id: string, kind: StreamTicket["kind"], signal?: Sig) => request<StreamTicket>(`/runs/${enc(id)}/tickets`, { method: "POST", body: { kind }, signal }),

  addServer: (id: string, body: ServerInput) => request<Server>(`/runs/${enc(id)}/servers`, { method: "POST", body }),
  startServer: (id: string, name: string) => request<Server>(`/runs/${enc(id)}/servers/${enc(name)}/start`, { method: "POST" }),
  stopServer: (id: string, name: string) => request<Server>(`/runs/${enc(id)}/servers/${enc(name)}/stop`, { method: "POST" }),
  restartServer: (id: string, name: string) => request<Server>(`/runs/${enc(id)}/servers/${enc(name)}/restart`, { method: "POST" }),
  removeServer: (id: string, name: string) => request<void>(`/runs/${enc(id)}/servers/${enc(name)}`, { method: "DELETE" }),
  serverLog: (id: string, name: string, tail = 200, signal?: Sig) => request<{ lines: ServerLogLine[] }>(`/runs/${enc(id)}/servers/${enc(name)}/log`, { query: { tail }, signal }).then((r) => r.lines ?? []),

  hosts: (tenant: Scope, p: HostListParams = {}, signal?: Sig) => request<{ hosts: Host[] }>("/hosts", { tenant, query: { all: p.all, pool: p.pool, state: p.state }, signal }).then((r) => r.hosts),
  /** A counted page of hosts in a sort's order (server-side). */
  hostsPage: (tenant: Scope, p: HostListParams, signal?: Sig) =>
    request<{ hosts: Host[]; next?: string; prev?: string; page?: string; total?: number; offset?: number }>("/hosts", {
      tenant,
      query: { all: p.all, pool: p.pool, state: p.state, lifecycle: p.lifecycle, limit: p.limit, offset: p.offset, ...pageQuery(p) },
      signal,
    }).then((r): Page<Host> => ({ rows: r.hosts, next: r.next, prev: r.prev, page: r.page, total: r.total, offset: r.offset })),
  host: (id: string, signal?: Sig) => request<Host>(`/hosts/${enc(id)}`, { signal }),
  hostHistory: (id: string, since: string, signal?: Sig) => request<History>(`/hosts/${enc(id)}/history`, { query: { since }, signal }),
  hostCost: (id: string, since: string, signal?: Sig) => request<HostCost>(`/hosts/${enc(id)}/cost`, { query: { since }, signal }),
  drainHost: (id: string, forceEvict = false) => request<{ draining: boolean; host: string }>(`/hosts/${enc(id)}/drain`, { method: "POST", body: { forceEvict } }),

  hostEvents: (id: string, tenant: Scope, page: EventRange, signal?: Sig) =>
    request<{ events: LifecycleEvent[] }>(`/hosts/${enc(id)}/events`, { tenant, query: { ...page, limit: EVENTS_PAGE }, signal }).then((r) => r.events),

  pools: (tenant: Scope, signal?: Sig) => request<{ pools: Pool[] }>("/pools", { tenant, signal }).then((r) => r.pools),
  /** Renames a pool; only its name changes. tenant: the pool's (an operator's tenant pool); platform: the platform's pool of that name. */
  renamePool: (tenant: Scope, name: string, newName: string, platform = false) =>
    request<Pool>(`/pools/${enc(name)}/rename`, { method: "POST", tenant, query: platform ? { owner: "platform" } : {}, body: { name: newName } }),
  /** Marks one of a tenant's pools as its default (tenant: the pool's, for an operator). */
  makePoolDefault: (tenant: Scope, name: string) => request<Pool>("/pools", { method: "POST", tenant, body: { name, isDefault: true } }),
  /** owner: which pool of that name, where a tenant's and the platform's share it; undefined: the server's default. */
  poolEvents: (name: string, tenant: Scope, owner: PoolOwner | undefined, page: EventRange, signal?: Sig) =>
    request<{ events: LifecycleEvent[] }>(`/pools/${enc(name)}/events`, { tenant, query: { owner, ...page, limit: EVENTS_PAGE }, signal }).then((r) => r.events),
  /** Every pool's figures over since, in one read. */
  poolStats: (tenant: Scope, since: string, signal?: Sig) => request<{ pools: PoolStats[] }>("/pools/stats", { tenant, query: { since }, signal }).then((r) => r.pools),
  poolMetrics: (name: string, tenant: Scope, owner: PoolOwner | undefined, since: string, signal?: Sig) => request<PoolMetrics>(`/pools/${enc(name)}/metrics`, { tenant, query: { owner, since }, signal }),
  poolCost: (name: string, tenant: Scope, owner: PoolOwner | undefined, since: string, interval: "hour" | "day", signal?: Sig) =>
    request<PoolCost>(`/pools/${enc(name)}/cost`, { tenant, query: { owner, since, interval }, signal }),
  /** A page of a pool's events in a sort's order. */
  poolEventsPage: (name: string, tenant: Scope, owner: PoolOwner | undefined, p: PageParams & { limit: number }, signal?: Sig) =>
    request<{ events: LifecycleEvent[]; next?: string; prev?: string; page?: string }>(`/pools/${enc(name)}/events`, { tenant, query: { owner, limit: p.limit, ...pageQuery(p) }, signal }).then(
      (r): Page<LifecycleEvent> => ({ rows: r.events, next: r.next, prev: r.prev, page: r.page }),
    ),
  tenants: (signal?: Sig) => request<{ tenants: Tenant[] }>("/tenants", { signal }).then((r) => r.tenants),
};

export type PoolOwner = "platform" | "tenant";

/** Pool and host events per request: the server's maximum. */
export const EVENTS_PAGE = 1000;

/** Which pool or host events, newest first: older than before, newer than after (both exclusive); none: the newest. */
export interface EventRange {
  before?: number;
  after?: number;
}

function pageQuery(p: PageParams) {
  return { sort: p.sort, dir: p.dir, next: p.next, prev: p.prev, at: p.at };
}

function enc(s: string): string {
  return encodeURIComponent(s);
}

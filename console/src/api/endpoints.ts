// Typed calls, one per endpoint. Lists are unwrapped from their envelope.
import { download, request } from "./client.ts";
import type { Artifact, Event, History, Host, HostListParams, MigrateRequest, Pool, ResumeRequest, Run, RunListParams, Server, ServerInput, ServerLogLine, Snapshot, Status, StreamTicket, Tenant, WhoAmI } from "./types.ts";

type Sig = AbortSignal | undefined;
/** Tenant scope of a list call: a tenant id or name, or undefined for all the key sees. */
type Scope = string | undefined;

export const api = {
  whoami: (signal?: Sig) => request<WhoAmI>("/whoami", { signal }),
  status: (tenant: Scope, signal?: Sig) => request<Status>("/status", { tenant, signal }),
  history: (tenant: Scope, since: string, signal?: Sig) => request<History>("/history", { tenant, query: { since }, signal }),

  runs: (tenant: Scope, p: RunListParams, signal?: Sig) =>
    request<{ runs: Run[] }>("/runs", { tenant, query: { state: p.state?.join(","), resumable: p.resumable, host: p.host, label: p.label, before: p.before, limit: p.limit }, signal }).then((r) => r.runs),
  run: (id: string, signal?: Sig) => request<Run>(`/runs/${enc(id)}`, { signal }),
  runHistory: (id: string, since: string, signal?: Sig) => request<History>(`/runs/${enc(id)}/history`, { query: { since }, signal }),
  runEvents: (id: string, after = 0, signal?: Sig) => request<{ events: Event[] }>(`/runs/${enc(id)}/events`, { query: { after }, signal }).then((r) => r.events),
  snapshots: (id: string, signal?: Sig) => request<{ snapshots: Snapshot[] }>(`/runs/${enc(id)}/snapshots`, { signal }).then((r) => r.snapshots),
  artifacts: (id: string, signal?: Sig) => request<{ artifacts: Artifact[] }>(`/runs/${enc(id)}/artifacts`, { signal }).then((r) => r.artifacts),
  downloadArtifact: (a: Artifact) => download(`/artifacts/${enc(a.id)}`, a.path.split("/").pop() || a.id),

  stopRun: (id: string) => request<Run>(`/runs/${enc(id)}/stop`, { method: "POST" }),
  cancelRun: (id: string) => request<Run>(`/runs/${enc(id)}/cancel`, { method: "POST" }),
  resumeRun: (id: string, body: ResumeRequest) => request<Run>(`/runs/${enc(id)}/resume`, { method: "POST", body }),
  migrateRun: (id: string, body: MigrateRequest) => request<Run>(`/runs/${enc(id)}/migrate`, { method: "POST", body }),
  inputRun: (id: string, text: string) => request<{ requestId: string }>(`/runs/${enc(id)}/input`, { method: "POST", body: { text } }),
  interruptRun: (id: string) => request<{ requestId: string }>(`/runs/${enc(id)}/input`, { method: "POST", body: { interrupt: true } }),

  /** A single-use ticket (60s) that lets a browser open the exec WebSocket or a preview without a header. */
  ticket: (id: string, kind: StreamTicket["kind"], signal?: Sig) => request<StreamTicket>(`/runs/${enc(id)}/tickets`, { method: "POST", body: { kind }, signal }),

  servers: (id: string, signal?: Sig) => request<{ servers: Server[] }>(`/runs/${enc(id)}/servers`, { signal }).then((r) => r.servers),
  addServer: (id: string, body: ServerInput) => request<Server>(`/runs/${enc(id)}/servers`, { method: "POST", body }),
  updateServer: (id: string, name: string, body: Omit<ServerInput, "name">) => request<Server>(`/runs/${enc(id)}/servers/${enc(name)}`, { method: "PUT", body }),
  startServer: (id: string, name: string) => request<Server>(`/runs/${enc(id)}/servers/${enc(name)}/start`, { method: "POST" }),
  stopServer: (id: string, name: string) => request<Server>(`/runs/${enc(id)}/servers/${enc(name)}/stop`, { method: "POST" }),
  restartServer: (id: string, name: string) => request<Server>(`/runs/${enc(id)}/servers/${enc(name)}/restart`, { method: "POST" }),
  removeServer: (id: string, name: string) => request<void>(`/runs/${enc(id)}/servers/${enc(name)}`, { method: "DELETE" }),
  serverLog: (id: string, name: string, tail = 200, signal?: Sig) => request<{ lines: ServerLogLine[] }>(`/runs/${enc(id)}/servers/${enc(name)}/log`, { query: { tail }, signal }).then((r) => r.lines ?? []),

  hosts: (tenant: Scope, p: HostListParams = {}, signal?: Sig) => request<{ hosts: Host[] }>("/hosts", { tenant, query: { all: p.all, pool: p.pool, state: p.state }, signal }).then((r) => r.hosts),
  host: (id: string, signal?: Sig) => request<Host>(`/hosts/${enc(id)}`, { signal }),
  hostHistory: (id: string, since: string, signal?: Sig) => request<History>(`/hosts/${enc(id)}/history`, { query: { since }, signal }),
  drainHost: (id: string, forceEvict = false) => request<{ draining: boolean; host: string }>(`/hosts/${enc(id)}/drain`, { method: "POST", body: { forceEvict } }),

  pools: (tenant: Scope, signal?: Sig) => request<{ pools: Pool[] }>("/pools", { tenant, signal }).then((r) => r.pools),
  tenants: (signal?: Sig) => request<{ tenants: Tenant[] }>("/tenants", { signal }).then((r) => r.tenants),
};

function enc(s: string): string {
  return encodeURIComponent(s);
}

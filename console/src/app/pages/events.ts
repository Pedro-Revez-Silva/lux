// One-line summaries of lifecycle events, from their data payloads.
import type { Event, LifecycleEvent } from "../../api/index.ts";
import { formatBytes, formatDuration } from "@lux/design-system";

function str(v: unknown): string | undefined {
  return typeof v === "string" && v !== "" ? v : undefined;
}

// Why a submitted Run got its pool (its event's poolFrom); a pool the spec named needs no note.
const POOL_FROM: Record<string, string> = {
  "tenant-default": "the tenant's default",
  "platform-default": "the platform's default",
  fallback: "no default pool marked",
};

// "pool burst (platform, the platform's default)": its owner (poolOwner, platform or tenant: a
// tenant pool and a platform pool may share the name) and why it got it.
function poolNote(d: Record<string, unknown>): string {
  const pool = str(d.pool);
  if (!pool) return "";
  const notes = [str(d.poolOwner), POOL_FROM[str(d.poolFrom) ?? ""]].filter(Boolean);
  return ` · pool ${pool}${notes.length > 0 ? ` (${notes.join(", ")})` : ""}`;
}

export function eventSummary(e: Event): string {
  const d = e.data ?? {};
  switch (e.type) {
    case "state": {
      const parts = [str(d.state) ?? "?"];
      if (str(d.reason)) parts.push(`(${str(d.reason)})`);
      if (str(d.host)) parts.push(`on ${str(d.host)}`);
      if (str(d.pool)) parts.push(`from pool ${str(d.pool)}`);
      return parts.join(" ");
    }
    case "submitted":
      return `by ${str(d.by) ?? "?"}${poolNote(d)}`;
    case "exited": {
      const code = typeof d.exitCode === "number" ? `exit ${d.exitCode}` : "exited";
      return [code, str(d.reason), str(d.message)].filter(Boolean).join(" · ");
    }
    case "snapshot":
      return `snapshot ${str(d.snapshotId) ?? ""} · ${typeof d.bytes === "number" ? `${d.bytes} bytes` : ""} · ${typeof d.volumes === "number" ? `${d.volumes} volumes` : ""}`.replace(/( · )+$/, "");
    case "snapshot.failed":
      return `snapshot failed: ${str(d.error) ?? "?"}`;
    case "activity":
      return str(d.activity) ?? "";
    case "session":
      return `session ${str(d.sessionId) ?? ""}`;
    case "input":
      return d.interrupt === true && !str(d.text) ? "interrupt" : `input: ${str(d.text) ?? `${typeof d.rawBytes === "number" ? d.rawBytes : 0} raw bytes`}`;
    case "input.delivered":
      return `input delivered${str(d.text) ? `: ${str(d.text)}` : ""}`;
    case "input.failed":
      return `input failed: ${str(d.error) ?? "?"}`;
    case "stop.requested":
    case "cancel.requested":
      return `by ${str(d.by) ?? "?"}`;
    case "migrate.requested":
      return `from ${str(d.from) ?? "?"} to ${str(d.to) ?? "any other host"}`;
    case "disk.exceeded":
      return `disk ${typeof d.usedBytes === "number" ? formatBytes(d.usedBytes) : "?"} over its limit of ${typeof d.limitBytes === "number" ? formatBytes(d.limitBytes) : "?"}`;
    case "push.requested":
      return `push ${str(d.requestId) ?? ""}`;
    case "resume.requested": {
      const added = Array.isArray(d.addedRepositories) && d.addedRepositories.length > 0 ? ` · adding ${d.addedRepositories.join(", ")}` : "";
      return `by ${str(d.by) ?? "?"}${added}`;
    }
    case "git.clone":
      return d.status === "failed"
        ? `${str(d.repo) ?? "?"} not cloned: ${str(d.error) ?? "?"}`
        : `${str(d.repo) ?? "?"} cloned at ${(str(d.commit) ?? "?").slice(0, 12)}`;
    default: {
      const keys = Object.keys(d);
      if (keys.length === 0) return "";
      return keys
        .slice(0, 4)
        .map((k) => `${k}=${typeof d[k] === "object" ? JSON.stringify(d[k]) : String(d[k])}`)
        .join(" ");
    }
  }
}

function val(v: unknown): string {
  return v == null || v === "" ? "–" : typeof v === "object" ? JSON.stringify(v) : String(v);
}

type Obj = Record<string, unknown>;

function obj(v: unknown): Obj {
  return v != null && typeof v === "object" && !Array.isArray(v) ? (v as Obj) : {};
}

function list(v: unknown): Obj[] {
  return Array.isArray(v) ? v.map(obj) : [];
}

// Memory and disk are bytes; cpus, runs and pids are counts.
function amount(resource: string, v: unknown): string {
  if (typeof v !== "number") return "?";
  return resource === "memory" || resource === "disk" ? formatBytes(v) : String(v);
}

/** Planner blockers: resource numbers in their units, or a constraint reason. */
function blockers(v: unknown): string {
  return list(v)
    .map((b) => {
      const res = str(b.resource);
      if (!res) return String(b.reason ?? "");
      return `${res} requested ${amount(res, b.requested)}, used ${amount(res, b.used)}, capacity ${amount(res, b.capacity)}, available ${amount(res, b.available)}`;
    })
    .join("; ");
}

// Sampled evidence; onHost names the actual host whose fit failed.
function deficits(v: unknown, onHost: boolean): string {
  return list(v)
    .map((e) => {
      const stage = String(e.stage ?? "").replaceAll("_", " ");
      const head = onHost ? `${String(e.host)} (${stage}) for ${String(e.run)}` : `${String(e.run)} ${stage}`;
      return `${head} [${blockers(e.blockers)}]`;
    })
    .join(", ");
}

// An expected-capacity resource of zero is an observed unlimited.
function capacity(c: Obj, resource: string): string {
  const v = c[resource];
  return typeof v !== "number" || v === 0 ? `${resource} unlimited` : `${resource} ${amount(resource, v)}`;
}

function capacityPlan(d: Obj): string {
  const n = (k: string) => (typeof d[k] === "number" ? String(d[k]) : "0");
  const parts = [`plan: ${n("ready")} ready, ${n("future")} future, ${n("planned")} planned, ${n("unmet")} unmet, ${n("blocked")} blocked`];
  if (d.probe === true) parts.push("probe: one host to re-observe capacity no expected host fits");
  const expected = d.expected == null ? undefined : obj(d.expected);
  if (expected) {
    const c = obj(expected.capacity);
    parts.push(`new host ${["cpus", "memory", "disk", "runs"].map((r) => capacity(c, r)).join(", ")} from ${String(expected.observations)} observation(s)`);
  } else if (str(d.unknown)) {
    parts.push(`new host capacity unknown: ${str(d.unknown)}`);
  }
  const def = deficits(d.deficits, false);
  if (def) parts.push(`deficits: ${def}`);
  const exh = deficits(d.exhausted, true);
  if (exh) parts.push(`exhausted: ${exh}`);
  const inel = list(d.ineligible).map((h) => `${String(h.host)} ${String(h.reason)}`);
  if (inel.length > 0) parts.push(`ineligible: ${inel.join(", ")}`);
  if (typeof d.omitted === "number" && d.omitted > 0) parts.push(`${d.omitted} more omitted`);
  return parts.join("; ");
}

/** A placement's requested resources, when its event has them. */
function resources(v: unknown): string {
  const r = obj(v);
  return ["cpus", "memory", "disk", "pids"]
    .filter((k) => typeof r[k] === "number" && r[k] !== 0)
    .map((k) => `${k} ${amount(k, r[k])}`)
    .join(", ");
}

/** One line for a pool's or a host's event (as lux pools/hosts events prints it). */
export function infraEventSummary(e: LifecycleEvent): string {
  const d = e.data ?? {};
  const s = (k: string) => (d[k] == null || d[k] === "" ? "" : String(d[k]));
  switch (e.type) {
    case "pool.scale_up": {
      const line = `+${s("hosts")} host${d.hosts === 1 ? "" : "s"} for ${s("reason")}: ${s("waiting")} waiting, warm ${s("warm")}, min ${s("min")}, max ${s("max")}; had ${s("total")} (${s("idle")} idle, ${s("provisioning")} provisioning)`;
      // Rows written before capacity planning carry no plan counts.
      return "ready" in d ? `${line}; ${capacityPlan(d)}` : line;
    }
    case "pool.scale_blocked":
      return `no host launched: ${s("waiting")} waiting, had ${s("total")}, max ${s("max")}; ${capacityPlan(d)}`;
    case "host.capacity_decision": {
      const head = `${s("decision")} (${s("stage").replaceAll("_", " ")}) in pool ${s("pool")}`;
      const why = blockers(d.blockers) || s("reason");
      return why ? `${head}: ${why}` : head;
    }
    case "pool.launch_requested":
      return `launching ${s("name")}`;
    case "pool.host_launched":
      return [`${s("name")} is ${s("providerId")}`, s("instanceType"), s("market"), s("zone")].filter(Boolean).join(" ");
    case "pool.launch_failed":
      return `launch failed: ${s("error")}`;
    case "pool.host_registered":
      return `${s("name")} registered`;
    case "pool.host_released":
      return `${s("name")} released: ${s("reason")}${s("idleSeconds") ? ` for ${formatDuration(Number(d.idleSeconds))}` : ""}`;
    case "pool.spot_interrupted":
      return `${s("host")} interrupted: ${s("reason")}`;
    case "pool.placement":
    case "host.placement_assigned": {
      const res = resources(d.resources);
      return `${s("run")} epoch ${s("epoch")} on ${s("host")}${res ? ` (${res})` : ""}`;
    }
    case "pool.config_changed":
    case "pool.retired":
    case "pool.restored": {
      const changes = (d.changes ?? {}) as Record<string, { old?: unknown; new?: unknown }>;
      const parts = Object.keys(changes)
        .sort()
        .map((k) => `${k} ${val(changes[k]?.old)}→${val(changes[k]?.new)}`);
      return (d.created === true ? "created: " : "") + parts.join(", ");
    }
    case "pool.renamed":
      return `${s("from")} → ${s("to")}`;
    case "pool.provider_error":
    case "host.provider_error":
      return `${s("op")}: ${s("error")}`;
    case "host.registered":
      return [s("name"), s("arch"), s("runner") && `runner ${s("runner")}`, s("providerId")].filter(Boolean).join(" ");
    case "host.ready":
      return `from ${s("from")}`;
    case "host.placement_ended":
      return `${s("run")} epoch ${s("epoch")}: ${s("outcome")}${s("reason") ? ` (${s("reason")})` : ""}`;
    case "host.drain_requested":
      return `${s("cause")}: ${s("reason")}${d.evict === true ? ", evicting its runs" : ""}`;
    default:
      return s("reason");
  }
}

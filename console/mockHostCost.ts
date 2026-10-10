// Host-tied cost for the mock, shaped as PR 2's luxd answers it
// (internal/server/poolstats.go poolCost, costread.go hostCost, costqueue.go
// Run lines): each host's compute and block-storage periods, split every
// hour into what its placements reserved (allocated) and what they did not
// (unallocated), per family. A host whose volumes are not known has no
// block-storage period, so no block-storage rows (missing, never zero).
// Amounts are integers of nano-units (luxd's 9 digits), so every sum is exact.
import { noise, SINCE } from "./mockCosts.ts";

interface Volume {
  type: string;
  sizeGiB: number;
  iops?: number;
  throughputMiBps?: number;
  assumed?: boolean;
}

interface CostHost {
  id: string;
  name: string;
  poolId: string;
  /** Seconds ago its billed window began (provision request). */
  startAgo: number;
  /** Seconds ago it was terminated; null while it lives. */
  endAgo: number | null;
  /** Compute list price per hour, nano-USD. */
  compute: number;
  /** Absent: not known yet (no block-storage period). */
  volumes?: Volume[];
}

const GP3_100: Volume = { type: "gp3", sizeGiB: 100, iops: 3000, throughputMiBps: 125 };
// eu-north-1's gp3 list price, as the e2e fake answers it: 0.0836 per GB-month, 730 hours a month.
const GP3_PER_GB_MONTH = 0.0836;
const HOURS_PER_MONTH = 730;
const ebsHourly = (vs: Volume[]) => Math.round(vs.reduce((a, v) => a + (v.sizeGiB * GP3_PER_GB_MONTH * 1e9) / HOURS_PER_MONTH, 0));

const COST_HOSTS: CostHost[] = [
  { id: "host_7f2cq9m1x0", name: "gp-eu-west-1-c4", poolId: "pool_default01", startAgo: 3 * 86400, endAgo: null, compute: 138_600_000, volumes: [GP3_100] },
  { id: "host_3k8wq2n5vz", name: "gp-eu-west-1-a7", poolId: "pool_default01", startAgo: 5 * 3600, endAgo: null, compute: 138_600_000, volumes: [{ ...GP3_100, assumed: true }] },
  // Launched before luxd recorded volumes and never backfilled: compute only.
  { id: "host_9p4tz6c1mh", name: "gp-eu-west-1-b2", poolId: "pool_default01", startAgo: 5 * 3600, endAgo: 28 * 60, compute: 138_600_000 },
  { id: "host_acme0ci001", name: "acme-ci-1", poolId: "pool_acme_ci01", startAgo: 2 * 86400, endAgo: null, compute: 96_000_000, volumes: [{ ...GP3_100, sizeGiB: 50 }] },
];

export const costHost = (id: string) => COST_HOSTS.find((h) => h.id === id);

const HOUR = 3600;

const money = (nanos: number) => {
  const s = String(Math.abs(nanos)).padStart(10, "0");
  return `${nanos < 0 ? "-" : ""}${s.slice(0, -9)}.${s.slice(-9)}`.replace(/\.?0+$/, "") || "0";
};
const iso = (t: number) => new Date(t * 1000).toISOString();

/** The share of a host its placements reserved over an hour (S clipped to 1): busy by day, part-filled, empty some hours. */
function occupancy(host: CostHost, hour: number): number {
  const local = new Date(hour * 1000).getUTCHours();
  const day = local >= 7 && local < 20 ? 1 : 0.35;
  const r = noise(hour / HOUR + host.name.length * 31);
  if (r < 0.12) return 0;
  return Math.min(1, Math.round((0.25 + r * 0.75) * day * 100) / 100);
}

interface HostHour {
  hour: number;
  host: CostHost;
  family: "compute" | "block-storage";
  allocated: number;
  unallocated: number;
}

/** The host-hour rows of [from, to): one per host, hour and family it has a period for; a partial hour is billed for its part. */
function hostHours(from: number, to: number, hosts: CostHost[]): HostHour[] {
  const now = Date.now() / 1000;
  const out: HostHour[] = [];
  for (const host of hosts) {
    const start = now - host.startAgo;
    const end = host.endAgo == null ? now : now - host.endAgo;
    for (let hour = from; hour < to; hour += HOUR) {
      const a = Math.max(hour, start);
      const b = Math.min(hour + HOUR, end);
      if (b <= a) continue;
      const part = (b - a) / HOUR;
      const used = occupancy(host, hour);
      const rates: [HostHour["family"], number][] = [["compute", host.compute]];
      if (host.volumes?.length) rates.push(["block-storage", ebsHourly(host.volumes)]);
      for (const [family, rate] of rates) {
        const billed = Math.round(rate * part);
        const allocated = Math.round(billed * used);
        out.push({ hour, host, family, allocated, unallocated: billed - allocated });
      }
    }
  }
  return out;
}

function range(u: URL, fallback: string): { from: number; to: number } {
  const to = Math.ceil(Date.now() / 1000 / HOUR) * HOUR;
  return { from: to - (SINCE[u.searchParams.get("since") ?? fallback] ?? 24) * HOUR, to };
}

const sumBy = <T>(rows: T[], key: (r: T) => string, pick: (r: T) => number) => {
  const m = new Map<string, number>();
  for (const r of rows) m.set(key(r), (m.get(key(r)) ?? 0) + pick(r));
  return m;
};

const FAMILIES = [{ family: "block-storage", displayName: "Block storage" }, { family: "compute", displayName: "Compute" }];
// The Runs on the default pool: a tenant (acme) has 40% of it. Their names are mockCosts.ts's.
const TOP_RUNS = [
  { id: "run_k3jq7x2mfa9vbn4z", name: "agent-refactor-42", tenant: "ten_acme", part: 0.22 },
  { id: "run_a4aao3fvcuzpa2k", name: "jervasion-abs-pr-5336-review", tenant: "ten_acme", part: 0.18 },
  { id: "run_6y52awww5ede2qf", name: "jervasion-sdk-plugins-pr-12-review", tenant: "ten_globex", part: 0.31 },
  { id: "run_2mpnc8a7xj0r1lw", name: "dude DASH-38 review", tenant: "ten_globex", part: 0.29 },
];

/**
 * GET /v1/pools/{name}/cost. tenant: the caller's tenant id (null: an
 * operator over every tenant). The Runs' cost is the caller's own Runs';
 * host time (idle, hostSeries, hosts) is returned to an operator and to the
 * tenant owning the pool, never a platform pool's to a tenant.
 */
export function poolCost(u: URL, pool: { id: string; tenantId: string | null }, tenant: string | null): unknown {
  const interval = u.searchParams.get("interval") === "day" ? "day" : "hour";
  const step = interval === "day" ? 86400 : HOUR;
  const { from, to } = range(u, "24h");
  const rows = hostHours(from, to, COST_HOSTS.filter((h) => h.poolId === pool.id));
  const bucket = (h: number) => Math.floor(h / step) * step;
  // The Runs' share of the allocated host time: all of it to an operator, a tenant its own Runs' part.
  const runs = pool.tenantId ? TOP_RUNS.map((r) => ({ ...r, tenant: pool.tenantId! })) : TOP_RUNS;
  const mine = runs.filter((r) => tenant == null || r.tenant === tenant);
  const part = mine.reduce((a, r) => a + r.part, 0);
  const series = [...sumBy(rows, (r) => `${bucket(r.hour)} ${r.family}`, (r) => Math.round(r.allocated * part))]
    .filter(([, v]) => v > 0)
    .map(([k, v]) => {
      const [at, family] = k.split(" ");
      return { at: iso(Number(at)), family: family!, currency: "USD", amount: money(v) };
    })
    .sort((a, b) => a.at.localeCompare(b.at) || a.family.localeCompare(b.family));
  const total = series.reduce((a, s) => a + Math.round(Number(s.amount) * 1e9), 0);
  const body: Record<string, unknown> = {
    poolId: pool.id,
    from: iso(from),
    to: iso(to),
    basis: "list",
    interval,
    totals: total ? [{ currency: "USD", amount: money(total) }] : [],
    series,
    families: FAMILIES.filter((f) => series.some((s) => s.family === f.family)),
    topRuns: total ? mine.map((r) => ({ id: r.id, name: r.name, currency: "USD", amount: money(Math.round((total * r.part) / part)), estimate: r.id === "run_k3jq7x2mfa9vbn4z" })).sort((a, b) => Number(b.amount) - Number(a.amount)) : [],
  };
  if (tenant != null && pool.tenantId !== tenant) return body;
  body.idle = [...sumBy(rows, (r) => r.family, (r) => r.unallocated)].sort(([a], [b]) => a.localeCompare(b)).map(([family, v]) => ({ family, currency: "USD", amount: money(v) }));
  body.hostSeries = [...sumBy(rows, (r) => `${bucket(r.hour)} ${r.family}`, (r) => r.allocated)].map(([k, allocated]) => {
    const [at, family] = k.split(" ");
    const unallocated = rows.filter((r) => bucket(r.hour) === Number(at) && r.family === family).reduce((a, r) => a + r.unallocated, 0);
    return { at: iso(Number(at)), family: family!, currency: "USD", allocated: money(allocated), unallocated: money(unallocated) };
  }).sort((a, b) => a.at.localeCompare(b.at) || a.family.localeCompare(b.family));
  const now = Date.now() / 1000;
  const perHost = [...new Set(rows.map((r) => r.host))].map((h) => {
    const mineRows = rows.filter((r) => r.host === h);
    const start = Math.max(from, now - h.startAgo);
    const end = Math.min(to, h.endAgo == null ? now : now - h.endAgo);
    const fams = [...new Set(mineRows.map((r) => r.family))].sort();
    return {
      total: mineRows.reduce((a, r) => a + r.allocated + r.unallocated, 0),
      rows: fams.map((family) => {
        const f = mineRows.filter((r) => r.family === family);
        return { hostId: h.id, hostName: h.name, family, currency: "USD", allocated: money(f.reduce((a, r) => a + r.allocated, 0)), unallocated: money(f.reduce((a, r) => a + r.unallocated, 0)), hours: Math.max(0, (end - start) / HOUR), ...(h.volumes ? { volumes: h.volumes } : {}) };
      }),
    };
  });
  body.hosts = perHost.sort((a, b) => b.total - a.total).flatMap((h) => h.rows);
  return body;
}

/** GET /v1/hosts/{id}/cost: hours per family; unallocated to an operator and to the tenant owning the host's pool; rates (operators) per family, block storage with what it was priced from. */
export function hostCost(u: URL, id: string, tenant: string | null, poolTenant: string | null): unknown {
  const host = costHost(id);
  const { from, to } = range(u, "24h");
  const seesIdle = tenant == null || poolTenant === tenant;
  const hours = host
    ? hostHours(from, to, [host])
        .sort((a, b) => a.hour - b.hour || a.family.localeCompare(b.family))
        .map((r) => ({ hour: iso(r.hour), family: r.family, currency: "USD", allocated: money(r.allocated), ...(seesIdle ? { unallocated: money(r.unallocated) } : {}) }))
    : [];
  const body: Record<string, unknown> = { hostId: id, from: iso(from), to: iso(to), basis: "list", hours };
  if (tenant != null || !host) return body;
  const now = Date.now() / 1000;
  const start = iso(Math.floor(now - host.startAgo));
  const end = host.endAgo == null ? {} : { to: iso(Math.floor(now - host.endAgo)) };
  // Ordered as luxd orders them: family descending, then from.
  const rates: Record<string, unknown>[] = [];
  // A spot host's compute price moved once in its window.
  const moved = Math.floor(now - Math.min(host.startAgo, 86400) / 2);
  rates.push({ family: "compute", from: start, to: iso(moved), perHour: money(host.compute - 2_100_000), currency: "USD", source: "ec2-spot-history" });
  rates.push({ family: "compute", from: iso(moved), ...end, perHour: money(host.compute), currency: "USD", source: "ec2-spot-history" });
  if (host.volumes?.length) {
    rates.unshift({
      family: "block-storage",
      from: start,
      ...end,
      perHour: money(ebsHourly(host.volumes)),
      currency: "USD",
      source: "ec2-ebs-pricing",
      details: { volumes: host.volumes, prices: { gp3: { currency: "USD", perGBMonth: String(GP3_PER_GB_MONTH), perIOPSMonth: "0.0052", perGiBpsMonth: "39.2" } }, hoursPerMonth: HOURS_PER_MONTH },
    });
  }
  body.rates = rates;
  return body;
}

/**
 * GET /v1/runs/{id}/cost for the mock Run: three placements (epochs 1-3,
 * 15 minutes each, the last live) of 4 CPUs on 16-CPU hosts (share 0.25).
 * Epoch 1 ran on gp-eu-west-1-b2, whose volumes are not known: its block
 * storage is missing, so compute is incomplete and its lines say so; the
 * other two carry a block-storage placement beside the compute one.
 */
export function runCost(runId: string, epochs: { epoch: number; hostId: string; assignedAt: string; exitedAt?: string }[]): unknown {
  const now = Date.now() / 1000;
  const share = 0.25;
  const lines: Record<string, unknown>[] = [];
  const byFamily = new Map<string, number>();
  let missing = false;
  for (const family of ["block-storage", "compute"] as const) {
    const placements: Record<string, unknown>[] = [];
    let amount = 0;
    let lineFrom = Infinity;
    let lineTo = 0;
    for (const p of epochs) {
      const host = costHost(p.hostId)!;
      if (family === "block-storage" && !host.volumes) {
        missing = true;
        continue;
      }
      const rate = family === "compute" ? host.compute : ebsHourly(host.volumes!);
      const a = Date.parse(p.assignedAt) / 1000;
      const b = p.exitedAt ? Date.parse(p.exitedAt) / 1000 : now;
      const nanos = Math.round((rate * (b - a) * share) / HOUR);
      amount += nanos;
      lineFrom = Math.min(lineFrom, a);
      lineTo = Math.max(lineTo, b);
      placements.push({ epoch: p.epoch, hostId: p.hostId, from: p.assignedAt, to: p.exitedAt ?? null, cpus: 4, memory: 8 * 1024 ** 3, amount: money(nanos), share, ratePerHour: money(rate), finalized: !!p.exitedAt, ...(family === "compute" ? { market: "spot", zone: "eu-west-1c" } : {}) });
    }
    if (!placements.length) continue;
    byFamily.set(family, amount);
    lines.push({ runId, source: "compute", family, item: family === "compute" ? "m8g.2xlarge:spot" : "gp3:100GiB", amount: money(amount), currency: "USD", from: iso(lineFrom), to: iso(lineTo), final: false, details: { placements, ...(missing ? { missingRate: true } : {}) }, reportedAt: iso(now - 40) });
  }
  // missingRate marks every compute-source line once anything is missing.
  if (missing) for (const l of lines) (l.details as Record<string, unknown>).missingRate = true;
  const ai = 1_284_310_000;
  byFamily.set("ai", ai);
  lines.unshift({ runId, source: "model-gateway", family: "ai", item: "claude-opus-4", amount: money(ai), currency: "USD", from: epochs[0]!.assignedAt, to: iso(now - 60), final: false, details: null, reportedAt: iso(now - 60) });
  const total = [...byFamily.values()].reduce((a, b) => a + b, 0);
  const meta: Record<string, { displayName?: string; color?: string }> = { ai: { displayName: "AI models", color: "violet" }, compute: { displayName: "Compute" }, "block-storage": { displayName: "Block storage" } };
  return {
    runId,
    status: missing ? "incomplete" : "complete",
    final: false,
    basis: "list",
    totals: [{ currency: "USD", amount: money(total), final: "0", estimate: money(total) }],
    byFamily: [...byFamily].sort(([a], [b]) => a.localeCompare(b)).map(([family, v]) => ({ family, ...meta[family], currency: "USD", amount: money(v), final: "0", estimate: money(v) })),
    lines,
    sources: [
      { source: "compute", status: missing ? "incomplete" : "ok", answeredAt: iso(now - 40) },
      { source: "model-gateway", status: "ok", answeredAt: iso(now - 60), nextAt: iso(now + 60) },
    ],
  };
}

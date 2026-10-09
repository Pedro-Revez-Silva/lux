// GET /v1/costs and /v1/costs/labels for the mock: hourly cost rows for the
// last 30 days, of a few Runs across two tenants, in two families (compute,
// and AI models from a plugin), with a quiet night with no rows (gaps, not
// zeros) and an AI spike two hours ago. Runs carry labels (app=jervasion or
// dude, one without app; repository and phase labels) and who submitted
// them (two named keys, a revoked one, an operator's, a person, and Runs
// from before tracking). MOCK_COST_CURRENCIES=USD,EUR adds a second
// currency. Amounts are integers of micro-units, so every sum is exact.

interface Row {
  hour: number;
  tenant: string;
  run: string;
  family: string;
  currency: string;
  micros: number;
}

interface MockRun {
  id: string;
  name: string;
  tenant: string;
  ai: number;
  compute: number;
  pool: string;
  labels: Record<string, string>;
  /** A key id, email:<address>, or null before tracking. */
  by: string | null;
}

const RUNS: MockRun[] = [
  { id: "run_a4aao3fvcuzpa2k", name: "jervasion-abs-pr-5336-review", tenant: "ten_acme", ai: 9, compute: 1, pool: "default", labels: { app: "jervasion", "jervasion.repository": "absmartly/abs", "jervasion.pr": "5336" }, by: "key_ci" },
  { id: "run_jd2inqktcsax7m1", name: "jervasion-jervasion-pr-126-review", tenant: "ten_acme", ai: 4, compute: 1, pool: "default", labels: { app: "jervasion", "jervasion.repository": "marcioapm/jervasion", "jervasion.pr": "126" }, by: "key_ci" },
  { id: "run_6y52awww5ede2qf", name: "jervasion-sdk-plugins-pr-12-review", tenant: "ten_globex", ai: 4, compute: 2, pool: "default", labels: { app: "jervasion", "jervasion.repository": "absmartly/sdk-plugins", "jervasion.pr": "12" }, by: "key_globex" },
  { id: "run_k1bz8sbd0yqv3ce", name: "dude DASH-41 implement", tenant: "ten_acme", ai: 3, compute: 1, pool: "gpu", labels: { app: "dude", "dude.phase": "implement", "dude.ticket": "DASH-41" }, by: "key_dude" },
  { id: "run_2mpnc8a7xj0r1lw", name: "dude DASH-38 review", tenant: "ten_globex", ai: 3, compute: 1, pool: "gpu", labels: { app: "dude", "dude.phase": "review", "dude.ticket": "DASH-38" }, by: "key_old" },
  { id: "run_6a62z4nsfa3jsx0", name: "jervasion-jervasion-pr-127-review", tenant: "ten_acme", ai: 2, compute: 1, pool: "default", labels: { app: "jervasion", "jervasion.repository": "marcioapm/jervasion", "jervasion.pr": "127" }, by: "email:ada@example.com" },
  { id: "run_bsn6jhmfcejni8a", name: "fix wi_0mun9t: retry lease on host loss", tenant: "ten_acme", ai: 0, compute: 3, pool: "default", labels: { team: "platform" }, by: null },
  { id: "run_5m4ji5pcevbiwq2", name: "", tenant: "ten_globex", ai: 1, compute: 1, pool: "default", labels: {}, by: "key_op" },
];

/** The API keys the submitters name: tenant keys, a revoked one, an operator's (no tenant). */
const KEYS: Record<string, { name: string; tenant: string | null; revoked?: boolean }> = {
  key_ci: { name: "ci-review-bot", tenant: "ten_acme" },
  key_dude: { name: "dude-prod", tenant: "ten_acme" },
  key_globex: { name: "globex-ci", tenant: "ten_globex" },
  key_old: { name: "marcio-laptop", tenant: "ten_globex", revoked: true },
  key_op: { name: "ops-console", tenant: null },
};

export const runInfo = (id: string) => RUNS.find((r) => r.id === id);

/** A Run's submittedBy as GET /v1/runs/{id} has it, for a caller of tenant (null: an operator). */
export function submittedBy(by: string | null, tenant: string | null) {
  if (!by) return undefined;
  if (by.startsWith("email:")) return { email: by.slice("email:".length) };
  const k = KEYS[by];
  const named = k && (tenant == null || k.tenant === tenant);
  return { keyId: by, ...(named ? { keyName: k.name } : {}), ...(k?.revoked ? { revoked: true } : {}) };
}

/** A deterministic 0..1 from a seed: the same data on every reload. */
function noise(seed: number): number {
  const x = Math.sin(seed * 12.9898) * 43758.5453;
  return x - Math.floor(x);
}

const HOUR = 3600;
const currencies = (process.env.MOCK_COST_CURRENCIES ?? "USD").split(",").filter(Boolean);

function generate(): { rows: Row[]; unallocated: Map<number, Map<string, number>> } {
  const nowHour = Math.floor(Date.now() / 1000 / HOUR) * HOUR;
  const rows: Row[] = [];
  const unallocated = new Map<number, Map<string, number>>();
  for (let h = 30 * 24; h >= 0; h--) {
    const hour = nowHour - h * HOUR;
    const local = new Date(hour * 1000).getHours();
    // A quiet night: nothing recorded at all from 01:00 to 04:00.
    if (local >= 1 && local < 4) continue;
    currencies.forEach((currency, ci) => {
      const scale = ci === 0 ? 1 : 0.3;
      RUNS.forEach((r, ri) => {
        const seed = hour / HOUR + ri * 101 + ci * 7;
        // Each Run is active a share of the hours.
        if (noise(seed) > 0.45) return;
        const compute = Math.round((20_000 + noise(seed + 1) * 60_000) * r.compute * scale);
        rows.push({ hour, tenant: r.tenant, run: r.id, family: "compute", currency, micros: compute });
        if (r.ai > 0) {
          let ai = Math.round((40_000 + noise(seed + 2) * 400_000) * r.ai * scale);
          if (h === 2 && ri === 0) ai = Math.round(34_812_345 * scale);
          if (h === 2 && ri === 3) ai = Math.round(3_412_000 * scale);
          if (h === 15 && ri === 1) ai = Math.round(4_620_000 * scale);
          rows.push({ hour, tenant: r.tenant, run: r.id, family: "ai", currency, micros: ai });
        }
      });
      const idle = new Map(unallocated.get(hour) ?? []);
      idle.set(currency, Math.round((30_000 + noise(hour / HOUR + ci) * 70_000) * scale));
      unallocated.set(hour, idle);
    });
  }
  return { rows, unallocated };
}

const money = (micros: number) => {
  const neg = micros < 0;
  const s = String(Math.abs(micros)).padStart(7, "0");
  return `${neg ? "-" : ""}${s.slice(0, -6)}.${s.slice(-6)}`.replace(/\.?0+$/, "");
};

const SINCE: Record<string, number> = { "1h": 1, "6h": 6, "24h": 24, "7d": 168, "30d": 720 };
const LABEL_KEY = /^[A-Za-z0-9]([A-Za-z0-9._/-]{0,62}[A-Za-z0-9])?$/;

type Bad = { error: { code: string; message: string } };

/** The range and label filters as luxd reads them; an error body for a malformed label. */
function scoped(u: URL, tenantOf: string | null): { from: number; to: number; rows: Row[]; unallocated: Map<number, Map<string, number>>; filtered: boolean } | Bad {
  const q = u.searchParams;
  const ceilHour = (t: number) => Math.ceil(t / 1000 / HOUR) * HOUR;
  const floorHour = (t: number) => Math.floor(t / 1000 / HOUR) * HOUR;
  const to = q.get("to") ? ceilHour(Date.parse(q.get("to")!)) : ceilHour(Date.now());
  const from = q.get("from") ? floorHour(Date.parse(q.get("from")!)) : to - (SINCE[q.get("since") ?? "1h"] ?? 1) * HOUR;
  const want = new Map<string, string[]>();
  for (const l of q.getAll("label")) {
    const i = l.indexOf("=");
    const k = i < 0 ? "" : l.slice(0, i);
    if (!LABEL_KEY.test(k)) return { error: { code: "bad_request", message: `label "${l}": want key=value with a valid label key` } };
    want.set(k, [...(want.get(k) ?? []), l.slice(i + 1)]);
  }
  const absent = q.getAll("nolabel");
  for (const k of absent) if (!LABEL_KEY.test(k)) return { error: { code: "bad_request", message: `nolabel "${k}": not a valid label key` } };
  const tenant = tenantOf ?? q.get("tenant");
  const runOk = (id: string) => {
    const r = runInfo(id)!;
    return [...want].every(([k, vs]) => r.labels[k] != null && vs.includes(r.labels[k]!)) && absent.every((k) => r.labels[k] == null);
  };
  const { rows, unallocated } = generate();
  const inRange = rows.filter((r) => r.hour >= from && r.hour < to && (!tenant || r.tenant === tenant) && (!q.get("family") || r.family === q.get("family")) && (!q.get("nofamily") || r.family !== q.get("nofamily")) && runOk(r.run));
  return { from, to, rows: inRange, unallocated, filtered: want.size > 0 || absent.length > 0 };
}

/** The summary for a request's range, groups, interval and label filters, as luxd answers it; tenantOf: a tenant key's tenant (null: an operator). */
export function costSummary(u: URL, tenantOf: string | null = null): { status: number; body: unknown } {
  const q = u.searchParams;
  const s = scoped(u, tenantOf);
  if ("error" in s) return { status: 400, body: s };
  const groups = q.getAll("group");
  const interval = q.get("interval");
  const tenant = tenantOf ?? q.get("tenant");
  const value = (r: Row, g: string) => {
    const run = runInfo(r.run)!;
    if (g === "tenant") return r.tenant;
    if (g === "run") return r.run;
    if (g === "family") return r.family;
    if (g === "pool") return r.family === "compute" ? run.pool : "(none)";
    if (g === "key") return run.by ?? "(none)";
    if (g.startsWith("label:")) return run.labels[g.slice("label:".length)] ?? "(none)";
    return "(none)";
  };
  const bucket = (h: number) => (interval === "day" ? Math.floor(h / 86400) * 86400 : h);
  // top=N: the first group's values past the N costliest per currency (by what rank counts) fold into rows marked other: true (read "(other)"), as luxd folds them.
  const top = Number(q.get("top") ?? 0);
  const rank = q.get("rank") ?? "all";
  const ranks = (r: Row) => rank === "all" || (r.family === "compute") === (rank === "compute");
  const folded = new Map<string, Set<string>>();
  const otherCount: Record<string, number> = {};
  if (top > 0 && groups.length) {
    const sums = new Map<string, Map<string, number | null>>();
    for (const r of s.rows) {
      const v = value(r, groups[0]!);
      if (v === "(none)") continue;
      const m = sums.get(r.currency) ?? new Map<string, number | null>();
      // null: no cost rank counts, which ranks last.
      m.set(v, ranks(r) ? (m.get(v) ?? 0) + r.micros : (m.get(v) ?? null));
      sums.set(r.currency, m);
    }
    for (const [c, m] of sums) {
      const rest = [...m].sort(([a, x], [b, y]) => (x == null ? (y == null ? 0 : 1) : y == null ? -1 : y - x) || (a < b ? -1 : 1)).slice(top);
      folded.set(c, new Set(rest.map(([v]) => v)));
      if (rest.length) otherCount[c] = rest.length;
    }
  }
  const isOther = (r: Row) => groups.length > 0 && !!folded.get(r.currency)?.has(value(r, groups[0]!));
  const groupOf = (r: Row) => Object.fromEntries(groups.map((g, i) => [g, i === 0 && isOther(r) ? "(other)" : value(r, g)]));
  // runs: with top, the Runs with ranked cost per first-group value; with runs=true (no top), every Run with cost.
  const withRuns = top > 0 || q.get("runs") === "true";
  const runsOf = new Map<string, Set<string>>();
  if (withRuns) for (const r of s.rows) if (top === 0 || ranks(r)) { const k = JSON.stringify([isOther(r), groupOf(r)[groups[0]!], r.currency]); runsOf.set(k, (runsOf.get(k) ?? new Set()).add(r.run)); }
  const aggregate = (withAt: boolean) => {
    const m = new Map<string, { at?: number; group: Record<string, string>; other: boolean; currency: string; micros: number }>();
    for (const r of s.rows) {
      const group = groupOf(r);
      const other = isOther(r);
      const at = withAt ? bucket(r.hour) : undefined;
      const k = JSON.stringify([at, group, other, r.currency]);
      const e = m.get(k) ?? { at, group, other, currency: r.currency, micros: 0 };
      e.micros += r.micros;
      m.set(k, e);
    }
    return [...m.values()]
      .sort((a, b) => (a.at ?? 0) - (b.at ?? 0) || JSON.stringify(a.group).localeCompare(JSON.stringify(b.group)) || Number(a.other) - Number(b.other) || a.currency.localeCompare(b.currency))
      .map((e) => ({ ...(e.at != null ? { at: new Date(e.at * 1000).toISOString() } : {}), ...(groups.length ? { group: e.group } : {}), currency: e.currency, amount: money(e.micros), ...(withRuns && !withAt ? { runs: runsOf.get(JSON.stringify([e.other, e.group[groups[0]!], e.currency]))?.size ?? 0 } : {}), ...(e.other ? { other: true } : {}) }));
  };
  const totals = aggregate(false);
  const body: Record<string, unknown> = { from: new Date(s.from * 1000).toISOString(), to: new Date(s.to * 1000).toISOString(), basis: "list", totals };
  if (Object.keys(otherCount).length) body.otherCount = otherCount;
  if (interval) body.series = aggregate(true);
  const named = (g: string) => [...new Set(totals.filter((t) => !(t.other && groups[0] === g)).map((t) => t.group![g]!))];
  if (groups.includes("family")) body.families = named("family").sort().map((f) => (f === "ai" ? { family: f, displayName: "AI models", color: "violet" } : f === "compute" ? { family: f, displayName: "Compute" } : { family: f }));
  if (groups.includes("run")) body.runs = named("run").sort().map((id) => ({ id, name: runInfo(id)?.name ?? "", ...(Object.keys(runInfo(id)?.labels ?? {}).length ? { labels: runInfo(id)!.labels } : {}) }));
  if (groups.includes("key")) {
    body.keys = named("key")
      .filter((k) => k !== "(none)")
      .sort()
      .map((id) => {
        if (id.startsWith("email:")) return { id, email: id.slice("email:".length) };
        const k = KEYS[id]!;
        const named = tenantOf == null || k.tenant === tenantOf;
        return { id, ...(named ? { name: k.name } : {}), ...(k.tenant == null ? { operator: true } : {}), ...(k.revoked ? { revoked: true } : {}) };
      });
  }
  if (!tenant && !s.filtered) {
    const idle = new Map<string, number>();
    for (const [h, by] of s.unallocated) if (h >= s.from && h < s.to) for (const [c, v] of by) idle.set(c, (idle.get(c) ?? 0) + v);
    body.unallocated = [...idle].sort(([a], [b]) => a.localeCompare(b)).map(([currency, v]) => ({ currency, amount: money(v) }));
  }
  return { status: 200, body };
}

/** GET /v1/costs/labels: the label keys on Runs with cost in range, with their Runs, most first. */
export function costLabels(u: URL, tenantOf: string | null = null): { status: number; body: unknown } {
  const s = scoped(u, tenantOf);
  if ("error" in s) return { status: 400, body: s };
  const runs = new Map<string, Set<string>>();
  for (const r of s.rows) for (const k of Object.keys(runInfo(r.run)!.labels)) runs.set(k, (runs.get(k) ?? new Set()).add(r.run));
  const keys = [...runs].map(([key, ids]) => ({ key, runs: ids.size })).sort((a, b) => b.runs - a.runs || a.key.localeCompare(b.key));
  return { status: 200, body: { from: new Date(s.from * 1000).toISOString(), to: new Date(s.to * 1000).toISOString(), keys } };
}

export const COST_TENANTS = [
  { id: "ten_acme", name: "acme" },
  { id: "ten_globex", name: "globex" },
];

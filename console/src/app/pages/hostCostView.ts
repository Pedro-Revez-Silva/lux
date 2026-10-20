// What the pool, host and Run cost screens show of host-tied cost: compute,
// block storage, public IPv4 and data transfer, always apart, each split
// into what Runs were charged and what no Run was: idle host time for the
// shared families, the host's own traffic for data transfer. Amounts stay
// decimal strings, summed exactly per currency and never across currencies;
// numbers are for geometry only.
import { familyDisplay, sumMoney } from "@lux/design-system";
import type { CostLine, CostPlacement, HostVolume, MoneyTotal, NetBytes } from "../../api/index.ts";
import { DATA_TRANSFER, HOST_FAMILIES, PUBLIC_IP } from "../../costFamilies.ts";
import { BLOCK_STORAGE, byCurrency, COMPUTE, push, ratio, sum } from "./costView.ts";

export { DATA_TRANSFER, HOST_FAMILIES, PUBLIC_IP };
const isHostFamily = (f: string) => HOST_FAMILIES.includes(f);
/** The families billed by time: utilisation is their Runs' share. */
const TIME_FAMILIES: readonly string[] = [COMPUTE, BLOCK_STORAGE];
/** The families shared between a host's Runs: their unallocated part is idle time. */
const SHARED_FAMILIES: readonly string[] = [COMPUTE, BLOCK_STORAGE, PUBLIC_IP];

const LABELS = familyDisplay(HOST_FAMILIES.map((family) => ({ family })));
export const familyLabel = (f: string) => LABELS.get(f)?.label ?? f;
/** Data transfer's part no Run was charged on one host; a pool's screens say "Hosts' own". */
export const HOST_OWN = "Host's own";
/** What a family's part no Run was charged is called: the host's own traffic for data transfer, idle time otherwise. */
export const ownPartLabel = (f: string, ownLabel = HOST_OWN) => (f === DATA_TRANSFER ? ownLabel : "unallocated");

/** One figure per currency, summed over families (a pool's idle per family and currency, say). */
export function sumPerCurrency(rows: readonly { currency: string; amount: string }[] | null | undefined): MoneyTotal[] {
  return [...byCurrency(rows ?? [])].flatMap(([currency, rs]) => {
    const amount = sum(rs.map((r) => r.amount));
    return amount == null ? [] : [{ currency, amount }];
  });
}

/** Host cost of one family: what Runs were charged, what no Run was (idle time; for data transfer, the host's own traffic), and both. */
export interface Paid {
  runs: string | null;
  unallocated: string | null;
  total: string | null;
}

export interface WhoPaid {
  currency: string;
  compute: Paid;
  blockStorage: Paid;
  publicIp: Paid;
  /** unallocated: the host's own traffic (image pulls, uploads, its link to luxd), not idle time. */
  dataTransfer: Paid;
  /** Every host-tied family. */
  all: Paid;
  /** Compute, block storage and public IPv4: unallocated is idle host time. */
  shared: Paid;
  /** Compute and block storage, the families billed by host time: utilisation's basis. */
  time: Paid;
}

interface HostTimeRow {
  family: string;
  currency: string;
  allocated: string;
  /** Absent where the reader may not see it: then nothing is unallocated, and the total is what Runs paid. */
  unallocated?: string;
}

const present = (a: (string | null | undefined)[]) => a.filter((v): v is string => v != null);

function paid(rows: readonly HostTimeRow[]): Paid {
  return {
    runs: sum(rows.map((r) => r.allocated)),
    unallocated: sum(present(rows.map((r) => r.unallocated))),
    total: sum(rows.flatMap((r) => present([r.allocated, r.unallocated]))),
  };
}

const ofFamilies = (rs: readonly HostTimeRow[], fs: readonly string[]) => paid(rs.filter((r) => fs.includes(r.family)));

/** Per currency, who paid for the host: Runs or no Run, per family and summed. A family with no row is all null, never zero. */
export function whoPaid(rows: readonly HostTimeRow[] | null | undefined): WhoPaid[] {
  return [...byCurrency((rows ?? []).filter((r) => isHostFamily(r.family)))].map(([currency, rs]) => ({
    currency,
    compute: ofFamilies(rs, [COMPUTE]),
    blockStorage: ofFamilies(rs, [BLOCK_STORAGE]),
    publicIp: ofFamilies(rs, [PUBLIC_IP]),
    dataTransfer: ofFamilies(rs, [DATA_TRANSFER]),
    all: paid(rs),
    shared: ofFamilies(rs, SHARED_FAMILIES),
    time: ofFamilies(rs, TIME_FAMILIES),
  }));
}

/** Runs' share of compute and block storage, the families billed by host time; null when they cost nothing. */
export const utilisation = (p: WhoPaid) => ratio(p.time.runs, p.time.total);

export interface HostCostRow {
  hostId: string;
  hostName: string;
  currency: string;
  /** Billed hours within the range; null when luxd does not say. */
  hours: number | null;
  compute: string | null;
  blockStorage: string | null;
  publicIp: string | null;
  dataTransfer: string | null;
  /** Every host-tied family. */
  total: string;
  allocated: string;
  /** Idle host time: compute, block storage and public IPv4 no Run reserved. */
  unallocated: string;
  /** Data transfer no Run sent: the host's own traffic; null when the host has no transfer row (an en dash, not $0). */
  hostOwn: string | null;
  /** Compute and block storage charged to Runs ÷ their total; null when they cost nothing. */
  utilisation: number | null;
}

/** A pool's hosts row: one host's time of one family and currency. */
type HostRow = HostTimeRow & { hostId?: string; hostName?: string; hours?: number };

/** A pool's hosts rows (one per host, family and currency) as one row per host and currency. */
export function hostCostRows(rows: readonly HostRow[] | null | undefined): HostCostRow[] {
  const groups = new Map<string, HostRow[]>();
  for (const r of rows ?? []) {
    if (r.hostId && isHostFamily(r.family)) push(groups, `${r.hostId}\u0000${r.currency}`, r);
  }
  return [...groups.values()].map((rs) => {
    const p = paid(rs);
    const time = ofFamilies(rs, TIME_FAMILIES);
    const fam = (f: string) => ofFamilies(rs, [f]).total;
    return {
      hostId: rs[0]!.hostId!,
      hostName: rs[0]!.hostName || rs[0]!.hostId!,
      currency: rs[0]!.currency,
      hours: rs.find((r) => r.hours != null)?.hours ?? null,
      compute: fam(COMPUTE),
      blockStorage: fam(BLOCK_STORAGE),
      publicIp: fam(PUBLIC_IP),
      dataTransfer: fam(DATA_TRANSFER),
      total: p.total ?? "0",
      allocated: p.runs ?? "0",
      unallocated: ofFamilies(rs, SHARED_FAMILIES).unallocated ?? "0",
      hostOwn: ofFamilies(rs, [DATA_TRANSFER]).unallocated,
      utilisation: ratio(time.runs, time.total ?? "0"),
    };
  });
}

/** The hours of the hosts, counted once per host (rows repeat a host per family); null when none says. */
export function hostHours(rows: readonly HostRow[] | null | undefined): number | null {
  const seen = new Map<string, number>();
  for (const r of rows ?? []) if (r.hostId && r.hours != null) seen.set(r.hostId, r.hours);
  return seen.size ? [...seen.values()].reduce((a, b) => a + b, 0) : null;
}

/** The billed hours of one host within [from, to): from its launch request (or registration, or creation) to its termination or now. */
export function billedHours(times: { provisionRequested?: string | null; registered?: string | null; created?: string | null; terminated?: string | null }, from: string, to: string, now = Date.now()): number | null {
  const start = Date.parse(times.provisionRequested ?? times.registered ?? times.created ?? "");
  if (!Number.isFinite(start)) return null;
  const end = Math.min(times.terminated ? Date.parse(times.terminated) : now, Date.parse(to));
  return Math.max(0, (end - Math.max(start, Date.parse(from))) / 3_600_000);
}

/** An amount over hours as a decimal string, for "per hour on average"; null when either is missing. */
export function perHour(amount: string | null | undefined, hours: number | null | undefined): string | null {
  if (amount == null || hours == null || !(hours > 0)) return null;
  // An average is a ratio: exact digits mean nothing past the 9th.
  return (Number(amount) / hours).toFixed(9);
}

export interface NetClassRow {
  netClass: string;
  rx: number;
  tx: number;
}

/** A host's own netClasses as rows, most bytes first; a class with no bytes either way is left out. rx is rxDelivered where the runner reports it (what transfer is priced by). */
export function netClassRows(classes: Record<string, NetBytes> | null | undefined): NetClassRow[] {
  return Object.entries(classes ?? {})
    .map(([netClass, b]) => ({ netClass, rx: b.rxDelivered ?? b.rx, tx: b.tx }))
    .filter((r) => r.rx > 0 || r.tx > 0)
    .sort((a, b) => b.rx + b.tx - (a.rx + a.tx) || a.netClass.localeCompare(b.netClass));
}

/** One volume set in words: "100 GiB gp3", several joined "100 GiB gp3 + 20 GiB gp2"; detail adds IOPS and throughput where given. */
export function volumeText(vs: readonly HostVolume[], detail = false): string {
  return vs
    .map((v) => [`${v.sizeGiB} GiB ${v.type}`, ...(detail && v.iops ? [`${v.iops} IOPS`] : []), ...(detail && v.throughputMiBps ? [`${v.throughputMiBps} MiB/s`] : [])].join(" · "))
    .join(" + ");
}

const shapeOf = (vs: readonly HostVolume[]) => JSON.stringify(vs.map((v) => [v.type, v.sizeGiB, v.iops ?? 0, v.throughputMiBps ?? 0]).sort());

/**
 * The block-storage tile's line for a set of hosts: the volumes when every
 * host has the same ("100 GiB gp3"), else how many shapes there are; hosts
 * whose volumes are not known are counted apart. null with no host to say.
 */
export function volumeSummary(hosts: readonly { volumes?: readonly HostVolume[] | null }[]): string | null {
  if (!hosts.length) return null;
  const known = hosts.filter((h) => h.volumes != null);
  const unknown = hosts.length - known.length;
  const shapes = new Set(known.map((h) => shapeOf(h.volumes!)));
  const note = unknown ? `${unknown} not known` : "";
  if (shapes.size === 1 && !unknown) return known[0]!.volumes!.length ? volumeText(known[0]!.volumes!) : "no volumes";
  // No host with known volumes (a static pool): nothing to say about disks.
  if (shapes.size === 0) return null;
  return [`${shapes.size} volume ${shapes.size === 1 ? "shape" : "shapes"}`, note].filter(Boolean).join(" · ");
}

/** A row of host time per bucket (or hour): its start in epoch seconds. */
interface TimedHostRow {
  t: number;
  family: string;
  currency: string;
  allocated: string;
  /** Absent when the reader may not see it. */
  unallocated?: string;
}

export interface HostChart {
  currency: string;
  x: number[];
  ys: (number | null)[][];
  series: { label: string; color: string; faded?: boolean }[];
  /** Each series' exact total over the range, aligned with series. */
  totals: string[];
}

/** The series of the host-cost chart: each family charged to Runs, then (when visible) its part no Run was charged, faded; a family with no row draws nothing, so its legend entry is absent too. */
function hostChartSeries(families: readonly string[], unallocated: boolean, ownLabel: string): { family: string; part: "runs" | "unallocated"; label: string; color: string; faded?: boolean }[] {
  return HOST_FAMILIES.filter((f) => families.includes(f)).flatMap((family) => {
    const color = LABELS.get(family)!.color;
    const runs = { family, part: "runs" as const, label: `${familyLabel(family)} · runs`, color };
    return unallocated ? [runs, { family, part: "unallocated" as const, label: `${familyLabel(family)} · ${ownPartLabel(family, ownLabel)}`, color, faded: true }] : [runs];
  });
}

/**
 * One chart per currency over every bucket of [from, to) of `step` seconds:
 * per family, charged to Runs and (when any row carries it) unallocated,
 * stacked. Rows finer than the step (hours on a daily chart) are summed
 * exactly; a bucket without a row is a gap, not zero.
 */
export function hostCharts(rows: readonly TimedHostRow[], from: string, to: string, step: number, ownLabel = HOST_OWN): HostChart[] {
  const start = Math.floor(Date.parse(from) / 1000 / step) * step;
  const end = Math.floor(Date.parse(to) / 1000);
  const x: number[] = [];
  for (let t = start; t < end; t += step) x.push(t);
  const index = new Map(x.map((t, i) => [t, i]));
  return [...byCurrency(rows.filter((r) => isHostFamily(r.family)))].map(([currency, rs]) => {
    const series = hostChartSeries([...new Set(rs.map((r) => r.family))], rs.some((r) => r.unallocated != null), ownLabel);
    const cells = series.map(() => x.map((): string[] => []));
    for (const r of rs) {
      const i = index.get(Math.floor(r.t / step) * step);
      if (i == null) continue;
      series.forEach((s, k) => {
        if (s.family !== r.family) return;
        const v = s.part === "runs" ? r.allocated : r.unallocated;
        if (v != null) cells[k]![i]!.push(v);
      });
    }
    // Chart geometry only: the figures in text are formatted from the strings.
    const ys = cells.map((c) => c.map((a) => (a.length ? Number(sumMoney(a)) : null)));
    const totals = cells.map((c) => sumMoney(c.flat()) ?? "0");
    return { currency, x, ys, series: series.map(({ label, color, faded }) => (faded ? { label, color, faded } : { label, color })), totals };
  });
}

export interface PlacementRow {
  epoch: number;
  hostId: string;
  currency: string;
  from: string;
  to: string | null;
  share: number | null;
  compute: string | null;
  /** null: this placement has no block storage (a static host, or volumes not known), not zero. */
  blockStorage: string | null;
  total: string;
}

const placementsOf = (l: CostLine): CostPlacement[] => {
  const p = (l.details as { placements?: unknown } | null)?.placements;
  return Array.isArray(p) ? (p as CostPlacement[]) : [];
};

/**
 * A Run's placements with what each paid of compute and block storage, joined from
 * the compute and block-storage lines' details.placements on epoch, host and
 * currency (one placement per epoch; one line per item, so a Run on two
 * instance types has two compute lines). A placement with no block-storage
 * entry has null there.
 */
export function placementRows(lines: readonly CostLine[]): PlacementRow[] {
  const rows = new Map<string, PlacementRow>();
  for (const l of lines) {
    if (l.source !== "compute" || !TIME_FAMILIES.includes(l.family)) continue;
    for (const p of placementsOf(l)) {
      const k = `${p.epoch}\u0000${p.hostId}\u0000${l.currency}`;
      const r = rows.get(k) ?? { epoch: p.epoch, hostId: p.hostId, currency: l.currency, from: p.from, to: p.to, share: null, compute: null, blockStorage: null, total: "0" };
      if (l.family === COMPUTE) {
        r.compute = sum([r.compute ?? "0", p.amount]);
        r.share = p.share;
      } else {
        r.blockStorage = sum([r.blockStorage ?? "0", p.amount]);
        r.share ??= p.share;
      }
      r.total = sumMoney([r.total, p.amount])!;
      rows.set(k, r);
    }
  }
  return [...rows.values()].sort((a, b) => a.epoch - b.epoch || a.hostId.localeCompare(b.hostId) || a.currency.localeCompare(b.currency));
}

/** The placements' sums per currency: compute, block storage and both. */
export function placementTotals(rows: readonly PlacementRow[]): { currency: string; compute: string | null; blockStorage: string | null; total: string }[] {
  return [...byCurrency(rows)].map(([currency, rs]) => ({
    currency,
    compute: sum(present(rs.map((r) => r.compute))),
    blockStorage: sum(present(rs.map((r) => r.blockStorage))),
    total: sumMoney(rs.map((r) => r.total))!,
  }));
}

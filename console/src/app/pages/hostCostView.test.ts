import { expect, test } from "bun:test";
import type { CostLine } from "../../api/index.ts";
import { billedHours, hostCharts, hostCostRows, hostHours, perHour, placementRows, placementTotals, sumPerCurrency, volumeSummary, volumeText, whoPaid } from "./hostCostView.ts";

test("sumPerCurrency: idle per family and currency becomes one exact figure per currency, never across currencies", () => {
  const idle = [
    { family: "block-storage", currency: "USD", amount: "0.000000001" },
    { family: "compute", currency: "USD", amount: "6.27" },
    { family: "compute", currency: "EUR", amount: "1.1" },
  ];
  expect(sumPerCurrency(idle)).toEqual([
    { currency: "EUR", amount: "1.1" },
    { currency: "USD", amount: "6.270000001" },
  ]);
  expect(sumPerCurrency([])).toEqual([]);
  expect(sumPerCurrency(undefined)).toEqual([]);
});

const hostRow = (hostId: string, family: string, allocated: string, unallocated: string, extra: object = {}) => ({ hostId, hostName: `${hostId}-name`, family, currency: "USD", allocated, unallocated, ...extra });

test("hostCostRows: one row per host with compute and block storage apart, total, unallocated and utilisation", () => {
  const rows = hostCostRows([
    hostRow("h1", "block-storage", "0.071", "0.008", { hours: 6.9 }),
    hostRow("h1", "compute", "0.704", "0.079", { hours: 6.9 }),
    // Volumes not known: compute only, block storage missing (null, not zero).
    hostRow("h2", "compute", "0.1", "0.3", { hours: 4.8 }),
  ]);
  expect(rows).toEqual([
    { hostId: "h1", hostName: "h1-name", currency: "USD", hours: 6.9, compute: "0.783", blockStorage: "0.079", total: "0.862", allocated: "0.775", unallocated: "0.087", utilisation: 0.775 / 0.862 },
    { hostId: "h2", hostName: "h2-name", currency: "USD", hours: 4.8, compute: "0.4", blockStorage: null, total: "0.4", allocated: "0.1", unallocated: "0.3", utilisation: 0.25 },
  ]);
  // A host that cost nothing has no utilisation (not 0%).
  expect(hostCostRows([hostRow("h3", "compute", "0", "0")])[0]!.utilisation).toBeNull();
});

test("hostHours counts each host once, though its rows repeat per family", () => {
  expect(hostHours([hostRow("h1", "compute", "1", "0", { hours: 2 }), hostRow("h1", "block-storage", "1", "0", { hours: 2 }), hostRow("h2", "compute", "1", "0", { hours: 3.5 })])).toBe(5.5);
  expect(hostHours([hostRow("h1", "compute", "1", "0")])).toBeNull();
});

test("whoPaid: Runs vs unallocated per family and both; a family with no row is null, not zero", () => {
  const p = whoPaid([
    { family: "compute", currency: "USD", allocated: "6.61", unallocated: "6.27" },
    { family: "block-storage", currency: "USD", allocated: "0.71", unallocated: "0.67" },
    { family: "compute", currency: "EUR", allocated: "1", unallocated: "0" },
  ]);
  expect(p.map((x) => x.currency)).toEqual(["EUR", "USD"]);
  const usd = p[1]!;
  expect(usd.compute).toEqual({ runs: "6.61", unallocated: "6.27", total: "12.88" });
  expect(usd.blockStorage).toEqual({ runs: "0.71", unallocated: "0.67", total: "1.38" });
  expect(usd.all).toEqual({ runs: "7.32", unallocated: "6.94", total: "14.26" });
  expect(p[0]!.blockStorage).toEqual({ runs: null, unallocated: null, total: null });
  // A reader who may not see unallocated: its total is what Runs paid.
  expect(whoPaid([{ family: "compute", currency: "USD", allocated: "2" }])[0]!.all).toEqual({ runs: "2", unallocated: null, total: "2" });
  // Families that are not a host's (AI) are not host cost.
  expect(whoPaid([{ family: "ai", currency: "USD", allocated: "5", unallocated: "0" }])).toEqual([]);
});

test("volumeSummary: the volumes when every host has the same, else the number of shapes; hosts not known counted apart", () => {
  const gp3 = { type: "gp3", sizeGiB: 100, iops: 3000, throughputMiBps: 125 };
  expect(volumeSummary([{ volumes: [gp3] }, { volumes: [{ ...gp3 }] }])).toBe("100 GiB gp3");
  // An assumed copy of the same disk is the same shape.
  expect(volumeSummary([{ volumes: [gp3] }, { volumes: [{ ...gp3, assumed: true }] }])).toBe("100 GiB gp3");
  expect(volumeSummary([{ volumes: [gp3] }, { volumes: [{ ...gp3, sizeGiB: 50 }] }])).toBe("2 volume shapes");
  expect(volumeSummary([{ volumes: [gp3] }, { volumes: [{ ...gp3, iops: 6000 }] }])).toBe("2 volume shapes");
  expect(volumeSummary([{ volumes: [gp3] }, {}])).toBe("1 volume shape · 1 not known");
  expect(volumeSummary([{ volumes: [] }])).toBe("no volumes");
  // A static pool: no disk to speak of.
  expect(volumeSummary([{}, { volumes: null }])).toBeNull();
  expect(volumeSummary([])).toBeNull();
  expect(volumeText([gp3], true)).toBe("100 GiB gp3 · 3000 IOPS · 125 MiB/s");
  expect(volumeText([gp3, { type: "gp2", sizeGiB: 20 }])).toBe("100 GiB gp3 + 20 GiB gp2");
});

test("billedHours and perHour: the host's window within the range, and an average per hour", () => {
  const h = { provisionRequested: "2026-10-10T08:00:00Z", terminated: "2026-10-10T11:30:00Z" };
  expect(billedHours(h, "2026-10-10T09:00:00Z", "2026-10-10T12:00:00Z")).toBe(2.5);
  expect(billedHours({ registered: "2026-10-10T10:00:00Z" }, "2026-10-10T09:00:00Z", "2026-10-10T12:00:00Z", Date.parse("2026-10-10T11:00:00Z"))).toBe(1);
  expect(billedHours({}, "2026-10-10T09:00:00Z", "2026-10-10T12:00:00Z")).toBeNull();
  expect(perHour("12.87", 121)).toBe((12.87 / 121).toFixed(9));
  expect(perHour("1", 0)).toBeNull();
  expect(perHour(null, 3)).toBeNull();
});

test("hostCharts: four series (each family runs and unallocated, faded), gaps not zeros, hours summed into days exactly", () => {
  const day = 86400;
  const t0 = Date.parse("2026-10-09T00:00:00Z") / 1000;
  const rows = [
    { t: t0, family: "compute", currency: "USD", allocated: "0.1", unallocated: "0.2" },
    { t: t0 + 3600, family: "compute", currency: "USD", allocated: "0.000000001", unallocated: "0" },
    { t: t0, family: "block-storage", currency: "USD", allocated: "0.01", unallocated: "0.02" },
  ];
  const [c] = hostCharts(rows, "2026-10-09T00:00:00Z", "2026-10-11T00:00:00Z", day);
  expect(c!.x).toEqual([t0, t0 + day]);
  expect(c!.series.map((s) => [s.label, !!s.faded])).toEqual([
    ["Compute · runs", false],
    ["Compute · unallocated", true],
    ["Block storage · runs", false],
    ["Block storage · unallocated", true],
  ]);
  expect(c!.series[0]!.color).toBe("var(--chart-1)");
  expect(c!.series[2]!.color).toBe("var(--chart-3)");
  expect(c!.ys).toEqual([
    [0.100000001, null],
    [0.2, null],
    [0.01, null],
    [0.02, null],
  ]);
  expect(c!.totals).toEqual(["0.100000001", "0.2", "0.01", "0.02"]);
  // Unallocated absent (a tenant on a platform pool): the runs series only; a family with no rows has none.
  const [t] = hostCharts([{ t: t0, family: "compute", currency: "USD", allocated: "1" }], "2026-10-09T00:00:00Z", "2026-10-09T02:00:00Z", 3600);
  expect(t!.series.map((s) => s.label)).toEqual(["Compute · runs"]);
  expect(t!.ys).toEqual([[1, null]]);
});

const line = (family: string, item: string, placements: object[], currency = "USD"): CostLine => ({ runId: "r", source: "compute", family, item, amount: "0", currency, from: "", to: "", final: true, details: { placements }, reportedAt: "" });
const pl = (epoch: number, hostId: string, amount: string, share = 0.47) => ({ epoch, hostId, from: `2026-10-05T1${epoch}:00:00Z`, to: `2026-10-05T1${epoch}:30:00Z`, cpus: 2, memory: 1, amount, share, ratePerHour: "1", finalized: true });

test("placementRows: compute and block storage joined per placement; a compute-only placement has no block storage (null, not 0)", () => {
  const lines = [
    line("ai", "claude", []),
    line("compute", "m8g.2xlarge:spot", [pl(1, "hA", "0.167"), pl(2, "hB", "0.3422")]),
    // A second instance type: another compute line.
    line("compute", "m7i.large", [pl(3, "hB", "0.0119")]),
    line("block-storage", "gp3:100GiB", [pl(2, "hB", "0.0348"), pl(3, "hB", "0.0012")]),
  ];
  const rows = placementRows(lines);
  expect(rows.map((r) => [r.epoch, r.hostId, r.compute, r.blockStorage, r.total, r.share])).toEqual([
    [1, "hA", "0.167", null, "0.167", 0.47],
    [2, "hB", "0.3422", "0.0348", "0.377", 0.47],
    [3, "hB", "0.0119", "0.0012", "0.0131", 0.47],
  ]);
  expect(placementTotals(rows)).toEqual([{ currency: "USD", compute: "0.5211", blockStorage: "0.036", total: "0.5571" }]);
  // A placement with block storage but its compute missing (no rate): compute null.
  const bsOnly = placementRows([line("block-storage", "gp3:100GiB", [pl(4, "hC", "0.01")])]);
  expect(bsOnly[0]!.compute).toBeNull();
  expect(bsOnly[0]!.blockStorage).toBe("0.01");
  // A plugin line (another source) is never joined, nor a line without placements.
  expect(placementRows([{ ...line("compute", "x", [pl(1, "h", "1")]), source: "gateway" }, { ...line("compute", "y", []), details: null }])).toEqual([]);
});

test("placementRows: the same placement in two currencies is two rows, each summed apart", () => {
  const rows = placementRows([line("compute", "static:USD", [pl(1, "h", "1")]), line("compute", "static:EUR", [pl(1, "h", "2")], "EUR")]);
  expect(rows.map((r) => [r.currency, r.total])).toEqual([
    ["EUR", "2"],
    ["USD", "1"],
  ]);
});

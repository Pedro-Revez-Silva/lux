import { expect, test } from "bun:test";
import type { CostSummary, CostSummaryRow } from "../../api/index.ts";
import {
  addFilter,
  bandFilter,
  bandLabel,
  breakdownBands,
  breakdownCharts,
  breakdownParam,
  breakdownRows,
  changes,
  defaultLabelKey,
  familyCharts,
  familyRows,
  filterQuery,
  filtersParams,
  filterText,
  keyLabel,
  NONE,
  OTHER,
  parseBreakdown,
  parseFilters,
  peakBands,
  peakRuns,
  peaks,
  peakWindows,
  previousWindow,
  runsListPath,
  familyCurrency,
  FOLDED,
  runsPerFamily,
  showFamily,
  shownTotals,
  sideTotals,
  topSplit,
} from "./costView.ts";

const fam = (family: string, currency: string, amount: string, at?: string): CostSummaryRow => ({ group: { family }, currency, amount, ...(at ? { at } : {}) });
const runFam = (run: string, family: string, currency: string, amount: string): CostSummaryRow => ({ group: { run, family }, currency, amount });

test("Show as the summary's family filter: compute is family=compute, external nofamily=compute", () => {
  expect(showFamily("all")).toEqual({});
  expect(showFamily("compute")).toEqual({ family: "compute" });
  expect(showFamily("external")).toEqual({ nofamily: "compute" });
});

const ROWS = [fam("compute", "USD", "3.87"), fam("ai", "USD", "55.08"), fam("video", "USD", "1.05"), fam("compute", "EUR", "2"), fam("ai", "EUR", "0.5")];

test("split: Compute is the compute family, External every other, per currency and exact", () => {
  expect(shownTotals(ROWS, "all")).toEqual([
    { currency: "EUR", amount: "2.5" },
    { currency: "USD", amount: "60" },
  ]);
  expect(shownTotals(ROWS, "compute")).toEqual([
    { currency: "EUR", amount: "2" },
    { currency: "USD", amount: "3.87" },
  ]);
  expect(shownTotals(ROWS, "external")).toEqual([
    { currency: "EUR", amount: "0.5" },
    { currency: "USD", amount: "56.13" },
  ]);
  expect(sideTotals(ROWS)).toEqual([
    { currency: "EUR", all: "2.5", compute: "2", external: "0.5" },
    { currency: "USD", all: "60", compute: "3.87", external: "56.13" },
  ]);
});

test("split: a side with no cost is no figure, not zero; a currency with no shown cost is absent", () => {
  const rows = [fam("ai", "USD", "1"), fam("compute", "EUR", "2")];
  expect(sideTotals(rows)).toEqual([
    { currency: "EUR", all: "2", compute: "2", external: null },
    { currency: "USD", all: "1", compute: null, external: "1" },
  ]);
  expect(shownTotals(rows, "compute")).toEqual([{ currency: "EUR", amount: "2" }]);
});

test("family rows: only the shown side, largest first per currency, share of the shown total", () => {
  expect(familyRows(ROWS, "external").map((r) => [r.currency, r.family, r.amount, r.share?.toFixed(3)])).toEqual([
    ["EUR", "ai", "0.5", "1.000"],
    ["USD", "ai", "55.08", "0.981"],
    ["USD", "video", "1.05", "0.019"],
  ]);
  expect(familyRows(ROWS, "all").filter((r) => r.currency === "USD").map((r) => r.family)).toEqual(["ai", "compute", "video"]);
});

const T = (h: number) => new Date(Date.UTC(2026, 9, 9, h)).toISOString();
const SUMMARY: CostSummary = {
  from: T(0),
  to: T(4),
  basis: "list",
  totals: [],
  series: [fam("compute", "USD", "1", T(0)), fam("ai", "USD", "2", T(0)), fam("compute", "USD", "1.5", T(1)), fam("ai", "USD", "40", T(3)), fam("ai", "EUR", "9", T(1)), fam("compute", "EUR", "3", T(2))],
  families: [{ family: "compute", displayName: "Compute" }, { family: "ai", displayName: "AI models", color: "violet" }],
};

test("charts: one per currency, a series per shown family, a bucket with no row a gap", () => {
  const all = familyCharts(SUMMARY, "hour", "all");
  expect(all.map((c) => c.currency)).toEqual(["EUR", "USD"]);
  const usd = all.find((c) => c.currency === "USD")!;
  expect(usd.x.length).toBe(4);
  expect(usd.series.map((s) => s.label)).toEqual(["Compute", "AI models"]);
  expect(usd.ys).toEqual([
    [1, 1.5, null, null],
    [2, null, null, 40],
  ]);
  expect(usd.totals).toEqual(["2.5", "42"]);
  const compute = familyCharts(SUMMARY, "hour", "compute");
  expect(compute.find((c) => c.currency === "USD")!.series.map((s) => s.label)).toEqual(["Compute"]);
  expect(compute.find((c) => c.currency === "USD")!.ys).toEqual([[1, 1.5, null, null]]);
  // External hides compute's figures: none of its values is in the chart.
  const ext = familyCharts(SUMMARY, "hour", "external").find((c) => c.currency === "EUR")!;
  expect(ext.ys).toEqual([[null, 9, null, null]]);
});

test("Top Runs: ranked by what Show counts, per currency, with every family's part kept for the split", () => {
  const rows = [runFam("r1", "compute", "USD", "10"), runFam("r1", "ai", "USD", "1"), runFam("r2", "ai", "USD", "5"), runFam("r2", "compute", "USD", "0.5"), runFam("r3", "ai", "EUR", "100")];
  expect(topSplit(rows, "run", "all").map((r) => [r.currency, r.key, r.amount, r.all])).toEqual([
    ["EUR", "r3", "100", "100"],
    ["USD", "r1", "11", "11"],
    ["USD", "r2", "5.5", "5.5"],
  ]);
  const ext = topSplit(rows, "run", "external");
  expect(ext.map((r) => [r.currency, r.key, r.amount])).toEqual([
    ["EUR", "r3", "100"],
    ["USD", "r2", "5"],
    ["USD", "r1", "1"],
  ]);
  expect(ext.find((r) => r.key === "r1")!.parts).toEqual([
    { family: "compute", amount: "10" },
    { family: "ai", amount: "1" },
  ]);
  // A Run with nothing on the shown side is not listed (not a $0 row).
  expect(topSplit(rows, "run", "compute").map((r) => r.key)).toEqual(["r1", "r2"]);
  expect(topSplit(rows, "run", "all", 1).map((r) => r.key)).toEqual(["r3", "r1"]);
});

test("previous window: the same length, just before", () => {
  expect(previousWindow({ from: T(4), to: T(8) })).toEqual({ from: T(0), to: T(4) });
  expect(previousWindow({})).toBeNull();
});

test("change vs previous: per currency; no earlier figure, or a zero or refund one, has none", () => {
  expect(changes([{ currency: "USD", amount: "13.8" }, { currency: "EUR", amount: "1" }, { currency: "GBP", amount: "1" }], [{ currency: "USD", amount: "10" }, { currency: "GBP", amount: "0" }]).map((c) => [c.currency, c.ratio == null ? null : Number(c.ratio.toFixed(3))])).toEqual([
    ["USD", 0.38],
    ["EUR", null],
    ["GBP", null],
  ]);
  expect(changes([{ currency: "USD", amount: "5" }], [{ currency: "USD", amount: "10" }])[0]!.ratio).toBe(-0.5);
});

test("peak: per currency, the largest bucket of what Show counts; hidden families do not lift it", () => {
  const all = peaks(SUMMARY, "all");
  expect(all.map((p) => [p.currency, new Date(p.at * 1000).toISOString(), p.amount])).toEqual([
    ["EUR", T(1), "9"],
    ["USD", T(3), "40"],
  ]);
  const compute = peaks(SUMMARY, "compute");
  expect(compute.map((p) => [p.currency, new Date(p.at * 1000).toISOString(), p.amount])).toEqual([
    ["EUR", T(2), "3"],
    ["USD", T(1), "1.5"],
  ]);
  expect(peakWindows(all, "hour")).toEqual([
    { from: T(1), to: T(2) },
    { from: T(3), to: T(4) },
  ]);
  expect(peakWindows([{ currency: "USD", at: Date.parse(T(0)) / 1000, amount: "1" }, { currency: "EUR", at: Date.parse(T(0)) / 1000, amount: "1" }], "day")).toEqual([{ from: T(0), to: T(24) }]);
});

test("peak Run: the costliest Run of that bucket, as Show counts, per currency", () => {
  const at = Date.parse(T(3)) / 1000;
  const ps = [{ currency: "USD", at, amount: "40" }];
  const rows = [runFam("big-compute", "compute", "USD", "30"), runFam("big-ai", "ai", "USD", "20"), runFam("eur", "ai", "EUR", "99")];
  expect(peakRuns(ps, [{ at, rows }], "all").get("USD")).toBe("big-compute");
  expect(peakRuns(ps, [{ at, rows }], "external").get("USD")).toBe("big-ai");
  expect(peakRuns(ps, [{ at: at + 3600, rows }], "all").size).toBe(0);
});

const app = (v: string, family: string, currency: string, amount: string, at?: string): CostSummaryRow => ({ group: { "label:app": v, family }, currency, amount, ...(at ? { at } : {}) });
/** luxd's fold row: other: true, its value reading (other). */
const folded = (family: string, currency: string, amount: string, extra: Partial<CostSummaryRow> = {}): CostSummaryRow => ({ group: { "label:app": "(other)", family }, currency, amount, other: true, ...extra });

test("breakdown: the top 7 values by shown cost, luxd's (other) as Other with its count, the value-less band last and grey", () => {
  // luxd folded three values past the top 7 into (other).
  const rows = Array.from({ length: 7 }, (_, i) => app(`v${i}`, "ai", "USD", String(100 - i)));
  rows.push(folded("ai", "USD", "200"), app(NONE, "compute", "USD", "500"));
  const bands = breakdownBands(rows, "label:app", "all", { USD: 3 }).get("USD")!;
  expect(bands.map((b) => b.id)).toEqual(["v0", "v1", "v2", "v3", "v4", "v5", "v6", OTHER, NONE]);
  expect(bands.find((b) => b.id === OTHER)!.values).toEqual([FOLDED]);
  expect(bandLabel(bands.find((b) => b.id === OTHER)!, { kind: "label", key: "app" })).toBe("Other (3)");
  expect(bands.at(-1)!.color).toBe("var(--st-neutral-dot)");
  expect(bands.find((b) => b.id === OTHER)!.color).toBe("var(--st-neutral-fg)");
  // Nothing folded: no Other.
  expect(breakdownBands(rows.slice(0, 7), "label:app", "all").get("USD")!.map((b) => b.id)).toEqual(["v0", "v1", "v2", "v3", "v4", "v5", "v6"]);
  // A value past the limit locally (Show ranks differently than luxd did) joins Other too.
  const twoRows = [app("a", "ai", "USD", "1"), app("b", "ai", "USD", "2"), folded("ai", "USD", "1")].map((r, i) => ({ ...r, runs: [3, 2, 4][i] }));
  const two = breakdownBands(twoRows, "label:app", "all", { USD: 4 }, 1).get("USD")!;
  expect(two.map((b) => [b.id, b.values, b.count])).toEqual([
    ["b", ["b"], undefined],
    [OTHER, ["a", FOLDED], 5],
  ]);
  // Other's Runs are the sum over every value it holds: a's 3 and the fold's 4.
  expect(breakdownRows(twoRows, "label:app", "all", new Map([["USD", two]])).map((r) => [r.band.id, r.runs])).toEqual([
    ["b", 2],
    [OTHER, 7],
  ]);
});

test("breakdown: Show still applies; a value with nothing shown has no band, and never leaks into the figures", () => {
  const rows = [app("jervasion", "ai", "USD", "50"), app("jervasion", "compute", "USD", "2"), app("dude", "compute", "USD", "7"), app(NONE, "compute", "USD", "1")];
  const ext = breakdownBands(rows, "label:app", "external");
  expect(ext.get("USD")!.map((b) => b.id)).toEqual(["jervasion"]);
  const all = breakdownBands(rows, "label:app", "all");
  expect(all.get("USD")!.map((b) => b.id)).toEqual(["jervasion", "dude", NONE]);
  const r = breakdownRows(rows, "label:app", "external", ext);
  expect(r.map((x) => [x.band.id, x.compute, x.external, x.amount, x.share])).toEqual([["jervasion", "2", "50", "50", 1]]);
  const comp = breakdownRows(rows, "label:app", "compute", breakdownBands(rows, "label:app", "compute"));
  expect(comp.map((x) => [x.band.id, x.amount])).toEqual([
    ["dude", "7"],
    ["jervasion", "2"],
    [NONE, "1"],
  ]);
});

test("breakdown: currencies stay apart; each has its own bands, ranks and shares, nothing summed across", () => {
  const rows = [app("a", "ai", "USD", "1"), app("b", "ai", "USD", "9"), app("a", "ai", "EUR", "100"), app("b", "ai", "EUR", "1")];
  const bands = breakdownBands(rows, "label:app", "all");
  expect(bands.get("USD")!.map((b) => b.id)).toEqual(["b", "a"]);
  expect(bands.get("EUR")!.map((b) => b.id)).toEqual(["a", "b"]);
  const out = breakdownRows(rows, "label:app", "all", bands);
  expect(out.map((x) => [x.currency, x.band.id, x.amount, x.share?.toFixed(2)])).toEqual([
    ["EUR", "a", "100", "0.99"],
    ["EUR", "b", "1", "0.01"],
    ["USD", "b", "9", "0.90"],
    ["USD", "a", "1", "0.10"],
  ]);
});

test("breakdown charts: a series per band, Other summed exactly, a bucket with no row a gap", () => {
  // The series as asked: grouped by the breakdown alone, Show applied by luxd.
  const one = (v: string, amount: string, at: string): CostSummaryRow => ({ group: { "label:app": v }, currency: "USD", amount, at });
  const series = [one("a", "1", T(0)), one("b", "0.1", T(0)), { ...one("(other)", "0.2", T(0)), other: true }, one("a", "2", T(2))];
  const d: CostSummary = { from: T(0), to: T(3), basis: "list", totals: [], series };
  const bands = breakdownBands([app("a", "ai", "USD", "3"), app("b", "ai", "USD", "0.1"), folded("ai", "USD", "0.2")], "label:app", "all", { USD: 1 }, 1);
  const [c] = breakdownCharts(d, "label:app", "hour", bands);
  expect(c!.bands.map((b) => b.id)).toEqual(["a", OTHER]);
  expect(c!.ys).toEqual([
    [1, null, 2],
    [0.3, null, null],
  ]);
  expect(c!.totals).toEqual(["3", "0.3"]);
});

test("breakdown charts: a series value the split has no band for is drawn in Other, and the stack still sums to the series", () => {
  const one = (v: string, amount: string, at: string): CostSummaryRow => ({ group: { "label:app": v }, currency: "USD", amount, at });
  // The split kept a and b; by the series poll c has overtaken b.
  const series = [one("a", "5", T(0)), one("c", "5.1", T(0)), { ...one("(other)", "5", T(0)), other: true }, one(NONE, "1", T(1))];
  const d: CostSummary = { from: T(0), to: T(2), basis: "list", totals: [], series };
  const split = [app("a", "ai", "USD", "5"), app("b", "ai", "USD", "4"), folded("ai", "USD", "5"), app(NONE, "ai", "USD", "1")];
  const [c] = breakdownCharts(d, "label:app", "hour", breakdownBands(split, "label:app", "all", { USD: 3 }, 2));
  expect(c!.bands.map((b) => b.id)).toEqual(["a", "b", OTHER, NONE]);
  expect(c!.ys).toEqual([
    [5, null],
    [null, null],
    [10.1, null],
    [null, 1],
  ]);
  expect(c!.totals).toEqual(["5", "0", "10.1", "1"]);
  // No fold in the split: an Other band is added before (none), in Other's colour.
  const [n] = breakdownCharts(d, "label:app", "hour", breakdownBands([app("a", "ai", "USD", "5"), app(NONE, "ai", "USD", "1")], "label:app", "all"));
  expect(n!.bands.map((b) => [b.id, b.color])).toEqual([
    ["a", "var(--chart-2)"],
    [OTHER, "var(--st-neutral-fg)"],
    [NONE, "var(--st-neutral-dot)"],
  ]);
  expect(n!.totals).toEqual(["5", "10.1", "1"]);
});

test("breakdown: Runs per band from the folded totals; the peak names the band that dominated it", () => {
  const rows: CostSummaryRow[] = [
    { group: { "label:app": "a", family: "ai" }, currency: "USD", amount: "1", runs: 2 },
    { group: { "label:app": "a", family: "compute" }, currency: "USD", amount: "1", runs: 2 },
    folded("ai", "USD", "1", { runs: 5 }),
    { group: { "label:app": NONE, family: "ai" }, currency: "USD", amount: "1", runs: 1 },
  ];
  const bands = breakdownBands(rows, "label:app", "all", { USD: 3 });
  expect(breakdownRows(rows, "label:app", "all", bands).map((r) => [r.band.id, r.runs])).toEqual([
    ["a", 2],
    [OTHER, 5],
    [NONE, 1],
  ]);
  // Without runs on the rows (no top), the count is unknown, not zero.
  expect(breakdownRows([app("a", "ai", "USD", "1")], "label:app", "all", breakdownBands([app("a", "ai", "USD", "1")], "label:app", "all"))[0]!.runs).toBeNull();
  const one = (v: string, amount: string, at: string): CostSummaryRow => ({ group: { "label:app": v }, currency: "USD", amount, at });
  const series = [one("a", "9", T(1)), one("b", "1", T(1)), one("b", "5", T(2))];
  const d: CostSummary = { from: T(0), to: T(3), basis: "list", totals: [], series };
  const sb = breakdownBands([app("a", "ai", "USD", "9"), app("b", "ai", "USD", "6")], "label:app", "all");
  const pk = peakBands(peaks({ ...d, series: series.map((r) => ({ ...r, group: { family: "ai" } })) }, "all"), d, "label:app", sb).get("USD")!;
  expect([pk.band.id, pk.share]).toEqual(["a", 0.9]);
  // The fold dominating the peak bucket names Other, keyed by other: true, not by the value it reads.
  const withFold = [one("a", "9", T(1)), { ...one("(other)", "30", T(1)), other: true }, one("b", "5", T(2))];
  const fd: CostSummary = { ...d, series: withFold };
  const fb = breakdownBands([app("a", "ai", "USD", "9"), app("b", "ai", "USD", "5"), folded("ai", "USD", "30")], "label:app", "all", { USD: 2 });
  const fpk = peakBands(peaks({ ...fd, series: withFold.map((r) => ({ ...r, group: { family: "ai" } })) }, "all"), fd, "label:app", fb).get("USD")!;
  expect([fpk.band.id, fpk.share]).toEqual([OTHER, 30 / 39]);
});

test("family Runs: from the family summary's runs, per family and currency", () => {
  expect(runsPerFamily([fam("compute", "USD", "1")])).toBeUndefined();
  const runs = runsPerFamily([{ ...fam("compute", "USD", "1"), runs: 3 }, { ...fam("compute", "EUR", "1"), runs: 1 }, { ...fam("ai", "USD", "1"), runs: 2 }])!;
  expect([familyCurrency("compute", "USD"), familyCurrency("compute", "EUR"), familyCurrency("ai", "USD"), familyCurrency("ai", "EUR")].map((k) => runs.get(k))).toEqual([3, 1, 2, undefined]);
});

test("Top Runs: luxd's fold is never a Run", () => {
  expect(topSplit([runFam("r1", "ai", "USD", "1"), { ...runFam("(other)", "ai", "USD", "9"), other: true }], "run", "all").map((r) => r.key)).toEqual(["r1"]);
});

test("a real value named (other) is a band of its own; Other is only luxd's fold, with no count when it has none", () => {
  const rows = [app("(other)", "ai", "USD", "5"), app("a", "ai", "USD", "1")];
  const bands = breakdownBands(rows, "label:app", "all").get("USD")!;
  expect(bands.map((b) => [b.id, bandLabel(b, { kind: "label", key: "app" })])).toEqual([
    ["(other)", "(other)"],
    ["a", "a"],
  ]);
  // Both at once: the value and the fold stay apart.
  const both = breakdownBands([...rows, folded("ai", "USD", "9")], "label:app", "all", { USD: 2 }).get("USD")!;
  expect(both.map((b) => [b.id, b.values])).toEqual([
    ["(other)", ["(other)"]],
    ["a", ["a"]],
    [OTHER, [FOLDED]],
  ]);
  expect(bandLabel(both.at(-1)!, { kind: "label", key: "app" })).toBe("Other (2)");
  // A fold whose count is unknown reads Other, never Other (0).
  expect(bandLabel(breakdownBands([folded("ai", "USD", "9")], "label:app", "all").get("USD")![0]!, { kind: "label", key: "app" })).toBe("Other");
});

test("ties break by code unit, as luxd's COLLATE \"C\": B before b", () => {
  const bands = breakdownBands([app("b", "ai", "USD", "1"), app("B", "ai", "USD", "1"), app("a", "ai", "USD", "1")], "label:app", "all").get("USD")!;
  expect(bands.map((b) => b.id)).toEqual(["B", "a", "b"]);
});

test("band colours: never compute's, in rank order; Other and (none) neutral", () => {
  const rows = Array.from({ length: 7 }, (_, i) => app(`v${i}`, "compute", "USD", String(100 - i)));
  rows.push(folded("compute", "USD", "1"), app(NONE, "compute", "USD", "1"));
  const bands = breakdownBands(rows, "label:app", "compute", { USD: 1 }).get("USD")!;
  expect(bands.map((b) => b.color)).toEqual(["var(--chart-2)", "var(--chart-3)", "var(--chart-4)", "var(--chart-5)", "var(--chart-6)", "var(--chart-7)", "var(--chart-8)", "var(--st-neutral-fg)", "var(--st-neutral-dot)"]);
  expect(bands.map((b) => b.color)).not.toContain("var(--chart-1)");
});

test("breakdown labels: label values, (no key label), key names with the operator and pre-tracking cases", () => {
  const keys = new Map([
    ["k1", { id: "k1", name: "ci-bot" }],
    ["ko", { id: "ko", operator: true }],
    ["email:ada@x.io", { id: "email:ada@x.io", email: "ada@x.io" }],
  ]);
  const band = (id: string) => ({ id, values: [id], color: "" });
  expect(bandLabel(band(NONE), { kind: "label", key: "app" })).toBe("(no app label)");
  expect(bandLabel({ id: OTHER, values: ["x", "y"], color: "" }, { kind: "label", key: "app" })).toBe("Other (2)");
  expect(["k1", "ko", "email:ada@x.io", NONE, "k9"].map((k) => keyLabel(k, keys))).toEqual(["ci-bot", "Operator key", "ada@x.io", "Before key tracking", "k9"]);
});

test("?by=: label:key, key, pool; tenant only where tenants are shown; anything else is Family", () => {
  expect(parseBreakdown("label:app", false)).toEqual({ kind: "label", key: "app" });
  expect(parseBreakdown("label:a=b", false)).toEqual({ kind: "label", key: "a=b" });
  expect(parseBreakdown("key", false)).toEqual({ kind: "key" });
  expect(parseBreakdown("tenant", false)).toEqual({ kind: "family" });
  expect(parseBreakdown("tenant", true)).toEqual({ kind: "tenant" });
  expect(parseBreakdown("bogus", true)).toEqual({ kind: "family" });
  expect(breakdownParam({ kind: "family" })).toBeNull();
  expect(breakdownParam({ kind: "label", key: "app" })).toBe("label:app");
  expect(defaultLabelKey("", ["team", "app"])).toBe("app");
  expect(defaultLabelKey("", ["team"])).toBe("team");
  expect(defaultLabelKey("repo", ["app"])).toBe("repo");
});

test("?label= and ?nolabel=: one key repeated is one filter of several values; a value keeps its = , and unicode; a key luxd refuses is dropped", () => {
  const fs = parseFilters(["app=jervasion", "repo=a/b", "app=dude", "app=jervasion", "note=x=y,z ü", "broken", "bad key=x", "-x=1"], ["phase", "a b", ""]);
  expect(fs).toEqual([
    { key: "app", values: ["jervasion", "dude"] },
    { key: "repo", values: ["a/b"] },
    { key: "note", values: ["x=y,z ü"] },
    { key: "phase", values: [], notSet: true },
  ]);
  expect(filtersParams(fs)).toEqual({ label: ["app=jervasion", "app=dude", "repo=a/b", "note=x=y,z ü"], nolabel: ["phase"] });
  expect(filterQuery([])).toEqual({});
  expect(filterQuery(fs)).toEqual({ label: ["app=jervasion", "app=dude", "repo=a/b", "note=x=y,z ü"], nolabel: ["phase"] });
  expect(filterText(fs[0]!)).toEqual({ key: "app", op: "∈", values: "jervasion, dude" });
  expect(filterText(fs[3]!)).toEqual({ key: "phase", op: "is", values: "not set" });
});

test("filters: a value joins its key (OR); not set replaces it; a clicked band becomes a filter", () => {
  let fs = addFilter([], { key: "app", values: ["a"] });
  fs = addFilter(fs, { key: "app", values: ["b"] });
  fs = addFilter(fs, { key: "team", values: ["x"] });
  expect(fs).toEqual([
    { key: "app", values: ["a", "b"] },
    { key: "team", values: ["x"] },
  ]);
  expect(addFilter(fs, { key: "app", values: [], notSet: true })).toEqual([
    { key: "team", values: ["x"] },
    { key: "app", values: [], notSet: true },
  ]);
  const lb = { kind: "label" as const, key: "app" };
  expect(bandFilter({ id: "dude", values: ["dude"], color: "" }, lb)).toEqual({ key: "app", values: ["dude"] });
  expect(bandFilter({ id: NONE, values: [NONE], color: "" }, lb)).toEqual({ key: "app", values: [], notSet: true });
  expect(bandFilter({ id: OTHER, values: ["x"], color: "" }, lb)).toBeNull();
  expect(bandFilter({ id: "k1", values: ["k1"], color: "" }, { kind: "key" })).toBeNull();
});

test("All Runs: the Runs list takes one key=value; more is the plain list", () => {
  expect(runsListPath([])).toBe("/runs");
  expect(runsListPath([{ key: "app", values: ["a b"] }])).toBe("/runs?label=app%3Da%20b");
  expect(runsListPath([{ key: "app", values: ["a", "b"] }])).toBe("/runs");
  expect(runsListPath([{ key: "app", values: [], notSet: true }])).toBe("/runs");
});

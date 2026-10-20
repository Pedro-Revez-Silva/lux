import { expect, test } from "bun:test";
import { costSummary } from "../mockCosts.ts";

interface Row {
  group: Record<string, string>;
  currency: string;
  amount: string;
  runs?: number;
  other?: boolean;
}
const ask = (q: string) => costSummary(new URL(`http://x/v1/costs?since=7d&${q}`)).body as { totals: Row[]; otherCount?: Record<string, number>; unallocated?: { family: string }[] };
const micros = (rows: Row[]) => rows.reduce((n, r) => n + Math.round(Number(r.amount) * 1e6), 0);

// The dev mock folds as luxd does, so screenshots and local runs show what luxd would.
test("mock /v1/costs top=N: the fold is other: true, otherCount counts it, nothing is lost", () => {
  const plain = ask("group=label:app");
  expect(plain.totals.map((r) => r.group["label:app"]).sort()).toEqual(["(none)", "dude", "jervasion"]);
  expect(plain.otherCount).toBeUndefined();

  // By compute, jervasion (5 per hour) leads dude (2): dude is folded.
  const top = ask("group=label:app&top=1&rank=compute");
  expect(top.totals.map((r) => [r.group["label:app"], !!r.other]).sort()).toEqual([
    ["(none)", false],
    ["(other)", true],
    ["jervasion", false],
  ]);
  expect(top.otherCount).toEqual({ USD: 1 });
  expect(micros(top.totals)).toBe(micros(plain.totals));
  // Each totals row carries runs: dude's two Runs under the fold.
  expect(top.totals.find((r) => r.other)!.runs).toBe(2);

  // Nothing to fold: no other row, no otherCount.
  const all = ask("group=label:app&top=5");
  expect(all.totals.some((r) => r.other)).toBe(false);
  expect(all.otherCount).toBeUndefined();
});

test("mock /v1/costs runs=true: Runs per family without a fold", () => {
  const fam = ask("group=family&runs=true");
  expect(Object.fromEntries(fam.totals.map((r) => [r.group.family, r.runs]))).toEqual({ ai: 7, "block-storage": 8, compute: 8, "data-transfer": 8, "public-ip": 8 });
  expect(ask("group=family").totals.every((r) => r.runs === undefined)).toBe(true);
});

test("mock /v1/costs: repeated family and nofamily keep or drop every value, totals and unallocated alike", () => {
  const fams = (q: string) => {
    const b = ask(`group=family&${q}`);
    return [b.totals.map((r) => r.group.family).sort(), [...new Set((b.unallocated ?? []).map((u) => u.family))].sort()];
  };
  expect(fams("family=compute&family=block-storage")).toEqual([["block-storage", "compute"], ["block-storage", "compute"]]);
  expect(fams("nofamily=compute&nofamily=block-storage&nofamily=public-ip&nofamily=data-transfer")).toEqual([["ai"], []]);
  expect(fams("family=compute")).toEqual([["compute"], ["compute"]]);
  expect(fams("nofamily=compute")).toEqual([["ai", "block-storage", "data-transfer", "public-ip"], ["block-storage", "data-transfer", "public-ip"]]);
});

test("mock /v1/costs rank: compute counts block storage, external never does", () => {
  const runsOf = (q: string) => Object.fromEntries(ask(q).totals.map((r) => [r.group.run, r.runs]));
  // Block storage alone, ranked by compute: every disk's Run counts as contributing.
  const disks = runsOf("group=run&family=block-storage&top=50&rank=compute");
  expect(Object.keys(disks).length).toBe(8);
  expect(Object.values(disks).every((n) => n === 1)).toBe(true);
  // run_bsn6jhmfcejni8a has compute and block storage only: no external cost, no contributing Run.
  const ext = runsOf("group=run&top=50&rank=external");
  expect(ext["run_bsn6jhmfcejni8a"]).toBe(0);
  expect(ext["run_a4aao3fvcuzpa2k"]).toBe(1);
});

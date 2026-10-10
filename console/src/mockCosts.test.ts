import { expect, test } from "bun:test";
import { costSummary } from "../mockCosts.ts";

interface Row {
  group: Record<string, string>;
  currency: string;
  amount: string;
  runs?: number;
  other?: boolean;
}
const ask = (q: string) => costSummary(new URL(`http://x/v1/costs?since=7d&${q}`)).body as { totals: Row[]; otherCount?: Record<string, number> };
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
  expect(Object.fromEntries(fam.totals.map((r) => [r.group.family, r.runs]))).toEqual({ ai: 7, "block-storage": 8, compute: 8 });
  expect(ask("group=family").totals.every((r) => r.runs === undefined)).toBe(true);
});

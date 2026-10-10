import { expect, test } from "bun:test";
import { renderToStaticMarkup } from "react-dom/server";
import { PartBar, partShares } from "./PartBar.tsx";

test("partShares: whole percents by largest remainder add up to exactly 100", () => {
  // 5.15 / 1.30 / 0.42 / 0.07 of 6.94: plain rounding gives 74 + 19 + 6 + 1 = 100 here,
  // and 33.3 x 3 would give 99; largest remainder fixes the latter.
  expect(partShares([5.15, 1.3, 0.42, 0.07]).map((s) => s.percent)).toEqual([74, 19, 6, 1]);
  expect(partShares([1, 1, 1]).map((s) => s.percent)).toEqual([34, 33, 33]);
  expect(partShares([2, 2, 2, 1]).map((s) => s.percent)).toEqual([29, 29, 28, 14]);
  for (const vs of [[1, 1, 1], [0.1, 0.2, 0.7], [3, 3, 3, 3, 3, 3, 3], [999, 1, 1]]) {
    expect(partShares(vs).reduce((a, s) => a + s.percent, 0)).toBe(100);
  }
});

test("partShares: a zero, negative or missing part has no share and no label; the rest share the whole", () => {
  const s = partShares([3, 0, null, -2, undefined, 1, Number.NaN]);
  expect(s.map((x) => x.share)).toEqual([0.75, 0, 0, 0, 0, 0.25, 0]);
  expect(s.map((x) => x.label)).toEqual(["75%", "", "", "", "", "25%", ""]);
});

test("partShares: a drawn part that rounds to 0% reads <1%, never 0%", () => {
  const s = partShares([1000, 1]);
  expect(s.map((x) => x.percent)).toEqual([100, 0]);
  expect(s[1]!.label).toBe("<1%");
  expect(s[1]!.share).toBeGreaterThan(0);
});

test("partShares: nothing drawn is all zero", () => {
  expect(partShares([0, null])).toEqual([
    { share: 0, percent: 0, label: "" },
    { share: 0, percent: 0, label: "" },
  ]);
  expect(partShares([])).toEqual([]);
});

test("PartBar draws a segment per non-zero part, sized by its share", () => {
  const html = renderToStaticMarkup(
    <PartBar
      label="Why"
      parts={[
        { label: "Busy", value: 3, color: "var(--chart-4)" },
        { label: "Idle", value: 0, color: "var(--chart-2)" },
        { label: "Booting", value: 1, color: "var(--chart-5)" },
      ]}
    />,
  );
  const parts = [...html.matchAll(/data-part="([^"]+)"/g)].map((m) => m[1]);
  expect(parts).toEqual(["Busy", "Booting"]);
  expect([...html.matchAll(/flex-grow:([\d.]+)/g)].map((m) => Number(m[1]))).toEqual([0.75, 0.25]);
});

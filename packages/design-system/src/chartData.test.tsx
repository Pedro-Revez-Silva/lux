import { expect, test } from "bun:test";
import { GlobalRegistrator } from "@happy-dom/global-registrator";
import { renderToStaticMarkup } from "react-dom/server";
import { barRange, barSegments, barStep, bucketText, stackData, stackTotal } from "./chartData.ts";
import { TimeSeriesChart } from "./TimeSeriesChart.tsx";

const none = new Set<number>();

test("barSegments stacks each visible series on the ones below it", () => {
  const s = barSegments([[1, 2, 3], [10, 20, 30]], none, 3, true);
  expect(s).toEqual([
    { y0: [0, 0, 0], y1: [1, 2, 3] },
    { y0: [1, 2, 3], y1: [11, 22, 33] },
  ]);
});

test("barSegments: a missing value draws no bar and adds nothing; the bar above starts where the stack is", () => {
  const s = barSegments([[null, 2, undefined], [10, null, 30]], none, 3, true);
  expect(s[0]).toEqual({ y0: [null, 0, null], y1: [null, 2, null] });
  // Bucket 0: nothing below, so the second series starts at zero, not on a phantom bar.
  expect(s[1]).toEqual({ y0: [0, null, 0], y1: [10, null, 30] });
});

test("barSegments: a bucket where every series is missing stays empty, not a zero-height bar", () => {
  const s = barSegments([[null, 1], [null, 2]], none, 2, true);
  expect(s.map((x) => x.y1[0])).toEqual([null, null]);
  expect(s.map((x) => x.y0[0])).toEqual([null, null]);
});

test("barSegments: a hidden series draws nothing and the next one restacks onto what is visible", () => {
  const s = barSegments([[1, 2], [10, 20], [100, 200]], new Set([1]), 2, true);
  expect(s[1]).toEqual({ y0: [null, null], y1: [null, null] });
  expect(s[2]).toEqual({ y0: [1, 2], y1: [101, 202] });
});

test("barSegments: a refund stacks down from zero, apart from the positive stack", () => {
  const s = barSegments([[2], [-6], [-1], [3]], none, 1, true);
  expect(s.map((x) => [x.y0[0], x.y1[0]])).toEqual([
    [0, 2],
    [0, -6],
    [-6, -7],
    [2, 5],
  ]);
});

test("barSegments unstacked: every bar starts at zero", () => {
  const s = barSegments([[1, 2], [3, null]], none, 2, false);
  expect(s).toEqual([
    { y0: [0, 0], y1: [1, 2] },
    { y0: [0, null], y1: [3, null] },
  ]);
});

test("barStep is the smallest positive step; barRange leaves half a bucket at either end", () => {
  expect(barStep([0, 3600, 7200, 14400])).toBe(3600);
  expect(barStep([0])).toBe(0);
  expect(barRange([3600, 7200, 10800])).toEqual([1800, 12600]);
  expect(barRange([3600])).toBeNull();
  expect(barRange([])).toBeNull();
});

test("bucketText names the bucket, not just its start", () => {
  const start = new Date(2026, 9, 9, 19, 0).getTime() / 1000;
  expect(bucketText(start, 3600)).toBe("2026-10-09 19:00–20:00");
  const day = new Date(2026, 9, 9, 0, 0).getTime() / 1000;
  expect(bucketText(day, 86400)).toBe("2026-10-09 00:00 – 2026-10-10 00:00");
});

test("stackData and stackTotal: a gap is not a zero", () => {
  expect(stackData([[1, null], [2, null]], none, 2)).toEqual([
    [1, null],
    [3, null],
  ]);
  expect(stackTotal([[1, null], [2, null]], none, 1)).toBeNull();
  expect(stackTotal([[1, null], [2, 5]], new Set([1]), 1)).toBeNull();
  expect(stackTotal([[1, null], [2, 5]], none, 1)).toBe(5);
});

test("TimeSeriesChart bars: legend entries carry their values and the note", async () => {
  // Colours are read from the document; uPlot itself needs a canvas, so only the markup around it is rendered.
  GlobalRegistrator.register();
  try {
    const html = renderToStaticMarkup(
      <TimeSeriesChart x={[0, 3600]} ys={[[1, 2], [3, 4]]} series={[{ label: "Compute" }, { label: "AI models" }]} unit="money" currency="USD" stacked bars legendValues={["$3.00", "$7.00"]} legendNote="each bar is one hour" />,
    );
    expect(html).toContain('class="tschart tschart-bars"');
    const legend = [...html.matchAll(/<button[^>]*tschart-legend-item[^>]*>(.*?)<\/button>/g)].map((m) => m[1]!.replace(/<[^>]+>/g, ""));
    expect(legend).toEqual(["Compute$3.00", "AI models$7.00"]);
    expect(html).toContain("each bar is one hour");
    // The label has an element of its own, apart from the value, so it can be found by its exact text.
    const el = document.createElement("div");
    el.innerHTML = html;
    const items = [...el.querySelectorAll(".tschart-legend-item")];
    expect(items.map((b) => b.querySelector(".tschart-legend-label")?.textContent)).toEqual(["Compute", "AI models"]);
    expect(items.map((b) => b.querySelector(".tschart-legend-value")?.textContent)).toEqual(["$3.00", "$7.00"]);
  } finally {
    await GlobalRegistrator.unregister();
  }
});

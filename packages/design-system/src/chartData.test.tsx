import { expect, test } from "bun:test";
import { GlobalRegistrator } from "@happy-dom/global-registrator";
import { renderToStaticMarkup } from "react-dom/server";
import { barRange, barSegments, barStep, barTicks, bucketText, chartColors, FADE_KEEP, fadedColor, stackData, stackTotal, timeTickText } from "./chartData.ts";
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

test("bucketText names the bucket, not just its start; a day bucket by its UTC date", () => {
  const start = new Date(2026, 9, 9, 19, 0).getTime() / 1000;
  expect(bucketText(start, 3600)).toBe("2026-10-09 19:00–20:00");
  const day = Date.UTC(2026, 9, 9) / 1000;
  expect(bucketText(day, 86400)).toBe("2026-10-09 UTC");
  expect(bucketText(day, 7 * 86400)).toBe("2026-10-09 – 2026-10-15 UTC");
});

/**
 * Runs fn with the process in time zone tz, then back in the one it had.
 * bun test runs in UTC when TZ is unset; deleting TZ again would leave the
 * last zone in effect, so the restore sets UTC explicitly.
 */
function inZone<T>(tz: string, fn: () => T): T {
  const was = process.env.TZ;
  process.env.TZ = tz;
  try {
    return fn();
  } finally {
    process.env.TZ = was ?? "UTC";
  }
}

test("day-bar ticks and tooltips name the UTC day, west and east of UTC", () => {
  const day = Date.UTC(2026, 9, 9) / 1000;
  const week = 7 * 86400;
  for (const [tz, localDate, localTick] of [
    ["America/Los_Angeles", 8, "10-08"],
    ["Pacific/Auckland", 9, "10-09"],
  ] as const) {
    inZone(tz, () => {
      // The zone took: local time is off UTC here (7h earlier, or 13h later).
      expect(new Date(day * 1000).getDate()).toBe(localDate);
      expect(new Date(day * 1000).getTimezoneOffset()).not.toBe(0);
      expect(timeTickText(day, week, true)).toBe("10-09");
      expect(timeTickText(day + 86400, week, true)).toBe("10-10");
      expect(bucketText(day, 86400)).toBe("2026-10-09 UTC");
      // Without day bars a tick over a week is the local month-day.
      expect(timeTickText(day, week).trim()).toBe(localTick);
    });
  }
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
    expect(html).toContain('class="tschart tschart-bars tschart-stacked"');
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

test("barTicks: one tick per bucket start while they fit, else every k-th from the first", () => {
  const days = [0, 1, 2, 3, 4, 5, 6, 7].map((d) => d * 86400);
  expect(barTicks(days, 800, 64)).toEqual(days);
  // 200px fits three: every third bucket.
  expect(barTicks(days, 200, 64)).toEqual([0, 3, 6].map((d) => d * 86400));
  expect(barTicks(days, 10, 64)).toEqual([0]);
  expect(barTicks([], 800, 64)).toEqual([]);
});

test("fadedColor mixes the colour toward the surface, keeping FADE_KEEP of it, as opaque hex", () => {
  // #2a78d6 over white, 42% kept: R 42·.42 + 255·.58 = 165.5 → a6, G 198.3 → c6, B 237.8 → ee.
  expect(FADE_KEEP).toBe(0.42);
  expect(fadedColor("#2a78d6", "#ffffff")).toBe("#a6c6ee");
  // Over the dark surface (#1b1c1f) it stays dark: a shade, not a pastel.
  expect(fadedColor("#3987e5", "#1b1c1f")).toBe("#284972");
  expect(fadedColor("#2a78d6", "#ffffff", 1)).toBe("#2a78d6");
  expect(fadedColor("#2a78d6", "#ffffff", 0)).toBe("#ffffff");
  // #rgb and rgb() read the same as #rrggbb.
  expect(fadedColor("#000", "rgb(255, 255, 255)")).toBe(fadedColor("#000000", "#ffffff"));
});

test("fadedColor leaves a colour it cannot read unchanged, never a wrong shade", () => {
  expect(fadedColor("var(--chart-1)", "#ffffff")).toBe("var(--chart-1)");
  expect(fadedColor("#2a78d6", "")).toBe("#2a78d6");
});

test("chartColors fades only the faded series; the others keep their colour", () => {
  expect(chartColors(["#2a78d6", "#2a78d6", "#1baf7a", "#1baf7a"], [false, true, undefined, true], "#ffffff")).toEqual(["#2a78d6", "#a6c6ee", "#1baf7a", fadedColor("#1baf7a", "#ffffff")]);
});

test("TimeSeriesChart: a faded series' legend key is the faded shade of its colour", async () => {
  GlobalRegistrator.register();
  try {
    document.documentElement.style.setProperty("--chart-1", "#2a78d6");
    document.documentElement.style.setProperty("--bg-surface", "#ffffff");
    const html = renderToStaticMarkup(
      <TimeSeriesChart x={[0, 86400]} ys={[[1, 2], [3, 4]]} series={[{ label: "Compute · runs", color: 1 }, { label: "Compute · unallocated", color: 1, faded: true }]} unit="money" currency="USD" stacked bars />,
    );
    const legend = html.slice(html.indexOf("tschart-legend"));
    const keys = [...legend.matchAll(/class="tschart-key" style="background:([^;"]+)/g)].map((m) => m[1]);
    expect(keys).toEqual(["#2a78d6", "#a6c6ee"]);
  } finally {
    await GlobalRegistrator.unregister();
  }
});

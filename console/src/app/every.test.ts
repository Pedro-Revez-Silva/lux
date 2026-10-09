import { expect, test } from "bun:test";
import { autoStep, autoSummary, costInterval, effectiveEvery, everyChoices, everyOptions, historyRes, parseEvery, resolveStep, stepNote, stepSentence } from "./every.ts";

test("?every=: minute, hour or day; anything else is Auto", () => {
  expect(["minute", "hour", "day", "week", "", null].map((v) => parseEvery(v))).toEqual(["minute", "hour", "day", "auto", "auto", "auto"]);
});

test("Auto: trends take the finest history kept with at most 2,000 points, cost is hourly to 24h and daily from 7d", () => {
  expect(["1h", "6h", "24h", "7d", "30d"].map((r) => autoStep(r as "1h", "trend"))).toEqual(["raw", "minute", "minute", "hour", "hour"]);
  expect(["1h", "6h", "24h", "7d", "30d"].map((r) => autoStep(r as "1h", "cost"))).toEqual(["hour", "hour", "hour", "day", "day"]);
  expect(autoSummary("24h")).toBe("min/hour");
  expect(autoSummary("7d")).toBe("hour/day");
  expect(stepSentence("24h", "auto")).toBe("every chart per minute, cost per hour (Auto)");
  expect(stepSentence("24h", "hour")).toBe("every chart per hour");
});

test("choices: off when no kind of chart gets 3 to 2,000 points, with the reason", () => {
  const off = (r: "1h" | "6h" | "24h" | "7d" | "30d") => Object.fromEntries(everyChoices(r).map((c) => [c.value, c.disabled ?? null]));
  expect(off("1h")).toEqual({ auto: null, minute: null, hour: null, day: "1 point · too coarse" });
  expect(off("24h")).toEqual({ auto: null, minute: null, hour: null, day: "1 point · too coarse" });
  // 7d per minute is 10,080 trend points, but cost can still use the hours: cost cannot use Minute at all.
  expect(off("7d")).toEqual({ auto: null, minute: "10,080 points · too many", hour: null, day: null });
  expect(off("30d")).toEqual({ auto: null, minute: "43,200 points · too many", hour: null, day: null });
  // 1h: an hour is one trend point, but cost reads 6h, so Hour stays on for cost.
  expect(everyChoices("1h").find((c) => c.value === "hour")).toMatchObject({ trend: 1, cost: 6 });
  expect(everyOptions("24h").map((o) => o.hint ?? o.disabled)).toEqual(["each chart picks", "1,440 points", "24 points", "1 point · too coarse"]);
});

test("a disabled choice in the URL is Auto", () => {
  expect(effectiveEvery("24h", "day")).toBe("auto");
  expect(effectiveEvery("30d", "minute")).toBe("auto");
  expect(resolveStep("24h", "day", "trend")).toEqual({ step: "minute", auto: true });
  expect(resolveStep("24h", "day", "cost")).toEqual({ step: "hour", auto: true });
});

test("per chart kind: Minute leaves cost hourly and says so; Day is daily for trends too", () => {
  expect(resolveStep("24h", "minute", "trend")).toEqual({ step: "minute", auto: false });
  const cost = resolveStep("24h", "minute", "cost");
  expect(cost).toEqual({ step: "hour", auto: false, note: "cost is never finer than an hour" });
  expect(stepNote(cost)).toBe("per hour · cost is never finer than an hour");
  expect(resolveStep("7d", "day", "trend")).toEqual({ step: "day", auto: false });
  expect(resolveStep("7d", "day", "cost")).toEqual({ step: "day", auto: false });
  expect(historyRes(resolveStep("7d", "day", "trend"))).toBe(86400);
  expect(costInterval(resolveStep("7d", "hour", "cost"))).toBe("hour");
});

test("a kind that cannot use the choice keeps its own step and says why", () => {
  // 1h per hour: one trend point; cost reads 6h and takes it.
  expect(resolveStep("1h", "hour", "trend")).toEqual({ step: "raw", auto: true, note: "per hour would be 1 point" });
  expect(resolveStep("1h", "hour", "cost")).toEqual({ step: "hour", auto: false });
  // 7d per hour: 168 points for both.
  expect(resolveStep("7d", "hour", "trend")).toEqual({ step: "hour", auto: false });
  // Auto asks luxd for no resolution: it picks the finest kept.
  expect(historyRes(resolveStep("7d", "auto", "trend"))).toBeUndefined();
  expect(historyRes(resolveStep("24h", "minute", "trend"))).toBe(60);
});

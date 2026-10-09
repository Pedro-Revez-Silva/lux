// The page-wide step (?every=): how finely the charts that follow the page's
// range are bucketed. Each kind of chart honours it as far as its data
// goes: trend charts read history samples (raw, minutes, hours, or days the
// server folds from hours), cost charts read hourly cost (by hour or UTC
// day, never finer). A chart that cannot honour the step keeps its own and
// says why.
import type { TimeRange } from "@lux/design-system";

export type Every = "auto" | "minute" | "hour" | "day";
export const EVERY_LIST: Every[] = ["auto", "minute", "hour", "day"];
export type ChartKind = "trend" | "cost";
export type Step = "raw" | "minute" | "hour" | "day";

/** ?every=: minute, hour or day; anything else is Auto. */
export function parseEvery(v: string | null): Every {
  return v === "minute" || v === "hour" || v === "day" ? v : "auto";
}

const RANGE_HOURS: Record<TimeRange, number> = { "1h": 1, "6h": 6, "24h": 24, "7d": 168, "30d": 720 };
const STEP_SECONDS: Record<Step, number> = { raw: 10, minute: 60, hour: 3600, day: 86400 };
/** History resolution (res) of a step. */
export const STEP_RES: Record<Step, number> = { raw: 0, minute: 60, hour: 3600, day: 86400 };
/** Fewer points than this says nothing over time; more cannot be told apart. */
export const MIN_POINTS = 3;
export const MAX_POINTS = 2000;
/** luxd keeps minute samples this long by default (LUX_HISTORY_MINUTES); older ranges read hours. */
export const MINUTE_KEEP_HOURS = 30 * 24;

/** The range cost charts read: 1h reads 6h, as cost is whole-hour buckets and one hour is one bar. */
export function costRange(range: TimeRange): TimeRange {
  return range === "1h" ? "6h" : range;
}

/** What Auto picks for a kind of chart over a range: trends the finest history kept with at most 2,000 points (as luxd picks it), cost hourly up to 24h and daily from 7d. */
export function autoStep(range: TimeRange, kind: ChartKind): Step {
  const hours = RANGE_HOURS[range];
  if (kind === "cost") return hours <= 24 ? "hour" : "day";
  if ((hours * 3600) / STEP_SECONDS.raw <= MAX_POINTS) return "raw";
  if (hours <= MINUTE_KEEP_HOURS && (hours * 3600) / STEP_SECONDS.minute <= MAX_POINTS) return "minute";
  return "hour";
}

/** Points a step gives a kind of chart over the range; null when the kind cannot use the step (cost has no minutes). */
export function points(range: TimeRange, step: Step, kind: ChartKind): number | null {
  if (kind === "cost" && (step === "raw" || step === "minute")) return null;
  const hours = RANGE_HOURS[kind === "cost" ? costRange(range) : range];
  return Math.ceil((hours * 3600) / STEP_SECONDS[step]);
}

const fits = (n: number | null) => n != null && n >= MIN_POINTS && n <= MAX_POINTS;

export interface EveryChoice {
  value: Every;
  /** The points it gives: trend charts', and cost charts' (null: cost cannot use it). */
  trend: number | null;
  cost: number | null;
  /** Why it cannot be picked for this range; undefined when it can. */
  disabled?: string;
}

const pointsText = (n: number) => `${n.toLocaleString("en-US")} ${n === 1 ? "point" : "points"}`;

/** Auto, Minute, Hour and Day for a range; a fixed choice is off when no kind of chart would get 3 to 2,000 points from it. */
export function everyChoices(range: TimeRange): EveryChoice[] {
  return EVERY_LIST.map((value) => {
    if (value === "auto") return { value, trend: null, cost: null };
    const trend = points(range, value, "trend");
    const cost = points(range, value, "cost");
    if (fits(trend) || fits(cost)) return { value, trend, cost };
    const n = Math.min(...[trend, cost].filter((x): x is number => x != null));
    return { value, trend, cost, disabled: `${pointsText(n)} · ${n < MIN_POINTS ? "too coarse" : "too many"}` };
  });
}

/** The choice in effect: the one asked for, or Auto when it is off for this range. */
export function effectiveEvery(range: TimeRange, every: Every): Every {
  return everyChoices(range).find((c) => c.value === every)?.disabled ? "auto" : every;
}

export interface Resolved {
  step: Step;
  /** The step is the kind's Auto (asked for, or fallen back to). */
  auto: boolean;
  /** Why the kind did not take the step asked for. */
  note?: string;
}

/** The step a kind of chart uses for a range and a choice, and why it differs from the choice when it does. */
export function resolveStep(range: TimeRange, every: Every, kind: ChartKind): Resolved {
  const e = effectiveEvery(range, every);
  const auto = autoStep(range, kind);
  if (e === "auto") return { step: auto, auto: true };
  if (kind === "cost" && e === "minute") return { step: "hour", auto: false, note: "cost is never finer than an hour" };
  const n = points(range, e, kind);
  if (!fits(n)) return { step: auto, auto: true, note: `per ${e} would be ${pointsText(n!)}` };
  return { step: e, auto: false };
}

/** History resolution to ask for: undefined for Auto (luxd picks the finest kept). */
export function historyRes(r: Resolved): number | undefined {
  return r.auto ? undefined : STEP_RES[r.step];
}

/** Cost interval of a resolved cost step. */
export function costInterval(r: Resolved): "hour" | "day" {
  return r.step === "day" ? "day" : "hour";
}

/** A step in running text: "per minute", "per 10s sample". */
export function stepText(step: Step): string {
  return step === "raw" ? "per 10s sample" : `per ${step}`;
}

/** A history response's resolution as a step. */
export function stepOfRes(res: number | undefined): Step | undefined {
  return res === 0 ? "raw" : res === 60 ? "minute" : res === 3600 ? "hour" : res === 86400 ? "day" : undefined;
}

/** A chart's subtitle tail: its step, and why it is not the one asked for. */
export function stepNote(r: Resolved, actual?: Step): string {
  const step = actual ?? r.step;
  return r.note ? `${stepText(step)} · ${r.note}` : stepText(step);
}

const SHORT: Record<Step, string> = { raw: "10s", minute: "min", hour: "hour", day: "day" };

/** What Auto resolves to for a range, in the picker's trigger: "min/hour" (trends per minute, cost per hour), or one word when both agree. */
export function autoSummary(range: TimeRange): string {
  const t = SHORT[autoStep(range, "trend")];
  const c = SHORT[autoStep(range, "cost")];
  return t === c ? t : `${t}/${c}`;
}

/** The step of every chart in words, for a page's description: "every chart per minute, cost per hour (Auto)". */
export function stepSentence(range: TimeRange, every: Every): string {
  const e = effectiveEvery(range, every);
  const t = resolveStep(range, every, "trend");
  const c = resolveStep(range, every, "cost");
  const tail = e === "auto" ? " (Auto)" : "";
  return t.step === c.step ? `every chart ${stepText(t.step)}${tail}` : `every chart ${stepText(t.step)}, cost ${stepText(c.step)}${tail}`;
}

/** The picker's options for a range: each with its points, or why it is off. */
export function everyOptions(range: TimeRange): { value: Every; hint?: string; disabled?: string }[] {
  return everyChoices(range).map((c) => {
    if (c.value === "auto") return { value: c.value, hint: "each chart picks" };
    if (c.disabled) return { value: c.value, disabled: c.disabled };
    // Trend charts' points where they can use it, else cost's.
    const n = fits(c.trend) ? c.trend! : c.cost!;
    return { value: c.value, hint: pointsText(n) };
  });
}

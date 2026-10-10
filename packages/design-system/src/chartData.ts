// Pure data shaping behind TimeSeriesChart: stacking, and the geometry of
// bars (one per bucket of x).
import { formatClock, formatTimestamp } from "./format.ts";

type Values = (number | null | undefined)[];

/** Running sums of the visible series, bottom first; hidden ones keep their raw values (they are not drawn). A missing value adds nothing; where every series below is missing too, the sum stays missing. */
export function stackData(ys: Values[], hidden: Set<number>, n: number): (number | null)[][] {
  const acc: (number | null)[] = new Array(n).fill(null);
  return ys.map((y, si) => {
    if (hidden.has(si)) return y.map((v) => v ?? null);
    return acc.map((a, i) => {
      const v = y[i];
      const next = v == null ? a : (a ?? 0) + v;
      acc[i] = next;
      return next;
    });
  });
}

/** The sum at one x of the visible series; null when none has a value there. */
export function stackTotal(ys: Values[], hidden: Set<number>, idx: number): number | null {
  let total: number | null = null;
  ys.forEach((y, i) => {
    const v = y[idx];
    if (!hidden.has(i) && v != null) total = (total ?? 0) + v;
  });
  return total;
}

export interface BarSegment {
  /** Bottom of each bucket's bar; null: no bar. */
  y0: (number | null)[];
  /** Top of each bucket's bar; null: no bar. */
  y1: (number | null)[];
}

/**
 * Each series' bar per bucket, bottom and top. Stacked, a visible series
 * sits on the visible ones below it: positive values stack up from zero,
 * negative ones (a refund) down from zero, so no bar overdraws another.
 * Unstacked, every bar starts at zero. A missing value, or a hidden
 * series, draws no bar, so a bucket with no figure stays empty rather
 * than reading as zero.
 */
export function barSegments(ys: Values[], hidden: Set<number>, n: number, stacked: boolean): BarSegment[] {
  const up: number[] = new Array(n).fill(0);
  const down: number[] = new Array(n).fill(0);
  return ys.map((y, si) => {
    const y0: (number | null)[] = new Array(n).fill(null);
    const y1: (number | null)[] = new Array(n).fill(null);
    if (hidden.has(si)) return { y0, y1 };
    for (let i = 0; i < n; i++) {
      const v = y[i];
      if (v == null) continue;
      const acc = v < 0 ? down : up;
      const base = stacked ? acc[i]! : 0;
      y0[i] = base;
      y1[i] = base + v;
      if (stacked) acc[i] = base + v;
    }
    return { y0, y1 };
  });
}

/** The bucket width of bars: the smallest step between consecutive x (seconds); 0 with fewer than two. */
export function barStep(x: readonly number[]): number {
  let step = 0;
  for (let i = 1; i < x.length; i++) {
    const d = x[i]! - x[i - 1]!;
    if (d > 0 && (step === 0 || d < step)) step = d;
  }
  return step;
}

/** The x scale of bars centred on their bucket's start: half a bucket of room at either end, so the first and last bars are whole. */
export function barRange(x: readonly number[]): [number, number] | null {
  const step = barStep(x);
  if (x.length === 0 || step === 0) return null;
  return [x[0]! - step / 2, x[x.length - 1]! + step / 2];
}

/**
 * Axis ticks for bars: the bucket starts (each bar's centre), every k-th so
 * ticks stay at least `space` px apart across `width` px.
 */
export function barTicks(x: readonly number[], width: number, space: number): number[] {
  if (x.length === 0) return [];
  const fit = Math.max(1, Math.floor(width / space));
  const every = Math.max(1, Math.ceil(x.length / fit));
  return x.filter((_, i) => i % every === 0);
}

/** How much of a series' own colour a faded series keeps; the rest is the chart's surface. */
export const FADE_KEEP = 0.42;

function rgbOf(color: string): [number, number, number] | null {
  const c = color.trim();
  const hex = /^#([0-9a-f]{6})$/i.exec(c);
  if (hex) {
    const n = parseInt(hex[1]!, 16);
    return [(n >> 16) & 255, (n >> 8) & 255, n & 255];
  }
  const short = /^#([0-9a-f])([0-9a-f])([0-9a-f])$/i.exec(c);
  if (short) return [parseInt(short[1]! + short[1]!, 16), parseInt(short[2]! + short[2]!, 16), parseInt(short[3]! + short[3]!, 16)];
  const rgb = /^rgba?\(\s*(\d+)[\s,]+(\d+)[\s,]+(\d+)\s*(?:[,/]\s*[\d.]+%?\s*)?\)$/i.exec(c);
  if (rgb) return [Number(rgb[1]), Number(rgb[2]), Number(rgb[3])];
  return null;
}

/**
 * A lighter shade of the same family: `color` mixed toward `surface`,
 * keeping `keep` of it, as an opaque #rrggbb ("same family, not charged to
 * runs"). Opaque, not translucent, so a faded bar or band reads the same
 * over the grid as in the legend and tooltip keys. The canvas cannot
 * resolve var() or color-mix(): both colours must be concrete; one that is
 * not #rgb, #rrggbb or rgb() is returned unchanged.
 */
export function fadedColor(color: string, surface: string, keep = FADE_KEEP): string {
  const a = rgbOf(color);
  const b = rgbOf(surface);
  if (!a || !b) return color;
  const mix = a.map((v, i) => Math.round(v * keep + b[i]! * (1 - keep)));
  return `#${mix.map((v) => v.toString(16).padStart(2, "0")).join("")}`;
}

/** A concrete CSS colour per series: the faded ones mixed toward the surface. */
export function chartColors(base: readonly string[], faded: readonly (boolean | undefined)[], surface: string): string[] {
  return base.map((c, i) => (faded[i] ? fadedColor(c, surface) : c));
}

/**
 * The same faded shade in CSS, for the DOM (a PartBar segment, a swatch):
 * `color` (a token such as var(--chart-3)) mixed toward the card surface,
 * keeping FADE_KEEP, so it matches the chart's faded series.
 */
export function fadedCss(color: string): string {
  return `color-mix(in srgb, ${color} ${Math.round(FADE_KEEP * 100)}%, var(--bg-surface))`;
}

/** A UTC calendar date, "2026-10-09". */
function utcDate(ms: number): string {
  return new Date(ms).toISOString().slice(0, 10);
}

/**
 * An x-axis tick label: the clock over a day or less, else the month and
 * day. Day bars are UTC-day buckets (cost), so with `utcDays` a tick names
 * its UTC date, not the local date of its start.
 */
export function timeTickText(v: number, span: number, utcDays = false): string {
  if (utcDays) return utcDate(v * 1000).slice(5);
  return span <= 86400 ? formatClock(v * 1000).slice(0, 5) : formatTimestamp(v * 1000, { seconds: false }).slice(5, 11);
}

/**
 * A bucket [start, start + step) in words: "2026-10-09 19:00–20:00" in
 * local time. A day or longer is a UTC-day bucket and is named by its UTC
 * dates: "2026-10-09 UTC", "2026-10-09 – 2026-10-15 UTC" (last day inclusive).
 */
export function bucketText(start: number, step: number): string {
  const from = formatTimestamp(start * 1000, { seconds: false });
  if (step <= 0) return from;
  if (step < 86400) return `${from}–${formatClock((start + step) * 1000).slice(0, 5)}`;
  const first = utcDate(start * 1000);
  const last = utcDate((start + step) * 1000 - 1);
  return first === last ? `${first} UTC` : `${first} – ${last} UTC`;
}

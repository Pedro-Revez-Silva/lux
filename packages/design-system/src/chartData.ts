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

/** A bucket [start, start + step) in words, local time: "2026-10-09 19:00–20:00"; a day or longer names both ends in full. */
export function bucketText(start: number, step: number): string {
  const from = formatTimestamp(start * 1000, { seconds: false });
  if (step <= 0) return from;
  const end = (start + step) * 1000;
  return step < 86400 ? `${from}–${formatClock(end).slice(0, 5)}` : `${from} – ${formatTimestamp(end, { seconds: false })}`;
}

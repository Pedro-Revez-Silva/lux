export interface NiceScale {
  min: number;
  max: number;
  step: number;
}

/**
 * A y scale through zero for data in [min, max], in about `ticks` steps:
 * the step is 1, 2, 2.5 or 5 × 10^n; the scale's max is the first multiple
 * of it >= max (and >= 0), its min the last multiple <= min (and <= 0).
 * Non-negative data gets min 0; empty or all-zero data gets [0, 1].
 */
export function niceScale(min: number | null | undefined, max: number | null | undefined, ticks: number): NiceScale {
  const hi = max != null && max > 0 ? max : 0;
  const lo = min != null && min < 0 ? min : 0;
  if (hi === 0 && lo === 0) return { min: 0, max: 1, step: 0.25 };
  const raw = (hi - lo) / ticks;
  const mag = 10 ** Math.floor(Math.log10(raw));
  const step = [1, 2, 2.5, 5, 10].map((m) => m * mag).find((s) => s >= raw * (1 - 1e-9))!;
  // The tolerance only picks a first guess, so float noise (0.1 + 0.2) does
  // not add a step; the loops then widen until the bound covers the data.
  let nHi = Math.ceil(hi / step - 1e-9);
  while (nHi * step < hi) nHi++;
  let nLo = Math.floor(lo / step + 1e-9);
  while (nLo * step > lo) nLo--;
  return { min: tidyBound(nLo * step, lo), max: tidyBound(nHi * step, hi), step };
}

/** `bound` without float noise (0.30000000000000004 → 0.3) when that still covers `data`. */
function tidyBound(bound: number, data: number): number {
  const tidy = Number(bound.toPrecision(12));
  return (bound >= 0 ? tidy >= data : tidy <= data) ? tidy : bound;
}

/** Gridlines of a niceScale: every step from min to max, zero among them. */
export function niceSplits(min: number, max: number, step: number): number[] {
  const out: number[] = [];
  for (let k = Math.round(min / step); k <= Math.round(max / step); k++) out.push(Number((k * step).toPrecision(12)));
  return out;
}

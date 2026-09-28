import { expect, test } from "bun:test";
import { niceScale, niceSplits } from "./scale.ts";

test("niceScale's max covers the data max: float noise and toPrecision never shrink it", () => {
  for (const max of [0.1 + 0.2, 0.3000000001, 0.30000000000001, 1, 1.0000001, 0.0375, 7.3, 1000000000000.1, 123456789.123456]) {
    for (const ticks of [2, 4, 5]) {
      const s = niceScale(0, max, ticks);
      expect([max, ticks, s.min, s.max >= max]).toEqual([max, ticks, 0, true]);
    }
  }
  // Noise adds no step: 0.1 + 0.2 tops out at 0.3 (plus at most float noise), not 0.4.
  expect(niceScale(0, 0.1 + 0.2, 3).max).toBeLessThan(0.31);
  expect(niceScale(0, 0.3, 3)).toEqual({ min: 0, max: 0.3, step: 0.1 });
});

test("niceScale starts at zero for non-negative data and goes below it for a refund", () => {
  expect(niceScale(0, 0, 4)).toEqual({ min: 0, max: 1, step: 0.25 });
  expect(niceScale(null, null, 4)).toEqual({ min: 0, max: 1, step: 0.25 });
  expect(niceScale(0.5, 3.7, 4)).toEqual({ min: 0, max: 4, step: 1 });
  // All negative: the top is zero, the bottom a whole step at or below the min.
  expect(niceScale(-0.5, -0.25, 4)).toEqual({ min: -0.6, max: 0, step: 0.2 });
  for (const [lo, hi] of [[-6, 9.7], [-0.3000000001, 1], [-1000000000000.1, 5], [-0.1 - 0.2, 0.2]] as const) {
    const s = niceScale(lo, hi, 4);
    expect([lo, hi, s.min <= lo, s.max >= hi, s.min < 0]).toEqual([lo, hi, true, true, true]);
  }
});

test("niceSplits puts a gridline on every step from the bottom to the top, zero included", () => {
  const s = niceScale(-6, 9.7, 4);
  const splits = niceSplits(s.min, s.max, s.step);
  expect(splits).toEqual([-10, -5, 0, 5, 10]);
  expect(niceSplits(0, 0.3, 0.1)).toEqual([0, 0.1, 0.2, 0.3]);
});

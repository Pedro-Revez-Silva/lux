import { expect, test } from "bun:test";
import { AXIS_MIN_SIZE, axisSize, measuredAxisSize } from "./axisSize.ts";

// About 6.5px per character, close to 11px Inter's digits.
function measure(text: string): number {
  return text.length * 6.5;
}

test("axisSize is the widest label rounded up, plus gap and inset", () => {
  // "186.3 GiB": 9 * 6.5 = 58.5 → 59 + 5 + 4.
  expect(axisSize(["0 B", "100 GiB", "186.3 GiB", "200 GiB"], measure)).toBe(68);
  // "$12,345.50": 10 * 6.5 = 65 + 5 + 4.
  expect(axisSize(["453.7 MiB", "$12,345.50"], measure)).toBe(74);
  expect(axisSize(["fractional"], () => 47.1)).toBe(57);
  expect(axisSize(["fractional"], () => 47.1, 80)).toBe(80);
});

test("axisSize keeps the 56px floor for short labels", () => {
  expect(axisSize(["0", "12", "40%", "100%"], measure)).toBe(AXIS_MIN_SIZE);
  expect(axisSize([], measure)).toBe(AXIS_MIN_SIZE);
  expect(axisSize([null, undefined, ""], measure)).toBe(AXIS_MIN_SIZE);
});

test("axisSize ignores label order and repeats its result for the same labels", () => {
  const labels = ["0 B", "250 GiB", "500 GiB"];
  const first = axisSize(labels, measure);
  expect(axisSize([...labels], measure)).toBe(first);
  expect(axisSize(labels, measure)).toBe(axisSize(labels.slice().reverse(), measure));
});

test("measuredAxisSize falls back to the floor without a DOM", () => {
  const size = measuredAxisSize("11px sans-serif");
  expect([size(null, null), size(null, ["0 B", "250 GiB", "500 GiB"])]).toEqual([AXIS_MIN_SIZE, AXIS_MIN_SIZE]);
});

// A canvas double measuring each character as half the font's px size, so widths depend on text and font.
function withCanvas(context: "2d" | null, run: () => void): void {
  const g = globalThis as { document?: unknown };
  const had = "document" in g;
  const original = g.document;
  const ctx = {
    font: "10px sans-serif",
    measureText(text: string) {
      const px = Number(/(\d+(?:\.\d+)?)px/.exec(this.font)?.[1] ?? 0);
      return { width: text.length * px * 0.5 };
    },
  };
  g.document = {
    createElement: (tag: string) => {
      if (tag !== "canvas") throw new Error(`unexpected element ${tag}`);
      return { getContext: (kind: string) => (kind === "2d" && context ? ctx : null) };
    },
  };
  try {
    run();
  } finally {
    if (had) g.document = original;
    else delete g.document;
  }
}

test("measuredAxisSize measures the current labels in its font", () => {
  withCanvas("2d", () => {
    const size = measuredAxisSize("11px sans-serif");
    // "1,234.5 GiB": 11 * 5.5 = 60.5 → 61 + 5 + 4.
    const long = ["0 B", "500 GiB", "1,234.5 GiB"];
    expect(size(null, null)).toBe(AXIS_MIN_SIZE);
    expect(size(null, long)).toBe(70);
    expect(size(null, long)).toBe(70);
    expect(size(null, ["0", "12", "40%"])).toBe(AXIS_MIN_SIZE);
    expect(size(null, long)).toBe(70);
    expect(size(null, [])).toBe(AXIS_MIN_SIZE);
    // 11 * 8 = 88 + 5 + 4.
    expect(measuredAxisSize("16px sans-serif")(null, long)).toBe(97);
  });
});

test("measuredAxisSize falls back to the floor when the canvas has no 2d context", () => {
  withCanvas(null, () => {
    expect(measuredAxisSize("11px sans-serif")(null, ["0 B", "500 GiB", "1,234.5 GiB"])).toBe(AXIS_MIN_SIZE);
  });
});

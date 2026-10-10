import { expect, test } from "bun:test";
import { renderToStaticMarkup } from "react-dom/server";
import { Meter, meterFill } from "./Meter.tsx";

/** The meter's text and its fill width as rendered. */
function meter(value: number | null | undefined, decimals?: number) {
  const html = renderToStaticMarkup(<Meter value={value} decimals={decimals} />);
  const text = /class="meter-text[^"]*">([^<]*)</.exec(html)![1];
  const width = /class="meter-fill" style="width:([^%]+)%/.exec(html)?.[1];
  return { text: text?.replace(/&lt;/g, "<"), width: width == null ? null : Number(width) };
}

test("meterFill clamps to 0..1; missing or not finite is null", () => {
  expect([0, 0.51, 1].map(meterFill)).toEqual([0, 0.51, 1]);
  expect(meterFill(1.4)).toBe(1);
  expect(meterFill(-0.2)).toBe(0);
  expect([null, undefined, Number.NaN, Number.POSITIVE_INFINITY].map(meterFill)).toEqual([null, null, null, null]);
});

test("Meter: the fill is the clamped ratio; the text keeps the true figure", () => {
  expect(meter(0.51)).toEqual({ text: "51%", width: 51 });
  expect(meter(0.912, 1)).toEqual({ text: "91.2%", width: 91.2 });
  expect(meter(1.25)).toEqual({ text: "125%", width: 100 });
});

test("Meter: no ratio is an en dash and no fill; zero is 0% and no fill", () => {
  expect(meter(null)).toEqual({ text: "–", width: null });
  expect(meter(undefined)).toEqual({ text: "–", width: null });
  expect(meter(0)).toEqual({ text: "0%", width: null });
});

test("Meter: a non-zero ratio that rounds to zero reads <1%, never 0%", () => {
  expect(meter(0.004).text).toBe("<1%");
  expect(meter(0.0004, 1).text).toBe("<0.1%");
  expect(meter(0.005).text).toBe("1%");
});

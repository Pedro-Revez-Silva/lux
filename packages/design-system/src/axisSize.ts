export const AXIS_MIN_SIZE = 56;
// uPlot's default gap between the label's right edge and the plot.
const AXIS_GAP = 5;
// Keep the first glyph clear of the canvas edge.
const AXIS_INSET = 4;

export function axisSize(labels: readonly (string | null | undefined)[], measure: (text: string) => number, min = AXIS_MIN_SIZE): number {
  let widest = 0;
  for (const label of labels) {
    if (label) widest = Math.max(widest, measure(label));
  }
  return Math.max(min, Math.ceil(widest) + AXIS_GAP + AXIS_INSET);
}

// Reuse the last label set's size so uPlot's resize cycle converges.
export function measuredAxisSize(font: string): (self: unknown, values: string[] | null) => number {
  let ctx: CanvasRenderingContext2D | null | undefined;
  let lastKey: string | null = null;
  let lastSize = AXIS_MIN_SIZE;
  function measure(text: string): number {
    if (ctx === undefined) {
      ctx = typeof document === "undefined" ? null : document.createElement("canvas").getContext("2d");
      if (ctx) ctx.font = font;
    }
    return ctx ? ctx.measureText(text).width : 0;
  }
  return (_self, values) => {
    if (!values) return AXIS_MIN_SIZE;
    const key = values.join("\n");
    if (key !== lastKey) {
      lastKey = key;
      lastSize = axisSize(values, measure);
    }
    return lastSize;
  };
}

import { useEffect, useLayoutEffect, useMemo, useRef, useState, type ReactNode } from "react";
import uPlot from "uplot";
import "uplot/dist/uPlot.min.css";
import { measuredAxisSize } from "./axisSize.ts";
import { barRange, barSegments, barStep, barTicks, bucketText, chartColors, stackData, stackTotal, timeTickText, type BarSegment } from "./chartData.ts";
import { formatTimestamp, formatUnit, type Unit } from "./format.ts";
import { niceScale, niceSplits } from "./scale.ts";
import { cssVar, useDensity, useTheme } from "./theme.ts";

export interface Series {
  label: string;
  /** Categorical slot 1..8 (fixed order, never cycled) or an explicit CSS color. */
  color?: number | string;
  /** Area wash under the line (~10% opacity). */
  area?: boolean;
  /** Draw as steps (for gauges like host counts). */
  step?: boolean;
  /** Dashed, for limits/thresholds. */
  dashed?: boolean;
  /**
   * A lighter shade of its colour, the same in the plot, legend and
   * tooltip: the same family as a neighbour, set apart ("compute, not
   * charged to runs" beside "compute, runs").
   */
  faded?: boolean;
}

export interface TimeSeriesChartProps {
  /** Epoch seconds, ascending. */
  x: number[];
  /** One array per series, aligned to x. null is a gap in the line; undefined is no point there (the line joins over it: several series on one x axis). */
  ys: (number | null | undefined)[][];
  series: Series[];
  unit: Unit;
  /** Plot height in px; defaults to the --chart-h token (grows with the screen, shrinks when compact). */
  height?: number;
  /** Start the y axis at zero (default true; counts/bytes should). */
  zeroBase?: boolean;
  /** Fixed y max (e.g. a host's memory limit). */
  yMax?: number;
  /** Show a legend row (always shown for >= 2 series; forced on with this). */
  legend?: boolean;
  /** Vertical markers (epoch seconds) with a short label, e.g. epoch boundaries. */
  marks?: ChartMark[];
  /**
   * Stack the series (first at the bottom) as filled bands: parts of one
   * whole, such as cost by family. A missing value adds nothing to the stack
   * and shows as missing in the tooltip, which also lists the total.
   */
  stacked?: boolean;
  /**
   * Draw each x as a bar for the bucket starting there (an amount per
   * bucket, such as cost per hour), not a line through the points. Buckets
   * are as wide as the smallest step of x. With `stacked` the bars stack. A
   * missing value draws no bar: an empty bucket, never a zero.
   */
  bars?: boolean;
  /** ISO 4217 code for unit "money". */
  currency?: string;
  /** Extra text at the legend's right end (what a bar is, say). */
  legendNote?: ReactNode;
  /** Text after a legend entry's label (its total, say), by series index. */
  legendValues?: (ReactNode | undefined)[];
  className?: string;
}

export interface ChartMark {
  x: number;
  label: string;
}

const MAX_SERIES = 8;
/** Fill of a stacked band: stronger than a line's 10% wash, so neighbouring bands read apart. */
const STACK_ALPHA = 0.28;
/** A bar's share of its bucket, and its widest, in CSS px. */
const BAR_SIZE: [number, number, number] = [0.78, 56, 1];
// uPlot's BarsPathBuilderFacetUnit.ScaleValue: a const enum, which isolated modules cannot read.
const SCALE_VALUE = 1 as uPlot.Series.BarsPathBuilderFacetUnit;

/** The --chart-h token as a number, tracking density and viewport changes. */
function useChartHeight(): number {
  const { density } = useDensity();
  const [h, setH] = useState(() => parseInt(cssVar("--chart-h")) || 200);
  useEffect(() => {
    const read = () => setH(parseInt(cssVar("--chart-h")) || 200);
    read();
    window.addEventListener("resize", read);
    return () => window.removeEventListener("resize", read);
  }, [density]);
  return h;
}

/** A concrete colour for the canvas, which cannot resolve var(--token). */
function seriesColor(s: Series, i: number): string {
  if (typeof s.color === "string") {
    const v = /^var\((--[\w-]+)\)$/.exec(s.color.trim());
    return v ? cssVar(v[1]!) || s.color : s.color;
  }
  const slot = Math.min(MAX_SERIES, s.color ?? i + 1);
  return cssVar(`--chart-${slot}`);
}

function hexWithAlpha(color: string, alpha: number): string {
  const m = /^#([0-9a-f]{6})$/i.exec(color.trim());
  if (!m) return color;
  const n = parseInt(m[1]!, 16);
  return `rgba(${(n >> 16) & 255}, ${(n >> 8) & 255}, ${n & 255}, ${alpha})`;
}

interface Hover {
  idx: number;
  left: number;
  top: number;
  plotLeft: number;
  /** Bars: the hovered bucket's left edge and width in the plot, CSS px. */
  bucket?: { left: number; width: number; top: number; height: number };
}

/** The x/y data uPlot plots: stacked sums for bands, each bar's top for bars, else the values. */
function plotData(x: number[], ys: (number | null | undefined)[][], hidden: Set<number>, stacked: boolean | undefined, segments: BarSegment[] | null): uPlot.AlignedData {
  if (segments) return [x, ...segments.map((s) => s.y1)] as uPlot.AlignedData;
  return [x, ...(stacked ? stackData(ys, hidden, x.length) : ys)] as uPlot.AlignedData;
}

/** uPlot line or bar chart: crosshair + one tooltip for every series, unit-aware axes, theme-aware, resizes. */
export function TimeSeriesChart({ x, ys, series: seriesProp, unit, height: heightProp, zeroBase = true, yMax, legend, marks, stacked, bars, currency, legendNote, legendValues, className }: TimeSeriesChartProps) {
  const host = useRef<HTMLDivElement>(null);
  const plot = useRef<uPlot | null>(null);
  const { resolved } = useTheme();
  const tokenHeight = useChartHeight();
  const height = heightProp ?? tokenHeight;
  const [hover, setHover] = useState<Hover | null>(null);
  const [hidden, setHidden] = useState<Set<number>>(() => new Set());
  const marksRef = useRef(marks);
  marksRef.current = marks;

  // Keyed by content: a caller's fresh but equal array must not rebuild the
  // plot; new data alone goes through setData below.
  const seriesKey = JSON.stringify(seriesProp);
  const series = useMemo(() => seriesProp, [seriesKey]);
  const colors = useMemo(() => chartColors(series.map((s, i) => seriesColor(s, i)), series.map((s) => s.faded), cssVar("--bg-surface")), [series, resolved]);
  const segments = useMemo(() => (bars ? barSegments(ys, hidden, x.length, !!stacked) : null), [bars, ys, hidden, x.length, stacked]);
  // The bar paths read their bottoms here, so new data needs no rebuild.
  const segmentsRef = useRef(segments);
  segmentsRef.current = segments;
  const data = useMemo(() => plotData(x, ys, hidden, stacked, segments), [x, ys, stacked, hidden, segments]);
  const fmt = (v: number | null | undefined) => formatUnit(v, unit, currency);
  const dayBars = useMemo(() => !!bars && barStep(x) >= 86400, [bars, x]);

  useLayoutEffect(() => {
    const el = host.current;
    if (!el || x.length < 2) return;
    const grid = cssVar("--chart-grid");
    const axis = cssVar("--chart-axis");
    const label = cssVar("--chart-label");
    const cursorColor = cssVar("--chart-cursor");
    const font = `11px ${cssVar("--font-sans")}`;
    // Zero-based without a fixed max: the top gridline is a whole step at or
    // above the largest (stacked) value, and below zero (a refund) the bottom
    // one a whole step at or below the smallest, so no point sits outside.
    const nice = zeroBase && yMax == null;
    let step = 0;
    const barPaths = (si: number) =>
      uPlot.paths.bars!({
        size: BAR_SIZE,
        disp: {
          y0: { unit: SCALE_VALUE, values: () => segmentsRef.current?.[si]?.y0 ?? [] },
          y1: { unit: SCALE_VALUE, values: () => segmentsRef.current?.[si]?.y1 ?? [] },
        },
      });

    const opts: uPlot.Options = {
      width: el.clientWidth || 300,
      height,
      padding: [8, 12, 0, 0],
      cursor: {
        y: false,
        points: bars ? { show: false } : { size: 8, width: 2, fill: (u, i) => (u.series[i]!.stroke as () => string)(), stroke: () => cssVar("--bg-surface") },
        drag: { x: false, y: false, setScale: false },
      },
      legend: { show: false },
      scales: {
        // Bars: half a bucket of room at either end, so the edge bars are whole.
        x: { time: true, range: bars ? (u, min, max) => barRange(u.data[0] as number[]) ?? [min, max] : undefined },
        y: {
          range: (_u, min, max) => {
            if (bars) {
              min = Math.min(min, 0);
              max = Math.max(max, 0);
            }
            if (!nice) return [zeroBase ? 0 : min, yMax ?? (max === 0 ? 1 : max * 1.05)];
            const s = niceScale(min, max, Math.max(2, Math.floor(height / 50)));
            step = s.step;
            return [s.min, s.max];
          },
        },
      },
      axes: [
        {
          stroke: label,
          font,
          grid: { show: false },
          ticks: { show: true, stroke: axis, width: 1, size: 4 },
          values: (u, vals) => {
            // From the plotted data, which setData replaces without a rebuild.
            const xs = u.data[0];
            const span = xs.length > 1 ? xs[xs.length - 1]! - xs[0]! : 0;
            return vals.map((v) => timeTickText(v, span, dayBars));
          },
          space: 64,
          // Day (or longer) bars: one tick per bucket, under its bar; a label per day, never a date twice.
          splits: dayBars ? (u) => barTicks(u.data[0] as number[], u.bbox.width / uPlot.pxRatio, 64) : undefined,
        },
        {
          stroke: label,
          font,
          grid: { stroke: grid, width: 1 },
          ticks: { show: false },
          size: measuredAxisSize(font),
          values: (_u, vals) => vals.map((v) => fmt(v)),
          space: 32,
          splits: nice ? (_u, _i, min, max) => niceSplits(min, max, step) : undefined,
        },
      ],
      series: [
        {},
        ...series.map((s, i) =>
          bars
            ? {
                label: s.label,
                stroke: colors[i],
                fill: colors[i],
                width: 0,
                paths: barPaths(i),
                points: { show: false },
                show: !hidden.has(i),
              }
            : {
                label: s.label,
                stroke: colors[i],
                width: 2,
                dash: s.dashed ? [4, 4] : undefined,
                fill: stacked ? hexWithAlpha(colors[i]!, STACK_ALPHA) : s.area ? hexWithAlpha(colors[i]!, 0.1) : undefined,
                paths: s.step ? uPlot.paths.stepped!({ align: 1 }) : undefined,
                // A value with no neighbour draws no line: mark it with a dot.
                points: { show: false, filter: isolatedPoints, size: 6, width: 0, fill: colors[i] },
                spanGaps: false,
                show: !hidden.has(i),
              },
        ),
      ],
      // Each visible band's fill is clipped to the one below it; bars carry their own bottoms.
      bands: stacked && !bars ? stackBands(series.length, hidden) : undefined,
      hooks: {
        draw: [
          (u) => {
            const ms = marksRef.current;
            if (!ms || ms.length === 0) return;
            const ctx = u.ctx;
            ctx.save();
            ctx.strokeStyle = cursorColor;
            ctx.fillStyle = label;
            ctx.font = font;
            ctx.setLineDash([3, 3]);
            ctx.lineWidth = 1;
            for (const m of ms) {
              const px = u.valToPos(m.x, "x", true);
              if (px < u.bbox.left || px > u.bbox.left + u.bbox.width) continue;
              ctx.beginPath();
              ctx.moveTo(px, u.bbox.top);
              ctx.lineTo(px, u.bbox.top + u.bbox.height);
              ctx.stroke();
              ctx.fillText(m.label, px + 4, u.bbox.top + 12);
            }
            ctx.restore();
          },
        ],
        setCursor: [
          (u) => {
            const idx = u.cursor.idx;
            if (idx == null || idx < 0) {
              setHover(null);
              return;
            }
            const plotLeft = u.bbox.left / uPlot.pxRatio;
            let bucket: Hover["bucket"];
            if (bars) {
              const xs = u.data[0] as number[];
              const w = barStep(xs);
              const l = u.valToPos(xs[idx]! - w / 2, "x");
              bucket = { left: l + plotLeft, width: u.valToPos(xs[idx]! + w / 2, "x") - l, top: u.bbox.top / uPlot.pxRatio, height: u.bbox.height / uPlot.pxRatio };
            }
            // cursor.left is relative to the plot area, which starts after the measured y axis.
            setHover({ idx, left: u.cursor.left ?? 0, top: u.cursor.top ?? 0, plotLeft, bucket });
          },
        ],
      },
    };

    const u = new uPlot(opts, data, el);
    // Theme the cursor line via CSS variable, uPlot draws it as a DOM element.
    el.style.setProperty("--uplot-cursor", cursorColor);
    plot.current = u;

    const ro = new ResizeObserver(() => {
      if (el.clientWidth > 0) u.setSize({ width: el.clientWidth, height });
    });
    ro.observe(el);
    return () => {
      ro.disconnect();
      u.destroy();
      plot.current = null;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [resolved, height, unit, currency, zeroBase, yMax, series, colors, hidden, stacked, bars, dayBars, x.length < 2]);

  useEffect(() => {
    plot.current?.setData(data);
  }, [data]);
  useEffect(() => {
    plot.current?.redraw(false, true);
  }, [marks]);

  const showLegend = legend ?? series.length >= 2;
  const toggle = (i: number) =>
    setHidden((h) => {
      const n = new Set(h);
      if (n.has(i)) n.delete(i);
      else n.add(i);
      return n;
    });

  const width = host.current?.clientWidth ?? 0;
  const flip = hover ? hover.left > width * 0.6 : false;

  return (
    <div className={["tschart", bars ? "tschart-bars" : "", stacked ? "tschart-stacked" : "", className ?? ""].join(" ").trim()}>
      {x.length < 2 ? (
        <div className="tschart-empty muted" style={{ height }}>
          {x.length === 0 ? "No samples in this range." : "Waiting for a second sample."}
        </div>
      ) : (
        <div className="tschart-plot" ref={host} style={{ height }} />
      )}
      {hover?.bucket && <div className="tschart-bucket" style={{ left: hover.bucket.left, width: hover.bucket.width, top: hover.bucket.top, height: hover.bucket.height }} aria-hidden="true" />}
      {hover && x[hover.idx] != null && (
        <div className={flip ? "tschart-tip is-left" : "tschart-tip"} style={{ left: hover.left + hover.plotLeft, top: 8 }}>
          <div className="tschart-tip-time mono">{bars ? bucketText(x[hover.idx]!, barStep(x)) : formatTimestamp(x[hover.idx]! * 1000)}</div>
          {series.map((s, i) =>
            hidden.has(i) ? null : (
              <div className="tschart-tip-row" key={s.label}>
                <span className="tschart-key" style={{ background: colors[i] }} />
                <span className="tschart-tip-value num">{fmt(ys[i]?.[hover.idx] ?? null)}</span>
                <span className="tschart-tip-label">{s.label}</span>
              </div>
            ),
          )}
          {stacked && (
            <div className="tschart-tip-row tschart-tip-total">
              <span className="tschart-key" />
              <span className="tschart-tip-value num">{fmt(stackTotal(ys, hidden, hover.idx))}</span>
              <span className="tschart-tip-label">Total</span>
            </div>
          )}
        </div>
      )}
      {(showLegend || legendNote) && (
        <div className="tschart-legend">
          {showLegend &&
            series.map((s, i) => (
              <button key={s.label} type="button" className={hidden.has(i) ? "tschart-legend-item is-hidden" : "tschart-legend-item"} onClick={() => toggle(i)} aria-pressed={!hidden.has(i)}>
                <span className="tschart-key" style={{ background: colors[i], borderStyle: s.dashed ? "dashed" : undefined }} />
                <span className="tschart-legend-label">{s.label}</span>
                {legendValues?.[i] != null && <span className="tschart-legend-value num">{legendValues[i]}</span>}
              </button>
            ))}
          {legendNote && <span className="tschart-legend-note">{legendNote}</span>}
        </div>
      )}
    </div>
  );
}

function stackBands(n: number, hidden: Set<number>): uPlot.Band[] {
  const visible = Array.from({ length: n }, (_, i) => i).filter((i) => !hidden.has(i));
  return visible.slice(1).map((upper, k) => ({ series: [upper + 1, visible[k]! + 1] as [number, number] }));
}

/** Indices of values with no value on either side: a line or band cannot show them. */
function isolatedPoints(u: uPlot, seriesIdx: number): number[] | null {
  const ys = u.data[seriesIdx]!;
  const out: number[] = [];
  for (let i = 0; i < ys.length; i++) {
    if (ys[i] != null && ys[i - 1] == null && ys[i + 1] == null) out.push(i);
  }
  return out.length ? out : null;
}

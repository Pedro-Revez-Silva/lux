import { useEffect, useLayoutEffect, useMemo, useRef, useState } from "react";
import uPlot from "uplot";
import "uplot/dist/uPlot.min.css";
import { formatClock, formatTimestamp, formatUnit, type Unit } from "./format.ts";
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
  /** ISO 4217 code for unit "money". */
  currency?: string;
  className?: string;
}

export interface ChartMark {
  x: number;
  label: string;
}

const MAX_SERIES = 8;
/** Fill of a stacked band: stronger than a line's 10% wash, so neighbouring bands read apart. */
const STACK_ALPHA = 0.28;

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
}

/** uPlot line chart: crosshair + one tooltip for every series, unit-aware axes, theme-aware, resizes. */
/** Running sums of the visible series, bottom first; hidden ones keep their raw values (they are not drawn). */
function stackData(ys: (number | null | undefined)[][], hidden: Set<number>, n: number): (number | null)[][] {
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

export function TimeSeriesChart({ x, ys, series, unit, height: heightProp, zeroBase = true, yMax, legend, marks, stacked, currency, className }: TimeSeriesChartProps) {
  const host = useRef<HTMLDivElement>(null);
  const plot = useRef<uPlot | null>(null);
  const { resolved } = useTheme();
  const tokenHeight = useChartHeight();
  const height = heightProp ?? tokenHeight;
  const [hover, setHover] = useState<Hover | null>(null);
  const [hidden, setHidden] = useState<Set<number>>(() => new Set());
  const marksRef = useRef(marks);
  marksRef.current = marks;

  const colors = useMemo(() => series.map((s, i) => seriesColor(s, i)), [series, resolved]);
  const data = useMemo(() => [x, ...(stacked ? stackData(ys, hidden, x.length) : ys)] as uPlot.AlignedData, [x, ys, stacked, hidden]);
  const fmt = (v: number | null | undefined) => formatUnit(v, unit, currency);
  const span = x.length > 1 ? x[x.length - 1]! - x[0]! : 0;

  useLayoutEffect(() => {
    const el = host.current;
    if (!el || x.length < 2) return;
    const grid = cssVar("--chart-grid");
    const axis = cssVar("--chart-axis");
    const label = cssVar("--chart-label");
    const cursorColor = cssVar("--chart-cursor");
    const font = `11px ${cssVar("--font-sans")}`;

    const opts: uPlot.Options = {
      width: el.clientWidth || 300,
      height,
      padding: [8, 12, 0, 0],
      cursor: {
        y: false,
        points: { size: 8, width: 2, fill: (u, i) => (u.series[i]!.stroke as () => string)(), stroke: () => cssVar("--bg-surface") },
        drag: { x: false, y: false, setScale: false },
      },
      legend: { show: false },
      scales: { x: { time: true }, y: { range: (_u, min, max) => [zeroBase ? 0 : min, yMax ?? (max === 0 ? 1 : max * 1.05)] } },
      axes: [
        {
          stroke: label,
          font,
          grid: { show: false },
          ticks: { show: true, stroke: axis, width: 1, size: 4 },
          values: (_u, vals) => vals.map((v) => (span <= 86400 ? formatClock(v * 1000).slice(0, 5) : formatTimestamp(v * 1000, { seconds: false }).slice(5, 11))),
          space: 64,
        },
        {
          stroke: label,
          font,
          grid: { stroke: grid, width: 1 },
          ticks: { show: false },
          size: 56,
          values: (_u, vals) => vals.map((v) => fmt(v)),
          space: 32,
        },
      ],
      series: [
        {},
        ...series.map((s, i) => ({
          label: s.label,
          stroke: colors[i],
          width: 2,
          dash: s.dashed ? [4, 4] : undefined,
          fill: stacked ? hexWithAlpha(colors[i]!, STACK_ALPHA) : s.area ? hexWithAlpha(colors[i]!, 0.1) : undefined,
          paths: s.step ? uPlot.paths.stepped!({ align: 1 }) : undefined,
          points: { show: false },
          spanGaps: false,
          show: !hidden.has(i),
        })),
      ],
      // Each visible band's fill is clipped to the one below it.
      bands: stacked ? stackBands(series.length, hidden) : undefined,
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
            setHover({ idx, left: u.cursor.left ?? 0, top: u.cursor.top ?? 0 });
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
  }, [resolved, height, unit, currency, zeroBase, yMax, series, colors, hidden, stacked, x.length < 2]);

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
    <div className={["tschart", className ?? ""].join(" ").trim()}>
      {x.length < 2 ? (
        <div className="tschart-empty muted" style={{ height }}>
          {x.length === 0 ? "No samples in this range." : "Waiting for a second sample."}
        </div>
      ) : (
        <div className="tschart-plot" ref={host} style={{ height }} />
      )}
      {hover && x[hover.idx] != null && (
        <div className={flip ? "tschart-tip is-left" : "tschart-tip"} style={{ left: hover.left + 56, top: 8 }}>
          <div className="tschart-tip-time mono">{formatTimestamp(x[hover.idx]! * 1000)}</div>
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
      {showLegend && (
        <div className="tschart-legend">
          {series.map((s, i) => (
            <button key={s.label} type="button" className={hidden.has(i) ? "tschart-legend-item is-hidden" : "tschart-legend-item"} onClick={() => toggle(i)} aria-pressed={!hidden.has(i)}>
              <span className="tschart-key" style={{ background: colors[i], borderStyle: s.dashed ? "dashed" : undefined }} />
              {s.label}
            </button>
          ))}
        </div>
      )}
    </div>
  );
}

function stackBands(n: number, hidden: Set<number>): uPlot.Band[] {
  const visible = Array.from({ length: n }, (_, i) => i).filter((i) => !hidden.has(i));
  return visible.slice(1).map((upper, k) => ({ series: [upper + 1, visible[k]! + 1] as [number, number] }));
}

/** The sum at one x of the visible series; null when none has a value there. */
function stackTotal(ys: (number | null | undefined)[][], hidden: Set<number>, idx: number): number | null {
  let total: number | null = null;
  ys.forEach((y, i) => {
    const v = y[idx];
    if (!hidden.has(i) && v != null) total = (total ?? 0) + v;
  });
  return total;
}

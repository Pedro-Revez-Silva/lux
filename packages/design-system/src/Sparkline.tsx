import { useId } from "react";

export interface SparklineProps {
  values: (number | null)[];
  width?: number;
  height?: number;
  /** CSS color; defaults to the muted de-emphasis hue. */
  color?: string;
  /** Faint area wash under the line. */
  area?: boolean;
  /** Mark the last point in the accent color. */
  endDot?: boolean;
  /** Fixed y-domain; defaults to the data extent. */
  min?: number;
  max?: number;
  className?: string;
  title?: string;
}

/** Tiny inline line chart, 2px stroke, no axes. */
export function Sparkline({ values, width = 96, height = 24, color = "var(--fg-faint)", area = false, endDot = true, min, max, className, title }: SparklineProps) {
  const id = useId();
  const pts = values.map((v, i) => [i, v] as const).filter((p): p is readonly [number, number] => p[1] != null && Number.isFinite(p[1]));
  if (pts.length < 2) return <svg width={width} height={height} className={className} aria-hidden="true" />;

  const lo = min ?? Math.min(...pts.map((p) => p[1]));
  const hi = max ?? Math.max(...pts.map((p) => p[1]));
  const span = hi - lo || 1;
  const pad = 2;
  const x = (i: number) => pad + (i / (values.length - 1)) * (width - pad * 2);
  const y = (v: number) => pad + (1 - (v - lo) / span) * (height - pad * 2);
  const d = pts.map(([i, v], k) => `${k === 0 ? "M" : "L"}${x(i).toFixed(1)},${y(v).toFixed(1)}`).join(" ");
  const last = pts[pts.length - 1]!;
  const first = pts[0]!;
  const areaD = `${d} L${x(last[0]).toFixed(1)},${height - pad} L${x(first[0]).toFixed(1)},${height - pad} Z`;

  return (
    <svg width={width} height={height} className={["sparkline", className ?? ""].join(" ").trim()} role={title ? "img" : undefined} aria-hidden={title ? undefined : "true"}>
      {title && <title>{title}</title>}
      {area && (
        <>
          <defs>
            <linearGradient id={id} x1="0" y1="0" x2="0" y2="1">
              <stop offset="0" stopColor={color} stopOpacity="0.18" />
              <stop offset="1" stopColor={color} stopOpacity="0.02" />
            </linearGradient>
          </defs>
          <path d={areaD} fill={`url(#${id})`} />
        </>
      )}
      <path d={d} fill="none" stroke={color} strokeWidth={1.5} strokeLinejoin="round" strokeLinecap="round" />
      {endDot && <circle cx={x(last[0])} cy={y(last[1])} r={2.5} fill="var(--accent)" stroke="var(--bg-surface)" strokeWidth={1.5} />}
    </svg>
  );
}

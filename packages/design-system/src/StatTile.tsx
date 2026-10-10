import type { ReactNode } from "react";
import { ColorKey } from "./Cost.tsx";
import { formatDelta } from "./format.ts";
import { Skeleton } from "./Spinner.tsx";
import { Sparkline } from "./Sparkline.tsx";

export interface StatTileProps {
  label: ReactNode;
  /** A swatch before the label, for a figure that is one series or cost family ("■ Compute"). */
  swatch?: string;
  /** Pre-formatted value ("12.9K", "1.4 GiB"). Proportional figures, semibold. */
  value: ReactNode;
  /** The line under the value: its unit, what it is out of, or a short note. */
  unit?: ReactNode;
  /** Signed delta vs `deltaLabel`; sign and `upIsGood` pick the color. */
  delta?: number | null;
  deltaUnit?: "%" | "";
  deltaLabel?: string;
  upIsGood?: boolean;
  /** Sparkline data; drawn in the de-emphasis hue with the current point in accent. */
  trend?: (number | null)[];
  /** Custom sparkline slot; overrides `trend`. */
  sparkline?: ReactNode;
  loading?: boolean;
  /** Severity tint on the value (for e.g. "hosts lost: 3"). */
  tone?: "default" | "warn" | "danger" | "success";
  /** The one figure the page is about: an accent-tinted tile. At most one per row. */
  lead?: boolean;
  onClick?: () => void;
}

export function StatTile(p: StatTileProps) {
  const { label, swatch, value, unit, delta, deltaUnit = "%", deltaLabel, upIsGood = true, trend, sparkline, loading, tone = "default", lead, onClick } = p;
  const dir = delta == null || delta === 0 ? "flat" : delta > 0 === upIsGood ? "good" : "bad";
  const Tag = onClick ? "button" : "div";
  return (
    <Tag className={["stat", `stat-${tone}`, lead ? "stat-lead" : "", onClick ? "is-clickable" : ""].join(" ").trim()} onClick={onClick} type={onClick ? "button" : undefined}>
      <div className="stat-label">{swatch ? <ColorKey color={swatch}>{label}</ColorKey> : label}</div>
      <div className="stat-main">
        <div className="stat-value">{loading ? <Skeleton width={72} height={20} /> : value}</div>
        {(sparkline || (trend && trend.length > 1)) && <div className="stat-spark">{sparkline ?? <Sparkline values={trend!} width={88} height={24} />}</div>}
      </div>
      {unit != null && unit !== "" && !loading && <div className="stat-unit">{unit}</div>}
      {delta != null && !loading && (
        <div className={`stat-delta stat-delta-${dir}`}>
          <span className="num">{formatDelta(delta, deltaUnit)}</span>
          {deltaLabel && <span className="stat-delta-label"> vs {deltaLabel}</span>}
        </div>
      )}
    </Tag>
  );
}

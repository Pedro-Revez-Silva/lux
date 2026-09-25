import type { ReactNode } from "react";
import { formatDelta } from "./format.ts";
import { Skeleton } from "./Spinner.tsx";
import { Sparkline } from "./Sparkline.tsx";

export interface StatTileProps {
  label: ReactNode;
  /** Pre-formatted value ("12.9K", "1.4 GiB"). Proportional figures, semibold. */
  value: ReactNode;
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
  onClick?: () => void;
}

export function StatTile(p: StatTileProps) {
  const { label, value, unit, delta, deltaUnit = "%", deltaLabel, upIsGood = true, trend, sparkline, loading, tone = "default", onClick } = p;
  const dir = delta == null || delta === 0 ? "flat" : delta > 0 === upIsGood ? "good" : "bad";
  const Tag = onClick ? "button" : "div";
  return (
    <Tag className={["stat", `stat-${tone}`, onClick ? "is-clickable" : ""].join(" ").trim()} onClick={onClick} type={onClick ? "button" : undefined}>
      <div className="stat-label">{label}</div>
      <div className="stat-main">
        <div className="stat-value">
          {loading ? (
            <Skeleton width={72} height={22} />
          ) : (
            <>
              {value}
              {unit && <span className="stat-unit">{unit}</span>}
            </>
          )}
        </div>
        {(sparkline || (trend && trend.length > 1)) && <div className="stat-spark">{sparkline ?? <Sparkline values={trend!} width={88} height={26} />}</div>}
      </div>
      {delta != null && !loading && (
        <div className={`stat-delta stat-delta-${dir}`}>
          <span className="num">{formatDelta(delta, deltaUnit)}</span>
          {deltaLabel && <span className="stat-delta-label"> vs {deltaLabel}</span>}
        </div>
      )}
    </Tag>
  );
}

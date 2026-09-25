import type { CSSProperties } from "react";

export function Spinner({ size = 14, className }: { size?: number; className?: string }) {
  return (
    <span
      className={["spinner", className ?? ""].join(" ").trim()}
      role="status"
      aria-label="Loading"
      style={{ width: size, height: size, borderWidth: Math.max(1.5, size / 8) }}
    />
  );
}

export interface SkeletonProps {
  width?: number | string;
  height?: number | string;
  /** Rounded like a pill. */
  round?: boolean;
  style?: CSSProperties;
  className?: string;
}

export function Skeleton({ width = "100%", height = 12, round, style, className }: SkeletonProps) {
  return (
    <span
      className={["skeleton", round ? "skeleton-round" : "", className ?? ""].join(" ").trim()}
      style={{ width, height, ...style }}
      aria-hidden="true"
    />
  );
}

/** A few lines of skeleton text. */
export function SkeletonLines({ lines = 3 }: { lines?: number }) {
  return (
    <div className="skeleton-lines">
      {Array.from({ length: lines }, (_, i) => (
        <Skeleton key={i} width={`${100 - (i % 3) * 18}%`} />
      ))}
    </div>
  );
}

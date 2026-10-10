import type { ReactNode } from "react";
import { ColorKey } from "./Cost.tsx";
import { Tooltip } from "./Tooltip.tsx";

export interface PartBarPart {
  label: string;
  /** Geometry only: a part's length is its share of the parts' sum. Zero, negative or missing draws nothing. */
  value: number | null | undefined;
  /** A token or series colour (`var(--chart-4)`, `familyColor(f)`). */
  color: string;
  /** The value as the page writes it ("$5.15"), shown in the segment's Tooltip. */
  text?: ReactNode;
}

export interface PartShare {
  /** 0..1 of the drawn parts' sum; 0 for a part that draws nothing. */
  share: number;
  /** Whole percent; the drawn parts' percents add up to exactly 100. */
  percent: number;
  /** "74%", "<1%" for a drawn part that rounds to 0, "" for one that draws nothing. */
  label: string;
}

function drawn(v: number | null | undefined): v is number {
  return v != null && Number.isFinite(v) && v > 0;
}

function shareLabel(share: number, percent: number): string {
  if (share === 0) return "";
  if (percent === 0) return "<1%";
  return `${percent}%`;
}

/**
 * Each part's share of the sum of the drawn parts, with whole percents by
 * largest remainder so they add up to 100 (74 + 19 + 6 + 1, never 99 or 101).
 * Ties in remainder go to the earlier part.
 */
export function partShares(values: readonly (number | null | undefined)[]): PartShare[] {
  const total = values.reduce<number>((a, v) => a + (drawn(v) ? v : 0), 0);
  if (total <= 0) return values.map(() => ({ share: 0, percent: 0, label: "" }));
  const shares = values.map((v) => (drawn(v) ? v / total : 0));
  const percents = shares.map((s) => Math.floor(s * 100));
  let left = 100 - percents.reduce((a, b) => a + b, 0);
  const order = shares
    .map((s, i) => ({ i, rem: s * 100 - percents[i]! }))
    .filter(({ i }) => shares[i]! > 0)
    .sort((a, b) => b.rem - a.rem || a.i - b.i);
  for (const { i } of order) {
    if (left <= 0) break;
    percents[i]! += 1;
    left -= 1;
  }
  return shares.map((share, i) => ({ share, percent: percents[i]!, label: shareLabel(share, percents[i]!) }));
}

export interface PartBarProps {
  parts: readonly PartBarPart[];
  /** The bar's accessible name ("Why $6.94 was unallocated"). */
  label: string;
  /** Height in px. */
  height?: number;
  className?: string;
}

/**
 * One total split into parts: a single bar of segments with 1px gaps, each
 * its share of the sum, each with a Tooltip (label, value, share). A part
 * that is zero or missing draws nothing. The parts and their figures belong
 * in a list or table beside it: the bar is the picture, not the record.
 */
export function PartBar({ parts, label, height = 12, className }: PartBarProps) {
  const shares = partShares(parts.map((p) => p.value));
  return (
    <div className={["part-bar", className ?? ""].join(" ").trim()} role="group" aria-label={label} style={{ height }}>
      {parts.map((p, i) => {
        const s = shares[i]!;
        if (s.share === 0) return null;
        return (
          <Tooltip
            key={p.label}
            className="part-bar-seg"
            style={{ flexGrow: s.share, background: p.color }}
            content={
              <span className="part-bar-tip">
                <ColorKey color={p.color}>{p.label}</ColorKey>
                {p.text != null && <span className="num">{p.text}</span>}
                <span className="num">{s.label}</span>
              </span>
            }
          >
            <span className="part-bar-hit" tabIndex={0} aria-label={`${p.label} ${s.label}`} data-part={p.label} />
          </Tooltip>
        );
      })}
    </div>
  );
}

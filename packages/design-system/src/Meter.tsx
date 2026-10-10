import { formatPercent } from "./format.ts";

/** A ratio as the meter draws it: clamped to 0..1, or null when there is none to draw. */
export function meterFill(value: number | null | undefined): number | null {
  if (value == null || !Number.isFinite(value)) return null;
  return Math.min(1, Math.max(0, value));
}

/** The meter's text: the true ratio as a percentage; a non-zero one that rounds to zero reads "<1%" (with decimals, "<0.1%"). */
export function meterText(value: number | null | undefined, decimals = 0): string {
  if (value == null || !Number.isFinite(value)) return formatPercent(null);
  const text = formatPercent(value, decimals);
  return value > 0 && Number(text.slice(0, -1)) === 0 ? `<${10 ** -decimals}%` : text;
}

export interface MeterProps {
  /** A ratio, 0..1; outside it the fill clamps but the text keeps the true figure. null → en dash. */
  value: number | null | undefined;
  /** Fill colour: a token or series colour. */
  color?: string;
  /** Decimals of the percentage. */
  decimals?: number;
}

/** A ratio inside a table cell: a 46px track with its fill, the percentage to its right (mono). */
export function Meter({ value, color = "var(--chart-1)", decimals = 0 }: MeterProps) {
  const fill = meterFill(value);
  return (
    <span className="meter">
      <span className="meter-track" aria-hidden="true">
        {fill != null && fill > 0 && <span className="meter-fill" style={{ width: `${fill * 100}%`, background: color }} />}
      </span>
      <span className="meter-text mono">{meterText(value, decimals)}</span>
    </span>
  );
}

import type { ReactNode } from "react";

export interface SegmentedOption<V extends string> {
  value: V;
  label: ReactNode;
  /** Tooltip text (title). */
  title?: string;
}

export interface SegmentedControlProps<V extends string> {
  options: readonly SegmentedOption<V>[];
  value: V;
  onChange: (v: V) => void;
  /** The group's accessible name. */
  label: string;
  size?: "sm" | "md";
  className?: string;
}

/**
 * One choice out of a few, side by side (a radio group drawn as joined
 * buttons): Live / All / Ended, Match console / Solarized light / dark.
 */
export function SegmentedControl<V extends string>({ options, value, onChange, label, size = "sm", className }: SegmentedControlProps<V>) {
  return (
    <div role="radiogroup" aria-label={label} className={["segmented", `segmented-${size}`, className ?? ""].join(" ").trim()}>
      {options.map((o) => (
        <button
          key={o.value}
          type="button"
          role="radio"
          aria-checked={o.value === value}
          title={o.title}
          className={o.value === value ? "segmented-opt is-on" : "segmented-opt"}
          onClick={() => o.value !== value && onChange(o.value)}
        >
          {o.label}
        </button>
      ))}
    </div>
  );
}

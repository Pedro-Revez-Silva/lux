import type { ReactNode } from "react";

export interface KeyValueItem {
  key: ReactNode;
  value: ReactNode;
  mono?: boolean;
}

export interface KeyValueProps {
  items: KeyValueItem[];
  /** Two columns of pairs on wide panels. */
  columns?: 1 | 2 | 3;
  /** Full-width rows under the grid (FactRow, LabelsFact): values that can be long never sit in a grid cell. */
  children?: ReactNode;
  /**
   * With children, grid and rows share a wrapper that draws its own card (border, background, padding).
   * Default on; set false inside a Card, so the rows draw without a second frame.
   */
  surface?: boolean;
  /** On the `<dl>`; with children, on the wrapper around grid and rows instead. */
  className?: string;
}

export function KeyValue({ items, columns = 1, children, surface = true, className }: KeyValueProps) {
  const grid = (
    <dl className={["kv", `kv-cols-${columns}`, children ? "" : (className ?? "")].join(" ").trim()}>
      {items.map((it, i) => (
        <div className="kv-row" key={i}>
          <dt className="kv-key">{it.key}</dt>
          <dd className={it.mono ? "kv-value mono" : "kv-value"}>{it.value ?? <span className="muted">–</span>}</dd>
        </div>
      ))}
    </dl>
  );
  if (!children) return grid;
  return (
    <div className={["kv-facts", surface ? "kv-facts-surface" : "", className ?? ""].filter(Boolean).join(" ")}>
      {grid}
      {children}
    </div>
  );
}

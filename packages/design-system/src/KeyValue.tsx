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
  className?: string;
}

export function KeyValue({ items, columns = 1, className }: KeyValueProps) {
  return (
    <dl className={["kv", `kv-cols-${columns}`, className ?? ""].join(" ").trim()}>
      {items.map((it, i) => (
        <div className="kv-row" key={i}>
          <dt className="kv-key">{it.key}</dt>
          <dd className={it.mono ? "kv-value mono" : "kv-value"}>{it.value ?? <span className="muted">–</span>}</dd>
        </div>
      ))}
    </dl>
  );
}

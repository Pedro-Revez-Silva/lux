import type { ReactNode } from "react";

export interface TabItem<K extends string = string> {
  key: K;
  label: ReactNode;
  count?: number;
  disabled?: boolean;
}

export interface TabsProps<K extends string = string> {
  items: TabItem<K>[];
  value: K;
  onChange: (key: K) => void;
  /** Smaller tabs for inside panels. */
  size?: "sm" | "md";
  className?: string;
}

export function Tabs<K extends string>({ items, value, onChange, size = "md", className }: TabsProps<K>) {
  return (
    <div role="tablist" className={["tabs", `tabs-${size}`, className ?? ""].join(" ").trim()}>
      {items.map((t) => (
        <button
          key={t.key}
          type="button"
          role="tab"
          aria-selected={t.key === value}
          disabled={t.disabled}
          className={t.key === value ? "tab is-active" : "tab"}
          onClick={() => onChange(t.key)}
        >
          {t.label}
          {t.count != null && <span className="tab-count num">{t.count}</span>}
        </button>
      ))}
    </div>
  );
}

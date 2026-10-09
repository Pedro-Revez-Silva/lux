import { useEffect, useId, useMemo, useRef, useState, type ReactNode } from "react";
import { IconCheck, IconChevronDown, IconSearch } from "./icons.tsx";

export interface SelectOption<V extends string = string> {
  value: V;
  label: ReactNode;
  /** Plain-text label for filtering and the trigger; defaults to value. */
  text?: string;
  description?: ReactNode;
  group?: string;
  /** Shown greyed and not pickable; the text says why (shown on the right, and as the option's title). */
  disabled?: string;
  /** A quiet note on the right of a pickable option ("24 points"). */
  hint?: string;
}

export interface SelectProps<V extends string = string> {
  options: SelectOption<V>[];
  value: V;
  onChange: (v: V) => void;
  /** Shown in the trigger when nothing matches. */
  placeholder?: string;
  /** Leading label in the trigger, e.g. "Tenant". */
  prefix?: ReactNode;
  icon?: ReactNode;
  searchable?: boolean;
  size?: "sm" | "md";
  disabled?: boolean;
  className?: string;
  /** Minimum trigger width. */
  width?: number;
  /** Below the options in the menu: what the choices mean. */
  footer?: ReactNode;
}

/** Listbox-style select with optional filter. Presets listed as rows, selection marked by a check. */
export function Select<V extends string>(props: SelectProps<V>) {
  const { options, value, onChange, placeholder = "Select…", prefix, icon, searchable, size = "md", disabled, className, width, footer } = props;
  const id = useId();
  const [open, setOpen] = useState(false);
  const [query, setQuery] = useState("");
  const [active, setActive] = useState(0);
  const root = useRef<HTMLDivElement>(null);
  const searchRef = useRef<HTMLInputElement>(null);

  const current = options.find((o) => o.value === value);
  const filtered = useMemo(() => {
    const q = query.trim().toLowerCase();
    if (!q) return options;
    return options.filter((o) => (o.text ?? o.value).toLowerCase().includes(q));
  }, [options, query]);

  useEffect(() => {
    if (!open) return;
    setQuery("");
    setActive(Math.max(0, options.findIndex((o) => o.value === value)));
    const onDoc = (e: MouseEvent) => {
      if (!root.current?.contains(e.target as Node)) setOpen(false);
    };
    document.addEventListener("mousedown", onDoc);
    requestAnimationFrame(() => searchRef.current?.focus());
    return () => document.removeEventListener("mousedown", onDoc);
  }, [open, options, value]);

  const pick = (v: V) => {
    if (options.find((o) => o.value === v)?.disabled) return;
    onChange(v);
    setOpen(false);
  };
  // Arrow keys step over disabled options.
  const step = (from: number, dir: 1 | -1) => {
    for (let i = from + dir; i >= 0 && i < filtered.length; i += dir) if (!filtered[i]!.disabled) return i;
    return from;
  };

  const onKey = (e: React.KeyboardEvent) => {
    if (!open && (e.key === "ArrowDown" || e.key === "Enter" || e.key === " ")) {
      e.preventDefault();
      setOpen(true);
      return;
    }
    if (!open) return;
    if (e.key === "Escape") {
      // Closes this menu only, not a dialog the select sits in.
      e.stopPropagation();
      setOpen(false);
    } else if (e.key === "ArrowDown") {
      e.preventDefault();
      setActive((i) => step(i, 1));
    } else if (e.key === "ArrowUp") {
      e.preventDefault();
      setActive((i) => step(i, -1));
    } else if (e.key === "Enter") {
      e.preventDefault();
      const o = filtered[active];
      if (o) pick(o.value);
    }
  };

  let lastGroup: string | undefined;
  return (
    <div ref={root} className={["select", `select-${size}`, open ? "is-open" : "", className ?? ""].join(" ").trim()} onKeyDown={onKey}>
      <button
        type="button"
        className="select-trigger"
        aria-haspopup="listbox"
        aria-expanded={open}
        aria-controls={id}
        disabled={disabled}
        onClick={() => setOpen((o) => !o)}
        style={width ? { minWidth: width } : undefined}
      >
        {icon && <span className="select-icon">{icon}</span>}
        {prefix && <span className="select-prefix">{prefix}</span>}
        <span className="select-value">{current ? (current.text ?? current.label) : <span className="muted">{placeholder}</span>}</span>
        <IconChevronDown size={12} className="select-chev" />
      </button>
      {open && (
        <div className="select-pop">
          {searchable && (
            <div className="select-search">
              <IconSearch size={12} />
              <input
                ref={searchRef}
                className="select-search-input"
                value={query}
                placeholder="Filter…"
                onChange={(e) => {
                  setQuery(e.target.value);
                  setActive(0);
                }}
              />
            </div>
          )}
          <ul role="listbox" id={id} className="select-list" tabIndex={-1}>
            {filtered.length === 0 && <li className="select-empty muted">No matches</li>}
            {filtered.map((o, i) => {
              const header = o.group && o.group !== lastGroup ? o.group : null;
              lastGroup = o.group;
              return (
                <li key={o.value} role="presentation">
                  {header && <div className="select-group">{header}</div>}
                  <div
                    role="option"
                    aria-selected={o.value === value}
                    aria-disabled={o.disabled ? true : undefined}
                    title={o.disabled}
                    className={["select-opt", i === active ? "is-active" : "", o.value === value ? "is-selected" : "", o.disabled ? "is-disabled" : ""].join(" ").trim()}
                    onMouseEnter={() => !o.disabled && setActive(i)}
                    onClick={() => pick(o.value)}
                  >
                    <span className="select-check">{o.value === value && <IconCheck size={14} strokeWidth={2.5} />}</span>
                    <span className="select-opt-body">
                      <span className="select-opt-label">{o.label}</span>
                      {o.description && <span className="select-opt-desc">{o.description}</span>}
                    </span>
                    {o.disabled ? <span className="select-opt-why">{o.disabled}</span> : o.hint && <span className="select-opt-why">{o.hint}</span>}
                  </div>
                </li>
              );
            })}
          </ul>
          {footer && <div className="select-footer">{footer}</div>}
        </div>
      )}
    </div>
  );
}

export const ALL_TENANTS = "*";

export interface Tenant {
  id: string;
  name?: string;
  /** e.g. active run count, shown dimmed on the right. */
  hint?: string;
}

export interface TenantPickerProps {
  tenants: Tenant[];
  value: string;
  onChange: (tenantId: string) => void;
  size?: "sm" | "md";
}

/** Global tenant scope. "*" means all tenants. */
export function TenantPicker({ tenants, value, onChange, size = "sm" }: TenantPickerProps) {
  const options: SelectOption[] = [
    { value: ALL_TENANTS, label: "All tenants", text: "All tenants" },
    ...tenants.map((t) => ({
      value: t.id,
      text: t.name ?? t.id,
      label: (
        <span className="tenant-opt">
          <span>{t.name ?? t.id}</span>
          {t.hint && <span className="muted num">{t.hint}</span>}
        </span>
      ),
      description: t.name ? <span className="mono">{t.id}</span> : undefined,
      group: "Tenants",
    })),
  ];
  return <Select options={options} value={value} onChange={onChange} prefix="Tenant" searchable={tenants.length > 6} size={size} width={180} />;
}

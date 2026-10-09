import { useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { Badge } from "./Badge.tsx";
import { Button } from "./Button.tsx";
import { ColorKey, Money, MoneyList, type MoneyAmount } from "./Cost.tsx";
import { compareMoney, formatMoney } from "./format.ts";
import { IconClose, IconInfo, IconPlus, IconSearch } from "./icons.tsx";
import { Select, type SelectOption } from "./Select.tsx";
import { Skeleton } from "./Spinner.tsx";
import { Table, type Column } from "./Table.tsx";

/* ---------- KPI strip ---------- */

export interface KpiProps {
  label: ReactNode;
  value: ReactNode;
  /** A line under the value. */
  sub?: ReactNode;
  loading?: boolean;
  /** Drawn faint: a figure the view hides but still states. */
  muted?: boolean;
  children?: ReactNode;
}

/** One figure of a KpiStrip: label, value, a line under it. */
export function Kpi({ label, value, sub, loading, muted, children }: KpiProps) {
  return (
    <div className={muted ? "kpi is-muted" : "kpi"}>
      <div className="kpi-label">{label}</div>
      <div className="kpi-value">{loading ? <Skeleton width={96} height={24} /> : value}</div>
      {!loading && sub && <div className="kpi-sub">{sub}</div>}
      {!loading && children}
    </div>
  );
}

/** A Kpi's further line (a change, a split bar's caption). */
export function KpiSub({ children, ...rest }: { children: ReactNode } & React.HTMLAttributes<HTMLDivElement>) {
  return (
    <div className="kpi-sub" {...rest}>
      {children}
    </div>
  );
}

/** Figures side by side across the top of a card: the first wider, two across in a narrow container, four from 760px. */
export function KpiStrip({ children }: { children: ReactNode }) {
  return (
    <div className="kpi-strip-box">
      <div className="kpi-strip">{children}</div>
    </div>
  );
}

/* ---------- SplitBar ---------- */

export interface SplitPart {
  amount: string | null;
  color: string;
  label: string;
  /** Drawn faint: a part the view hides, never dropped. */
  faint?: boolean;
}

export interface SplitBarProps {
  parts: SplitPart[];
  /** The whole the parts are drawn against. */
  whole: string | null;
  currency: string;
  /** Before the bar (a currency code when there are several). */
  label?: string;
  /** The bar's length as a share of its container (a row's whole against the largest row's). */
  scale?: number;
}

/** Parts of one amount as one thin bar: each part's length its share of the whole; the amounts in the title. */
export function SplitBar({ parts, whole, currency, label, scale = 1 }: SplitBarProps) {
  const w = Number(whole);
  const shares = parts.map((p) => (p.amount == null || !Number.isFinite(w) || w === 0 ? 0 : Math.max(0, Number(p.amount) / w)));
  const sum = shares.reduce((a, b) => a + b, 0) || 1;
  const title = parts.flatMap((p) => (p.amount == null ? [] : [`${p.label} ${formatMoney(p.amount, currency)}`])).join(" · ");
  return (
    <div className="split-bar" title={title || undefined} data-split-bar>
      {label && <span className="split-bar-label">{label}</span>}
      <span className="split-bar-track" style={{ width: `${Math.max(2, scale * 100)}%` }}>
        {parts.map((p, i) => (shares[i]! > 0 ? <span key={p.label} className={p.faint ? "split-bar-part is-faint" : "split-bar-part"} style={{ flexGrow: shares[i]! / sum, background: p.color }} /> : null))}
      </span>
    </div>
  );
}

/* ---------- Breakdown table ---------- */

export interface BreakdownTableRow {
  id: string;
  label: ReactNode;
  color: string;
  currency: string;
  runs: number | null;
  compute: string | null;
  external: string | null;
  /** What the view counts. */
  total: string;
  share: number | null;
  /** The name set in mono (ids, label values, key names). */
  mono?: boolean;
  /** A muted name: the row of what has no value ("(no app label)", "Before key tracking"). */
  quiet?: boolean;
  /** A pill after the name ("revoked"). */
  pill?: string;
}

export interface BreakdownTableProps {
  rows: BreakdownTableRow[];
  /** The first column's header: the breakdown ("app", "API key"). */
  lead: string;
  loading?: boolean;
  /** A row is a filter: clicking adds it. Rows without one are not clickable. */
  onRowClick?: (r: BreakdownTableRow) => void;
  empty?: ReactNode;
}

const pct = (r: number | null) => (r == null ? "–" : `${Math.round(r * 100)}%`);
const money = (a: string | null, currency: string) => (a == null ? <span className="muted">–</span> : <Money amount={a} currency={currency} decimals={2} />);

/**
 * Cost by one dimension: each value with its swatch, its Runs, its Compute
 * and External parts, the total the view counts and its share. Shares are
 * per currency; with several currencies each row names its own.
 */
export function BreakdownTable({ rows, lead, loading, onRowClick, empty = "No cost in this range." }: BreakdownTableProps) {
  const multi = new Set(rows.map((r) => r.currency)).size > 1;
  const cols: Column<BreakdownTableRow>[] = [
    {
      key: "name",
      header: lead,
      lead: true,
      cell: (r) => (
        <span className={["breakdown-name", r.quiet ? "is-quiet" : ""].join(" ").trim()}>
          <ColorKey color={r.color}>
            <span className={r.mono ? "mono" : undefined}>{r.label}</span>
          </ColorKey>
          {r.pill && <Badge tone="neutral">{r.pill}</Badge>}
        </span>
      ),
    },
    { key: "runs", header: "Runs", align: "right", mono: true, width: 64, cell: (r) => (r.runs == null ? <span className="muted">–</span> : r.runs) },
    { key: "compute", header: "Compute", align: "right", mono: true, width: 96, cell: (r) => money(r.compute, r.currency) },
    { key: "external", header: "External", align: "right", mono: true, width: 96, cell: (r) => money(r.external, r.currency) },
    { key: "total", header: "Total", align: "right", mono: true, width: 104, cell: (r) => <strong>{money(r.total, r.currency)}</strong> },
    { key: "share", header: multi ? "Share (per currency)" : "Share", align: "right", mono: true, width: multi ? 92 : 64, cell: (r) => pct(r.share) },
  ];
  return <Table columns={cols} rows={rows} rowKey={(r) => `${r.currency}:${r.id}`} loading={loading} loadingRows={3} onRowClick={onRowClick} empty={empty} dense />;
}

/* ---------- Label chips ---------- */

/** A Run's labels as key=value chips, the first `max` and a count of the rest; each in full in its title. */
export function LabelChips({ labels, max = 3, first }: { labels: Record<string, string> | undefined; max?: number; first?: string[] }) {
  const entries = Object.entries(labels ?? {}).sort(([a], [b]) => {
    const ia = first?.indexOf(a) ?? -1;
    const ib = first?.indexOf(b) ?? -1;
    return (ia < 0 ? Infinity : ia) - (ib < 0 ? Infinity : ib) || a.localeCompare(b);
  });
  if (entries.length === 0) return <span className="muted">–</span>;
  const shown = entries.slice(0, max);
  const rest = entries.length - shown.length;
  return (
    <span className="label-chips">
      {shown.map(([k, v]) => (
        <span key={k} className="label-chip" title={`${k}=${v}`}>
          {k}={v}
        </span>
      ))}
      {rest > 0 && (
        <span className="label-chip is-more" title={entries.slice(max).map(([k, v]) => `${k}=${v}`).join("\n")}>
          +{rest}
        </span>
      )}
    </span>
  );
}

/* ---------- Info strip ---------- */

/** A one-line notice inside a card: why some figures read as they do. */
export function InfoStrip({ tone = "info", children }: { tone?: "info" | "warn"; children: ReactNode }) {
  return (
    <div className={`info-strip info-strip-${tone}`} role="note">
      <IconInfo size={13} />
      <span>{children}</span>
    </div>
  );
}

/* ---------- Filter bar ---------- */

export interface FilterChipProps {
  name: string;
  /** "=", "∈" or "is". */
  op: string;
  value: string;
  onRemove: () => void;
}

/** One active filter: name, operator, value(s), and a button that removes it. */
export function FilterChip({ name, op, value, onRemove }: FilterChipProps) {
  return (
    <span className="filter-chip" data-filter-chip={name}>
      <span className="filter-chip-name mono">{name}</span>
      <span className="filter-chip-op">{op}</span>
      <span className="filter-chip-value">{value}</span>
      <button type="button" className="filter-chip-remove" aria-label={`Remove filter ${name}`} onClick={onRemove}>
        <IconClose size={12} />
      </button>
    </span>
  );
}

/** "Filter", the active chips, the add control, and a note on what the filters reach. */
export function FilterBar({ children, add, note }: { children?: ReactNode; add: ReactNode; note?: ReactNode }) {
  return (
    <div className="filter-bar" role="group" aria-label="Filters">
      <span className="filter-bar-label">Filter</span>
      {children}
      {add}
      {note && <span className="filter-bar-note">{note}</span>}
    </div>
  );
}

/* ---------- Label filter popover ---------- */

export interface LabelValueOption {
  value: string;
  /** The cost in range, one figure per currency, largest first; empty: none. */
  amounts: MoneyAmount[];
}

export interface LabelFilterPopoverProps {
  /** Label keys to pick from, with their Runs. */
  keys: { key: string; runs?: number }[];
  /** The key picked first. */
  initialKey?: string;
  /** The values of a key with their cost; undefined while loading. */
  values: (key: string) => LabelValueOption[] | undefined;
  /** The cost of Runs without the key (the "(not set)" row); undefined: unknown. */
  notSet?: (key: string) => MoneyAmount[] | undefined;
  onApply: (f: { key: string; values: string[]; notSet: boolean }) => void;
  /** The key in the picker, as it changes while open (to fetch its values). */
  onKeyChange?: (key: string) => void;
  /** The button's text. */
  label?: string;
}

/** Values largest first by their first currency's amount, then by name. */
function byCost(a: LabelValueOption, b: LabelValueOption): number {
  const x = a.amounts[0];
  const y = b.amounts[0];
  if (x && y && x.currency === y.currency) return compareMoney(y.amount, x.amount) || a.value.localeCompare(b.value);
  return Number(!x) - Number(!y) || a.value.localeCompare(b.value);
}

/** The values a search keeps (case-insensitive, anywhere in the value), biggest cost first. */
export function matchValues(values: LabelValueOption[], query: string): LabelValueOption[] {
  const q = query.trim().toLowerCase();
  return values.filter((v) => !q || v.value.toLowerCase().includes(q)).sort(byCost);
}

/**
 * "＋ Label filter": pick a label key, then values (searchable checkboxes,
 * each with its cost in range, the biggest first) or "(not set)". Values
 * of one filter are alternatives; Apply adds the filter, Cancel or Escape
 * leaves things as they were.
 */
export function LabelFilterPopover({ keys, initialKey, values, notSet, onApply, onKeyChange, label = "Label filter" }: LabelFilterPopoverProps) {
  const [open, setOpen] = useState(false);
  const [key, setKey] = useState(initialKey ?? keys[0]?.key ?? "");
  const [query, setQuery] = useState("");
  const [picked, setPicked] = useState<string[]>([]);
  const [none, setNone] = useState(false);
  const root = useRef<HTMLDivElement>(null);
  const addButton = useRef<HTMLButtonElement>(null);
  const pop = useRef<HTMLDivElement>(null);
  const wasOpen = useRef(false);

  useEffect(() => {
    if (!open) {
      // Focus goes back to the button that opened it, unless the user clicked elsewhere.
      if (wasOpen.current && (document.activeElement === document.body || root.current?.contains(document.activeElement))) addButton.current?.focus();
      wasOpen.current = false;
      return;
    }
    wasOpen.current = true;
    setKey(initialKey ?? keys[0]?.key ?? "");
    setQuery("");
    setPicked([]);
    setNone(false);
    // Into the dialog: the key picker, or the search when there is no key.
    (pop.current?.querySelector<HTMLElement>(".label-filter-key .select-trigger") ?? pop.current?.querySelector<HTMLElement>(".label-filter-search-input"))?.focus();
    const onDoc = (e: MouseEvent) => {
      if (!root.current?.contains(e.target as Node)) setOpen(false);
    };
    // The key Select stops its own Escape while its menu is open.
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") setOpen(false);
    };
    document.addEventListener("mousedown", onDoc);
    document.addEventListener("keydown", onKey);
    return () => {
      document.removeEventListener("mousedown", onDoc);
      document.removeEventListener("keydown", onKey);
    };
    // Reset only on opening.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open]);

  useEffect(() => {
    if (open && !key && keys.length) setKey(initialKey ?? keys[0]!.key);
  }, [open, key, keys, initialKey]);
  useEffect(() => {
    if (open && key) onKeyChange?.(key);
  }, [open, key, onKeyChange]);

  const all = values(key);
  const shown = useMemo(() => matchValues(all ?? [], query), [all, query]);
  const unset = notSet?.(key);
  const keyOptions: SelectOption[] = keys.map((k) => ({ value: k.key, text: k.key, label: <span className="mono">{k.key}</span>, hint: k.runs != null ? `${k.runs} Runs` : undefined }));
  const toggle = (v: string) => setPicked((p) => (p.includes(v) ? p.filter((x) => x !== v) : [...p, v]));
  const apply = () => {
    onApply({ key, values: picked, notSet: none });
    setOpen(false);
  };

  return (
    <div className="label-filter" ref={root}>
      <button ref={addButton} type="button" className="filter-add" aria-expanded={open} aria-haspopup="dialog" onClick={() => setOpen((o) => !o)}>
        <IconPlus size={12} /> {label}
      </button>
      {open && (
        <div ref={pop} className="label-filter-pop" role="dialog" aria-label="Add a label filter">
          <div className="label-filter-section">Label</div>
          {keys.length === 0 ? (
            <div className="muted label-filter-empty">No Run with cost in this range has a label.</div>
          ) : (
            <Select options={keyOptions} value={key} onChange={(k) => (setKey(k), setPicked([]), setNone(false), setQuery(""))} searchable={keys.length > 8} size="sm" className="label-filter-key" />
          )}
          <div className="label-filter-section">
            is any of <span className="muted">· cost in range</span>
          </div>
          <label className="label-filter-search">
            <IconSearch size={12} />
            <input className="label-filter-search-input" placeholder="Search values" value={query} onChange={(e) => setQuery(e.target.value)} aria-label="Search values" />
          </label>
          <ul className="label-filter-values" aria-label={`Values of ${key}`}>
            {all == null && (
              <li className="label-filter-empty">
                <Skeleton width="80%" />
              </li>
            )}
            {all != null && shown.length === 0 && <li className="label-filter-empty muted">No values{query ? " match" : ""}.</li>}
            {shown.map((v) => (
              <li key={v.value}>
                <label className="label-filter-value">
                  <input type="checkbox" checked={picked.includes(v.value)} disabled={none} onChange={() => toggle(v.value)} />
                  <span className="mono ellipsis">{v.value}</span>
                  <MoneyList amounts={v.amounts} decimals={2} className="label-filter-amount" />
                </label>
              </li>
            ))}
            {!query && (
              <li>
                <label className="label-filter-value">
                  <input type="checkbox" checked={none} onChange={() => (setNone((n) => !n), setPicked([]))} />
                  <span className="mono">(not set)</span>
                  {unset && <MoneyList amounts={unset} decimals={2} className="label-filter-amount" />}
                </label>
              </li>
            )}
          </ul>
          <div className="label-filter-actions">
            <Button size="sm" variant="default" onClick={() => setOpen(false)}>
              Cancel
            </Button>
            <Button size="sm" variant="primary" disabled={!key || (!none && picked.length === 0)} onClick={apply}>
              Apply
            </Button>
          </div>
        </div>
      )}
    </div>
  );
}

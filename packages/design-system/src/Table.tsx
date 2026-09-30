import { useEffect, useLayoutEffect, useMemo, useRef, useState, type KeyboardEvent, type ReactNode } from "react";
import { EmptyState } from "./EmptyState.tsx";
import { Skeleton } from "./Spinner.tsx";

/** Container width (px) under which optional columns are dropped. */
const NARROW = 1100;
/** The least width (px) a column without one gets before optional columns drop. */
const FLEX_MIN = 140;

/**
 * Whether optional columns drop at a container `width`: under NARROW, or
 * when with them the table would not fit: the fixed and percentage widths
 * leave each column without a width under FLEX_MIN (with none, when they
 * alone are wider than the container).
 */
/** A percentage width's share (0.22 for "22%"); a malformed one is 0, never NaN. */
function pct(width: string): number {
  return (parseFloat(width) || 0) / 100;
}

export function dropsOptional(columns: readonly Pick<Column<unknown>, "width" | "optional">[], width: number): boolean {
  if (width <= 0) return false;
  if (width < NARROW) return true;
  if (!columns.some((c) => c.optional)) return false;
  let fixed = 0;
  let flex = 0;
  for (const c of columns) {
    if (typeof c.width === "number") fixed += c.width;
    else if (typeof c.width === "string" && c.width.endsWith("%")) fixed += pct(c.width) * width;
    else flex++;
  }
  return width - fixed < flex * FLEX_MIN;
}

/**
 * A sensible floor for the table's width: fixed widths plus FLEX_MIN per
 * column without one, with percentage columns taking their share on top
 * (a 22% column leaves the rest 78% of the table, not the table less a
 * guess). Other widths (em, auto) count as 120px.
 */
export function tableFloor(columns: readonly Pick<Column<unknown>, "width">[]): number {
  let fixed = 0;
  let share = 0;
  for (const c of columns) {
    if (typeof c.width === "number") fixed += c.width;
    else if (typeof c.width === "string" && c.width.endsWith("%")) share += pct(c.width);
    else fixed += c.width ? 120 : FLEX_MIN;
  }
  return Math.ceil(fixed / Math.max(1 - share, 0.1));
}

export type SortDir = "asc" | "desc";
export type SortValue = string | number | null | undefined;

export interface Column<Row> {
  key: string;
  header: ReactNode;
  /** Cell renderer. */
  cell: (row: Row) => ReactNode;
  /** Value used for sorting loaded rows (client sort). Missing values sort last either way. */
  sortValue?: (row: Row) => SortValue;
  /** Sortable without a sortValue: the server sorts (sortMode "server"). */
  sortable?: boolean;
  /**
   * Direction of the first click. Default: "desc" for numbers, times and
   * durations (right-aligned columns, or a numeric sortValue), "asc" for text.
   */
  sortFirst?: SortDir;
  /** What the sort orders, for the sort in words (sortInWords): newest/oldest, largest/smallest, A→Z. Default: number when right-aligned, else text. */
  sortKind?: "time" | "number" | "text";
  /** The header as plain text, where `header` is an element. */
  label?: string;
  align?: "left" | "right";
  /** Monospace + tabular numerals (ids, logs, numbers). Names and labels stay in the sans. */
  mono?: boolean;
  /** Fixed width. Columns without one share the remaining width. */
  width?: number | string;
  /** Prevent wrapping and let the cell ellipsize (the default; kept for callers). */
  nowrap?: boolean;
  /** Let long content wrap onto several lines. */
  wrap?: boolean;
  /** The row's name: rendered in the foreground colour, medium weight. */
  lead?: boolean;
  /** Dropped when the table's container is under 1100px, or too narrow for the table with it (each column without a width gets 140px). */
  optional?: boolean;
}

export interface SortState {
  key: string;
  dir: SortDir;
}

export interface TableProps<Row> {
  columns: Column<Row>[];
  rows: Row[];
  rowKey: (row: Row) => string;
  onRowClick?: (row: Row) => void;
  /** Currently selected row key (highlighted). */
  selected?: string | null;
  /** Controlled sort. With sortMode "server" the rows are shown as given: the caller fetches them in this order. */
  sort?: SortState;
  onSortChange?: (s: SortState) => void;
  /** Uncontrolled default sort. */
  defaultSort?: SortState;
  /**
   * "client" (default) sorts the loaded rows by sortValue: for small,
   * unpaged tables. "server": a paged table whose whole result set the
   * server sorts; the table only reports the chosen sort.
   */
  sortMode?: "client" | "server";
  loading?: boolean;
  /** Rows to render as skeletons while loading with no data. */
  loadingRows?: number;
  empty?: ReactNode;
  /** Height for the scroll container; header sticks inside it. */
  maxHeight?: number | string;
  /** Denser rows (the density's dense row height). */
  dense?: boolean;
  /**
   * Below this width the table scrolls sideways inside its card, with the
   * first column pinned. Defaults to the sum of column widths plus room for
   * the flexible ones.
   */
  minWidth?: number;
  /** Under the table, outside its scroll area (a Pagination). */
  footer?: ReactNode;
  className?: string;
}

/** Whether a column sorts at all. */
export function isSortable<Row>(c: Column<Row>): boolean {
  return c.sortValue != null || c.sortable === true;
}

/** The first-click direction of a column (see Column.sortFirst). */
export function firstSortDir<Row>(c: Column<Row>, rows: readonly Row[] = []): SortDir {
  if (c.sortFirst) return c.sortFirst;
  if (c.align === "right") return "desc";
  if (c.sortValue) {
    for (const r of rows) {
      const v = c.sortValue(r);
      if (v != null) return typeof v === "number" ? "desc" : "asc";
    }
  }
  return "asc";
}

/** The sort after clicking column c: the other direction if it is the sorted one, else its first direction. */
export function nextSort<Row>(sort: SortState | undefined, c: Column<Row>, rows: readonly Row[] = []): SortState {
  if (sort?.key === c.key) return { key: c.key, dir: sort.dir === "asc" ? "desc" : "asc" };
  return { key: c.key, dir: firstSortDir(c, rows) };
}

/**
 * A sort in words, for a cursor pager: "Created, newest first",
 * "Cost, largest first", "Pool, A→Z". Undefined for a key no column has.
 */
export function sortInWords<Row>(columns: readonly Column<Row>[], sort: SortState): string | undefined {
  const c = columns.find((x) => x.key === sort.key);
  if (!c) return undefined;
  const name = c.label ?? (typeof c.header === "string" ? c.header : c.key);
  const kind = c.sortKind ?? (c.align === "right" ? "number" : "text");
  if (kind === "time") return `${name}, ${sort.dir === "desc" ? "newest" : "oldest"} first`;
  if (kind === "number") return `${name}, ${sort.dir === "desc" ? "largest" : "smallest"} first`;
  return `${name}, ${sort.dir === "asc" ? "A→Z" : "Z→A"}`;
}

const collator = new Intl.Collator(undefined, { numeric: true, sensitivity: "base" });

/**
 * Rows ordered by sortValue in dir: missing values (null, undefined, NaN,
 * "") last in both directions; ties keep their given order.
 */
export function sortRows<Row>(rows: readonly Row[], sortValue: (r: Row) => SortValue, dir: SortDir): Row[] {
  const sign = dir === "asc" ? 1 : -1;
  const missing = (v: SortValue) => v == null || v === "" || (typeof v === "number" && Number.isNaN(v));
  return rows
    .map((row, i) => ({ row, i, v: sortValue(row) }))
    .sort((a, b) => {
      const ma = missing(a.v);
      const mb = missing(b.v);
      if (ma || mb) return ma === mb ? a.i - b.i : ma ? 1 : -1;
      const c = typeof a.v === "number" && typeof b.v === "number" ? a.v - b.v : collator.compare(String(a.v), String(b.v));
      return c * sign || a.i - b.i;
    })
    .map((x) => x.row);
}

/**
 * Data table. Fixed layout: columns with a width keep it and the rest share
 * what is left, so wide screens stretch the text columns rather than the
 * gaps. On narrow screens it scrolls sideways inside its container with
 * the first column pinned; columns marked optional drop out first. Every
 * sortable header is a keyboard control with aria-sort.
 */
export function Table<Row>(props: TableProps<Row>) {
  const { rows, rowKey, onRowClick, selected, loading, loadingRows = 6, empty, maxHeight, dense, minWidth, footer, className, sortMode = "client" } = props;
  const [internalSort, setInternalSort] = useState<SortState | undefined>(props.defaultSort);
  const sort = props.sort ?? internalSort;
  const wrap = useRef<HTMLDivElement>(null);
  const [scrolled, setScrolled] = useState(false);
  const [wrapWidth, setWrapWidth] = useState(0);

  useEffect(() => {
    const el = wrap.current;
    if (!el) return;
    const onScroll = () => setScrolled(el.scrollLeft > 0);
    el.addEventListener("scroll", onScroll, { passive: true });
    return () => el.removeEventListener("scroll", onScroll);
  }, []);

  // Optional columns drop out when the table's container is narrow.
  useLayoutEffect(() => {
    const el = wrap.current;
    if (!el) return;
    const measure = () => setWrapWidth(el.clientWidth);
    measure();
    const ro = new ResizeObserver(measure);
    ro.observe(el);
    return () => ro.disconnect();
  }, []);

  const narrow = dropsOptional(props.columns, wrapWidth);
  const columns = useMemo(() => (narrow ? props.columns.filter((c) => !c.optional) : props.columns), [props.columns, narrow]);

  const sorted = useMemo(() => {
    if (!sort || sortMode === "server") return rows;
    // Any declared column, shown or not: a default sort by an optional
    // column holds when a narrow container drops it.
    const col = props.columns.find((c) => c.key === sort.key);
    if (!col?.sortValue) return rows;
    return sortRows(rows, col.sortValue, sort.dir);
  }, [rows, sort, sortMode, props.columns]);

  const toggleSort = (c: Column<Row>) => {
    if (!isSortable(c)) return;
    const next = nextSort(sort, c, rows);
    if (props.sort === undefined) setInternalSort(next);
    props.onSortChange?.(next);
  };
  const onHeadKey = (e: KeyboardEvent, c: Column<Row>) => {
    if (e.key === "Enter" || e.key === " ") {
      e.preventDefault();
      toggleSort(c);
    }
  };

  const floor = useMemo(() => (minWidth != null ? minWidth : tableFloor(columns)), [columns, minWidth]);

  const showSkeleton = loading && rows.length === 0;
  const showEmpty = !loading && rows.length === 0;
  const cellClass = (c: Column<Row>) =>
    [c.mono ? "mono" : "", c.wrap ? "td-wrap" : "", c.align === "right" ? "num" : "", c.lead ? "td-lead" : ""].join(" ").trim() || undefined;

  const table = (
    <div ref={wrap} className={["table-wrap", loading && rows.length > 0 ? "is-refreshing" : "", scrolled ? "is-scrolled" : "", footer ? "" : className ?? ""].join(" ").trim()} style={maxHeight ? { maxHeight } : undefined}>
      <table className={dense ? "table table-dense" : "table"} style={{ minWidth: floor }}>
        <colgroup>
          {columns.map((c) => (
            <col key={c.key} style={{ width: c.width }} />
          ))}
        </colgroup>
        <thead>
          <tr>
            {columns.map((c) => {
              const sortable = isSortable(c);
              const active = sortable && sort?.key === c.key;
              return (
                <th
                  key={c.key}
                  style={{ textAlign: c.align ?? "left" }}
                  className={[sortable ? "is-sortable" : "", active ? "is-sorted" : ""].join(" ").trim() || undefined}
                  aria-sort={sortable ? (active ? (sort!.dir === "asc" ? "ascending" : "descending") : "none") : undefined}
                  tabIndex={sortable ? 0 : undefined}
                  onClick={sortable ? () => toggleSort(c) : undefined}
                  onKeyDown={sortable ? (e) => onHeadKey(e, c) : undefined}
                >
                  <span className="th-inner">
                    {c.header}
                    {sortable && (
                      <span className={active ? "th-sort" : "th-sort th-sort-idle"} aria-hidden="true">
                        {active ? (sort!.dir === "asc" ? "↑" : "↓") : "↕"}
                      </span>
                    )}
                  </span>
                </th>
              );
            })}
          </tr>
        </thead>
        <tbody>
          {showSkeleton &&
            Array.from({ length: loadingRows }, (_, i) => (
              <tr key={`sk-${i}`} className="row-skeleton">
                {columns.map((c) => (
                  <td key={c.key} style={{ textAlign: c.align ?? "left" }}>
                    <Skeleton width={`${50 + ((i * 7 + c.key.length * 13) % 45)}%`} />
                  </td>
                ))}
              </tr>
            ))}
          {showEmpty && (
            <tr>
              <td colSpan={columns.length} className="td-empty" style={{ position: "static" }}>
                {typeof empty === "string" || empty == null ? <EmptyState compact title={empty ?? "Nothing here"} /> : empty}
              </td>
            </tr>
          )}
          {sorted.map((row) => {
            const k = rowKey(row);
            return (
              <tr
                key={k}
                className={[onRowClick ? "is-clickable" : "", selected === k ? "is-selected" : ""].join(" ").trim()}
                onClick={onRowClick ? () => onRowClick(row) : undefined}
                tabIndex={onRowClick ? 0 : undefined}
                onKeyDown={
                  onRowClick
                    ? (e) => {
                        if (e.key === "Enter") onRowClick(row);
                      }
                    : undefined
                }
              >
                {columns.map((c) => (
                  <td key={c.key} style={{ textAlign: c.align ?? "left" }} className={cellClass(c)}>
                    {c.cell(row)}
                  </td>
                ))}
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
  if (!footer) return table;
  return (
    <div className={className}>
      {table}
      {footer}
    </div>
  );
}

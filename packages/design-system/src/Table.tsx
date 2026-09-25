import { useEffect, useLayoutEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { EmptyState } from "./EmptyState.tsx";
import { Skeleton } from "./Spinner.tsx";
import { IconChevronDown, IconChevronUp } from "./icons.tsx";

/** Container width (px) under which optional columns are dropped. */
const NARROW = 1100;

export interface Column<Row> {
  key: string;
  header: ReactNode;
  /** Cell renderer. */
  cell: (row: Row) => ReactNode;
  /** Value used for sorting; omit to make the column unsortable. */
  sortValue?: (row: Row) => string | number | null | undefined;
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
  /** Dropped when the table's container is narrow (under 1100px). */
  optional?: boolean;
}

export interface SortState {
  key: string;
  dir: "asc" | "desc";
}

export interface TableProps<Row> {
  columns: Column<Row>[];
  rows: Row[];
  rowKey: (row: Row) => string;
  onRowClick?: (row: Row) => void;
  /** Currently selected row key (highlighted). */
  selected?: string | null;
  sort?: SortState;
  onSortChange?: (s: SortState) => void;
  /** Uncontrolled default sort. */
  defaultSort?: SortState;
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
  className?: string;
}

/**
 * Data table. Fixed layout: columns with a width keep it and the rest share
 * what is left, so wide screens stretch the text columns rather than the
 * gaps. On narrow screens it scrolls sideways inside its container with
 * the first column pinned; columns marked optional drop out first.
 */
export function Table<Row>(props: TableProps<Row>) {
  const { rows, rowKey, onRowClick, selected, loading, loadingRows = 6, empty, maxHeight, dense, minWidth, className } = props;
  const [internalSort, setInternalSort] = useState<SortState | undefined>(props.defaultSort);
  const sort = props.sort ?? internalSort;
  const setSort = props.onSortChange ?? setInternalSort;
  const wrap = useRef<HTMLDivElement>(null);
  const [scrolled, setScrolled] = useState(false);
  const [narrow, setNarrow] = useState(false);

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
    const measure = () => setNarrow(el.clientWidth > 0 && el.clientWidth < NARROW);
    measure();
    const ro = new ResizeObserver(measure);
    ro.observe(el);
    return () => ro.disconnect();
  }, []);

  const columns = useMemo(() => (narrow ? props.columns.filter((c) => !c.optional) : props.columns), [props.columns, narrow]);

  const sorted = useMemo(() => {
    if (!sort) return rows;
    const col = columns.find((c) => c.key === sort.key);
    if (!col?.sortValue) return rows;
    const sv = col.sortValue;
    const dir = sort.dir === "asc" ? 1 : -1;
    return [...rows].sort((a, b) => {
      const x = sv(a);
      const y = sv(b);
      if (x == null && y == null) return 0;
      if (x == null) return 1;
      if (y == null) return -1;
      if (typeof x === "number" && typeof y === "number") return (x - y) * dir;
      return String(x).localeCompare(String(y)) * dir;
    });
  }, [rows, sort, columns]);

  const toggleSort = (c: Column<Row>) => {
    if (!c.sortValue) return;
    if (sort?.key === c.key) setSort({ key: c.key, dir: sort.dir === "asc" ? "desc" : "asc" });
    else setSort({ key: c.key, dir: c.align === "right" ? "desc" : "asc" });
  };

  // A sensible floor: fixed widths plus 140px per flexible column.
  const floor = useMemo(() => {
    if (minWidth != null) return minWidth;
    let w = 0;
    for (const c of columns) w += typeof c.width === "number" ? c.width : c.width ? 120 : 140;
    return w;
  }, [columns, minWidth]);

  const showSkeleton = loading && rows.length === 0;
  const showEmpty = !loading && rows.length === 0;
  const cellClass = (c: Column<Row>) =>
    [c.mono ? "mono" : "", c.wrap ? "td-wrap" : "", c.align === "right" ? "num" : "", c.lead ? "td-lead" : ""].join(" ").trim() || undefined;

  return (
    <div ref={wrap} className={["table-wrap", loading && rows.length > 0 ? "is-refreshing" : "", scrolled ? "is-scrolled" : "", className ?? ""].join(" ").trim()} style={maxHeight ? { maxHeight } : undefined}>
      <table className={dense ? "table table-dense" : "table"} style={{ minWidth: floor }}>
        <colgroup>
          {columns.map((c) => (
            <col key={c.key} style={{ width: c.width }} />
          ))}
        </colgroup>
        <thead>
          <tr>
            {columns.map((c) => {
              const active = sort?.key === c.key;
              return (
                <th
                  key={c.key}
                  style={{ textAlign: c.align ?? "left" }}
                  className={[c.sortValue ? "is-sortable" : "", active ? "is-sorted" : ""].join(" ").trim() || undefined}
                  aria-sort={active ? (sort.dir === "asc" ? "ascending" : "descending") : undefined}
                  onClick={() => toggleSort(c)}
                >
                  <span className="th-inner">
                    {c.header}
                    {c.sortValue && (
                      <span className="th-sort" aria-hidden="true">
                        {active ? sort.dir === "asc" ? <IconChevronUp size={11} /> : <IconChevronDown size={11} /> : <IconChevronDown size={11} className="th-sort-idle" />}
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
}

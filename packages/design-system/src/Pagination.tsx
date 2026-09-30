import { Button } from "./Button.tsx";
import { formatCount } from "./format.ts";

/** Page sizes a Pagination offers unless told otherwise. */
export const PAGE_SIZES = [25, 50, 100] as const;

interface CommonProps {
  pageSize: number;
  pageSizes?: readonly number[];
  onPageSize?: (n: number) => void;
  /** A request is in flight: buttons stay put but do nothing. */
  busy?: boolean;
  /** What the rows are ("hosts", "runs", "events"). */
  noun?: string;
}

export interface CountPaginationProps extends CommonProps {
  mode: "count";
  /** 1-based. */
  page: number;
  total: number;
  onPage: (page: number) => void;
}

export interface CursorPaginationProps extends CommonProps {
  mode: "cursor";
  /** 1-based page number reached by Next/Previous from the first page. */
  page: number;
  /** Rows on this page. */
  count: number;
  hasPrev: boolean;
  hasNext: boolean;
  onFirst: () => void;
  onPrev: () => void;
  onNext: () => void;
  /** The order the pages follow ("Created, newest first"). */
  sortLabel?: string;
}

export type PaginationProps = CountPaginationProps | CursorPaginationProps;

/**
 * The page numbers a count pager shows: first, last, and the current page
 * with its neighbours, with null where pages are skipped.
 */
export function pageList(page: number, last: number): (number | null)[] {
  const want = [...new Set([1, page - 1, page, page + 1, last])].filter((p) => p >= 1 && p <= last).sort((a, b) => a - b);
  const out: (number | null)[] = [];
  want.forEach((p, i) => {
    if (i > 0 && p - want[i - 1]! > 1) out.push(null);
    out.push(p);
  });
  return out;
}

/**
 * The footer of a paged Table or EventTable. Count mode: "1–25 of 1,284",
 * numbered pages and a page size, where the server counts the whole
 * result. Cursor mode: First / Previous / Next in the current sort order,
 * where an exact total is expensive or keeps changing.
 */
export function Pagination(props: PaginationProps) {
  const { pageSize, pageSizes = PAGE_SIZES, onPageSize, busy, noun = "rows" } = props;
  const size = onPageSize ? (
    <select className="input pager-size" aria-label="Rows per page" value={pageSize} onChange={(e) => onPageSize(Number(e.target.value))}>
      {pageSizes.map((n) => (
        <option key={n} value={n}>
          {n} / page
        </option>
      ))}
    </select>
  ) : null;
  if (props.mode === "count") {
    const { page, total, onPage } = props;
    const last = Math.max(1, Math.ceil(total / pageSize));
    const from = total === 0 ? 0 : (page - 1) * pageSize + 1;
    const to = Math.min(total, page * pageSize);
    const go = (p: number) => !busy && p >= 1 && p <= last && p !== page && onPage(p);
    return (
      <nav className="pager" aria-label="Pages">
        <span className="pager-range num">
          {formatCount(from)}–{formatCount(to)} of {formatCount(total)} {noun}
        </span>
        <span className="pager-grow" />
        <Button size="sm" variant="ghost" disabled={page <= 1} onClick={() => go(page - 1)}>
          ‹ Previous
        </Button>
        <span className="pager-pages">
          {pageList(page, last).map((p, i) =>
            p == null ? (
              <span key={`gap-${i}`} className="pager-gap" aria-hidden="true">
                …
              </span>
            ) : (
              <button key={p} type="button" className={p === page ? "pager-page is-on" : "pager-page"} aria-current={p === page ? "page" : undefined} aria-label={`Page ${p}`} onClick={() => go(p)}>
                {p}
              </button>
            ),
          )}
        </span>
        <Button size="sm" variant="ghost" disabled={page >= last} onClick={() => go(page + 1)}>
          Next ›
        </Button>
        {size}
      </nav>
    );
  }
  const { page, count, hasPrev, hasNext, onFirst, onPrev, onNext, sortLabel } = props;
  const from = (page - 1) * pageSize + 1;
  return (
    <nav className="pager" aria-label="Pages">
      <span className="pager-range num">
        Page {page}
        {count > 0 ? ` · ${noun} ${formatCount(from)}–${formatCount(from + count - 1)}` : ""}
      </span>
      {sortLabel && <span className="muted">· {sortLabel}</span>}
      <span className="pager-grow" />
      {/* Past page 1 the top is always reachable, even when the page lost its previous one. */}
      <Button size="sm" variant="ghost" disabled={page <= 1 && !hasPrev} onClick={() => !busy && onFirst()}>
        « First
      </Button>
      <Button size="sm" variant="ghost" disabled={!hasPrev} onClick={() => !busy && onPrev()}>
        ‹ Previous
      </Button>
      <Button size="sm" variant="ghost" disabled={!hasNext} onClick={() => !busy && onNext()}>
        Next ›
      </Button>
      {size}
    </nav>
  );
}

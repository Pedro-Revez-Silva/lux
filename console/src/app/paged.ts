// A server-paged list: its sort, page size and where the page on screen
// was read from, and the request that reads it. Filters, tenant, sort and
// size changes start over at the first page; a request made for an older
// view is aborted, and its answer, if it lands anyway, is dropped. A
// refresh re-reads the page on screen from its first row (the page's own
// cursor), so polling never moves the reader to another page; the first
// page is re-read from the top, where new rows arrive.
import { useCallback, useMemo, useRef, useState } from "react";
import type { SortState } from "@lux/design-system";
import { useQuery, type Page, type PageParams } from "../api/index.ts";

/** Where a page is read from: the top, a cursor, or (counted lists) an offset. */
export type Nav = { kind: "first" } | { kind: "next" | "prev"; cursor: string } | { kind: "offset"; offset: number };

export interface PagedRequest extends PageParams {
  limit: number;
  offset?: number;
}

/** The request for a view at nav; a refresh (self: the page's own cursor) re-reads that page in place. */
export function pageRequest(sort: SortState, size: number, nav: Nav, self: string | undefined, pageNo: number): PagedRequest {
  const base: PagedRequest = { sort: sort.key, dir: sort.dir, limit: size };
  if (self && pageNo > 1) return { ...base, at: self };
  switch (nav.kind) {
    case "first":
      return base;
    case "offset":
      return nav.offset > 0 ? { ...base, offset: nav.offset } : base;
    case "next":
      return { ...base, next: nav.cursor };
    case "prev":
      return { ...base, prev: nav.cursor };
  }
}

export interface Paged<T> {
  rows: T[];
  loading: boolean;
  error: string | null;
  refetch: () => Promise<void>;
  sort: SortState;
  setSort: (s: SortState) => void;
  size: number;
  setSize: (n: number) => void;
  /** 1-based: counted from the offset (counted lists), else from Next/Previous. */
  page: number;
  total?: number;
  hasNext: boolean;
  hasPrev: boolean;
  first: () => void;
  next: () => void;
  prev: () => void;
  /** Counted lists: jump to a numbered page. */
  goto: (page: number) => void;
}

/**
 * view: what the list shows (tenant and filters); a change starts over.
 * fetch reads one page for a request.
 */
export function usePaged<T>(view: string, fetch: (req: PagedRequest, signal: AbortSignal) => Promise<Page<T>>, opts: { defaultSort: SortState; defaultSize: number; interval: number }): Paged<T> {
  const [sort, setSortState] = useState(opts.defaultSort);
  const [size, setSizeState] = useState(opts.defaultSize);
  const [nav, setNav] = useState<{ view: string; nav: Nav; page: number; seq: number }>({ view, nav: { kind: "first" }, page: 1, seq: 0 });
  // A view change starts over at the first page.
  const here = nav.view === view ? nav : { view, nav: { kind: "first" } as Nav, page: 1, seq: nav.seq + 1 };
  const key = `${view}|${sort.key}:${sort.dir}|${size}|${here.seq}`;
  // The page on screen's own cursor, for refreshes of that page.
  const self = useRef<{ key: string; cursor?: string }>({ key: "" });
  const fetchRef = useRef(fetch);
  fetchRef.current = fetch;
  const pageNo = here.page;
  const navNow = here.nav;
  const q = useQuery(
    `paged:${key}`,
    async (signal) => {
      const cur = self.current.key === key ? self.current.cursor : undefined;
      const res = await fetchRef.current(pageRequest(sort, size, navNow, cur, pageNo), signal);
      if (!signal.aborted) self.current = { key, cursor: res.page };
      return { key, res };
    },
    { interval: opts.interval, keep: true },
  );
  const data = q.data?.key === key ? q.data.res : undefined;
  const stale = q.data && q.data.key !== key ? q.data.res : undefined;
  const shown = data ?? stale;

  const move = useCallback((n: Nav, page: number) => setNav((o) => ({ view, nav: n, page, seq: (o.view === view ? o.seq : o.seq + 1) + 1 })), [view]);
  const counted = shown?.total != null;
  const page = data?.offset != null ? Math.floor(data.offset / size) + 1 : pageNo;
  return useMemo(
    () => ({
      rows: shown?.rows ?? [],
      loading: q.loading || (!data && q.fetching),
      error: q.error,
      refetch: q.refetch,
      sort,
      setSort: (s: SortState) => {
        setSortState(s);
        move({ kind: "first" }, 1);
      },
      size,
      setSize: (n: number) => {
        setSizeState(n);
        move({ kind: "first" }, 1);
      },
      page,
      total: shown?.total,
      hasNext: !!data?.next,
      hasPrev: !!data?.prev || (counted && page > 1),
      first: () => move({ kind: "first" }, 1),
      next: () => data?.next && move({ kind: "next", cursor: data.next }, page + 1),
      // Back to page 1 is the top of the list, where new rows arrive.
      prev: () => data?.prev && (page <= 2 ? move({ kind: "first" }, 1) : move({ kind: "prev", cursor: data.prev }, page - 1)),
      goto: (p: number) => move(p <= 1 ? { kind: "first" } : { kind: "offset", offset: (p - 1) * size }, p),
    }),
    [shown, data, q.loading, q.fetching, q.error, q.refetch, sort, size, page, counted, move],
  );
}

/** "Created, newest first": a sort in words, for a cursor pager. */
export function sortLabel(header: string, s: SortState, kind: "time" | "number" | "text"): string {
  if (kind === "time") return `${header}, ${s.dir === "desc" ? "newest" : "oldest"} first`;
  if (kind === "number") return `${header}, ${s.dir === "desc" ? "largest" : "smallest"} first`;
  return `${header}, ${s.dir === "asc" ? "A→Z" : "Z→A"}`;
}

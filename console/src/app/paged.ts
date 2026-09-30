// A server-paged list: its sort, page size and where the page on screen
// was read from, and the request that reads it. Filters, tenant, sort and
// size changes start over at the first page; a request made for an older
// view is aborted, and its answer, if it lands anyway, is dropped. A
// refresh re-reads the page on screen in place: a cursor page from its own
// cursor, a counted page at its offset. The first page is re-read from the
// top, where new rows arrive.
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import type { SortState } from "@lux/design-system";
import { useQuery, type Page, type PageParams } from "../api/index.ts";

/** Where a page is read from: the top, a cursor, or (counted lists) an offset. */
export type Nav = { kind: "first" } | { kind: "next" | "prev"; cursor: string } | { kind: "offset"; offset: number };

export interface PagedRequest extends PageParams {
  limit: number;
  offset?: number;
}

/** The request for a view at nav; a refresh of a cursor page (self: the page's own cursor) re-reads it in place. */
export function pageRequest(sort: SortState, size: number, nav: Nav, self: string | undefined, pageNo: number): PagedRequest {
  const base: PagedRequest = { sort: sort.key, dir: sort.dir, limit: size };
  if (nav.kind === "offset") return nav.offset > 0 ? { ...base, offset: nav.offset } : base;
  if (self && pageNo > 1) return { ...base, at: self };
  switch (nav.kind) {
    case "first":
      return base;
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
  /** 1-based, from navigation: Next/Previous, or the page asked for. */
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

export interface PagedOptions {
  defaultSort: SortState;
  defaultSize: number;
  interval: number;
  /** useQuery's live: the slower poll while the event stream is up (for lists that live.ts invalidates). */
  live?: number;
}

interface NavState {
  view: string;
  nav: Nav;
  page: number;
  /** Bumped by every navigation: part of the query key, so no two views or pages share one. */
  seq: number;
}

/**
 * prefix: the query key's prefix (`${prefix}:paged:…`), what invalidate()
 * matches. view: what the list shows (tenant and filters); a change starts
 * over. fetch reads one page for a request.
 */
export function usePaged<T>(prefix: string, view: string, fetch: (req: PagedRequest, signal: AbortSignal) => Promise<Page<T>>, opts: PagedOptions): Paged<T> {
  const [sort, setSortState] = useState(opts.defaultSort);
  const [size, setSizeState] = useState(opts.defaultSize);
  const [nav, setNav] = useState<NavState>({ view, nav: { kind: "first" }, page: 1, seq: 0 });
  // A view change starts over at the first page, and is stored: going back
  // to an earlier view starts over too.
  const here: NavState = nav.view === view ? nav : { view, nav: { kind: "first" }, page: 1, seq: nav.seq + 1 };
  if (here !== nav) setNav(here);
  const key = `${prefix}:paged:${view}|${sort.key}:${sort.dir}|${size}|${here.seq}`;
  // The page on screen's own cursor, from the answer shown for this key.
  const self = useRef<{ key: string; cursor?: string }>({ key: "" });
  const fetchRef = useRef(fetch);
  fetchRef.current = fetch;
  const page = here.page;
  const q = useQuery(
    key,
    async (signal) => {
      const cur = self.current.key === key ? self.current.cursor : undefined;
      return { key, res: await fetchRef.current(pageRequest(sort, size, here.nav, cur, page), signal) };
    },
    { interval: opts.interval, live: opts.live, keep: true },
  );
  // keep: until this key's answer lands, q.data is the previous key's. It
  // is shown, but its cursors belong to another view or page.
  const data = q.data?.key === key ? q.data.res : undefined;
  const shown = data ?? q.data?.res;
  if (data) self.current = { key, cursor: data.page };

  const move = useCallback((n: Nav, page: number) => setNav((o) => ({ view, nav: n, page, seq: o.seq + 1 })), [view]);
  // A page past the first that comes back empty, or (a cursor page) with
  // nothing before it, has lost its place: start over at the top.
  const lost = data != null && page > 1 && (data.rows.length === 0 || (here.nav.kind !== "offset" && !data.prev));
  useEffect(() => {
    if (lost) move({ kind: "first" }, 1);
  }, [lost, move]);
  const counted = shown?.total != null;
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

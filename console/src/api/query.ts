// A small polling query hook. One in-flight request at a time, refetch on an
// interval (paused while the tab is hidden) and on invalidate(), cancelled on
// unmount or when the key changes. No cache: pages are short-lived and the
// API is local.
import { useCallback, useEffect, useRef, useState } from "react";
import { errorText } from "./client.ts";
import { useLiveStatus } from "./live.ts";

export interface QueryState<T> {
  data: T | undefined;
  error: string | null;
  /** True until the first response (or error) arrives. */
  loading: boolean;
  /** A refetch is in flight. */
  fetching: boolean;
  refetch: () => Promise<void>;
  /** Replace the data locally (after an action returns the new object). */
  setData: (d: T) => void;
}

export interface QueryOptions {
  /** Poll interval in ms; 0 or undefined polls never. */
  interval?: number;
  /**
   * The data changes only with Run events, which invalidate it: while the
   * event stream is live, poll this slowly instead (a safety net). While it
   * is down, `interval` applies.
   */
  live?: number;
  enabled?: boolean;
  /** On a key change, keep showing the old data until the new arrives (e.g. loading another page). */
  keep?: boolean;
}

/** At most one invalidated refetch per query this often. */
const COALESCE_MS = 300;

/** Mounted queries by key, for invalidate(). */
const mounted = new Map<string, Set<() => void>>();

/** Refetch mounted queries soon: those whose key starts with `match`, or satisfies it. Coalesced per query. */
export function invalidate(match: string | ((key: string) => boolean)) {
  const hit = typeof match === "string" ? (k: string) => k.startsWith(match) : match;
  for (const [k, pokes] of mounted) if (hit(k)) pokes.forEach((p) => p());
}

export function useQuery<T>(key: string, fn: (signal: AbortSignal) => Promise<T>, opts: QueryOptions = {}): QueryState<T> {
  const { enabled = true, keep = false } = opts;
  const streaming = useLiveStatus() === "live";
  const interval = opts.interval && opts.live && streaming ? opts.live : (opts.interval ?? 0);
  const keepRef = useRef(keep);
  keepRef.current = keep;
  const [data, setData] = useState<T | undefined>(undefined);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(enabled);
  const [fetching, setFetching] = useState(false);
  const fnRef = useRef(fn);
  fnRef.current = fn;
  const ctrl = useRef<AbortController | null>(null);
  const keyRef = useRef(key);
  // Invalidation: when the last fetch started, a pending trailing refetch,
  // and whether one is owed once the in-flight fetch lands (rather than
  // aborting it, so a burst of events cannot starve a slow request).
  const last = useRef(0);
  const pending = useRef<ReturnType<typeof setTimeout> | null>(null);
  const inFlight = useRef(false);
  const owed = useRef(false);

  const run = useCallback(async () => {
    ctrl.current?.abort();
    const c = new AbortController();
    ctrl.current = c;
    if (pending.current != null) clearTimeout(pending.current);
    pending.current = null;
    last.current = Date.now();
    owed.current = false;
    inFlight.current = true;
    setFetching(true);
    try {
      const d = await fnRef.current(c.signal);
      if (c.signal.aborted) return;
      setData(d);
      setError(null);
    } catch (e) {
      if (c.signal.aborted) return;
      setError(errorText(e));
    } finally {
      if (!c.signal.aborted) {
        inFlight.current = false;
        setLoading(false);
        setFetching(false);
        if (owed.current) poke();
      }
    }
  }, []);

  const poke = useCallback(() => {
    if (document.hidden || pending.current != null) return;
    if (inFlight.current) {
      owed.current = true;
      return;
    }
    pending.current = setTimeout(
      () => {
        pending.current = null;
        void run();
      },
      Math.max(0, last.current + COALESCE_MS - Date.now()),
    );
  }, [run]);

  // Fetch on mount and key change; register for invalidate().
  useEffect(() => {
    if (!enabled) {
      setLoading(false);
      return;
    }
    if (keyRef.current !== key) {
      keyRef.current = key;
      if (!keepRef.current) {
        setData(undefined);
        setError(null);
        setLoading(true);
      }
    }
    void run();
    let pokes = mounted.get(key);
    if (!pokes) mounted.set(key, (pokes = new Set()));
    pokes.add(poke);
    return () => {
      pokes.delete(poke);
      if (pokes.size === 0) mounted.delete(key);
      if (pending.current != null) clearTimeout(pending.current);
      pending.current = null;
      owed.current = false;
      inFlight.current = false;
      ctrl.current?.abort();
    };
  }, [key, enabled, run, poke]);

  // Poll, paused while the tab is hidden; catch up when it shows again.
  useEffect(() => {
    if (!enabled) return;
    let timer: ReturnType<typeof setInterval> | undefined;
    const start = () => {
      if (interval > 0 && timer == null) timer = setInterval(() => void run(), interval);
    };
    const stop = () => {
      if (timer != null) clearInterval(timer);
      timer = undefined;
    };
    const onVis = () => {
      if (document.hidden) stop();
      else {
        void run();
        start();
      }
    };
    start();
    document.addEventListener("visibilitychange", onVis);
    return () => {
      stop();
      document.removeEventListener("visibilitychange", onVis);
    };
  }, [interval, enabled, run]);

  return { data, error, loading, fetching, refetch: run, setData };
}

// The shared clock lives in the design system, beside RelativeTime.
export { useNow } from "@lux/design-system";

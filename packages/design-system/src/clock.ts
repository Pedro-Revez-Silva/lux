import { useCallback, useSyncExternalStore } from "react";

/** One shared clock per interval, so many cells ticking cost one timer. */
const clocks = new Map<number, { now: number; listeners: Set<() => void>; timer?: ReturnType<typeof setInterval> }>();

function clock(ms: number) {
  let c = clocks.get(ms);
  if (!c) {
    c = { now: Date.now(), listeners: new Set() };
    clocks.set(ms, c);
  }
  return c;
}

/** Re-render every `ms` so relative times and counting durations stay fresh. Returns the clock's Date.now(). */
export function useNow(ms = 10_000): number {
  const c = clock(ms);
  const subscribe = useCallback(
    (cb: () => void) => {
      if (c.listeners.size === 0) {
        c.now = Date.now();
        c.timer = setInterval(() => {
          c.now = Date.now();
          c.listeners.forEach((l) => l());
        }, ms);
      }
      c.listeners.add(cb);
      return () => {
        c.listeners.delete(cb);
        if (c.listeners.size === 0) clearInterval(c.timer);
      };
    },
    [c, ms],
  );
  return useSyncExternalStore(subscribe, () => c.now, () => c.now);
}

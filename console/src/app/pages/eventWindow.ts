// The events a pool or host page keeps, newest first, each once. A refresh
// reads only the newest page; when more than a page arrived since the last
// one, the events between the two are read back, a page at a time, before
// the list is contiguous again.
import type { LifecycleEvent } from "../../api/index.ts";

/**
 * Contiguous runs of events, newest run first, each newest first. There is
 * more than one only while a gap is being read back: the events between
 * two runs exist but are not loaded yet.
 */
export interface EventWindow {
  runs: LifecycleEvent[][];
  /** The oldest run reaches the owner's first event. */
  done: boolean;
}

export const emptyWindow: EventWindow = { runs: [], done: false };

/**
 * Events kept per view. New events never grow the list past this (or past
 * what "load older" has made it): the oldest go, and "load older" reads
 * below the oldest kept.
 */
export const EVENT_CAP = 5000;

const newest = (r: LifecycleEvent[]) => r[0]!.id;
const oldest = (r: LifecycleEvent[]) => r[r.length - 1]!.id;
const size = (w: EventWindow) => w.runs.reduce((n, r) => n + r.length, 0);

/** a's events and b's, one each by id (a's copy wins: the fresher read), newest first. */
function merge(a: LifecycleEvent[], b: LifecycleEvent[]): LifecycleEvent[] {
  const ids = new Set(a.map((e) => e.id));
  return [...a, ...b.filter((e) => !ids.has(e.id))].sort((x, y) => y.id - x.id);
}

// No id lies between n and n + 1, so a run ending at the next run's
// newest + 1 has nothing between them to read.
const meets = (above: LifecycleEvent[], below: LifecycleEvent[]) => oldest(above) <= newest(below) + 1;

/** Joins run i with the runs below it while they meet: nothing lies between them. */
function joinFrom(runs: LifecycleEvent[][], i: number): LifecycleEvent[][] {
  const out = runs.slice();
  while (i + 1 < out.length && meets(out[i]!, out[i + 1]!)) out.splice(i, 2, merge(out[i]!, out[i + 1]!));
  return out;
}

/** Drops the oldest events past cap; the oldest run then no longer reaches the first event. */
function capped(w: EventWindow, cap: number): EventWindow {
  let total = size(w);
  if (total <= cap) return w;
  const runs = w.runs.slice();
  while (total > cap) {
    const last = runs[runs.length - 1]!;
    const drop = Math.min(last.length, total - cap);
    total -= drop;
    if (drop === last.length) runs.pop();
    else runs[runs.length - 1] = last.slice(0, last.length - drop);
  }
  return { runs, done: false };
}

/**
 * The owner's newest page, read at a refresh (pageSize: the size asked
 * for). Joined to the newest run when they meet; a short page holds
 * every event there is, so it closes every gap; otherwise it is a new run
 * above a gap.
 */
export function withNewest(w: EventWindow, page: LifecycleEvent[], pageSize: number, cap = EVENT_CAP): EventWindow {
  if (page.length === 0) return w;
  const limit = Math.max(cap, size(w));
  if (page.length < pageSize) return capped({ runs: [w.runs.reduce(merge, page)], done: true }, limit);
  if (w.runs.length === 0) return capped({ runs: [page], done: false }, limit);
  if (meets(page, w.runs[0]!)) return capped({ ...w, runs: joinFrom([merge(page, w.runs[0]!), ...w.runs.slice(1)], 0) }, limit);
  return capped({ ...w, runs: [page, ...w.runs] }, limit);
}

export interface Gap {
  /** Both exclusive: the newest event below the gap, the oldest above it. */
  after: number;
  before: number;
}

/**
 * The gap to read next, or null when the list is contiguous. The lowest:
 * a refresh landing mid-read adds a run above, and reading carries on
 * below what is already read rather than starting over.
 */
export function nextGap(w: EventWindow): Gap | null {
  const n = w.runs.length;
  if (n < 2) return null;
  return { after: newest(w.runs[n - 1]!), before: oldest(w.runs[n - 2]!) };
}

/**
 * A page read for gap (newest first, at most pageSize), kept at once under
 * the run above the gap; a short page is the rest of the gap, so that run
 * then meets the one below. A page for a gap the window no longer has
 * (dropped by the cap, or closed by a refresh) is ignored.
 */
export function withGap(w: EventWindow, gap: Gap, page: LifecycleEvent[], pageSize: number, cap = EVENT_CAP): EventWindow {
  const i = w.runs.findIndex((r) => oldest(r) === gap.before);
  const below = w.runs[i + 1];
  if (i < 0 || !below || newest(below) !== gap.after) return w;
  const limit = Math.max(cap, size(w));
  const runs = w.runs.slice();
  runs[i] = merge(runs[i]!, page);
  if (page.length < pageSize) runs.splice(i, 2, merge(runs[i]!, below));
  return capped({ ...w, runs: joinFrom(runs, i) }, limit);
}

/** Where "load older" reads from: below the oldest event kept, if there are older ones. */
export function olderFrom(w: EventWindow): number | undefined {
  const last = w.runs[w.runs.length - 1];
  return w.done || !last ? undefined : oldest(last);
}

/**
 * A page read below before (olderFrom). Not capped: asked for. Ignored if
 * the oldest event kept has changed since.
 */
export function withOlder(w: EventWindow, before: number, page: LifecycleEvent[], pageSize: number): EventWindow {
  const n = w.runs.length;
  if (n === 0 || oldest(w.runs[n - 1]!) !== before) return w;
  const runs = w.runs.slice();
  runs[n - 1] = merge(runs[n - 1]!, page);
  return { runs, done: page.length < pageSize };
}

/** A window and the view (pool or host) it is for. */
export interface KeyedWindow {
  key: string;
  w: EventWindow;
}

/** withOlder for the view key asked from; v unchanged if the page shows another view now. */
export function withOlderFor(v: KeyedWindow, key: string, before: number, page: LifecycleEvent[], pageSize: number): KeyedWindow {
  return v.key === key ? { key, w: withOlder(v.w, before, page, pageSize) } : v;
}

/** Every event kept, newest first. */
export function windowEvents(w: EventWindow): LifecycleEvent[] {
  return w.runs.flat();
}

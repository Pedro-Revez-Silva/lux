import { describe, expect, test } from "bun:test";
import type { LifecycleEvent } from "../../api/index.ts";
import { emptyWindow, nextGap, olderFrom, windowEvents, withGap, withNewest, withOlder, withOlderFor, type EventWindow } from "./eventWindow.ts";

const PAGE = 10;

/** An owner's events on the server, ids 1..n, read as the API reads them. */
class Stream {
  events: LifecycleEvent[] = [];
  requests = 0;
  add(n: number) {
    for (let i = 0; i < n; i++) {
      const id = this.events.length + 1;
      this.events.push({ id, type: "pool.placement", data: {}, count: 1, time: new Date(id * 1000).toISOString() });
    }
  }
  /** Newest first, both bounds exclusive, at most PAGE. */
  page(q: { before?: number; after?: number } = {}): LifecycleEvent[] {
    this.requests++;
    return this.events
      .filter((e) => (q.before == null || e.id < q.before) && (q.after == null || e.id > q.after))
      .sort((a, b) => b.id - a.id)
      .slice(0, PAGE);
  }
}

/** What the page does on a poll: read the newest page, then fill gaps until none is left. */
function refresh(s: Stream, w: EventWindow): EventWindow {
  w = withNewest(w, s.page(), PAGE);
  return fill(s, w);
}

/** Reads gaps until none is left; a read that makes no progress fails rather than spins. */
function fill(s: Stream, w: EventWindow, cap?: number): EventWindow {
  for (let gap = nextGap(w), reads = 0; gap; gap = nextGap(w)) {
    if (++reads > 100) throw new Error("gap reads make no progress");
    w = withGap(w, gap, s.page(gap), PAGE, cap);
  }
  return w;
}

function loadOlder(s: Stream, w: EventWindow): EventWindow {
  const before = olderFrom(w);
  return before == null ? w : withOlder(w, before, s.page({ before }), PAGE);
}

const ids = (w: EventWindow) => windowEvents(w).map((e) => e.id);
const range = (hi: number, lo: number) => Array.from({ length: hi - lo + 1 }, (_, i) => hi - i);

describe("eventWindow", () => {
  test("a refresh that overlaps what is kept reads nothing more", () => {
    const s = new Stream();
    s.add(25);
    let w = refresh(s, emptyWindow);
    s.add(3);
    s.requests = 0;
    w = refresh(s, w);
    expect(s.requests).toBe(1);
    expect(ids(w)).toEqual(range(28, 16));
    expect(nextGap(w)).toBeNull();
    // Nothing new: the poll is the one request.
    w = refresh(s, w);
    expect(s.requests).toBe(2);
  });

  test("more than a page arrived: the gap is read back, exactly", () => {
    const s = new Stream();
    s.add(25);
    let w = refresh(s, emptyWindow);
    s.add(14);
    w = withNewest(w, s.page(), PAGE);
    expect(nextGap(w)).toEqual({ after: 25, before: 30 });
    w = fill(s, w);
    expect(ids(w)).toEqual(range(39, 16));
  });

  test("a full newest page ending just above what is kept joins it: no gap, no read", () => {
    const s = new Stream();
    s.add(25);
    let w = refresh(s, emptyWindow);
    s.add(PAGE);
    s.requests = 0;
    w = withNewest(w, s.page(), PAGE);
    expect(nextGap(w)).toBeNull();
    expect(ids(w)).toEqual(range(35, 16));
    expect(s.requests).toBe(1);
  });

  test("a full gap page ending just above the run below joins it: no empty read after", () => {
    const s = new Stream();
    s.add(25);
    let w = refresh(s, emptyWindow);
    s.add(2 * PAGE);
    w = withNewest(w, s.page(), PAGE);
    const gap = nextGap(w)!;
    expect(gap).toEqual({ after: 25, before: 36 });
    s.requests = 0;
    w = withGap(w, gap, s.page(gap), PAGE);
    expect(nextGap(w)).toBeNull();
    expect(ids(w)).toEqual(range(45, 16));
    expect(s.requests).toBe(1);
  });

  test("a gap larger than a page is read a page at a time, each kept as it arrives", () => {
    const s = new Stream();
    s.add(25);
    let w = refresh(s, emptyWindow);
    s.add(35);
    w = withNewest(w, s.page(), PAGE);
    const seen: number[] = [];
    for (let gap = nextGap(w); gap && seen.length < 10; gap = nextGap(w)) {
      seen.push(gap.before);
      w = withGap(w, gap, s.page(gap), PAGE);
    }
    // 51..60 on the page; 26..50 read below it: 41..50, 31..40, 26..30.
    expect(seen).toEqual([51, 41, 31]);
    expect(ids(w)).toEqual(range(60, 16));
  });

  test("the stream advancing mid-read: reading carries on where it was", () => {
    const s = new Stream();
    s.add(25);
    let w = refresh(s, emptyWindow);
    s.add(35);
    w = withNewest(w, s.page(), PAGE);
    const first = nextGap(w)!;
    w = withGap(w, first, s.page(first), PAGE);
    const second = nextGap(w)!;
    const inFlight = s.page(second);
    // A poll lands while the second page is in flight: 40 more, another gap above.
    s.add(40);
    w = withNewest(w, s.page(), PAGE);
    expect(nextGap(w)).toEqual(second);
    w = withGap(w, second, inFlight, PAGE);
    w = fill(s, w);
    expect(ids(w)).toEqual(range(100, 16));
  });

  test("burst, then load older: every event once, in order", () => {
    const s = new Stream();
    s.add(110);
    let w = refresh(s, emptyWindow);
    s.add(103);
    w = refresh(s, w);
    while (olderFrom(w) != null) w = loadOlder(s, w);
    expect(ids(w)).toEqual(range(213, 1));
    expect(w.done).toBe(true);
  });

  test("a short newest page holds every event: it closes whatever gap there is", () => {
    const s = new Stream();
    s.add(5);
    let w = refresh(s, emptyWindow);
    expect(w.done).toBe(true);
    s.add(3);
    w = refresh(s, w);
    expect(ids(w)).toEqual(range(8, 1));
    expect(olderFrom(w)).toBeUndefined();
  });

  test("a refresh keeps the fresher copy of an event (its count moved)", () => {
    const s = new Stream();
    s.add(12);
    let w = refresh(s, emptyWindow);
    s.events[11] = { ...s.events[11]!, count: 4 };
    w = refresh(s, w);
    expect(windowEvents(w)[0]!.count).toBe(4);
  });

  test("the cap drops the oldest; load older reads below the oldest kept", () => {
    const s = new Stream();
    s.add(30);
    let w = refresh(s, emptyWindow);
    const cap = 25;
    for (let i = 0; i < 4; i++) {
      s.add(PAGE - 2);
      w = withNewest(w, s.page(), PAGE, cap);
    }
    expect(ids(w)).toEqual(range(62, 38));
    s.add(40);
    w = withNewest(w, s.page(), PAGE, cap);
    w = fill(s, w, cap);
    expect(ids(w)).toEqual(range(102, 78));
    expect(olderFrom(w)).toBe(78);
    w = loadOlder(s, w);
    expect(ids(w)).toEqual(range(102, 68));
    // A refresh with nothing new does not undo what was asked for.
    w = withNewest(w, s.page(), PAGE, cap);
    expect(ids(w)).toHaveLength(35);
  });

  test("a gap wholly past the cap is dropped, and the list is contiguous again", () => {
    const s = new Stream();
    s.add(20);
    let w = refresh(s, emptyWindow);
    s.add(100);
    w = withNewest(w, s.page(), PAGE, 15);
    const gap = nextGap(w)!;
    w = withGap(w, gap, s.page(gap), PAGE, 15);
    expect(nextGap(w)).toBeNull();
    expect(ids(w)).toEqual(range(120, 106));
    // A page for the gap that is gone changes nothing.
    expect(withGap(w, gap, s.page(gap), PAGE, 15)).toBe(w);
  });

  test("an older page read for one view does not land in another", () => {
    const a = new Stream();
    a.add(25);
    const aw = refresh(a, emptyWindow);
    const before = olderFrom(aw)!;
    const aOlder = a.page({ before });
    // B's oldest kept is the same id as A's, so only the key tells them apart.
    const b = new Stream();
    b.add(70);
    const bView = { key: "B", w: withNewest(emptyWindow, b.page({ before: before + PAGE }), PAGE) };
    expect(olderFrom(bView.w)).toBe(before);
    expect(withOlderFor(bView, "A", before, aOlder, PAGE)).toBe(bView);
    expect(ids(withOlderFor({ key: "A", w: aw }, "A", before, aOlder, PAGE).w)).toEqual(range(25, 6));
  });
});

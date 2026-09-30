import { afterAll, beforeAll, expect, test } from "bun:test";
import { GlobalRegistrator } from "@happy-dom/global-registrator";
import { act } from "react";
import { createRoot } from "react-dom/client";

let useQuery: typeof import("./query.ts").useQuery;
let invalidate: typeof import("./query.ts").invalidate;
beforeAll(async () => {
  GlobalRegistrator.register();
  ({ useQuery, invalidate } = await import("./query.ts"));
  (globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;
});
afterAll(async () => {
  await GlobalRegistrator.unregister();
});

const sleep = (ms: number) => act(() => new Promise<void>((r) => setTimeout(r, ms)));

test("a poll tick while a fetch is in flight is skipped, not an abort: a slow request still lands", async () => {
  const calls: { signal: AbortSignal; resolve: (v: string) => void }[] = [];
  let data: string | undefined;
  function Probe() {
    data = useQuery("slow", (signal) => new Promise<string>((resolve) => calls.push({ signal, resolve })), { interval: 20 }).data;
    return null;
  }
  const el = document.createElement("div");
  const root = createRoot(el);
  await act(async () => root.render(<Probe />));
  try {
    expect(calls.length).toBe(1);
    // Several intervals pass with the first request unanswered.
    await sleep(200);
    expect(calls.length).toBe(1);
    expect(calls[0]!.signal.aborted).toBe(false);
    // The tab becoming visible again is a tick too: skipped, not an abort.
    await act(async () => void document.dispatchEvent(new Event("visibilitychange")));
    expect(calls.length).toBe(1);
    expect(calls[0]!.signal.aborted).toBe(false);
    await act(async () => calls[0]!.resolve("slow answer"));
    expect(data).toBe("slow answer");
    // Once it has landed, polling resumes.
    await sleep(200);
    expect(calls.length).toBeGreaterThan(1);
  } finally {
    await act(async () => root.unmount());
  }
  // Unmount still aborts the one in flight.
  expect(calls.at(-1)!.signal.aborted).toBe(true);
});

// Mounts a query whose fetch takes `ms`, pokes it every 25 ms for `window`
// ms, and returns the start times of the fetches.
async function pokeBurst(key: string, ms: number, window: number): Promise<number[]> {
  const starts: number[] = [];
  function Probe() {
    useQuery(key, () => {
      starts.push(Date.now());
      return new Promise<string>((r) => setTimeout(() => r("ok"), ms));
    });
    return null;
  }
  const root = createRoot(document.createElement("div"));
  await act(async () => root.render(<Probe />));
  const poker = setInterval(() => invalidate(key), 25);
  try {
    await sleep(window);
  } finally {
    clearInterval(poker);
    await act(async () => root.unmount());
  }
  return starts;
}

test("invalidations space a slow fetch by twice its duration, so a burst keeps it at most half busy", async () => {
  // 400 ms fetches over 2 s: starts at about 0, 800, 1600 ms. Spaced only by
  // the 300 ms coalesce, one would start as each landed (5 or 6).
  const starts = await pokeBurst("burst-slow", 400, 2000);
  expect(starts.length).toBeGreaterThanOrEqual(2);
  expect(starts.length).toBeLessThanOrEqual(3);
  for (let i = 1; i < starts.length; i++) expect(starts[i]! - starts[i - 1]!).toBeGreaterThanOrEqual(780);
});

test("invalidations of a fast fetch are still coalesced to one per 300 ms", async () => {
  const starts = await pokeBurst("burst-fast", 0, 1400);
  expect(starts.length).toBeGreaterThanOrEqual(4);
  expect(starts.length).toBeLessThanOrEqual(5);
  for (let i = 1; i < starts.length; i++) expect(starts[i]! - starts[i - 1]!).toBeGreaterThanOrEqual(290);
});

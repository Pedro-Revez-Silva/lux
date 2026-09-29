import { afterAll, beforeAll, expect, test } from "bun:test";
import { GlobalRegistrator } from "@happy-dom/global-registrator";
import { act } from "react";
import { createRoot } from "react-dom/client";
import { EVENTS_PAGE, type EventRange, type LifecycleEvent } from "../../api/index.ts";

// The page's imports (the router) touch window at load: register the DOM first.
let InfraEvents: typeof import("./InfraEvents.tsx").InfraEvents;
beforeAll(async () => {
  GlobalRegistrator.register();
  ({ InfraEvents } = await import("./InfraEvents.tsx"));
  (globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;
});
afterAll(async () => {
  await GlobalRegistrator.unregister();
});

/** A full page of events, newest first, ids hi down to hi - EVENTS_PAGE + 1. */
const fullPage = (hi: number): LifecycleEvent[] =>
  Array.from({ length: EVENTS_PAGE }, (_, i) => ({ id: hi - i, type: "pool.placement", data: {}, count: 1, time: new Date((hi - i) * 1000).toISOString() }));

test("a Load older response for the view left behind is dropped: the new view's window is untouched", async () => {
  // Pool A's older page is held until after the switch to pool B.
  let releaseOlder!: (p: LifecycleEvent[]) => void;
  let olderSignal: AbortSignal | undefined;
  let olderAsked = false;
  const pageA = (q: EventRange, s?: AbortSignal) => {
    if (q.before == null) return Promise.resolve(fullPage(2 * EVENTS_PAGE));
    olderAsked = true;
    olderSignal = s;
    return new Promise<LifecycleEvent[]>((r) => (releaseOlder = r));
  };
  const pageB = (q: EventRange) => Promise.resolve(q.before == null ? fullPage(7 * EVENTS_PAGE) : []);

  const el = document.createElement("div");
  document.body.appendChild(el);
  const root = createRoot(el);
  const text = () => el.textContent ?? "";
  try {
    await act(async () => root.render(<InfraEvents queryKey="pool:A" page={pageA} interval={0} subtitle="s" />));
    expect(text()).toContain(`${EVENTS_PAGE} events`);
    const button = [...el.querySelectorAll("button")].find((b) => b.textContent?.includes("Load older")) as HTMLButtonElement;
    await act(async () => button.click());
    expect(olderAsked).toBe(true);

    await act(async () => root.render(<InfraEvents queryKey="pool:B" page={pageB} interval={0} subtitle="s" />));
    expect(text()).toContain(`${EVENTS_PAGE} events`);

    // A's page lands anyway (a fetch that ignores its signal).
    await act(async () => releaseOlder(fullPage(EVENTS_PAGE)));
    expect(text()).toContain(`${EVENTS_PAGE} events`);
    const ids = [...el.querySelectorAll("tbody tr")].map((tr) => Number(tr.querySelector("td")?.textContent));
    expect([ids[0], ids[ids.length - 1]]).toEqual([7 * EVENTS_PAGE, 6 * EVENTS_PAGE + 1]);
    // And the request itself was cancelled at the switch.
    expect(olderSignal?.aborted).toBe(true);
  } finally {
    await act(async () => root.unmount());
    el.remove();
  }
});

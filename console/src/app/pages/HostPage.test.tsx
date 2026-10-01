import { afterAll, beforeAll, expect, test } from "bun:test";
import { GlobalRegistrator } from "@happy-dom/global-registrator";
import { act } from "react";
import { createRoot } from "react-dom/client";
import type { Host, LifecycleEvent } from "../../api/index.ts";

// The page's imports (the router) touch window at load: register the DOM first.
let HostPage: typeof import("./HostPage.tsx").HostPage;
let ScopeProvider: typeof import("../scope.tsx").ScopeProvider;
let ToastProvider: typeof import("@lux/design-system").ToastProvider;
let formatTimestamp: typeof import("@lux/design-system").formatTimestamp;
let fakeApi: typeof import("../testing.ts").fakeApi;
let hostStages: typeof import("./hostStages.ts").hostStages;
let api: typeof import("../../api/index.ts");
let setSearchParams: typeof import("../router.tsx").setSearchParams;
beforeAll(async () => {
  GlobalRegistrator.register();
  ({ HostPage } = await import("./HostPage.tsx"));
  ({ ScopeProvider } = await import("../scope.tsx"));
  ({ ToastProvider, formatTimestamp } = await import("@lux/design-system"));
  ({ fakeApi } = await import("../testing.ts"));
  ({ hostStages } = await import("./hostStages.ts"));
  api = await import("../../api/index.ts");
  ({ setSearchParams } = await import("../router.tsx"));
  (globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;
});
afterAll(async () => {
  await GlobalRegistrator.unregister();
});

const sleep = (ms: number) => act(() => new Promise<void>((r) => setTimeout(r, ms)));

const T0 = Date.parse("2026-09-01T12:00:00Z");
const iso = (s: number) => new Date(T0 + s * 1000).toISOString();

function host(over: Partial<Host> = {}): Host {
  const times = { created: null, provisionRequested: null, provisioned: null, registered: null, firstPlacement: null, lastPlacementEnded: null, drainRequested: null, terminateRequested: null, terminated: null, lost: null };
  return {
    id: "h1",
    name: "host-1",
    tenant: "acme",
    pool: "burst",
    state: "ready",
    draining: false,
    labels: {},
    capacity: { cpus: 4, memory: 1024, disk: 1024, runs: 4 },
    allocated: {},
    versions: {},
    platform: false,
    liveRuns: 0,
    ...over,
    times: { ...times, created: iso(0), ...over.times },
  };
}

const events = (from: number, n: number): LifecycleEvent[] => Array.from({ length: n }, (_, i) => ({ id: from - i, type: "host.placement_assigned", data: { run: `r${from - i}` }, count: 1, time: iso(from - i) }));

/** A ready host, registered 10s after it was created. */
const registered = (over: Partial<Host> = {}) => host({ times: { registered: iso(10) } as Host["times"], ...over });

const launchFailed = (over: Partial<Host> = {}) =>
  host({ state: "terminated", launch: { outcome: "failed", requestedAt: iso(0), finishedAt: iso(5), error: "InsufficientInstanceCapacity" }, times: { provisionRequested: iso(0) } as Host["times"], ...over });

/** A request path's query, as an object. */
const query = (path: string) => Object.fromEntries(new URLSearchParams(path.split("?")[1]));

/** Clicks the card's Next page button. */
async function clickNext(card: Element) {
  const next = [...card.querySelectorAll("button")].find((b) => b.textContent?.includes("Next")) as HTMLButtonElement;
  await act(async () => next.click());
  await sleep(50);
}

/**
 * The host page, signed in as role, with a fake API: the host, and its
 * events as cursor pages of 50. url is the page's (its ?tenant= narrows an
 * operator).
 */
async function render(h: Host, role: "tenant" | "operator" = "tenant", url = "http://localhost/hosts/h1") {
  api.signIn("k");
  api.setRole(role);
  const fake = fakeApi((path) => {
    if (path.startsWith("/v1/hosts/h1/events")) {
      return query(path).next ? { events: events(50, 50), prev: "p2", page: "s2" } : { events: events(100, 50), next: "n1", page: "s1" };
    }
    if (path.startsWith("/v1/hosts/h1/history")) return { from: iso(0), to: iso(60), resolution: 60, samples: [] };
    if (path.startsWith("/v1/hosts/h1/cost")) return { hostId: "h1", from: iso(0), to: iso(60), basis: "list", hours: [] };
    if (path.startsWith("/v1/hosts/h1")) return h;
    if (path.startsWith("/v1/runs")) return { runs: [] };
    return {};
  });
  (window as unknown as { happyDOM: { setURL: (u: string) => void } }).happyDOM.setURL(url);
  const el = document.createElement("div");
  document.body.appendChild(el);
  const root = createRoot(el);
  await act(async () =>
    root.render(
      <ToastProvider>
        <ScopeProvider>
          <HostPage id="h1" />
        </ScopeProvider>
      </ToastProvider>,
    ),
  );
  await sleep(50);
  return {
    el,
    fake,
    eventCalls: () => fake.calls.filter((c) => c.startsWith("/v1/hosts/h1/events")),
    eventsCard: () => [...el.querySelectorAll(".card")].find((c) => c.querySelector(".card-title")?.textContent === "Events"),
    done: async () => {
      await act(async () => root.unmount());
      el.remove();
      fake.restore();
      api.signOut();
    },
  };
}

async function pagesThroughEvents(h: Host) {
  const p = await render(h);
  try {
    // The first page: sorted by time, newest first, 50 at a time; no
    // before/after (the old unpaged reads).
    expect(query(p.eventCalls()[0]!)).toEqual({ sort: "time", dir: "desc", limit: "50" });
    const card = p.eventsCard()!;
    expect(card).toBeDefined();
    expect(card.querySelectorAll("tbody tr").length).toBe(50);
    expect(card.textContent).toContain("Page 1");
    expect(card.textContent).toContain("Time, newest first");
    expect(card.textContent).not.toContain("Load older");
    // Next reads the next page by its cursor, in the same sort.
    await clickNext(card);
    expect(query(p.eventCalls().at(-1)!)).toEqual({ sort: "time", dir: "desc", limit: "50", next: "n1" });
    expect(card.textContent).toContain("Page 2");
    expect(card.querySelector("tbody tr")?.textContent).toContain(formatTimestamp(iso(50)));
  } finally {
    await p.done();
  }
}

test("a host's events are server-sorted cursor pages", async () => {
  await pagesThroughEvents(registered());
});

test("a host whose launch failed pages its events the same way", async () => {
  await pagesThroughEvents(launchFailed());
});

async function noEventsAsTenant(h: Host) {
  const p = await render(h);
  try {
    expect(p.fake.calls.some((c) => c.startsWith("/v1/hosts/h1"))).toBe(true);
    expect(p.el.querySelector(".page-title")).not.toBeNull();
    expect(p.eventsCard()).toBeUndefined();
    expect(p.eventCalls()).toEqual([]);
  } finally {
    await p.done();
  }
}

test("a tenant sees no events of a platform host and reads none", async () => {
  await noEventsAsTenant(registered({ platform: true, tenant: "" }));
});

test("a tenant sees no events of a platform host whose launch failed and reads none", async () => {
  await noEventsAsTenant(launchFailed({ platform: true, tenant: "" }));
});

test("an operator narrowed to a tenant reads the host's events as that tenant; a scope change starts over at page 1", async () => {
  const p = await render(registered(), "operator", "http://localhost/hosts/h1?tenant=acme");
  try {
    expect(query(p.eventCalls()[0]!)).toEqual({ tenant: "acme", sort: "time", dir: "desc", limit: "50" });
    const card = p.eventsCard()!;
    await clickNext(card);
    expect(query(p.eventCalls().at(-1)!)).toEqual({ tenant: "acme", sort: "time", dir: "desc", limit: "50", next: "n1" });
    expect(card.textContent).toContain("Page 2");
    const before = p.eventCalls().length;
    await act(async () => setSearchParams({ tenant: "globex" }));
    await sleep(50);
    expect(p.eventCalls().slice(before).map(query)).toEqual([{ tenant: "globex", sort: "time", dir: "desc", limit: "50" }]);
    expect(p.eventsCard()!.textContent).toContain("Page 1");
  } finally {
    await p.done();
  }
});

test("Details does not sort (the server has no index for it); Type asks the server for its key", async () => {
  const p = await render(registered());
  try {
    const header = (name: string) => [...p.eventsCard()!.querySelectorAll("th")].find((t) => t.textContent?.startsWith(name)) as HTMLElement;
    const details = header("Details");
    expect(details.hasAttribute("aria-sort")).toBe(false);
    const before = p.eventCalls().length;
    await act(async () => details.click());
    await sleep(50);
    expect(p.eventCalls().slice(before).map(query).filter((q) => q.sort !== "time")).toEqual([]);
    await act(async () => header("Type").click());
    await sleep(50);
    expect(query(p.eventCalls().at(-1)!)).toEqual({ sort: "type", dir: "asc", limit: "50" });
    expect(header("Type").getAttribute("aria-sort")).toBe("ascending");
  } finally {
    await p.done();
  }
});

test("an ended host's last stage is a point where it ended; a live host's is in progress", () => {
  const ended = hostStages(host({ state: "terminated", times: { registered: iso(10), terminateRequested: iso(100), terminated: iso(130) } as Host["times"] }));
  expect(ended.map((s) => [s.key, s.start, s.end ?? null, !!s.point])).toEqual([
    ["created", T0, T0 + 10_000, false],
    ["registered", T0 + 10_000, T0 + 100_000, false],
    ["terminateRequested", T0 + 100_000, T0 + 130_000, false],
    ["terminated", T0 + 130_000, null, true],
  ]);
  const lost = hostStages(host({ state: "lost", times: { registered: iso(10), lost: iso(40) } as Host["times"] }));
  expect(lost.at(-1)).toMatchObject({ key: "lost", start: T0 + 40_000, point: true });
  const live = hostStages(registered());
  expect(live.at(-1)).toMatchObject({ key: "registered", start: T0 + 10_000, end: null });
  expect(live.some((s) => s.point)).toBe(false);
});

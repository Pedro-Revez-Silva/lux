import { afterAll, beforeAll, expect, test } from "bun:test";
import { GlobalRegistrator } from "@happy-dom/global-registrator";
import { act } from "react";
import { createRoot } from "react-dom/client";
import type { Host, HostCost, LifecycleEvent } from "../../api/index.ts";

// The page's imports (the router) touch window at load: register the DOM first.
let HostPage: typeof import("./HostPage.tsx").HostPage;
let ScopeProvider: typeof import("../scope.tsx").ScopeProvider;
let ToastProvider: typeof import("@lux/design-system").ToastProvider;
let formatTimestamp: typeof import("@lux/design-system").formatTimestamp;
let fakeApi: typeof import("../testing.ts").fakeApi;
let until: typeof import("../testing.ts").until;
let hostStages: typeof import("./hostStages.ts").hostStages;
let api: typeof import("../../api/index.ts");
let setSearchParams: typeof import("../router.tsx").setSearchParams;
beforeAll(async () => {
  GlobalRegistrator.register();
  ({ HostPage } = await import("./HostPage.tsx"));
  ({ ScopeProvider } = await import("../scope.tsx"));
  ({ ToastProvider, formatTimestamp } = await import("@lux/design-system"));
  ({ fakeApi, until } = await import("../testing.ts"));
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
 * operator); events are on the Events tab (a launch-failed host has no tabs).
 */
async function render(h: Host, role: "tenant" | "operator" = "tenant", url = "http://localhost/hosts/h1?tab=events", cost?: HostCost | ((path: string) => HostCost)) {
  api.signIn("k");
  api.setRole(role);
  const fake = fakeApi((path) => {
    if (path.startsWith("/v1/hosts/h1/events")) {
      return query(path).next ? { events: events(50, 50), prev: "p2", page: "s2" } : { events: events(100, 50), next: "n1", page: "s1" };
    }
    if (path.startsWith("/v1/hosts/h1/history")) return { from: iso(0), to: iso(60), resolution: 60, samples: [] };
    if (path.startsWith("/v1/hosts/h1/cost")) return (typeof cost === "function" ? cost(path) : cost) ?? { hostId: "h1", from: iso(0), to: iso(60), basis: "list", hours: [] };
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

const cardTitles = (el: Element) => [...el.querySelectorAll(".card-title")].map((t) => t.textContent);
const tab = (el: Element, name: string) => [...el.querySelectorAll<HTMLButtonElement>('[role="tab"]')].find((b) => b.textContent?.startsWith(name))!;

test("the host's tabs: Overview by default; each tab shows its own cards and ?tab= follows", async () => {
  const p = await render(registered(), "tenant", "http://localhost/hosts/h1");
  try {
    expect(tab(p.el, "Overview").getAttribute("aria-selected")).toBe("true");
    expect(cardTitles(p.el)).toEqual(["Details", "Lifecycle", "Live placements"]);
    // Nothing of another tab is mounted, so its reads wait for it.
    expect(p.eventCalls()).toEqual([]);
    for (const [name, key, titles] of [
      // No runner samples in the fake history: no Runner cards.
      ["Metrics", "metrics", ["CPU", "Memory", "Disk", "Placements"]],
      // A tenant's own host in its own pool: its host cost, unallocated included (no rate periods: those are the operators').
      ["Cost", "cost", ["Host cost per hour"]],
      ["Runs", "runs", ["Recent runs on this host"]],
      ["Events", "events", ["Events"]],
    ] as const) {
      await act(async () => tab(p.el, name).click());
      await until(() => JSON.stringify(cardTitles(p.el)) === JSON.stringify(titles), `the ${name} tab's cards`);
      expect(new URLSearchParams(location.search).get("tab")).toBe(key);
      expect(tab(p.el, name).getAttribute("aria-selected")).toBe("true");
      expect(cardTitles(p.el)).toEqual([...titles]);
    }
    await act(async () => tab(p.el, "Overview").click());
    expect(new URLSearchParams(location.search).get("tab")).toBeNull();
  } finally {
    await p.done();
  }
});

test("a tenant on a platform host: Cost and Events are disabled tabs, and ?tab=cost reads as Overview", async () => {
  const p = await render(registered({ platform: true, tenant: "" }), "tenant", "http://localhost/hosts/h1?tab=cost");
  try {
    expect(tab(p.el, "Cost").disabled).toBe(true);
    expect(tab(p.el, "Events").disabled).toBe(true);
    expect(tab(p.el, "Overview").getAttribute("aria-selected")).toBe("true");
    expect(tab(p.el, "Cost").getAttribute("aria-selected")).toBe("false");
    expect(cardTitles(p.el)).toEqual(["Details", "Lifecycle", "Live placements"]);
    expect(p.fake.calls.some((c) => c.startsWith("/v1/hosts/h1/cost"))).toBe(false);
  } finally {
    await p.done();
  }
});

test("an operator on a platform host's Events who narrows to a tenant reads as Overview and reads no events as the tenant", async () => {
  const p = await render(registered({ platform: true, tenant: "" }), "operator", "http://localhost/hosts/h1?tab=events");
  try {
    expect(p.eventsCard()).toBeDefined();
    await act(async () => setSearchParams({ tenant: "acme" }));
    await until(() => tab(p.el, "Events").disabled, "the Events tab to disable");
    expect(new URLSearchParams(location.search).get("tab")).toBe("events");
    expect(tab(p.el, "Overview").getAttribute("aria-selected")).toBe("true");
    expect(cardTitles(p.el)).toEqual(["Details", "Lifecycle", "Live placements"]);
    expect(p.eventCalls().filter((c) => query(c).tenant)).toEqual([]);
  } finally {
    await p.done();
  }
});

test("an unknown ?tab= reads as Overview", async () => {
  const p = await render(registered(), "tenant", "http://localhost/hosts/h1?tab=nope");
  try {
    expect(tab(p.el, "Overview").getAttribute("aria-selected")).toBe("true");
    expect(cardTitles(p.el)).toContain("Details");
  } finally {
    await p.done();
  }
});

test("an operator narrowed to a tenant reads the host's events as that tenant; a scope change starts over at page 1", async () => {
  const p = await render(registered(), "operator", "http://localhost/hosts/h1?tenant=acme&tab=events");
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

const HOUR0 = "2026-09-01T12:00:00.000Z";
const gp3 = { type: "gp3", sizeGiB: 100, iops: 3000, throughputMiBps: 125 };
/** A host's cost as luxd answers an operator: an hour of each family, its rate periods per family. */
const opCost: HostCost = {
  hostId: "h1",
  from: HOUR0,
  to: "2026-09-01T13:00:00.000Z",
  basis: "list",
  hours: [
    { hour: HOUR0, family: "block-storage", currency: "USD", allocated: "0.003", unallocated: "0.008" },
    { hour: HOUR0, family: "compute", currency: "USD", allocated: "0.1", unallocated: "0.3" },
  ],
  rates: [
    { family: "block-storage", from: HOUR0, perHour: "0.011452055", currency: "USD", source: "ec2-ebs-pricing", details: { volumes: [{ ...gp3, assumed: true }], prices: { gp3: { currency: "USD", perGBMonth: "0.0836" } }, hoursPerMonth: 730 } },
    { family: "compute", from: HOUR0, perHour: "0.4", currency: "USD", source: "ec2-pricing" },
  ],
};
/** The same host as luxd answers a tenant on a platform pool's host: no unallocated, no rates. */
const runsOnlyCost: HostCost = { ...opCost, rates: undefined, hours: opCost.hours.map(({ unallocated: _, ...h }) => h) };
/** A range with no hours. */
const noHours: HostCost = { hostId: "h1", from: HOUR0, to: "2026-09-01T13:00:00.000Z", basis: "list", hours: [] };
const ec2Host = (over: Partial<Host> = {}) => registered({ times: { provisionRequested: iso(0), registered: iso(10) } as Host["times"], volumes: [gp3], ...over });
const tiles = (el: Element) => [...el.querySelectorAll(".host-cost .stat")].map((s) => [s.querySelector(".stat-label")?.textContent, s.querySelector(".stat-value")?.textContent]);

test("host Cost tab, operator: tiles with unallocated, a four-series chart and rate periods per family with block storage's details", async () => {
  const p = await render(ec2Host({ platform: true, tenant: "" }), "operator", "http://localhost/hosts/h1?tab=cost", opCost);
  try {
    await until(() => tiles(p.el).length === 5, "the five tiles");
    expect(tiles(p.el)).toEqual([
      ["Host cost (24h)", "$0.41"],
      ["Compute", "$0.40"],
      ["Block storage", "$0.01"],
      ["Unallocated", "$0.31"],
      ["Utilisation", "25%"],
    ]);
    expect(cardTitles(p.el)).toEqual(["Host cost per hour", "Rate periods"]);
    const legend = [...p.el.querySelectorAll(".tschart-legend-label")].map((l) => l.textContent);
    expect(legend).toEqual(["Compute · runs", "Compute · unallocated", "Block storage · runs", "Block storage · unallocated"]);
    const rates = [...p.el.querySelectorAll(".card")].find((c) => c.querySelector(".card-title")?.textContent === "Rate periods")!;
    const rows = [...rates.querySelectorAll("tbody tr")].map((tr) => [...tr.querySelectorAll("td")].map((td) => td.textContent));
    expect(rows.map((r) => [r[0], r[2], r[3]])).toEqual([
      ["Block storage", "$0.0115/h", "EBS list price100 GiB gp3 · 3000 IOPS · 125 MiB/s · assumed"],
      ["Compute", "$0.40/h", "on-demand"],
    ]);
  } finally {
    await p.done();
  }
});

test("host Cost tab: a non-zero share under 1% reads <1%, never 0%", async () => {
  const busy: HostCost = { ...opCost, hours: [{ hour: HOUR0, family: "compute", currency: "USD", allocated: "9.99", unallocated: "0.01" }] };
  const p = await render(ec2Host({ platform: true, tenant: "" }), "operator", "http://localhost/hosts/h1?tab=cost", busy);
  try {
    await until(() => tiles(p.el).length === 5, "the five tiles");
    const unit = (label: string) => [...p.el.querySelectorAll(".host-cost .stat")].find((s) => s.querySelector(".stat-label")?.textContent === label)!.querySelector(".stat-unit")!.textContent;
    expect(unit("Unallocated")).toBe("<1% of host cost · no Run reserved it");
    expect(tiles(p.el)[4]).toEqual(["Utilisation", "100%"]);
  } finally {
    await p.done();
  }
});

test("host Cost tab, a tenant on its own host: its unallocated too, no rate periods", async () => {
  const { rates: _, ...own } = opCost;
  const p = await render(ec2Host(), "tenant", "http://localhost/hosts/h1?tab=cost", own);
  try {
    await until(() => tiles(p.el).length === 5, "the five tiles");
    expect(tiles(p.el)[3]).toEqual(["Unallocated", "$0.31"]);
    expect(cardTitles(p.el)).toEqual(["Host cost per hour"]);
  } finally {
    await p.done();
  }
});

const costCalls = (calls: readonly string[]) => calls.filter((c) => c.startsWith("/v1/hosts/h1/cost"));

test("host Cost tab with no hours, an operator narrowed to a tenant on a platform host: that tenant's view, no unallocated and no rate periods", async () => {
  // luxd 403s a host's cost to a tenant other than its owner, so the narrowed operator gets a tenant's view of a
  // platform host: a disabled Cost tab, ?tab=cost reading as Overview, and no /cost read.
  const p = await render(ec2Host({ platform: true, tenant: "" }), "operator", "http://localhost/hosts/h1?tenant=acme&tab=cost", noHours);
  try {
    expect(tab(p.el, "Cost").disabled).toBe(true);
    expect(tab(p.el, "Overview").getAttribute("aria-selected")).toBe("true");
    expect(new URLSearchParams(location.search).get("tab")).toBe("cost");
    expect(cardTitles(p.el)).toEqual(["Details", "Lifecycle", "Live placements"]);
    expect(p.el.querySelector(".host-cost")).toBeNull();
    expect(costCalls(p.fake.calls)).toEqual([]);
  } finally {
    await p.done();
  }
  // The same answer to an operator over every tenant: the host's own view, its rate periods card included.
  const q = await render(ec2Host({ platform: true, tenant: "" }), "operator", "http://localhost/hosts/h1?tab=cost", noHours);
  try {
    await until(() => tiles(q.el).length === 5, "the five tiles");
    expect(costCalls(q.fake.calls).map(query)).toEqual([{ since: "24h" }]);
    expect(tiles(q.el)[3]![0]).toBe("Unallocated");
    expect(cardTitles(q.el)).toEqual(["Host cost per hour", "Rate periods"]);
  } finally {
    await q.done();
  }
});

test("host Cost tab, an operator narrowed to the tenant of its own host: the read carries ?tenant=, and luxd's answer without unallocated renders the tenant's view", async () => {
  // luxd answers ?tenant=acme as acme: for a host in a platform pool, no unallocated and no rates.
  const p = await render(ec2Host(), "operator", "http://localhost/hosts/h1?tab=cost", (path) => (query(path).tenant === "acme" ? runsOnlyCost : opCost));
  try {
    await until(() => tiles(p.el).length === 5, "the operator's five tiles");
    expect(costCalls(p.fake.calls).map(query)).toEqual([{ since: "24h" }]);
    expect(cardTitles(p.el)).toEqual(["Host cost per hour", "Rate periods"]);
    // Narrowing refetches as the tenant.
    await act(async () => setSearchParams({ tenant: "acme" }));
    await until(() => tiles(p.el).length === 3, "the tenant's three tiles");
    expect(costCalls(p.fake.calls).map(query).at(-1)).toEqual({ since: "24h", tenant: "acme" });
    expect(tiles(p.el)).toEqual([
      ["Charged to your Runs (24h)", "$0.10"],
      ["Compute", "$0.10"],
      ["Block storage", "<$0.01"],
    ]);
    expect(p.el.querySelector(".host-cost")!.textContent).not.toMatch(/nallocated|Utilisation/);
    expect(cardTitles(p.el)).toEqual(["Charged to your Runs per hour"]);
  } finally {
    await p.done();
  }
});

test("host Cost tab with no hours, an operator narrowed to the tenant of its own host: the read carries ?tenant=, and it reads as that tenant, no rate periods", async () => {
  const p = await render(ec2Host(), "operator", "http://localhost/hosts/h1?tenant=acme&tab=cost", noHours);
  try {
    await until(() => tiles(p.el).length === 5, "the five tiles");
    expect(costCalls(p.fake.calls).map(query)).toEqual([{ tenant: "acme", since: "24h" }]);
    // What a tenant reads of its own host with no hours: unallocated (its own pool's), no rate periods.
    expect(tiles(p.el).map(([l]) => l)).toEqual(["Host cost (24h)", "Compute", "Block storage", "Unallocated", "Utilisation"]);
    expect(cardTitles(p.el)).toEqual(["Host cost per hour"]);
  } finally {
    await p.done();
  }
});

test("host Cost tab, an operator over every tenant on a tenant's host: no ?tenant=, unallocated and rate periods", async () => {
  const p = await render(ec2Host(), "operator", "http://localhost/hosts/h1?tab=cost", opCost);
  try {
    await until(() => tiles(p.el).length === 5, "the five tiles");
    expect(costCalls(p.fake.calls).map(query)).toEqual([{ since: "24h" }]);
    expect(tiles(p.el)[3]).toEqual(["Unallocated", "$0.31"]);
    expect(cardTitles(p.el)).toEqual(["Host cost per hour", "Rate periods"]);
  } finally {
    await p.done();
  }
});

test("host Cost tab, a tenant's host in a platform pool: luxd omits unallocated, so only what its Runs were charged", async () => {
  const p = await render(ec2Host(), "tenant", "http://localhost/hosts/h1?tab=cost", runsOnlyCost);
  try {
    await until(() => tiles(p.el).length === 3, "the three tiles");
    expect(tiles(p.el)).toEqual([
      ["Charged to your Runs (24h)", "$0.10"],
      ["Compute", "$0.10"],
      ["Block storage", "<$0.01"],
    ]);
    expect(p.el.querySelector(".host-cost")!.textContent).not.toMatch(/nallocated|Utilisation/);
    expect(cardTitles(p.el)).toEqual(["Charged to your Runs per hour"]);
  } finally {
    await p.done();
  }
});

test("host Details: its volumes in words, marked assumed when an operator supplied them; not known yet on a launched host without them", async () => {
  for (const [h, want] of [
    [ec2Host(), "100 GiB gp3 · 3000 IOPS · 125 MiB/s"],
    [ec2Host({ volumes: [{ ...gp3, assumed: true }] }), "100 GiB gp3 · 3000 IOPS · 125 MiB/sassumed"],
    [ec2Host({ volumes: undefined }), "not known yet"],
    [registered(), "–"],
  ] as const) {
    const p = await render(h, "operator", "http://localhost/hosts/h1");
    try {
      await until(() => p.el.textContent?.includes("Volumes") ?? false, "the Details card");
      const details = [...p.el.querySelectorAll(".card")].find((c) => c.querySelector(".card-title")?.textContent === "Details")!;
      const text = details.textContent!;
      expect(text).toContain(`Volumes${want}`);
    } finally {
      await p.done();
    }
  }
});

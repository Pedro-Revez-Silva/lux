import { afterAll, beforeAll, expect, test } from "bun:test";
import { GlobalRegistrator } from "@happy-dom/global-registrator";
import { act } from "react";
import { createRoot } from "react-dom/client";
import type { Pool, PoolCost } from "../../api/index.ts";

// The page's imports (the router) touch window at load: register the DOM first.
let PoolPage: typeof import("./PoolPage.tsx").PoolPage;
let ScopeProvider: typeof import("../scope.tsx").ScopeProvider;
let fakeApi: typeof import("../testing.ts").fakeApi;
let until: typeof import("../testing.ts").until;
let api: typeof import("../../api/index.ts");
beforeAll(async () => {
  GlobalRegistrator.register();
  ({ PoolPage } = await import("./PoolPage.tsx"));
  ({ ScopeProvider } = await import("../scope.tsx"));
  ({ fakeApi, until } = await import("../testing.ts"));
  api = await import("../../api/index.ts");
  (globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;
});
afterAll(async () => {
  await GlobalRegistrator.unregister();
});

const sleep = (ms: number) => act(() => new Promise<void>((r) => setTimeout(r, ms)));

// One bucket: uPlot needs a canvas happy-dom lacks, and draws from two.
const FROM = "2026-10-10T10:00:00Z";
const TO = "2026-10-10T11:00:00Z";

const PLATFORM: Pool = { id: "p-shared", name: "burst", provider: "ec2", minHosts: 0, maxHosts: 4, warmHosts: 0, shared: true, platform: true };
const OWN: Pool = { ...PLATFORM, id: "p-own", tenant: "acme", shared: false, platform: false };

const vol = { type: "gp3", sizeGiB: 100, iops: 3000, throughputMiBps: 125 };
/** A pool cost answer as luxd gives it to a reader that may see host time. */
const withHostTime: PoolCost = {
  poolId: "p",
  from: FROM,
  to: TO,
  basis: "list",
  interval: "hour",
  totals: [{ currency: "USD", amount: "7.32" }],
  series: [
    { at: FROM, family: "block-storage", currency: "USD", amount: "0.71" },
    { at: FROM, family: "compute", currency: "USD", amount: "6.61" },
  ],
  families: [{ family: "block-storage", displayName: "Block storage" }, { family: "compute", displayName: "Compute" }],
  topRuns: [{ id: "run_a", name: "review web#412", currency: "USD", amount: "2.5", estimate: false }],
  idle: [
    { family: "block-storage", currency: "USD", amount: "0.67" },
    { family: "compute", currency: "USD", amount: "6.27" },
  ],
  hostSeries: [
    { at: FROM, family: "block-storage", currency: "USD", allocated: "0.71", unallocated: "0.67" },
    { at: FROM, family: "compute", currency: "USD", allocated: "6.61", unallocated: "6.27" },
  ],
  hosts: [
    { hostId: "h1", hostName: "default-5ar45u7s", family: "block-storage", currency: "USD", allocated: "0.07", unallocated: "0.009", hours: 6.9, volumes: [vol] },
    { hostId: "h1", hostName: "default-5ar45u7s", family: "compute", currency: "USD", allocated: "0.704", unallocated: "0.079", hours: 6.9, volumes: [vol] },
    { hostId: "h2", hostName: "default-hjogolfb", family: "block-storage", currency: "USD", allocated: "0.64", unallocated: "0.661", hours: 4.8, volumes: [vol] },
    { hostId: "h2", hostName: "default-hjogolfb", family: "compute", currency: "USD", allocated: "5.906", unallocated: "6.191", hours: 4.8, volumes: [vol] },
  ],
};
/** The same to a tenant on a platform pool: its own Runs' cost, no host time at all. An AI row (luxd from before PR 2) is not a pool's cost and draws nothing. */
const runsOnly: PoolCost = { ...withHostTime, totals: [{ currency: "USD", amount: "1.2" }], series: [{ at: FROM, family: "compute", currency: "USD", amount: "1.1" }, { at: FROM, family: "block-storage", currency: "USD", amount: "0.1" }, { at: FROM, family: "ai", currency: "USD", amount: "9" }], idle: undefined, hostSeries: undefined, hosts: undefined };

type Answer = PoolCost | (() => Response) | Promise<never>;

async function render(role: "tenant" | "operator", pool: Pool, cost: Answer) {
  api.signIn("k");
  api.setRole(role);
  const fake = fakeApi((path) => {
    if (path.startsWith("/v1/pools?") || path === "/v1/pools") return { pools: [pool] };
    if (path.startsWith(`/v1/pools/${pool.name}/metrics`)) return { poolId: pool.id, from: FROM, to: TO, resolution: 60, samples: [], now: { hosts: { ready: 2 }, capacityCpus: 8, capacityMemory: 1, allocatedCpus: 1, allocatedMemory: 1, running: 1, queued: 0, launchFailures: 0 } };
    // The tile's read (interval=hour, the page's range) and the tab's answer alike; a Response is read once, so each read gets its own.
    if (path.startsWith(`/v1/pools/${pool.name}/cost`)) return typeof cost === "function" ? cost() : cost;
    return {};
  });
  (window as unknown as { happyDOM: { setURL: (u: string) => void } }).happyDOM.setURL(`http://localhost/pools/${pool.name}?tab=cost&range=24h`);
  const el = document.createElement("div");
  document.body.appendChild(el);
  const root = createRoot(el);
  await act(async () =>
    root.render(
      <ScopeProvider>
        <PoolPage name={pool.name} />
      </ScopeProvider>,
    ),
  );
  await sleep(50);
  const tab = () => el.querySelector(".pool-cost");
  return {
    el,
    fake,
    tab,
    tiles: () => [...(tab()?.querySelectorAll(".stat") ?? [])].map((s) => [s.querySelector(".stat-label")?.textContent, s.querySelector(".stat-value")?.textContent, s.querySelector(".stat-unit")?.textContent]),
    titles: () => [...(tab()?.querySelectorAll(".card-title") ?? [])].map((t) => t.textContent),
    card: (title: string) => [...(tab()?.querySelectorAll(".card") ?? [])].find((c) => c.querySelector(".card-title")?.textContent === title),
    summary: () => [...el.querySelectorAll(".stat")].find((s) => /^(Host cost|Cost) \(/.test(s.querySelector(".stat-label")?.textContent ?? "") && !tab()?.contains(s)),
    done: async () => {
      await act(async () => root.unmount());
      el.remove();
      fake.restore();
      api.signOut();
    },
  };
}

const rowsOf = (card: Element | undefined) => [...(card?.querySelectorAll("tbody tr") ?? [])].map((tr) => [...tr.querySelectorAll("td")].map((td) => td.textContent?.trim()));

async function fullView(role: "operator" | "tenant", pool: Pool) {
  const p = await render(role, pool, withHostTime);
  try {
    await until(() => p.tiles().length === 5, "the five tiles");
    expect(p.tiles()).toEqual([
      ["Host cost (24h)", "$14.26", "list price · 11.7 host-hours"],
      ["Compute", "$12.88", `${"$1.1009"}/h avg`],
      ["Block storage", "$1.38", `100 GiB gp3 · ${"$0.1179"}/h avg`],
      ["Unallocated", "$6.94", "49% of host cost · no Run reserved it"],
      ["Utilisation", "51%", "charged to Runs ÷ host cost"],
    ]);
    expect(p.titles()).toEqual(["Host cost per hour", "Who paid", "Cost by host", "Top runs in this pool"]);
    // Who paid: Compute and Block storage apart, then their total.
    expect(rowsOf(p.card("Who paid"))).toEqual([
      ["Compute", "$6.61", "$6.27", "$12.88"],
      ["Block storage", "$0.71", "$0.67", "$1.38"],
      ["Total", "$7.32", "$6.94", "$14.26"],
    ]);
    expect([...p.card("Who paid")!.querySelectorAll("[data-family-split]")].map((s) => s.textContent)).toEqual(["Compute51% charged to Runs · 49% unallocated", "Block storage51% charged to Runs · 49% unallocated"]);
    // Cost by host: costliest first, each family apart, a link to its Cost tab.
    const hosts = rowsOf(p.card("Cost by host"));
    expect(hosts.map((r) => r.slice(0, 6))).toEqual([
      ["default-hjogolfb", "4.8 h", "$12.097", "$1.301", "$13.398", "$6.852"],
      ["default-5ar45u7s", "6.9 h", "$0.783", "$0.079", "$0.862", "$0.088"],
    ]);
    expect(hosts.map((r) => r[6])).toEqual(["49%", "90%"]);
    expect(p.card("Cost by host")!.querySelector("a")!.getAttribute("href")).toStartWith("/hosts/h2?tab=cost");
    // The chart's legend: four series, faded unallocated; never an AI one.
    const legend = [...p.card("Host cost per hour")!.querySelectorAll(".tschart-legend-label")].map((l) => l.textContent);
    expect(legend).toEqual(["Compute · runs", "Compute · unallocated", "Block storage · runs", "Block storage · unallocated"]);
    expect(p.tab()!.textContent).not.toMatch(/AI models/);
    expect(p.tab()!.querySelector(".callout")!.textContent).toBe("Pool cost is the machines only — instance and disk. AI and other external costs belong to Runs.");
    // The summary tile above the tabs: one host cost and one unallocated figure, summed over families.
    expect(p.summary()!.querySelector(".stat-label")!.textContent).toBe("Host cost (24h)");
    expect(p.summary()!.querySelector(".stat-value")!.textContent).toBe("$14.26");
    expect(p.summary()!.querySelector(".stat-unit")!.textContent).toBe("list price · $6.94 unallocated");
  } finally {
    await p.done();
  }
}

test("pool Cost tab, operator: host cost, Compute and Block storage apart, unallocated, who paid and by host", async () => {
  await fullView("operator", PLATFORM);
});

test("pool Cost tab, a tenant on its own pool: the same figures, unallocated included", async () => {
  await fullView("tenant", OWN);
});

test("pool Cost tab, a tenant on a platform pool: its Runs' figures only, no unallocated, no zeros or dashes for it", async () => {
  const p = await render("tenant", PLATFORM, runsOnly);
  try {
    await until(() => p.tiles().length === 3, "the three tiles");
    expect(p.tiles()).toEqual([
      ["Charged to your Runs (24h)", "$1.20", "list price · compute and block storage"],
      ["Compute", "$1.10", "your Runs' share of the machines"],
      ["Block storage", "$0.10", "your Runs' share of the disks"],
    ]);
    expect(p.titles()).toEqual(["Charged to your Runs per hour", "Top runs in this pool"]);
    const text = p.tab()!.textContent!;
    expect(text).not.toMatch(/Unallocated|unallocated|Utilisation|Who paid/);
    const legend = [...p.tab()!.querySelectorAll(".tschart-legend-label")].map((l) => l.textContent);
    expect(legend).toEqual(["Compute · runs", "Block storage · runs"]);
    expect(p.summary()!.querySelector(".stat-label")!.textContent).toBe("Cost (24h)");
    expect(p.summary()!.querySelector(".stat-value")!.textContent).toBe("$1.20");
    expect(p.summary()!.textContent).not.toMatch(/unallocated|idle/);
  } finally {
    await p.done();
  }
});

test("pool Cost tab while loading: skeleton tiles, no figure, no zero", async () => {
  const p = await render("operator", PLATFORM, new Promise<never>(() => {}));
  try {
    await until(() => p.tab() != null, "the tab");
    expect(p.tab()!.querySelectorAll(".stat .skeleton").length).toBeGreaterThan(0);
    expect(p.tab()!.textContent).not.toMatch(/\$0/);
    expect(p.tab()!.textContent).toContain("Loading…");
  } finally {
    await p.done();
  }
});

/**
 * An empty range as luxd sends it to a reader who may see host time: idle
 * is [] (present), and the empty families, hostSeries and hosts are omitted.
 */
const emptyVisible: PoolCost = { poolId: "p", from: FROM, to: TO, basis: "list", interval: "hour", totals: [], series: [], topRuns: [], idle: [] };

async function emptyHostView(role: "operator" | "tenant", pool: Pool) {
  const p = await render(role, pool, emptyVisible);
  try {
    await until(() => p.tab()?.textContent?.includes("No host time recorded here in this range.") ?? false, "the Cost by host empty state");
    expect(p.tiles()[0]![0]).toBe("Host cost (24h)");
    expect(p.titles()).toEqual(["Host cost per hour", "Who paid", "Cost by host", "Top runs in this pool"]);
    expect(p.card("Cost by host")!.textContent).toContain("No host time recorded here in this range.");
    expect(p.tab()!.textContent).not.toMatch(/your Runs|\$0/);
    await until(() => p.summary() != null, "the summary tile");
    expect(p.summary()!.querySelector(".stat-label")!.textContent).toBe("Host cost (24h)");
    expect(p.summary()!.textContent).not.toMatch(/your Runs/);
  } finally {
    await p.done();
  }
}

test("pool Cost tab with nothing costed: an empty state, never $0", async () => {
  const p = await render("operator", PLATFORM, emptyVisible);
  try {
    await until(() => p.tab()?.textContent?.includes("No host cost recorded in this range") ?? false, "the empty state");
    expect(p.tab()!.textContent).not.toMatch(/\$0/);
    expect(p.tab()!.textContent).toContain("No host time recorded here in this range.");
  } finally {
    await p.done();
  }
});

test("pool Cost tab, operator on an empty range: still the host cost view, Cost by host empty", async () => {
  await emptyHostView("operator", PLATFORM);
});

test("pool Cost tab, a tenant on its own pool on an empty range: still the host cost view, Cost by host empty", async () => {
  await emptyHostView("tenant", OWN);
});

test("pool Cost tab when the read fails: the error, not an empty pool", async () => {
  const p = await render("operator", PLATFORM, () => new Response(JSON.stringify({ error: { code: "internal", message: "database is down" } }), { status: 500, headers: { "Content-Type": "application/json" } }));
  try {
    await until(() => p.el.textContent?.includes("database is down") ?? false, "the error");
    expect(p.el.textContent).toContain("Request failed");
    expect(p.el.textContent).not.toContain("No host cost recorded");
  } finally {
    await p.done();
  }
});

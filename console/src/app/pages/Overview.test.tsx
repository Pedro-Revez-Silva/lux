import { afterAll, beforeAll, expect, test } from "bun:test";
import { GlobalRegistrator } from "@happy-dom/global-registrator";
import { act } from "react";
import { createRoot } from "react-dom/client";
import type { Sample } from "../../api/index.ts";

// The page's imports (the router) touch window at load: register the DOM first.
let Overview: typeof import("./Overview.tsx").Overview;
let storedSeries: typeof import("./Overview.tsx").storedSeries;
let ScopeProvider: typeof import("../scope.tsx").ScopeProvider;
let fakeApi: typeof import("../testing.ts").fakeApi;
let until: typeof import("../testing.ts").until;
let api: typeof import("../../api/index.ts");
beforeAll(async () => {
  GlobalRegistrator.register();
  ({ Overview, storedSeries } = await import("./Overview.tsx"));
  ({ ScopeProvider } = await import("../scope.tsx"));
  ({ fakeApi, until } = await import("../testing.ts"));
  api = await import("../../api/index.ts");
  (globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;
});
afterAll(async () => {
  await GlobalRegistrator.unregister();
});

const sleep = (ms: number) => act(() => new Promise<void>((r) => setTimeout(r, ms)));

const T0 = Date.parse("2026-09-01T12:00:00Z");
const iso = (s: number) => new Date(T0 + s * 1000).toISOString();

const SAMPLES: Sample[] = [
  { at: iso(0), storedVolume: 100, storedOutput: 7, storedArtifact: 0, storedContext: 40 },
  { at: iso(60), storedVolume: 120, storedOutput: 9, storedArtifact: 3, storedContext: 40 },
];

test("Stored: one series per charted kind, values from its own field; storedContext is not charted", () => {
  const s = storedSeries(SAMPLES);
  expect(s.series.map((x) => x.label)).toEqual(["Snapshots", "Output", "Artifacts"]);
  expect(s.series.map((x) => x.color)).toEqual(["var(--chart-3)", "var(--chart-7)", "var(--chart-4)"]);
  expect(s.x).toEqual([T0 / 1000, T0 / 1000 + 60]);
  expect(s.ys).toEqual([
    [100, 120],
    [7, 9],
    [0, 3],
  ]);
  // The stack's total leaves out the 40 bytes of storedContext.
  expect(s.ys.reduce((t, y) => t + (y[1] ?? 0), 0)).toBe(132);
});

test("Stored: a sample without the fields is a gap, not zero", () => {
  expect(storedSeries([{ at: iso(0) }]).ys).toEqual([[null], [null], [null]]);
});

/** The Overview, as role at url. History answers one sample: uPlot needs a canvas happy-dom lacks, and draws from two. */
async function render(role: "tenant" | "operator", url: string, answer?: (path: string) => unknown) {
  api.signIn("k");
  api.setRole(role);
  const fake = fakeApi((path) => {
    const a = answer?.(path);
    if (a !== undefined) return a;
    if (path.startsWith("/v1/history")) return { from: iso(0), to: iso(60), resolution: 60, samples: SAMPLES.slice(0, 1) };
    if (path.startsWith("/v1/status")) return { runs: {}, busy: 0, idle: 0, queued: 0, startLatency: { n: 0 }, hosts: {}, capacity: { cpus: 0, memory: 0 }, allocated: { cpus: 0, memory: 0 } };
    return {};
  });
  (window as unknown as { happyDOM: { setURL: (u: string) => void } }).happyDOM.setURL(url);
  const el = document.createElement("div");
  document.body.appendChild(el);
  const root = createRoot(el);
  await act(async () =>
    root.render(
      <ScopeProvider>
        <Overview />
      </ScopeProvider>,
    ),
  );
  await sleep(50);
  return {
    fake,
    el,
    card: () => [...el.querySelectorAll(".card")].find((c) => c.querySelector(".card-title")?.textContent === "Stored"),
    done: async () => {
      await act(async () => root.unmount());
      el.remove();
      fake.restore();
      api.signOut();
    },
  };
}

for (const c of [
  { name: "a tenant's own view", role: "tenant" as const, url: "http://localhost/?tab=storage", tenant: undefined },
  { name: "the whole system", role: "operator" as const, url: "http://localhost/?tab=storage", tenant: undefined },
  { name: "an operator narrowed to a tenant", role: "operator" as const, url: "http://localhost/?tab=storage&tenant=acme", tenant: "acme" },
]) {
  test(`the Stored card in ${c.name}`, async () => {
    const p = await render(c.role, c.url);
    try {
      const card = p.card();
      expect(card).toBeDefined();
      expect(card!.textContent).toContain("in S3, by kind");
      const legend = [...card!.querySelectorAll(".tschart-legend-item")].map((b) => b.textContent);
      expect(legend).toEqual(["Snapshots", "Output", "Artifacts"]);
      const history = p.fake.calls.find((u) => u.startsWith("/v1/history"))!;
      expect(new URL(history, "http://localhost").searchParams.get("tenant") ?? undefined).toBe(c.tenant);
    } finally {
      await p.done();
    }
  });
}

const costCalls = (calls: string[]) => calls.filter((u) => u.startsWith("/v1/costs?")).map((u) => new URL(u, "http://x").searchParams);
const historyCall = (calls: string[]) => new URL(calls.find((u) => u.startsWith("/v1/history"))!, "http://x").searchParams;

const titles = (el: Element) => [...el.querySelectorAll(".card-title")].map((t) => t.textContent);

test("the Overview's tabs: Activity by default (tiles, trends, feed); Cost and Storage each alone; ?tab= follows a click", async () => {
  const p = await render("tenant", "http://localhost/");
  try {
    const tab = (name: string) => [...p.el.querySelectorAll<HTMLButtonElement>('[role="tab"]')].find((b) => b.textContent === name)!;
    expect([...p.el.querySelectorAll('[role="tab"]')].map((b) => b.textContent)).toEqual(["Activity", "Cost", "Storage"]);
    expect(tab("Activity").getAttribute("aria-selected")).toBe("true");
    expect(p.el.querySelectorAll(".stat").length).toBeGreaterThan(0);
    expect(titles(p.el)).toContain("Runs");
    expect(titles(p.el)).not.toContain("Stored");
    expect(p.el.querySelector(".cost-panel")).toBeNull();
    // The cost panel's reads wait for its tab.
    expect(costCalls(p.fake.calls)).toEqual([]);

    await act(async () => tab("Cost").click());
    await until(() => p.el.querySelector(".cost-panel") != null, "the cost panel");
    expect(new URLSearchParams(location.search).get("tab")).toBe("cost");
    expect(p.el.querySelectorAll(".stat").length).toBe(0);
    await until(() => costCalls(p.fake.calls).length > 0, "a cost read");

    await act(async () => tab("Storage").click());
    await until(() => titles(p.el).includes("Stored"), "the Stored card");
    expect(new URLSearchParams(location.search).get("tab")).toBe("storage");
    expect(titles(p.el)).toEqual(["Stored"]);

    await act(async () => tab("Activity").click());
    expect(new URLSearchParams(location.search).get("tab")).toBeNull();
  } finally {
    await p.done();
  }
});

test("lost hosts show on the Hosts ready tile, the lost figure in danger; the row stays six tiles either way", async () => {
  const status = (lost: number) => ({ runs: {}, busy: 0, idle: 0, queued: 0, startLatency: { n: 0 }, hosts: { ready: 3, draining: 1, lost }, capacity: { cpus: 0, memory: 0 }, allocated: { cpus: 0, memory: 0 } });
  for (const lost of [0, 2]) {
    const p = await render("operator", "http://localhost/", (path) => (path.startsWith("/v1/status") ? status(lost) : undefined));
    try {
      const tiles = [...p.el.querySelectorAll(".stat")];
      expect(tiles.length).toBe(6);
      const hosts = tiles.find((t) => t.textContent!.includes("Hosts ready"))!;
      expect(hosts.querySelector(".stat-unit")!.textContent).toBe(`1 draining · ${lost} lost`);
      expect(hosts.classList.contains("stat-danger")).toBe(false);
      expect(hosts.querySelector(".text-danger")?.textContent ?? null).toBe(lost > 0 ? `${lost} lost` : null);
      await act(async () => (hosts as HTMLElement).click());
      expect(location.pathname + location.search).toBe(lost > 0 ? "/hosts?state=lost" : "/hosts");
    } finally {
      await p.done();
    }
  }
});

test("a failed status read shows on Activity, which it feeds, and not on Cost or Storage", async () => {
  const failed = () => new Response(JSON.stringify({ error: { code: "internal", message: "status unavailable" } }), { status: 500, headers: { "Content-Type": "application/json" } });
  const answer = (path: string) => (path.startsWith("/v1/status") ? failed() : undefined);
  for (const [url, shown] of [
    ["http://localhost/", true],
    ["http://localhost/?tab=cost", false],
    ["http://localhost/?tab=storage", false],
  ] as const) {
    const p = await render("tenant", url, answer);
    try {
      expect(p.fake.calls.some((u) => u.startsWith("/v1/status"))).toBe(true);
      expect(p.el.textContent!.includes("status unavailable")).toBe(shown);
    } finally {
      await p.done();
    }
  }
});

test("the Cost panel's filters and Every reach every cost request, and Every the history's res", async () => {
  const p = await render("tenant", "http://localhost/?tab=cost&range=7d&every=hour&label=app%3Da&label=app%3Db&nolabel=phase");
  try {
    const costs = costCalls(p.fake.calls);
    expect(costs.length).toBeGreaterThan(0);
    for (const q of costs) {
      expect(q.getAll("label")).toEqual(["app=a", "app=b"]);
      expect(q.getAll("nolabel")).toEqual(["phase"]);
    }
    // Family is the default breakdown: one request sends interval, the family + series one.
    expect(costs.filter((q) => q.has("interval")).map((q) => q.get("interval"))).toEqual(["hour"]);
    const labels = p.fake.calls.find((u) => u.startsWith("/v1/costs/labels"));
    expect(labels).toBeDefined();
    expect(historyCall(p.fake.calls).get("res")).toBe("3600");
  } finally {
    await p.done();
  }
});

test("Every Auto sends no history res", async () => {
  const p = await render("tenant", "http://localhost/?range=7d");
  try {
    expect(historyCall(p.fake.calls).has("res")).toBe(false);
  } finally {
    await p.done();
  }
});

for (const [cost, rank, showFilter] of [
  ["all", "all", {}],
  ["compute", "compute", { family: "compute" }],
  ["external", "external", { nofamily: "compute" }],
] as const) {
  test(`?cost=${cost}: every summary that lists values is folded ranked by ${rank}; only the breakdown series filters by family`, async () => {
    const p = await render("operator", `http://localhost/?tab=cost&by=key&cost=${cost}`);
    try {
      const costs = costCalls(p.fake.calls);
      const of = (g: string[]) => costs.filter((q) => JSON.stringify(q.getAll("group")) === JSON.stringify(g));
      const filterOf = (q: URLSearchParams) => ({ family: q.get("family") ?? undefined, nofamily: q.get("nofamily") ?? undefined });
      for (const [g, top] of [
        [["key"], "7"],
        [["key", "family"], "7"],
        [["run", "family"], "10"],
        [["tenant", "family"], "10"],
      ] as const) {
        const qs = of([...g]);
        expect(qs.length).toBe(1);
        expect([qs[0]!.get("top"), qs[0]!.get("rank")]).toEqual([top, rank]);
      }
      expect(filterOf(of(["key"])[0]!)).toEqual({ family: undefined, nofamily: undefined, ...showFilter });
      for (const g of [["key", "family"], ["run", "family"], ["tenant", "family"], ["family"]]) {
        expect(filterOf(of(g)[0]!)).toEqual({ family: undefined, nofamily: undefined });
      }
      // The family summary is not folded: it asks for runs, not top.
      expect([of(["family"])[0]!.get("runs"), of(["family"])[0]!.has("top")]).toEqual(["true", false]);
      // No summary is grouped by a value and run: Runs per value come with the fold.
      expect(costs.some((q) => q.getAll("group").length === 2 && q.getAll("group")[1] === "run")).toBe(false);
    } finally {
      await p.done();
    }
  });
}

const costRow = (group: Record<string, string>, amount: string, extra: Record<string, unknown> = {}) => ({ group, currency: "USD", amount, ...extra });
// One hourly bucket: two would draw a uPlot chart, which needs a canvas happy-dom lacks.
const summary = (totals: unknown[], series: unknown[] = [], extra: Record<string, unknown> = {}) => ({ from: iso(0), to: iso(3600), basis: "list", totals, series, ...extra });
const groupsOf = (path: string) => new URL(path, "http://x").searchParams.getAll("group").join(",");
const cellsOf = (el: Element, heading: string) =>
  [...el.querySelectorAll(".card")]
    .find((c) => c.querySelector(".card-title")?.textContent === heading)!
    .querySelectorAll("tbody tr");

test("the peak Run is asked folded to one Run, ranked by Show, over the peak bucket", async () => {
  const p = await render("tenant", "http://localhost/?tab=cost&cost=compute", (path) => {
    if (!path.startsWith("/v1/costs?")) return undefined;
    if (groupsOf(path) === "family") return summary([costRow({ family: "compute" }, "5")], [costRow({ family: "compute" }, "5", { at: iso(0) })]);
    return summary([]);
  });
  try {
    const peak = costCalls(p.fake.calls).filter((q) => groupsOf(`/?${q}`) === "run,family" && q.has("from"));
    expect(peak.length).toBe(1);
    expect([peak[0]!.get("top"), peak[0]!.get("rank"), peak[0]!.get("from"), peak[0]!.get("to")]).toEqual(["1", "compute", iso(0), iso(3600)]);
  } finally {
    await p.done();
  }
});

test("By family: each family's Runs from the family summary's runs, per currency", async () => {
  const p = await render("tenant", "http://localhost/?tab=cost", (path) => {
    if (!path.startsWith("/v1/costs?")) return undefined;
    if (groupsOf(path) === "family") return summary([costRow({ family: "compute" }, "5", { runs: 3 }), costRow({ family: "ai" }, "2", { runs: 1 }), costRow({ family: "ai" }, "1", { runs: 2, currency: "EUR" })]);
    return summary([]);
  });
  try {
    const rows = [...cellsOf(p.el, "By family")].map((r) => [...r.querySelectorAll("td")].map((td) => td.textContent));
    expect(rows.map((r) => [r[0], r[1]])).toEqual([
      ["ai", "2"],
      ["Compute", "3"],
      ["ai", "1"],
    ]);
  } finally {
    await p.done();
  }
});

test("Other's count is the Show-filtered series call's, not the split's", async () => {
  const p = await render("tenant", "http://localhost/?tab=cost&by=key&cost=external", (path) => {
    if (!path.startsWith("/v1/costs?")) return undefined;
    const g = groupsOf(path);
    if (g === "family") return summary([costRow({ family: "ai" }, "9")]);
    if (g === "key") return summary([costRow({ key: "k1" }, "4"), costRow({ key: "(other)" }, "5", { other: true })], [costRow({ key: "k1" }, "4", { at: iso(0) }), costRow({ key: "(other)" }, "5", { at: iso(0), other: true })], { otherCount: { USD: 2 } });
    if (g === "key,family") return summary([costRow({ key: "k1", family: "ai" }, "4", { runs: 1 }), costRow({ key: "(other)", family: "ai" }, "5", { runs: 6, other: true })], [], { otherCount: { USD: 5 } });
    return summary([]);
  });
  try {
    const names = [...cellsOf(p.el, "By API key")].map((r) => r.querySelector("td")!.textContent);
    expect(names).toEqual(["k1", "Other (2)"]);
  } finally {
    await p.done();
  }
});

test("with Break down by API key, Runs from before key tracking are noted with their count", async () => {
  const p = await render("tenant", "http://localhost/?tab=cost&by=key", (path) => {
    if (!path.startsWith("/v1/costs?")) return undefined;
    const g = groupsOf(path);
    if (g === "family") return summary([costRow({ family: "ai" }, "9")]);
    if (g === "key,family") return summary([costRow({ key: "(none)", family: "ai" }, "4", { runs: 4 }), costRow({ key: "k1", family: "ai" }, "5", { runs: 1 })]);
    return summary([]);
  });
  try {
    expect(p.el.querySelector(".cost-panel")!.textContent).toContain("4 Runs in this range were submitted before Lux recorded the submitting key");
  } finally {
    await p.done();
  }
});

test("Break down by Label with the keys failing: the breakdown falls back to app, never 'Loading…' under the error", async () => {
  const failed = () => new Response(JSON.stringify({ error: { code: "internal", message: "labels unavailable" } }), { status: 500, headers: { "Content-Type": "application/json" } });
  const p = await render("tenant", "http://localhost/?tab=cost&by=label", (path) => (path.startsWith("/v1/costs/labels") ? failed() : undefined));
  try {
    await sleep(50);
    const panel = p.el.querySelector(".cost-panel")!;
    expect(panel.textContent).toContain("labels unavailable");
    expect(panel.querySelector(".cost-charts")!.textContent).not.toContain("Loading");
    expect(costCalls(p.fake.calls).some((q) => q.getAll("group").includes("label:app"))).toBe(true);
  } finally {
    await p.done();
  }
});

test("Break down by Label names its key in the URL once the keys are known", async () => {
  let release!: () => void;
  const keysArrive = new Promise<void>((r) => (release = r));
  const p = await render("tenant", "http://localhost/?tab=cost", (path) => (path.startsWith("/v1/costs/labels") ? keysArrive.then(() => ({ from: iso(0), to: iso(60), keys: [{ key: "team", runs: 3 }, { key: "app", runs: 2 }] })) : undefined));
  try {
    const radio = (name: string) => [...p.el.querySelectorAll<HTMLButtonElement>('[role="radiogroup"][aria-label="Break down by"] [role="radio"]')].find((b) => b.textContent === name)!;
    const before = history.length;
    // Before the keys arrive: by=label, no key yet, and no breakdown call (it would name a key luxd may not have).
    await act(async () => radio("Label").click());
    expect(new URLSearchParams(location.search).get("by")).toBe("label");
    expect(history.length).toBe(before);
    expect(costCalls(p.fake.calls).some((q) => q.getAll("group").some((g) => g.startsWith("label:")))).toBe(false);
    // Then the default key, app, replaces it (no history entry).
    await act(async () => release());
    await sleep(20);
    expect(location.search).toContain("by=label%3Aapp");
    expect(history.length).toBe(before);
    // Once known, a click writes the key at once.
    await act(async () => radio("Family").click());
    expect(new URLSearchParams(location.search).get("by")).toBeNull();
    await act(async () => radio("Label").click());
    expect(location.search).toContain("by=label%3Aapp");
  } finally {
    await p.done();
  }
});

test("a cost request that fails shows the error, never also 'No cost'", async () => {
  const tooLarge = () => new Response(JSON.stringify({ error: { code: "too_large", message: "cost response exceeds 10000 rows" } }), { status: 413, headers: { "Content-Type": "application/json" } });
  const p = await render("tenant", "http://localhost/?tab=cost&by=key", (path) => {
    if (!path.startsWith("/v1/costs?")) return undefined;
    const q = new URL(path, "http://x").searchParams;
    if (q.getAll("group").includes("key") && q.has("interval")) return tooLarge();
    return { from: iso(0), to: iso(3600), basis: "list", totals: q.getAll("group").join() === "family" ? [{ group: { family: "compute" }, currency: "USD", amount: "1" }] : [], series: [] };
  });
  try {
    const panel = p.el.querySelector(".cost-panel")!;
    expect(panel.textContent).toContain("10000 rows");
    expect(panel.querySelector(".cost-charts")!.textContent).not.toContain("cost in this range");
  } finally {
    await p.done();
  }
});

const BS_FAMILIES = [costRow({ family: "ai" }, "625.76", { runs: 312 }), costRow({ family: "block-storage" }, "0.71", { runs: 341 }), costRow({ family: "compute" }, "6.61", { runs: 341 })];
const UNALLOCATED = [
  { family: "block-storage", currency: "USD", amount: "0.67" },
  { family: "compute", currency: "USD", amount: "6.27" },
];
const familyAnswer = (path: string) => {
  if (!path.startsWith("/v1/costs?")) return undefined;
  // luxd sends no displayName for block-storage on older answers: the console names it anyway.
  if (groupsOf(path) === "family") return summary(BS_FAMILIES, [], { unallocated: UNALLOCATED, families: [{ family: "ai", displayName: "AI models", color: "violet" }, { family: "compute", displayName: "Compute" }] });
  return summary([]);
};
const kpis = (el: Element) => [...el.querySelectorAll(".kpi")].map((k) => [k.querySelector(".kpi-label")?.textContent, k.querySelector(".kpi-value")?.textContent]);

test("Overview Cost by family: Block storage is its own KPI and row, apart from Compute and External", async () => {
  const p = await render("operator", "http://localhost/?tab=cost", familyAnswer);
  try {
    await until(() => kpis(p.el).some(([l]) => l === "Block storage"), "the Block storage KPI");
    expect(kpis(p.el).slice(0, 4)).toEqual([
      ["Total · last 24 hours", "$633.08"],
      ["Compute", "$6.61"],
      ["Block storage", "$0.71"],
      ["External", "$625.76"],
    ]);
    // Block storage is 0.11% of the total: a non-zero share under 1% reads <1%, never 0%.
    const bs = [...p.el.querySelectorAll(".kpi")].find((k) => k.querySelector(".kpi-label")?.textContent === "Block storage")!;
    expect(bs.textContent).toContain("<1% · the disks");
    const rows = [...cellsOf(p.el, "By family")].map((r) => [...r.querySelectorAll("td")].map((td) => td.textContent));
    expect(rows).toEqual([
      ["AI models", "312", "–", "–", "$625.76", "$625.76", "99%"],
      ["Compute", "341", "$6.61", "–", "–", "$6.61", "1%"],
      ["Block storage", "341", "–", "$0.71", "–", "$0.71", "<1%"],
    ]);
  } finally {
    await p.done();
  }
});

test("Show External: the total is every family but compute and says so; the External KPI is the plugins' alone", async () => {
  const p = await render("operator", "http://localhost/?tab=cost&cost=external", familyAnswer);
  try {
    await until(() => kpis(p.el).some(([l]) => l === "Block storage"), "the Block storage KPI");
    expect(kpis(p.el).slice(0, 4)).toEqual([
      ["Non-compute total · last 24 hours", "$626.47"],
      ["Compute", "$6.61"],
      ["Block storage", "$0.71"],
      ["External", "$625.76"],
    ]);
    const option = [...p.el.querySelectorAll<HTMLElement>('[role="radiogroup"][aria-label="Show"] [role="radio"]')].find((b) => b.textContent === "External")!;
    expect(option.getAttribute("title")).toBe("Every family but compute: block storage and what cost plugins report");
  } finally {
    await p.done();
  }
});

test("Overview Cost: unallocated host time is split into Compute and Block storage; a tenant sees its own pools' when luxd sends them", async () => {
  for (const [role, url] of [
    ["operator", "http://localhost/?tab=cost"],
    ["tenant", "http://localhost/?tab=cost"],
  ] as const) {
    const p = await render(role, url, familyAnswer);
    try {
      await until(() => p.el.querySelector(".cost-unallocated [data-unallocated]") != null, "the unallocated split");
      const u = p.el.querySelector(".cost-unallocated")!;
      expect(u.querySelector('[data-unallocated="compute"]')!.textContent).toBe("Compute $6.27");
      expect(u.querySelector('[data-unallocated="block-storage"]')!.textContent).toBe("Block storage $0.67");
    } finally {
      await p.done();
    }
  }
  // luxd sends a tenant no unallocated rows when it has no pool of its own: nothing is shown, not $0.
  const p = await render("tenant", "http://localhost/?tab=cost", (path) => (path.startsWith("/v1/costs?") && groupsOf(path) === "family" ? summary(BS_FAMILIES, [], { unallocated: [] }) : familyAnswer(path)));
  try {
    await until(() => kpis(p.el).some(([l]) => l === "Block storage"), "the panel");
    expect(p.el.querySelector(".cost-unallocated")).toBeNull();
  } finally {
    await p.done();
  }
});

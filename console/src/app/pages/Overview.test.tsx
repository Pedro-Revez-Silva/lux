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
let api: typeof import("../../api/index.ts");
beforeAll(async () => {
  GlobalRegistrator.register();
  ({ Overview, storedSeries } = await import("./Overview.tsx"));
  ({ ScopeProvider } = await import("../scope.tsx"));
  ({ fakeApi } = await import("../testing.ts"));
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

test("Stored: one series per kind, every kind, values from its own field", () => {
  const s = storedSeries(SAMPLES);
  expect(s.series.map((x) => x.label)).toEqual(["Snapshots", "Output", "Artifacts", "Build contexts"]);
  expect(s.series.map((x) => x.color)).toEqual(["var(--chart-3)", "var(--chart-7)", "var(--chart-4)", "var(--chart-5)"]);
  expect(s.x).toEqual([T0 / 1000, T0 / 1000 + 60]);
  expect(s.ys).toEqual([
    [100, 120],
    [7, 9],
    [0, 3],
    [40, 40],
  ]);
  // The stack sums to every byte in S3.
  expect(s.ys.reduce((t, y) => t + (y[1] ?? 0), 0)).toBe(172);
});

test("Stored: a sample without the fields is a gap, not zero", () => {
  expect(storedSeries([{ at: iso(0) }]).ys).toEqual([[null], [null], [null], [null]]);
});

/** The Overview, as role at url. History answers one sample: uPlot needs a canvas happy-dom lacks, and draws from two. */
async function render(role: "tenant" | "operator", url: string) {
  api.signIn("k");
  api.setRole(role);
  const fake = fakeApi((path) => {
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
  { name: "a tenant's own view", role: "tenant" as const, url: "http://localhost/", tenant: undefined },
  { name: "the whole system", role: "operator" as const, url: "http://localhost/", tenant: undefined },
  { name: "an operator narrowed to a tenant", role: "operator" as const, url: "http://localhost/?tenant=acme", tenant: "acme" },
]) {
  test(`the Stored card in ${c.name}`, async () => {
    const p = await render(c.role, c.url);
    try {
      const card = p.card();
      expect(card).toBeDefined();
      expect(card!.textContent).toContain("in S3, by kind");
      const legend = [...card!.querySelectorAll(".tschart-legend-item")].map((b) => b.textContent);
      expect(legend).toEqual(["Snapshots", "Output", "Artifacts", "Build contexts"]);
      const history = p.fake.calls.find((u) => u.startsWith("/v1/history"))!;
      expect(new URL(history, "http://localhost").searchParams.get("tenant") ?? undefined).toBe(c.tenant);
    } finally {
      await p.done();
    }
  });
}

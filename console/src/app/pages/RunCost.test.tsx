import { afterAll, beforeAll, expect, test } from "bun:test";
import { GlobalRegistrator } from "@happy-dom/global-registrator";
import { act } from "react";
import { createRoot } from "react-dom/client";
import type { Run, RunCost as RunCostData } from "../../api/index.ts";

// The page's imports (the router) touch window at load: register the DOM first.
let RunCost: typeof import("./RunCost.tsx").RunCost;
let fakeApi: typeof import("../testing.ts").fakeApi;
let until: typeof import("../testing.ts").until;
let api: typeof import("../../api/index.ts");
beforeAll(async () => {
  GlobalRegistrator.register();
  ({ RunCost } = await import("./RunCost.tsx"));
  ({ fakeApi, until } = await import("../testing.ts"));
  api = await import("../../api/index.ts");
  (globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;
});
afterAll(async () => {
  await GlobalRegistrator.unregister();
});

const run = { id: "r1", state: "succeeded", placements: [{ epoch: 1, host: "hA", hostName: "default-bukmjepf" }, { epoch: 2, host: "hB", hostName: "default-5ar45u7s" }] } as unknown as Run;
const pl = (epoch: number, hostId: string, amount: string) => ({ epoch, hostId, from: `2026-10-05T1${epoch}:00:00Z`, to: `2026-10-05T1${epoch}:30:00Z`, cpus: 2, memory: 1, amount, share: 0.47, ratePerHour: "1", finalized: true });
const line = (family: string, source: string, item: string, amount: string, placements?: object[]) => ({ runId: "r1", source, family, item, amount, currency: "USD", from: "2026-10-05T11:00:00Z", to: "2026-10-05T12:30:00Z", final: true, details: placements ? { placements } : null, reportedAt: "2026-10-05T13:00:00Z" });
const COST: RunCostData = {
  runId: "r1",
  status: "final",
  final: true,
  basis: "list",
  totals: [{ currency: "USD", amount: "2.8858", final: "2.8858", estimate: "0" }],
  byFamily: [
    { family: "ai", displayName: "AI models", color: "violet", currency: "USD", amount: "2.3114", final: "2.3114", estimate: "0" },
    { family: "block-storage", displayName: "Block storage", currency: "USD", amount: "0.0348", final: "0.0348", estimate: "0" },
    { family: "compute", displayName: "Compute", currency: "USD", amount: "0.5396", final: "0.5396", estimate: "0" },
  ],
  lines: [
    line("ai", "llm-proxy", "claude-opus-5-5", "2.3114"),
    line("block-storage", "compute", "gp3:100GiB", "0.0348", [pl(2, "hB", "0.0348")]),
    // Epoch 1 on a host whose volumes were not known: compute only.
    line("compute", "compute", "m8g.2xlarge:spot", "0.5396", [pl(1, "hA", "0.1974"), pl(2, "hB", "0.3422")]),
  ],
  sources: [{ source: "compute", status: "final" }, { source: "llm-proxy", status: "final" }],
};

async function render(cost: RunCostData) {
  api.signIn("k");
  api.setRole("tenant");
  const fake = fakeApi((path) => (path.startsWith("/v1/runs/r1/cost") ? cost : {}));
  (window as unknown as { happyDOM: { setURL: (u: string) => void } }).happyDOM.setURL("http://localhost/runs/r1?tab=resources");
  const el = document.createElement("div");
  document.body.appendChild(el);
  const root = createRoot(el);
  await act(async () => root.render(<RunCost run={run} />));
  return {
    el,
    card: (title: string) => [...el.querySelectorAll(".card")].find((c) => c.querySelector(".card-title")?.textContent === title),
    done: async () => {
      await act(async () => root.unmount());
      el.remove();
      fake.restore();
      api.signOut();
    },
  };
}

const cells = (card: Element | undefined) => [...(card?.querySelectorAll("tbody tr") ?? [])].map((tr) => [...tr.querySelectorAll("td")].map((td) => td.textContent?.trim()));

test("Run cost: Block storage is its own family row and line, named and swatched as luxd's family", async () => {
  const p = await render(COST);
  try {
    await until(() => p.card("Cost")?.querySelector("tbody tr") != null, "the lines");
    const card = p.card("Cost")!;
    const families = [...card.querySelectorAll(".kv-key, dt")].map((k) => k.textContent);
    expect(families.filter((f) => f && /models|Compute|Block/.test(f))).toEqual(["AI models", "Block storage", "Compute"]);
    const lines = cells(card);
    expect(lines.find((l) => l[1] === "gp3:100GiB")![0]).toBe("Block storage");
    // The swatch is the design system's fixed slot for block storage.
    const swatch = [...card.querySelectorAll(".color-key")].find((k) => k.textContent === "Block storage")?.querySelector<HTMLElement>(".color-key-swatch, [style]");
    expect(swatch?.getAttribute("style")).toContain("var(--chart-3)");
  } finally {
    await p.done();
  }
});

test("Run cost placements: one row per placement, compute and block storage apart, an en dash where a placement has no block storage", async () => {
  const p = await render(COST);
  try {
    await until(() => p.card("Placements") != null, "the placements card");
    const card = p.card("Placements")!;
    const rows = cells(card);
    expect(rows.map((r) => [r[0], r[1], r[3], r[4], r[5], r[6]])).toEqual([
      ["1", "default-bukmjepf", "47%", "$0.1974", "–", "$0.1974"],
      ["2", "default-5ar45u7s", "47%", "$0.3422", "$0.0348", "$0.377"],
    ]);
    // The host links to its page.
    expect(card.querySelector("tbody a")!.getAttribute("href")).toStartWith("/hosts/hA");
    expect(card.querySelector("[data-placements-total]")!.textContent).toBe("TotalCompute $0.5396Block storage $0.0348$0.5744");
  } finally {
    await p.done();
  }
});

test("Run cost placements: a non-zero share under 1% of the host reads <1%, never 0%", async () => {
  const small = { ...pl(1, "hA", "0.0012"), share: 0.004 };
  const p = await render({ ...COST, lines: [line("compute", "compute", "m8g.metal-48xl", "0.0012", [small])] });
  try {
    await until(() => p.card("Placements") != null, "the placements card");
    expect(cells(p.card("Placements")).map((r) => [r[0], r[3]])).toEqual([["1", "<1%"]]);
  } finally {
    await p.done();
  }
});

test("Run cost placements: no compute line (a pending Run, or only plugin lines) means no Placements card", async () => {
  const p = await render({ ...COST, lines: [line("ai", "llm-proxy", "claude", "1")] });
  try {
    await until(() => p.card("Cost")?.querySelector("tbody tr") != null, "the lines");
    expect(p.card("Placements")).toBeUndefined();
  } finally {
    await p.done();
  }
});

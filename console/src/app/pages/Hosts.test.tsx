import { afterAll, beforeAll, expect, test } from "bun:test";
import { GlobalRegistrator } from "@happy-dom/global-registrator";
import { act } from "react";
import { createRoot } from "react-dom/client";
import type { HostSummary } from "../../api/index.ts";

// The page's imports (the router) touch window at load: register the DOM first.
let HostsList: typeof import("./Hosts.tsx").HostsList;
let ScopeProvider: typeof import("../scope.tsx").ScopeProvider;
let formatBytes: typeof import("@lux/design-system").formatBytes;
let formatCores: typeof import("@lux/design-system").formatCores;
let fakeApi: typeof import("../testing.ts").fakeApi;
let api: typeof import("../../api/index.ts");
beforeAll(async () => {
  GlobalRegistrator.register();
  ({ HostsList } = await import("./Hosts.tsx"));
  ({ ScopeProvider } = await import("../scope.tsx"));
  ({ formatBytes, formatCores } = await import("@lux/design-system"));
  ({ fakeApi } = await import("../testing.ts"));
  api = await import("../../api/index.ts");
  (globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;
});
afterAll(async () => {
  await GlobalRegistrator.unregister();
});

const sleep = (ms: number) => act(() => new Promise<void>((r) => setTimeout(r, ms)));

const GiB = 1024 ** 3;
const SUMMARY: HostSummary = { live: 2, capacity: { cpus: 16, memory: 64 * GiB }, allocated: { cpus: 4, memory: 8 * GiB } };

/**
 * The hosts list with a fake API: an empty hosts page, and summary(path)
 * as /v1/hosts/summary's answer. url is the page's (its ?tenant= narrows
 * an operator).
 */
async function render(opts: { summary: (path: string) => unknown; role?: "tenant" | "operator"; url?: string; embedded?: boolean }) {
  api.signIn("k");
  api.setRole(opts.role ?? "tenant");
  const fake = fakeApi((path) => {
    if (path.startsWith("/v1/hosts/summary")) return opts.summary(path);
    if (path.startsWith("/v1/hosts")) return { hosts: [], total: 0, offset: 0 };
    if (path.startsWith("/v1/pools")) return { pools: [] };
    return {};
  });
  (window as unknown as { happyDOM: { setURL: (u: string) => void } }).happyDOM.setURL(opts.url ?? "http://localhost/hosts");
  const el = document.createElement("div");
  document.body.appendChild(el);
  const root = createRoot(el);
  await act(async () =>
    root.render(
      <ScopeProvider>
        <HostsList pool="" poolId={opts.embedded ? "p1" : undefined} embedded={opts.embedded} />
      </ScopeProvider>,
    ),
  );
  await sleep(50);
  return {
    fake,
    summaryCalls: () => fake.calls.filter((c) => c.startsWith("/v1/hosts/summary")),
    header: () => el.querySelector(".page-desc")?.textContent,
    done: async () => {
      await act(async () => root.unmount());
      el.remove();
      fake.restore();
      api.signOut();
    },
  };
}

test("the Hosts header shows the served summary, allocated of capacity", async () => {
  const p = await render({ summary: () => SUMMARY });
  try {
    expect(p.header()).toBe(`0 hosts · 2 live · ready and draining: ${formatCores(4)} of ${formatCores(16)} CPU, ${formatBytes(8 * GiB)} of ${formatBytes(64 * GiB)} memory allocated`);
    expect(p.summaryCalls()).toEqual(["/v1/hosts/summary"]);
  } finally {
    await p.done();
  }
});

test("the Hosts header shows a dash while the summary is pending, not zeros", async () => {
  const p = await render({ summary: () => new Promise(() => {}) });
  try {
    expect(p.summaryCalls().length).toBe(1);
    expect(p.header()).toBe("0 hosts · –");
  } finally {
    await p.done();
  }
});

test("the Hosts header says the summary is unavailable when it fails", async () => {
  const p = await render({ summary: () => new Response(JSON.stringify({ error: { code: "internal", message: "boom" } }), { status: 500, headers: { "Content-Type": "application/json" } }) });
  try {
    expect(p.header()).toBe("0 hosts · summary unavailable");
  } finally {
    await p.done();
  }
});

test("an operator narrowed to a tenant reads that tenant's summary", async () => {
  const p = await render({ role: "operator", url: "http://localhost/hosts?tenant=acme", summary: () => SUMMARY });
  try {
    expect(p.summaryCalls()).toEqual(["/v1/hosts/summary?tenant=acme"]);
  } finally {
    await p.done();
  }
});

test("the pool page's embedded hosts list reads no summary", async () => {
  const p = await render({ embedded: true, summary: () => SUMMARY });
  try {
    expect(p.fake.calls.some((c) => c.startsWith("/v1/hosts?"))).toBe(true);
    expect(p.summaryCalls()).toEqual([]);
  } finally {
    await p.done();
  }
});

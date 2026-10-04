import { afterAll, beforeAll, expect, test } from "bun:test";
import { GlobalRegistrator } from "@happy-dom/global-registrator";
import { act } from "react";
import { createRoot } from "react-dom/client";
import type { Tenant } from "../../api/index.ts";

// The page's imports (the router) touch window at load: register the DOM first.
let Tenants: typeof import("./Tenants.tsx").Tenants;
let ScopeProvider: typeof import("../scope.tsx").ScopeProvider;
let fakeApi: typeof import("../testing.ts").fakeApi;
let api: typeof import("../../api/index.ts");
beforeAll(async () => {
  GlobalRegistrator.register();
  ({ Tenants } = await import("./Tenants.tsx"));
  ({ ScopeProvider } = await import("../scope.tsx"));
  ({ fakeApi } = await import("../testing.ts"));
  api = await import("../../api/index.ts");
  (globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;
});
afterAll(async () => {
  await GlobalRegistrator.unregister();
});

const sleep = (ms: number) => act(() => new Promise<void>((r) => setTimeout(r, ms)));

/** Polls (10 ms steps, inside act) until ok() holds; fails after 5 s. */
async function until(ok: () => boolean, what: string) {
  for (const end = Date.now() + 5000; !ok(); ) {
    if (Date.now() > end) throw new Error(`never: ${what}`);
    await sleep(10);
  }
}

/** n's text as a reader gets it: without aria-hidden decoration (a sort arrow). */
function visibleText(n: Element): string {
  const c = n.cloneNode(true) as Element;
  for (const hidden of c.querySelectorAll('[aria-hidden="true"]')) hidden.remove();
  return c.textContent?.trim() ?? "";
}

const tenant = (id: string, expireAfterDays: number): Tenant => ({
  id,
  name: id,
  retentionDays: 30,
  expireAfterDays,
  activeRuns: 0,
  runs: 0,
  hosts: 0,
  storedBytes: 0,
  createdAt: "2026-01-01T00:00:00Z",
});

/** The Tenants page as an operator, /v1/tenants answering tenants. */
async function render(tenants: Tenant[]) {
  api.signIn("k");
  api.setRole("operator");
  const fake = fakeApi((path) => (path.startsWith("/v1/tenants") ? { tenants } : {}));
  (window as unknown as { happyDOM: { setURL: (u: string) => void } }).happyDOM.setURL("http://localhost/tenants");
  const el = document.createElement("div");
  document.body.appendChild(el);
  const root = createRoot(el);
  await act(async () =>
    root.render(
      <ScopeProvider>
        <Tenants />
      </ScopeProvider>,
    ),
  );
  const table = () => el.querySelector("table");
  // Column headers and body rows by the elements carrying those table roles
  // (th: columnheader, tbody tr: row, td: cell), never the design system's
  // wrapper classes.
  const headerCells = () => [...(table()?.querySelectorAll("thead th") ?? [])];
  const headers = () => headerCells().map(visibleText);
  const bodyRows = () => [...(table()?.querySelectorAll("tbody tr") ?? [])];
  const cellTexts = () => bodyRows().map((tr) => [...tr.querySelectorAll("td")].map((td) => visibleText(td)));
  const done = async () => {
    await act(async () => root.unmount());
    el.remove();
    fake.restore();
    api.signOut();
  };
  try {
    await until(() => cellTexts().some((cells) => cells.includes(tenants[0]!.name)), `a row for ${tenants[0]!.name}`);
  } catch (e) {
    await done();
    throw e;
  }
  return {
    // Each row's tenant name and Expiry cell, in the order shown.
    expiries: () => {
      const name = headers().indexOf("Tenant");
      const exp = headers().indexOf("Expiry");
      return cellTexts().map((cells) => [cells[name], cells[exp]]);
    },
    sortByExpiry: async () => {
      const th = headerCells().find((h) => visibleText(h) === "Expiry") as HTMLElement;
      await act(async () => th.click());
    },
    done,
  };
}

test("the Tenants page shows each tenant's expiry, never for 0", async () => {
  const p = await render([tenant("acme", 90), tenant("brisk", 7), tenant("keep", 0)]);
  try {
    expect(p.expiries()).toEqual([
      ["acme", "90d"],
      ["brisk", "7d"],
      ["keep", "never"],
    ]);
  } finally {
    await p.done();
  }
});

test("sorted by Expiry, never is the longest", async () => {
  const p = await render([tenant("keep", 0), tenant("acme", 90), tenant("brisk", 7)]);
  try {
    await p.sortByExpiry();
    expect(p.expiries().map((r) => r[0])).toEqual(["keep", "acme", "brisk"]);
    await p.sortByExpiry();
    expect(p.expiries().map((r) => r[0])).toEqual(["brisk", "acme", "keep"]);
  } finally {
    await p.done();
  }
});

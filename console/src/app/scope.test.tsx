import { afterAll, beforeAll, expect, test } from "bun:test";
import { GlobalRegistrator } from "@happy-dom/global-registrator";
import { act } from "react";
import { createRoot } from "react-dom/client";

// The router touches window at load: register the DOM first.
let ScopeProvider: typeof import("./scope.tsx").ScopeProvider;
let useScope: typeof import("./scope.tsx").useScope;
let scoped: typeof import("./router.tsx").scoped;
beforeAll(async () => {
  GlobalRegistrator.register();
  ({ ScopeProvider, useScope } = await import("./scope.tsx"));
  ({ scoped } = await import("./router.tsx"));
  (globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;
});
afterAll(async () => {
  await GlobalRegistrator.unregister();
});

/** The scope at url, and a way to act on it. */
async function at(url: string) {
  (window as unknown as { happyDOM: { setURL: (u: string) => void } }).happyDOM.setURL(url);
  let scope: ReturnType<typeof useScope> | undefined;
  const Probe = () => {
    scope = useScope();
    return null;
  };
  const el = document.createElement("div");
  const root = createRoot(el);
  await act(async () =>
    root.render(
      <ScopeProvider>
        <Probe />
      </ScopeProvider>,
    ),
  );
  return {
    scope: () => scope!,
    done: () => act(async () => root.unmount()),
  };
}

test("every, cost, by, label and nolabel read from the URL; anything unknown is the default", async () => {
  for (const [url, every, show, by] of [
    ["http://localhost/", "auto", "all", "family"],
    ["http://localhost/?every=minute&cost=compute&by=key", "minute", "compute", "key"],
    ["http://localhost/?every=day&range=7d&cost=external&by=label:app", "day", "external", "label"],
    ["http://localhost/?every=week&cost=machine&by=tenant", "auto", "all", "family"],
  ] as const) {
    const p = await at(url);
    expect([url, p.scope().every, p.scope().costShow, p.scope().costBy.kind]).toEqual([url, every, show, by]);
    await p.done();
  }
  const p = await at("http://localhost/?label=app%3Djervasion&label=app%3Ddude&label=note%3Da%3Db&nolabel=phase");
  expect(p.scope().costFilters).toEqual([
    { key: "app", values: ["jervasion", "dude"] },
    { key: "note", values: ["a=b"] },
    { key: "phase", values: [], notSet: true },
  ]);
  await p.done();
});

test("a disabled Every in the URL is in effect Auto, and its charts say so", async () => {
  const p = await at("http://localhost/?every=day");
  expect([p.scope().every, p.scope().everyInEffect, p.scope().step("trend").auto]).toEqual(["day", "auto", true]);
  await p.done();
});

test("setting Every, Show, Break down by and filters writes the URL; the defaults leave it", async () => {
  const p = await at("http://localhost/?range=7d");
  await act(async () => p.scope().setEvery("day"));
  await act(async () => p.scope().setCostShow("external"));
  await act(async () => p.scope().setCostBy({ kind: "label", key: "app" }));
  await act(async () => p.scope().setCostFilters([{ key: "app", values: ["a", "b"] }, { key: "phase", values: [], notSet: true }]));
  expect(window.location.search).toBe("?range=7d&every=day&cost=external&by=label%3Aapp&label=app%3Da&label=app%3Db&nolabel=phase");
  expect([p.scope().every, p.scope().costShow, p.scope().costBy]).toEqual(["day", "external", { kind: "label", key: "app" }]);
  await act(async () => p.scope().setEvery("auto"));
  await act(async () => p.scope().setCostShow("all"));
  await act(async () => p.scope().setCostBy({ kind: "family" }));
  await act(async () => p.scope().setCostFilters([]));
  expect(window.location.search).toBe("?range=7d");
  await p.done();
});

test("links carry every and cost with the tenant and range; per, by and filters stay on the page", () => {
  expect(scoped("/runs", "?tenant=acme&range=7d&every=hour&cost=compute&by=key&label=app%3Da&per=day&tab=x")).toBe("/runs?tenant=acme&range=7d&every=hour&cost=compute");
  expect(scoped("/?cost=external", "?cost=compute")).toBe("/?cost=external");
});

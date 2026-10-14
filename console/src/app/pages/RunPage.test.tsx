import { afterAll, beforeAll, expect, test } from "bun:test";
import { GlobalRegistrator } from "@happy-dom/global-registrator";
import { act } from "react";
import { createRoot } from "react-dom/client";

// The page's imports (the router) touch window at load: register the DOM first.
let RunPage: typeof import("./RunPage.tsx").RunPage;
let ScopeProvider: typeof import("../scope.tsx").ScopeProvider;
let ToastProvider: typeof import("@lux/design-system").ToastProvider;
let fakeApi: typeof import("../testing.ts").fakeApi;
let until: typeof import("../testing.ts").until;
let api: typeof import("../../api/index.ts");
beforeAll(async () => {
  GlobalRegistrator.register();
  ({ RunPage } = await import("./RunPage.tsx"));
  ({ ScopeProvider } = await import("../scope.tsx"));
  ({ ToastProvider } = await import("@lux/design-system"));
  ({ fakeApi, until } = await import("../testing.ts"));
  api = await import("../../api/index.ts");
  (globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;
});
afterAll(async () => {
  await GlobalRegistrator.unregister();
});

const run = (adapter: string) => ({ id: "run_1", tenant: "t", state: "succeeded", epoch: 1, labels: {}, createdAt: new Date(0).toISOString(), spec: { image: { ref: "img" }, resources: {}, workload: { adapter } } });

async function render(adapter: string, url: string) {
  api.signIn("k");
  const fake = fakeApi((path) => (path.startsWith("/v1/runs/run_1/output") ? new Response("event: end\ndata: {}\n\n", { headers: { "Content-Type": "text/event-stream" } }) : path.startsWith("/v1/runs/run_1") ? run(adapter) : {}));
  (window as unknown as { happyDOM: { setURL: (u: string) => void } }).happyDOM.setURL(url);
  const el = document.createElement("div");
  document.body.appendChild(el);
  const root = createRoot(el);
  await act(async () =>
    root.render(
      <ToastProvider>
        <ScopeProvider>
          <RunPage id="run_1" />
        </ScopeProvider>
      </ToastProvider>,
    ),
  );
  await until(() => el.querySelector('[role="tab"]') != null, "Run tabs");
  const tabs = () => [...el.querySelectorAll('[role="tab"]')].map((t) => t.textContent?.trim().split("\n")[0]);
  const selected = () => el.querySelector('[role="tab"][aria-selected="true"]')?.textContent?.trim();
  return {
    tabs,
    selected,
    done: async () => {
      await act(async () => root.unmount());
      el.remove();
      fake.restore();
      api.signOut();
    },
  };
}

test("?tab=session on a generic Run selects Output and draws no Session tab", async () => {
  const r = await render("generic", "http://localhost/runs/run_1?tab=session");
  try {
    expect(r.selected()).toBe("Output");
    expect(r.tabs()).not.toContain("Session");
  } finally {
    await r.done();
  }
});

test("an opencode Run without ?tab opens on Session", async () => {
  const r = await render("opencode", "http://localhost/runs/run_1");
  try {
    expect(r.tabs()[0]).toBe("Session");
    expect(r.selected()).toBe("Session");
  } finally {
    await r.done();
  }
});

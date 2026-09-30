import { afterAll, beforeAll, expect, test } from "bun:test";
import { GlobalRegistrator } from "@happy-dom/global-registrator";
import { act } from "react";
import { createRoot } from "react-dom/client";

// The page's imports (the router) touch window at load: register the DOM first.
let Runs: typeof import("./Runs.tsx").Runs;
let ScopeProvider: typeof import("../scope.tsx").ScopeProvider;
let api: typeof import("../../api/index.ts");
let fakeApi: typeof import("../testing.ts").fakeApi;
beforeAll(async () => {
  GlobalRegistrator.register();
  ({ Runs } = await import("./Runs.tsx"));
  ({ ScopeProvider } = await import("../scope.tsx"));
  ({ fakeApi } = await import("../testing.ts"));
  api = await import("../../api/index.ts");
  (globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;
});
afterAll(async () => {
  await GlobalRegistrator.unregister();
});

const sleep = (ms: number) => act(() => new Promise<void>((r) => setTimeout(r, ms)));

test("the Runs list refetches on a Run event (runs: prefix) and, while the stream is live, does not poll every 5 s", async () => {
  const fake = fakeApi((path) => (path.startsWith("/v1/runs") ? { runs: [] } : path.startsWith("/v1/history") ? { samples: [] } : {}));
  let status = "";
  function Stream() {
    api.useLiveStream(undefined);
    status = api.useLiveStatus();
    return null;
  }
  const lists = () => fake.calls.filter((c) => c.startsWith("/v1/runs?") || c === "/v1/runs").length;
  const root = createRoot(document.createElement("div"));
  try {
    await act(async () => root.render(<Stream />));
    for (let i = 0; i < 50 && status !== "live"; i++) await sleep(10);
    expect(status).toBe("live");
    await act(async () =>
      root.render(
        <>
          <Stream />
          <ScopeProvider>
            <Runs />
          </ScopeProvider>
        </>,
      ),
    );
    await sleep(50);
    expect(lists()).toBe(1);
    // live.ts invalidates Run lists by the runs: prefix.
    api.invalidate((k) => k.startsWith("runs:"));
    await sleep(400);
    expect(lists()).toBe(2);
    // The 5 s interval would have polled by now; the 60 s live poll has not.
    await sleep(5200);
    expect(lists()).toBe(2);
  } finally {
    await act(async () => root.unmount());
    fake.restore();
  }
}, 10_000);

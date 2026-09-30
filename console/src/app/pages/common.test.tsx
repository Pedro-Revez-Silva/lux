import { afterAll, beforeAll, expect, test } from "bun:test";
import { GlobalRegistrator } from "@happy-dom/global-registrator";
import { act } from "react";
import { createRoot } from "react-dom/client";
import type { Run } from "../../api/index.ts";

let common: typeof import("./common.tsx");
beforeAll(async () => {
  GlobalRegistrator.register();
  common = await import("./common.tsx");
  (globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;
});
afterAll(async () => {
  await GlobalRegistrator.unregister();
});

const run = (o: Partial<Run>): Run =>
  ({ id: "r1", tenant: "t", labels: {}, state: "running", epoch: 1, spec: { workload: { adapter: "generic" } }, secrets: [], createdAt: new Date().toISOString(), runtimeSeconds: 0, placementSeconds: 0, placementWaitSeconds: 0, placementStartSeconds: 0, ...o }) as Run;

async function text(node: React.ReactNode): Promise<string> {
  const el = document.createElement("div");
  const root = createRoot(el);
  await act(async () => root.render(node));
  const t = el.textContent ?? "";
  await act(async () => root.unmount());
  return t;
}

test("a negative runtime or placement time from the server reads 0s, never a minus", async () => {
  const { RuntimeCell, PlacementTimeCell } = common;
  expect(await text(<RuntimeCell run={run({ runtimeSeconds: -161 })} />)).toBe("0s");
  expect(await text(<RuntimeCell run={run({ runtimeSeconds: -161, runtimeSince: new Date().toISOString() })} />)).toBe("0s");
  expect(await text(<PlacementTimeCell run={run({ placementSeconds: -161, placementWaitSeconds: -100, placementStartSeconds: -61 })} />)).toBe("0s");
  expect(await text(<RuntimeCell run={run({ runtimeSeconds: 161 })} />)).toBe("2m 41s");
  expect(await text(<PlacementTimeCell run={run({ placementSeconds: 12 })} />)).toBe("12s");
  // Never run: a dash.
  expect(await text(<RuntimeCell run={run({ epoch: 0 })} />)).toBe("–");
});

test("ratioText: one unit when both figures share it", () => {
  const { ratioText } = common;
  expect(ratioText(162, 576, "cores")).toBe("162 / 576 cores");
  expect(ratioText(4 * 1024 ** 3, 16 * 1024 ** 3, "bytes")).toBe("4 / 16 GiB");
  expect(ratioText(512 * 1024 ** 2, 16 * 1024 ** 3, "bytes")).toBe("512 MiB / 16 GiB");
});

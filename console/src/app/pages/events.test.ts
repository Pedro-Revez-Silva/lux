import { expect, test } from "bun:test";
import { eventSummary, infraEventSummary } from "./events.ts";

function submitted(data: Record<string, unknown>): string {
  return eventSummary({ id: 1, type: "submitted", data: { by: "k1", ...data }, time: "" });
}

test("a submitted Run's pool names its owner and why it got it", () => {
  expect(submitted({ pool: "burst", poolFrom: "platform-default", poolOwner: "platform" })).toBe(
    "by k1 · pool burst (platform, the platform's default)",
  );
  expect(submitted({ pool: "burst", poolFrom: "spec", poolOwner: "tenant" })).toBe("by k1 · pool burst (tenant)");
});

test("without an owner, as for a name no pool has, only why", () => {
  expect(submitted({ pool: "default", poolFrom: "fallback" })).toBe("by k1 · pool default (no default pool marked)");
  expect(submitted({ pool: "gpu", poolFrom: "spec" })).toBe("by k1 · pool gpu");
  expect(submitted({})).toBe("by k1");
});

test("a pool's rename says from, to, what followed, and a kept default", () => {
  const e = { id: 1, type: "pool.renamed", count: 1, time: "", data: { from: "burst", to: "burst-eu", hosts: 2, runs: 1, instances: 0, isDefault: true } };
  expect(infraEventSummary(e)).toBe("burst → burst-eu: 2 hosts, 1 Runs, 0 instances followed; still the default");
  expect(infraEventSummary({ ...e, data: { ...e.data, isDefault: false } })).toBe("burst → burst-eu: 2 hosts, 1 Runs, 0 instances followed");
});

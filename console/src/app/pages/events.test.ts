import { expect, test } from "bun:test";
import { eventSummary } from "./events.ts";

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

import { expect, test } from "bun:test";
import type { Pool } from "../../api/index.ts";
import { currentDefaultText } from "./defaultPool.ts";

function pool(name: string, owner: string, isDefault = false): Pool {
  return { name, tenant: owner || undefined, platform: owner === "", isDefault, provider: "static", minHosts: 0, maxHosts: 0, warmHosts: 0, shared: false };
}

test("a tenant's own default is named first", () => {
  const pools = [pool("arm", "t1", true), pool("x86", "t1"), pool("plat", "", true)];
  expect(currentDefaultText(pools, pool("x86", "t1"), "your")).toBe("arm is your default pool now.");
});

test("without one of its own, the platform's default is where Runs go", () => {
  const pools = [pool("x86", "t1"), pool("plat", "", true), pool("theirs", "t2", true)];
  expect(currentDefaultText(pools, pool("x86", "t1"), "tenant t1's")).toBe(
    "None of tenant t1's pools is the default, so Runs naming no pool go to the platform's default pool, plat, now.",
  );
});

test("with neither, a pool named default", () => {
  const pools = [pool("x86", "t1"), pool("plat", ""), pool("theirs", "t2", true)];
  expect(currentDefaultText(pools, pool("x86", "t1"), "your")).toBe(
    'None of your pools is the default, and the platform has none: Runs naming no pool go to a pool named "default" now.',
  );
});

test("a platform pool is measured against the platform's default only", () => {
  expect(currentDefaultText([pool("a", "", true), pool("b", "")], pool("b", ""), "the platform's")).toBe("a is the platform's default pool now.");
  expect(currentDefaultText([pool("a", "t1", true), pool("b", "")], pool("b", ""), "the platform's")).toBe(
    'The platform has no default pool now: Runs naming no pool go to a pool named "default", for tenants without a default pool of their own.',
  );
});

import { expect, test } from "bun:test";
import { sumMoney } from "@lux/design-system";
import { hostCost, poolCost, runCost } from "../mockHostCost.ts";

const DEFAULT = { id: "pool_default01", tenantId: null };
const ACME_CI = { id: "pool_acme_ci01", tenantId: "ten_acme" };
type PoolBody = { idle?: { family: string; amount: string }[]; hosts?: { hostId: string; family: string; allocated: string; unallocated: string; hours?: number }[]; hostSeries?: unknown[]; series: { family: string }[]; totals: { amount: string }[] };
const pool = (p: { id: string; tenantId: string | null }, tenant: string | null) => poolCost(new URL("http://x/?since=24h&interval=hour"), p, tenant) as PoolBody;

// The mock answers as poolCost does: host time per family to operators and the owner tenant only.
test("mock pool cost: host time per family, hidden from a tenant on a platform pool, shown on its own", () => {
  const op = pool(DEFAULT, null);
  expect(op.idle!.map((i) => i.family)).toEqual(["block-storage", "compute"]);
  expect(new Set(op.series.map((s) => s.family))).toEqual(new Set(["compute", "block-storage"]));
  // The host whose volumes are not known has compute rows only.
  expect(op.hosts!.filter((h) => h.hostId === "host_9p4tz6c1mh").map((h) => h.family)).toEqual(["compute"]);
  // Idle per family is the hosts' unallocated summed.
  for (const f of ["compute", "block-storage"]) {
    expect(op.idle!.find((i) => i.family === f)!.amount).toBe(sumMoney(op.hosts!.filter((h) => h.family === f).map((h) => h.unallocated))!);
  }
  const tenantOnPlatform = pool(DEFAULT, "ten_acme");
  expect(tenantOnPlatform.idle).toBeUndefined();
  expect(tenantOnPlatform.hosts).toBeUndefined();
  expect(tenantOnPlatform.hostSeries).toBeUndefined();
  expect(Number(tenantOnPlatform.totals[0]!.amount)).toBeLessThan(Number(op.totals[0]!.amount));
  const own = pool(ACME_CI, "ten_acme");
  expect(own.idle!.length).toBe(2);
});

test("mock host cost: hours per family; unallocated and rates as luxd scopes them", () => {
  const u = new URL("http://x/?since=24h");
  type H = { hours: { family: string; unallocated?: string }[]; rates?: { family: string; details?: unknown }[] };
  const op = hostCost(u, "host_7f2cq9m1x0", null, null) as H;
  expect(new Set(op.hours.map((h) => h.family))).toEqual(new Set(["compute", "block-storage"]));
  expect(op.hours.every((h) => h.unallocated != null)).toBe(true);
  expect(op.rates!.map((r) => r.family)).toEqual(["block-storage", "compute", "compute"]);
  expect(op.rates![0]!.details).toBeDefined();
  const own = hostCost(u, "host_acme0ci001", "ten_acme", "ten_acme") as H;
  expect(own.hours.every((h) => h.unallocated != null) && own.rates === undefined).toBe(true);
});

test("mock run cost: block storage beside compute for placements on hosts with known volumes", () => {
  const ago = (s: number) => new Date(Date.now() - s * 1000).toISOString();
  const c = runCost("r", [
    { epoch: 1, hostId: "host_9p4tz6c1mh", assignedAt: ago(2700), exitedAt: ago(1800) },
    { epoch: 2, hostId: "host_7f2cq9m1x0", assignedAt: ago(1800), exitedAt: ago(900) },
  ]) as { status: string; lines: { family: string; details: { placements: { epoch: number }[] } | null }[] };
  const fam = (f: string) => c.lines.find((l) => l.family === f)!.details!.placements.map((p) => p.epoch);
  expect(fam("compute")).toEqual([1, 2]);
  expect(fam("block-storage")).toEqual([2]);
  expect(c.status).toBe("incomplete");
});

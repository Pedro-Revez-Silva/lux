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

test("a rename says from and to", () => {
  expect(infraEventSummary({ id: 1, type: "pool.renamed", data: { from: "burst", to: "burst2" }, count: 1, time: "" })).toBe("burst → burst2");
});

function infra(type: string, data: Record<string, unknown>): string {
  return infraEventSummary({ id: 1, type, data, count: 1, time: "" });
}

const GiB = 1024 ** 3;

test("a scale-up from before capacity planning keeps its line", () => {
  expect(infra("pool.scale_up", { hosts: 2, reason: "waiting runs", waiting: 2, warm: 0, min: 0, max: 10, total: 1, idle: 0, provisioning: 1 })).toBe(
    "+2 hosts for waiting runs: 2 waiting, warm 0, min 0, max 10; had 1 (0 idle, 1 provisioning)",
  );
});

test("a planned scale-up adds its plan, expected capacity and evidence", () => {
  const data = {
    hosts: 1, reason: "waiting runs", waiting: 3, warm: 0, min: 0, max: 10, total: 2, idle: 1, provisioning: 1,
    ready: 1, future: 1, planned: 1, unmet: 0, blocked: 1,
    expected: { capacity: { cpus: 8, memory: 32 * GiB, disk: 0, runs: 4 }, observations: 3 },
    deficits: [{ run: "r4", stage: "prerequisite", blockers: [{ reason: "snapshot upload pending" }] }],
    exhausted: [{ run: "r3", host: "h1", stage: "ready", blockers: [
      { resource: "memory", requested: 16 * GiB, used: 24 * GiB, capacity: 32 * GiB, available: 8 * GiB },
      { resource: "cpus", requested: 4, used: 6, capacity: 8, available: 2 },
    ] }],
    ineligible: [{ host: "h2", reason: "draining" }],
    omitted: 2,
  };
  expect(infra("pool.scale_up", data)).toBe(
    "+1 host for waiting runs: 3 waiting, warm 0, min 0, max 10; had 2 (1 idle, 1 provisioning); " +
      "plan: 1 ready, 1 future, 1 planned, 0 unmet, 1 blocked; " +
      "new host cpus 8, memory 32 GiB, disk unlimited, runs 4 from 3 observation(s); " +
      "deficits: r4 prerequisite [snapshot upload pending]; " +
      "exhausted: h1 (ready) for r3 [memory requested 16 GiB, used 24 GiB, capacity 32 GiB, available 8 GiB; cpus requested 4, used 6, capacity 8, available 2]; " +
      "ineligible: h2 draining; 2 more omitted",
  );
});

test("a cold pool's scale-up says new-host capacity is unknown", () => {
  const data = {
    hosts: 1, reason: "waiting runs", waiting: 1, warm: 0, min: 0, max: 0, total: 0, idle: 0, provisioning: 0,
    ready: 0, future: 0, planned: 0, unmet: 1, blocked: 0, expected: null,
    unknown: "no registered host observations for current template",
    deficits: [{ run: "r1", stage: "new_host", blockers: [{ reason: "new host capacity unknown" }] }],
  };
  expect(infra("pool.scale_up", data)).toBe(
    "+1 host for waiting runs: 1 waiting, warm 0, min 0, max 0; had 0 (0 idle, 0 provisioning); " +
      "plan: 0 ready, 0 future, 0 planned, 1 unmet, 0 blocked; " +
      "new host capacity unknown: no registered host observations for current template; " +
      "deficits: r1 new host [new host capacity unknown]",
  );
});

test("a host's capacity decision says why", () => {
  expect(infra("host.capacity_decision", { pool: "burst", stage: "ready", decision: "exhausted", blockers: [
    { resource: "disk", requested: 10 * GiB, used: 100 * GiB, capacity: 100 * GiB, available: 0 },
    { resource: "runs", requested: 1, used: 4, capacity: 4, available: 0 },
  ] })).toBe("exhausted (ready) in pool burst: disk requested 10 GiB, used 100 GiB, capacity 100 GiB, available 0 B; runs requested 1, used 4, capacity 4, available 0");
  expect(infra("host.capacity_decision", { pool: "burst", stage: "starting", decision: "blocked", blockers: [{ reason: "required labels do not match" }] })).toBe(
    "blocked (starting) in pool burst: required labels do not match",
  );
  expect(infra("host.capacity_decision", { pool: "burst", stage: "ready", decision: "ineligible", reason: "heartbeat stale" })).toBe(
    "ineligible (ready) in pool burst: heartbeat stale",
  );
  expect(infra("host.capacity_decision", { pool: "burst", stage: "starting", decision: "reserved" })).toBe("reserved (starting) in pool burst");
});

test("a placement shows its resources when it has them", () => {
  expect(infra("pool.placement", { run: "r1", epoch: 2, host: "h1", resources: { cpus: 2, memory: 4 * GiB, disk: 20 * GiB } })).toBe(
    "r1 epoch 2 on h1 (cpus 2, memory 4 GiB, disk 20 GiB)",
  );
  expect(infra("host.placement_assigned", { run: "r1", epoch: 1, host: "h1" })).toBe("r1 epoch 1 on h1");
});

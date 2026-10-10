import { expect, test } from "bun:test";
import { sumHostTime, sumIdle } from "./poolHostTime.ts";

const at = "2026-10-10T10:00:00Z";
const next = "2026-10-10T11:00:00Z";

test("host series sum compute and block storage per bucket and currency, exactly", () => {
  const rows = sumHostTime([
    { at, family: "block-storage", currency: "USD", allocated: "0.000000398", unallocated: "0.000016052" },
    { at, family: "compute", currency: "USD", allocated: "0.1", unallocated: "0.2" },
    { at, family: "compute", currency: "EUR", allocated: "1", unallocated: "2" },
    { at: next, family: "compute", currency: "USD", allocated: "0.3", unallocated: "0" },
  ]);
  expect(rows).toEqual([
    { at, hostId: undefined, hostName: undefined, currency: "USD", allocated: "0.100000398", unallocated: "0.200016052" },
    { at, hostId: undefined, hostName: undefined, currency: "EUR", allocated: "1", unallocated: "2" },
    { at: next, hostId: undefined, hostName: undefined, currency: "USD", allocated: "0.3", unallocated: "0" },
  ]);
});

test("hosts get one row per host and currency, their families summed", () => {
  const rows = sumHostTime([
    { hostId: "ha", hostName: "a", family: "block-storage", currency: "USD", allocated: "0.03", unallocated: "0.01" },
    { hostId: "ha", hostName: "a", family: "compute", currency: "USD", allocated: "0.3", unallocated: "0.1" },
    { hostId: "hb", hostName: "b", family: "compute", currency: "USD", allocated: "0.2", unallocated: "0.9" },
  ]);
  expect(rows.map((r) => [r.hostId, r.currency, r.allocated, r.unallocated])).toEqual([
    ["ha", "USD", "0.33", "0.11"],
    ["hb", "USD", "0.2", "0.9"],
  ]);
});

test("idle is one amount per currency, its families summed", () => {
  expect(
    sumIdle([
      { currency: "USD", amount: "0.01" },
      { currency: "EUR", amount: "5" },
      { currency: "USD", amount: "0.1" },
    ]),
  ).toEqual([
    { currency: "EUR", amount: "5" },
    { currency: "USD", amount: "0.11" },
  ]);
  expect(sumIdle(undefined)).toEqual([]);
});

// The pool page's host time, per currency: luxd sends one row per host-tied
// family (compute, block-storage) and currency; this page shows their sum
// until it shows the families apart. Exact: amounts stay decimal strings.
import { sumMoney } from "@lux/design-system";
import type { MoneyTotal, PoolHostTime } from "../../api/index.ts";

/** Rows summed across families per (bucket or host, currency), in first-seen order. */
export function sumHostTime(rows: readonly PoolHostTime[]): PoolHostTime[] {
  const out = new Map<string, { row: PoolHostTime; allocated: string[]; unallocated: string[] }>();
  for (const r of rows) {
    const k = `${r.at ?? ""}\u0000${r.hostId ?? ""}\u0000${r.currency}`;
    const e = out.get(k) ?? { row: { at: r.at, hostId: r.hostId, hostName: r.hostName, currency: r.currency, allocated: "0", unallocated: "0" }, allocated: [], unallocated: [] };
    e.allocated.push(r.allocated);
    e.unallocated.push(r.unallocated);
    out.set(k, e);
  }
  return [...out.values()].map((e) => ({ ...e.row, allocated: sumMoney(e.allocated) ?? "0", unallocated: sumMoney(e.unallocated) ?? "0" }));
}

/** Idle amounts summed across families per currency, sorted by currency. */
export function sumIdle(idle: readonly MoneyTotal[] | undefined): MoneyTotal[] {
  const by = new Map<string, string[]>();
  for (const i of idle ?? []) by.set(i.currency, [...(by.get(i.currency) ?? []), i.amount]);
  return [...by].sort(([a], [b]) => a.localeCompare(b)).map(([currency, amounts]) => ({ currency, amount: sumMoney(amounts) ?? "0" }));
}

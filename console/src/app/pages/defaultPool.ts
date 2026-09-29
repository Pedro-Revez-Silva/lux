import type { Pool } from "../../api/index.ts";

/** The owner a pool's default mark is among: its tenant's name, or "" for the platform. */
export function poolOwner(p: Pool): string {
  return p.platform ? "" : p.tenant ?? "";
}

/**
 * Where Runs naming no pool go now, for the Make default dialog on `marking`
 * (whose: "your", "tenant t1's" or "the platform's"). The order is luxd's:
 * the tenant's default, else the platform's, else a pool named "default".
 */
export function currentDefaultText(pools: Pool[], marking: Pool, whose: string): string {
  const own = pools.find((p) => p.isDefault && p.platform === marking.platform && poolOwner(p) === poolOwner(marking));
  if (own) return `${own.name} is ${whose} default pool now.`;
  if (marking.platform) {
    return `The platform has no default pool now: Runs naming no pool go to a pool named "default", for tenants without a default pool of their own.`;
  }
  const platform = pools.find((p) => p.isDefault && p.platform);
  if (platform) return `None of ${whose} pools is the default, so Runs naming no pool go to the platform's default pool, ${platform.name}, now.`;
  return `None of ${whose} pools is the default, and the platform has none: Runs naming no pool go to a pool named "default" now.`;
}

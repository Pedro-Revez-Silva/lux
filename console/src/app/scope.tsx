// Global scope (tenant + time range) shared by every page. Persisted in the URL
// query (router.tsx's location store) so links carry their scope.
import { createContext, useCallback, useContext, useMemo, type ReactNode } from "react";
import { ALL_TENANTS, TIME_RANGES, type TimeRange } from "@lux/design-system";
import { useQuery, useSession, type QueryOptions, type QueryState } from "../api/index.ts";
import { setSearchParams, useSearchParams } from "./router.tsx";

export interface Scope {
  /** Tenant id, or ALL_TENANTS. */
  tenant: string;
  /** The tenant to pass to list calls: undefined for all tenants. */
  apiTenant: string | undefined;
  range: TimeRange;
  setTenant: (t: string) => void;
  setRange: (r: TimeRange) => void;
  /** The key is an operator's. */
  operator: boolean;
  /** Lists span several tenants: an operator looking at all of them. Tables show a Tenant column. */
  showTenant: boolean;
}

const ScopeCtx = createContext<Scope | null>(null);

const RANGES = TIME_RANGES.map((t) => t.value);

export function ScopeProvider({ children }: { children: ReactNode }) {
  const params = useSearchParams();
  const operator = useSession().role === "operator";
  const tenant = params.get("tenant") ?? ALL_TENANTS;
  const r = params.get("range");
  const range: TimeRange = RANGES.includes(r as TimeRange) ? (r as TimeRange) : "24h";
  const setTenant = useCallback((t: string) => setSearchParams({ tenant: t === ALL_TENANTS ? null : t }), []);
  const setRange = useCallback((x: TimeRange) => setSearchParams({ range: x === "24h" ? null : x }), []);
  const value = useMemo(() => {
    const apiTenant = tenant === ALL_TENANTS ? undefined : tenant;
    return { tenant, apiTenant, range, setTenant, setRange, operator, showTenant: operator && apiTenant === undefined };
  }, [tenant, range, setTenant, setRange, operator]);
  return <ScopeCtx.Provider value={value}>{children}</ScopeCtx.Provider>;
}

export function useScope(): Scope {
  const s = useContext(ScopeCtx);
  if (!s) throw new Error("useScope outside ScopeProvider");
  return s;
}

/**
 * useQuery for a tenant-scoped list call: the scope's tenant goes into both
 * the cache key and the request (undefined: every tenant the key sees).
 */
export function useScopedQuery<T>(key: string, fn: (tenant: string | undefined, signal: AbortSignal) => Promise<T>, opts?: QueryOptions): QueryState<T> {
  const { tenant, apiTenant } = useScope();
  return useQuery(`${key}@${tenant}`, (signal) => fn(apiTenant, signal), opts);
}

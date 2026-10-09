// Global scope (tenant, time range, step) shared by every page, and the
// Overview Cost panel's Show, Break down by and label filters. Persisted in
// the URL query (router.tsx's location store) so links carry their scope.
import { createContext, useCallback, useContext, useMemo, type ReactNode } from "react";
import { ALL_TENANTS, TIME_RANGES, type TimeRange } from "@lux/design-system";
import { useQuery, useSession, type QueryOptions, type QueryState } from "../api/index.ts";
import { effectiveEvery, parseEvery, resolveStep, type ChartKind, type Every, type Resolved } from "./every.ts";
import { setSearchParams, useSearchParams } from "./router.tsx";
import { breakdownParam, filtersParams, parseBreakdown, parseFilters, parseShow, type Breakdown, type CostShow, type LabelFilter } from "./pages/costView.ts";

export interface Scope {
  /** Tenant id, or ALL_TENANTS. */
  tenant: string;
  /** The tenant to pass to list calls: undefined for all tenants. */
  apiTenant: string | undefined;
  range: TimeRange;
  setTenant: (t: string) => void;
  setRange: (r: TimeRange) => void;
  /** ?every=: the step as asked (absent: auto). */
  every: Every;
  /** The step in effect: every, or Auto when every is off for the range. */
  everyInEffect: Every;
  setEvery: (e: Every) => void;
  /** The step a kind of chart that follows the range uses. */
  step: (kind: ChartKind) => Resolved;
  /** The key is an operator's. */
  operator: boolean;
  /** Lists span several tenants: an operator looking at all of them. Tables show a Tenant column. */
  showTenant: boolean;
  /** ?cost=: which costs the Cost panel counts (absent: all). */
  costShow: CostShow;
  setCostShow: (s: CostShow) => void;
  /** ?by=: what the Cost panel breaks down by (absent: family). */
  costBy: Breakdown;
  setCostBy: (b: Breakdown) => void;
  /** ?label= and ?nolabel=: the Cost panel's label filters. */
  costFilters: LabelFilter[];
  setCostFilters: (f: LabelFilter[]) => void;
}

const ScopeCtx = createContext<Scope | null>(null);

const RANGES = TIME_RANGES.map((t) => t.value);

export function ScopeProvider({ children }: { children: ReactNode }) {
  const params = useSearchParams();
  const operator = useSession().role === "operator";
  const tenant = params.get("tenant") ?? ALL_TENANTS;
  const r = params.get("range");
  const range: TimeRange = RANGES.includes(r as TimeRange) ? (r as TimeRange) : "24h";
  const every = parseEvery(params.get("every"));
  const costShow = parseShow(params.get("cost"));
  const byParam = params.get("by");
  const labelParams = params.getAll("label").join("\n");
  const nolabelParams = params.getAll("nolabel").join("\n");
  const setTenant = useCallback((t: string) => setSearchParams({ tenant: t === ALL_TENANTS ? null : t }), []);
  const setRange = useCallback((x: TimeRange) => setSearchParams({ range: x === "24h" ? null : x }), []);
  const setEvery = useCallback((e: Every) => setSearchParams({ every: e === "auto" ? null : e }), []);
  const setCostShow = useCallback((s: CostShow) => setSearchParams({ cost: s === "all" ? null : s }), []);
  const setCostBy = useCallback((b: Breakdown) => setSearchParams({ by: breakdownParam(b) }), []);
  const setCostFilters = useCallback((f: LabelFilter[]) => setSearchParams(filtersParams(f)), []);
  const value = useMemo((): Scope => {
    const apiTenant = tenant === ALL_TENANTS ? undefined : tenant;
    const showTenant = operator && apiTenant === undefined;
    const costFilters = parseFilters(labelParams ? labelParams.split("\n") : [], nolabelParams ? nolabelParams.split("\n") : []);
    return {
      tenant,
      apiTenant,
      range,
      setTenant,
      setRange,
      every,
      everyInEffect: effectiveEvery(range, every),
      setEvery,
      step: (kind) => resolveStep(range, every, kind),
      operator,
      showTenant,
      costShow,
      setCostShow,
      costBy: parseBreakdown(byParam, showTenant),
      setCostBy,
      costFilters,
      setCostFilters,
    };
  }, [tenant, range, setTenant, setRange, every, setEvery, operator, costShow, setCostShow, byParam, setCostBy, labelParams, nolabelParams, setCostFilters]);
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

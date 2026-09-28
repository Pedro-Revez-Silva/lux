import { useMemo, type ReactNode } from "react";
import { Card, compareMoney, EmptyState, familyDisplay, ListPriceNote, Money, MoneyList, SectionHeader, StatTile, sumMoney, Table, TimeSeriesChart, type Column } from "@lux/design-system";
import { api, type CostSummary, type CostSummaryRow, type MoneyTotal } from "../../api/index.ts";
import { useScope, useScopedQuery } from "../scope.tsx";
import { ErrorStrip, RunLink, RunNameLink, runPath } from "./common.tsx";
import { go } from "../router.tsx";

const TOP = 10;

/**
 * Cost over the page's range: stacked by family per currency, the top
 * tenants (operators across tenants) and top Runs, and, for an operator
 * viewing all tenants, the unallocated host cost. Costs are hourly: the 1h
 * range reads 6h, which a chart can show.
 */
export function OverviewCost() {
  const scope = useScope();
  const since = scope.range === "1h" ? "6h" : scope.range;
  const interval: "hour" | "day" = since === "30d" ? "day" : "hour";
  const byFamily = useScopedQuery(`costs:family:${since}:${interval}`, (t, s) => api.costs(t, { group: ["family"], interval, since }, s), { interval: 60_000 });
  const byRun = useScopedQuery(`costs:run:${since}`, (t, s) => api.costs(t, { group: ["run"], since }, s), { interval: 60_000 });
  const byTenant = useScopedQuery(`costs:tenant:${since}`, (t, s) => api.costs(t, { group: ["tenant"], since }, s), { interval: 60_000, enabled: scope.showTenant });
  const tenantNames = useScopedQuery("tenants-names", (_t, s) => api.tenants(s), { enabled: scope.showTenant });

  const charts = useMemo(() => familyCharts(byFamily.data, interval), [byFamily.data, interval]);
  const totals = useMemo(() => currencyTotals(byFamily.data?.totals ?? []), [byFamily.data]);
  const names = useMemo(() => new Map((tenantNames.data ?? []).map((t) => [t.id, t.name])), [tenantNames.data]);
  // Names come with the summary grouped by run: one call, not one per row.
  const runNames = useMemo(() => new Map((byRun.data?.runs ?? []).map((r) => [r.id, r.name ?? ""])), [byRun.data]);
  const error = byFamily.error ?? byRun.error ?? byTenant.error;

  return (
    <div className="stack">
      <SectionHeader
        title="Cost"
        note={
          <span className="row">
            {interval === "hour" ? "hourly" : "daily"} over the last {since}
            <ListPriceNote />
          </span>
        }
      />
      <ErrorStrip error={error} />
      <div className="grid grid-stats">
        <StatTile label={`Cost (${since})`} loading={byFamily.loading} value={<MoneyList amounts={totals} />} />
        {scope.showTenant && <StatTile label={`Unallocated (${since})`} loading={byFamily.loading} value={<MoneyList amounts={byFamily.data?.unallocated} />} unit="host time no Run reserved" />}
      </div>
      <div className="grid grid-charts">
        {charts.length === 0 ? (
          <Card title="Cost by family" subtitle={`over the last ${since}`}>
            <EmptyState compact title={byFamily.loading ? "Loading…" : "No cost recorded in this range"} description={byFamily.loading ? undefined : "Nothing has been costed yet in this range; no figure is not a zero."} />
          </Card>
        ) : (
          charts.map((c) => (
            <Card key={c.currency} title={charts.length > 1 ? `Cost by family · ${c.currency}` : "Cost by family"} subtitle={`stacked, per ${interval}`}>
              <TimeSeriesChart x={c.x} ys={c.ys} series={c.series} unit="money" currency={c.currency} stacked legend />
            </Card>
          ))
        )}
        {scope.showTenant && (
          <Card title="Top tenants" subtitle={`by cost, over the last ${since}`} flush>
            <TopTable rows={top(byTenant.data?.totals ?? [], "tenant")} name={(k) => names.get(k) ?? k} loading={byTenant.loading} empty="No tenant has a cost in this range." />
          </Card>
        )}
        <Card title="Top Runs" subtitle={`by cost, over the last ${since}`} flush>
          <TopTable rows={top(byRun.data?.totals ?? [], "run")} name={(k) => <RunNameLink id={k} name={runNames.get(k)} />} id={(k) => <RunLink id={k} />} onClick={(k) => go(runPath(k))} loading={byRun.loading} empty="No Run has a cost in this range." />
        </Card>
      </div>
    </div>
  );
}

interface TopRow {
  key: string;
  currency: string;
  amount: string;
}

/** The largest TOP per currency, largest first; never ranks across currencies. */
function top(rows: CostSummaryRow[], group: string): TopRow[] {
  const byCurrency = new Map<string, TopRow[]>();
  for (const r of rows) {
    const key = r.group?.[group];
    if (!key) continue;
    byCurrency.set(r.currency, [...(byCurrency.get(r.currency) ?? []), { key, currency: r.currency, amount: r.amount }]);
  }
  return [...byCurrency.keys()].sort().flatMap((c) => byCurrency.get(c)!.sort((a, b) => compareMoney(b.amount, a.amount)).slice(0, TOP));
}

function TopTable({ rows, name, id, onClick, loading, empty }: { rows: TopRow[]; name: (key: string) => ReactNode; id?: (key: string) => ReactNode; onClick?: (key: string) => void; loading: boolean; empty: string }) {
  const cols: Column<TopRow>[] = [
    { key: "name", header: "Name", cell: (r) => name(r.key), lead: true },
    // Not optional: the card is narrower than Table's breakpoint, and the id is what a name supports.
    ...(id ? [{ key: "id", header: "Id", cell: (r: TopRow) => id(r.key), mono: true, width: 190 }] : []),
    { key: "amount", header: "Cost", cell: (r) => <Money amount={r.amount} currency={r.currency} />, align: "right", mono: true, width: 110 },
  ];
  return <Table columns={cols} rows={rows} rowKey={(r) => `${r.currency}:${r.key}`} loading={loading} loadingRows={3} onRowClick={onClick ? (r) => onClick(r.key) : undefined} empty={empty} dense />;
}

/** The family rows' amounts summed exactly per currency, never across currencies. */
function currencyTotals(rows: CostSummaryRow[]): MoneyTotal[] {
  const by = new Map<string, string[]>();
  for (const r of rows) by.set(r.currency, [...(by.get(r.currency) ?? []), r.amount]);
  return [...by.keys()].sort().flatMap((currency) => {
    const amount = sumMoney(by.get(currency)!);
    return amount == null ? [] : [{ currency, amount }];
  });
}

interface FamilyChart {
  currency: string;
  x: number[];
  ys: (number | null)[][];
  series: { label: string; color: string }[];
}

/** One chart per currency: a series per family, aligned on the buckets. */
function familyCharts(d: CostSummary | undefined, interval: "hour" | "day"): FamilyChart[] {
  if (!d?.series?.length) return [];
  // Every bucket of the range: one with no row is a gap, not a zero.
  const step = interval === "hour" ? 3600 : 86400;
  const start = Math.floor(Date.parse(d.from) / 1000 / step) * step;
  const end = Math.floor(Date.parse(d.to) / 1000);
  const x: number[] = [];
  for (let t = start; t < end; t += step) x.push(t);
  const index = new Map(x.map((t, i) => [t, i]));
  const currencies = [...new Set(d.series.map((r) => r.currency))].sort();
  // Label and colour as on the Run page: displayName and hint from the describe.
  const meta = new Map((d.families ?? []).map((f) => [f.family, f]));
  return currencies.map((currency) => {
    const rows = d.series!.filter((r) => r.currency === currency);
    const families = [...new Set(rows.map((r) => r.group?.family ?? "(none)"))].sort((a, b) => (a === "compute" ? -1 : b === "compute" ? 1 : a.localeCompare(b)));
    const display = familyDisplay(families.map((family) => meta.get(family) ?? { family }));
    const ys = families.map(() => x.map((): number | null => null));
    for (const r of rows) {
      const i = index.get(Math.floor(Date.parse(r.at!) / 1000));
      // Chart geometry only: the figures in text are formatted from the strings.
      if (i != null) ys[families.indexOf(r.group?.family ?? "(none)")]![i] = Number(r.amount);
    }
    return { currency, x, ys, series: families.map((f) => display.get(f)!) };
  });
}


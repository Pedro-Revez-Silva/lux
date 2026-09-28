import { useMemo } from "react";
import { Card, EmptyState, familyColor, formatMoney, formatTimestamp, KeyValue, ListPriceNote, TimeSeriesChart, Tooltip, type KeyValueItem, type Series } from "@lux/design-system";
import { api, useQuery, type HostCost as HostCostData, type HostCostRate } from "../../api/index.ts";
import { ErrorStrip } from "./common.tsx";

/**
 * A host's compute cost per hour: allocated to Runs and, for operators,
 * unallocated, stacked; and (operators) its rate periods. Shown to those who
 * can read the host's history: operators, and a tenant for its own host.
 */
export function HostCost({ id, range, operator }: { id: string; range: string; operator: boolean }) {
  // Hourly buckets: the 1h range would chart one or two points.
  const since = range === "1h" ? "6h" : range;
  const q = useQuery(`host-cost:${id}:${since}`, (s) => api.hostCost(id, since, s), { interval: 30_000 });
  const c = q.data;
  // Stable per response: the host page re-renders on its 10s clock, and a
  // fresh series or ys array would rebuild each chart.
  const charts = useMemo(() => (c ? byCurrency(c) : []).map((ch) => chartProps(ch, operator)), [c, operator]);
  const sub = operator ? `allocated to Runs vs unallocated, per hour, over ${since}` : `allocated to your Runs, per hour, over ${since}`;
  return (
    <>
      <ErrorStrip error={q.error} />
      <div className={operator ? "grid grid-2" : "stack"}>
        {charts.length === 0 ? (
          <Card title="Cost" subtitle={sub} actions={<ListPriceNote />}>
            <EmptyState compact title={c ? "No cost recorded in this range" : "Loading cost…"} description={c ? "Host hours are costed once the host has a price; a host without one has no figure, not a zero." : undefined} />
          </Card>
        ) : (
          charts.map((ch, i) => (
            <Card key={ch.currency} title={charts.length > 1 ? `Cost · ${ch.currency}` : "Cost"} subtitle={sub} actions={i === 0 ? <ListPriceNote /> : undefined}>
              <TimeSeriesChart x={ch.x} ys={ch.ys} series={ch.series} unit="money" currency={ch.currency} stacked={operator} legend />
            </Card>
          ))
        )}
        {operator && (
          <Card title="Rate periods" subtitle="what this host is priced at, over the range">
            {c?.rates && c.rates.length > 0 ? <KeyValue items={c.rates.map(rateItem)} /> : <EmptyState compact title={c ? "No rate in this range" : "Loading…"} description={c ? "An EC2 host is priced from the Pricing API; a static host from its price (lux hosts price)." : undefined} />}
          </Card>
        )}
      </div>
    </>
  );
}

interface CurrencySeries {
  currency: string;
  x: number[];
  allocated: (number | null)[];
  unallocated: (number | null)[];
}

const OPERATOR_SERIES: Series[] = [{ label: "Allocated", color: familyColor("compute") }, { label: "Unallocated", color: "var(--st-neutral-dot)" }];
const TENANT_SERIES: Series[] = [{ label: "Allocated", color: familyColor("compute"), area: true }];

/** A currency's chart: allocated and unallocated stacked for operators, allocated alone otherwise. */
function chartProps(ch: CurrencySeries, operator: boolean) {
  return { currency: ch.currency, x: ch.x, ys: operator ? [ch.allocated, ch.unallocated] : [ch.allocated], series: operator ? OPERATOR_SERIES : TENANT_SERIES };
}

/** One aligned hourly series per currency; an hour without a row is missing (null), not zero. */
function byCurrency(c: HostCostData): CurrencySeries[] {
  const from = Math.floor(Date.parse(c.from) / 1000);
  const to = Math.floor(Date.parse(c.to) / 1000);
  const x: number[] = [];
  for (let t = from; t < to; t += 3600) x.push(t);
  const index = new Map(x.map((t, i) => [t, i]));
  const out = new Map<string, CurrencySeries>();
  for (const h of c.hours) {
    let s = out.get(h.currency);
    if (!s) out.set(h.currency, (s = { currency: h.currency, x, allocated: x.map(() => null), unallocated: x.map(() => null) }));
    const i = index.get(Math.floor(Date.parse(h.hour) / 1000));
    if (i == null) continue;
    // Chart geometry only: the figures in text come from the strings.
    s.allocated[i] = Number(h.allocated);
    s.unallocated[i] = h.unallocated != null ? Number(h.unallocated) : null;
  }
  return [...out.values()];
}

/** Where a rate came from, once: static, on-demand or spot, with the raw source in a tooltip when it says more. */
function RateSource({ source }: { source: string }) {
  const label = source === "static" ? "static" : source.endsWith("spot-history") ? "spot" : "on-demand";
  if (label === source) return <span className="secondary">{label}</span>;
  return (
    <Tooltip content={`price source: ${source}`}>
      <span className="secondary">{label}</span>
    </Tooltip>
  );
}

/** Every digit of a rate: "$0.384", not "$0.38". */
function rateDecimals(perHour: string): number {
  const frac = perHour.split(".")[1]?.replace(/0+$/, "") ?? "";
  return Math.min(9, Math.max(2, frac.length));
}

function rateItem(r: HostCostRate): KeyValueItem {
  return {
    key: `${formatTimestamp(r.from, { seconds: false })} – ${r.to ? formatTimestamp(r.to, { seconds: false }) : "now"}`,
    value: (
      <>
        <span className="mono">{formatMoney(r.perHour, r.currency, { decimals: rateDecimals(r.perHour) })}/h</span> <span className="secondary">·</span> <RateSource source={r.source} />
      </>
    ),
  };
}

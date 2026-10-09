import { useMemo } from "react";
import { Card, EmptyState, familyColor, formatTimestamp, KeyValue, ListPriceNote, Money, sumMoney, TimeSeriesChart, Tooltip, type KeyValueItem, type Series, type TimeRange } from "@lux/design-system";
import { api, useQuery, type HostCost as HostCostData, type HostCostRate } from "../../api/index.ts";
import { costRange, stepNote } from "../every.ts";
import { useScope } from "../scope.tsx";
import { ErrorStrip } from "./common.tsx";

/**
 * A host's compute cost per hour or UTC day (the page's step): allocated to
 * Runs and, for operators, unallocated, stacked; and (operators) its rate
 * periods. Shown to those who can read the host's history: operators, and a
 * tenant for its own host.
 */
export function HostCost({ id, range, operator }: { id: string; range: TimeRange; operator: boolean }) {
  const scope = useScope();
  const step = scope.step("cost");
  const daily = step.step === "day";
  // Hourly buckets: the 1h range would chart one or two points.
  const since = costRange(range);
  const q = useQuery(`host-cost:${id}:${since}`, (s) => api.hostCost(id, since, s), { interval: 30_000 });
  const c = q.data;
  // Stable per response: the host page re-renders on its 10s clock, and a
  // fresh series or ys array would rebuild each chart.
  const charts = useMemo(() => (c ? byCurrency(c, daily ? 86400 : 3600) : []).map((ch) => chartProps(ch, operator)), [c, operator, daily]);
  const per = stepNote(step);
  const sub = operator ? `allocated to Runs vs unallocated, ${per}, over ${since}` : `allocated to your Runs, ${per}, over ${since}`;
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

/** One aligned series per currency in buckets of `step` seconds (hours, or UTC days summed exactly from them); a bucket without a row is missing (null), not zero. */
function byCurrency(c: HostCostData, step: number): CurrencySeries[] {
  const from = Math.floor(Date.parse(c.from) / 1000 / step) * step;
  const to = Math.floor(Date.parse(c.to) / 1000);
  const x: number[] = [];
  for (let t = from; t < to; t += step) x.push(t);
  const index = new Map(x.map((t, i) => [t, i]));
  const sums = new Map<string, { allocated: string[][]; unallocated: string[][] }>();
  for (const h of c.hours) {
    let s = sums.get(h.currency);
    if (!s) sums.set(h.currency, (s = { allocated: x.map(() => []), unallocated: x.map(() => []) }));
    const i = index.get(Math.floor(Date.parse(h.hour) / 1000 / step) * step);
    if (i == null) continue;
    s.allocated[i]!.push(h.allocated);
    if (h.unallocated != null) s.unallocated[i]!.push(h.unallocated);
  }
  // Chart geometry only: the figures in text come from the strings.
  const num = (a: string[]) => (a.length ? Number(sumMoney(a)) : null);
  return [...sums].map(([currency, s]) => ({ currency, x, allocated: s.allocated.map(num), unallocated: s.unallocated.map(num) }));
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

function rateItem(r: HostCostRate): KeyValueItem {
  return {
    key: `${formatTimestamp(r.from, { seconds: false })} – ${r.to ? formatTimestamp(r.to, { seconds: false }) : "now"}`,
    value: (
      <>
        <span className="mono"><Money amount={r.perHour} currency={r.currency} />/h</span> <span className="secondary">·</span> <RateSource source={r.source} />
      </>
    ),
  };
}

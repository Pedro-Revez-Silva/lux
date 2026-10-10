import { useMemo } from "react";
import { Card, ColorKey, EmptyState, familyColor, formatTimestamp, Money, Table, Tooltip, type Column, type TimeRange } from "@lux/design-system";
import { api, useQuery, type Host, type HostCostRate } from "../../api/index.ts";
import { costRange, stepNote } from "../every.ts";
import { useScope } from "../scope.tsx";
import { ErrorBlock, ErrorStrip } from "./common.tsx";
import { BLOCK_STORAGE } from "./costView.ts";
import { HostCostChart, HostCostTiles } from "./HostCostParts.tsx";
import { billedHours, familyLabel, hostCharts, volumeSummary, volumeText, whoPaid } from "./hostCostView.ts";

/**
 * A host's cost per hour or UTC day (the page's step): compute and block
 * storage apart, each charged to Runs and (when the reader may see it)
 * unallocated, stacked; and, for operators, its rate periods per family.
 * Shown to those who can read the host's history: operators, and a tenant
 * for its own host. The tenant sees unallocated only while the host is in
 * one of its own pools: luxd omits it otherwise.
 */
export function HostCost({ host, range, operator }: { host: Host; range: TimeRange; operator: boolean }) {
  const scope = useScope();
  const step = scope.step("cost");
  const daily = step.step === "day";
  // Hourly buckets: the 1h range would chart one or two points.
  const since = costRange(range);
  const tenant = scope.apiTenant;
  const q = useQuery(`host-cost:${host.id}:${since}@${tenant ?? ""}`, (s) => api.hostCost(host.id, since, tenant, s), { interval: 30_000 });
  const c = q.data;
  // Stable per response: the host page re-renders on its clock, and a fresh
  // series or ys array would rebuild the chart.
  const view = useMemo(() => {
    if (!c) return null;
    const rows = c.hours.map((h) => ({ t: Date.parse(h.hour) / 1000, family: h.family, currency: h.currency, allocated: h.allocated, unallocated: h.unallocated }));
    // luxd says by omitting unallocated; with no hour to say it, an operator and a tenant's own (non-platform) host can see it.
    return { paid: whoPaid(rows), charts: hostCharts(rows, c.from, c.to, daily ? 86400 : 3600), visible: c.hours.length === 0 ? operator || !host.platform : c.hours.every((h) => h.unallocated != null) };
  }, [c, daily, operator, host.platform]);
  if (q.error && !c) return <ErrorBlock error={q.error} onRetry={q.refetch} />;
  const per = stepNote(step);
  const visible = view?.visible ?? operator;
  const hours = c ? billedHours(host.times, c.from, c.to) : null;
  const volumes = host.volumes ? volumeSummary([host]) : null;
  return (
    <div className="stack host-cost">
      <ErrorStrip error={q.error} />
      <HostCostTiles since={since} paid={view?.paid ?? []} hours={hours} volumes={volumes} unallocated={visible} loading={!c} />
      <HostCostChart
        charts={view?.charts ?? []}
        title={visible ? `Host cost per ${daily ? "day" : "hour"}` : `Charged to your Runs per ${daily ? "day" : "hour"}`}
        subtitle={visible ? `charged to Runs vs unallocated, ${per}, over ${since}` : `allocated to your Runs, ${per}, over ${since}`}
        loading={!c}
        step={daily ? "day" : "hour"}
      />
      {operator && (
        <Card title="Rate periods" subtitle="what this host is priced at, per family, over the range" flush={!!c?.rates?.length}>
          {c?.rates && c.rates.length > 0 ? <RatesTable rates={c.rates} /> : <EmptyState compact title={c ? "No rate in this range" : "Loading…"} description={c ? "An EC2 host is priced from the Pricing API; a static host from its price (lux hosts price)." : undefined} />}
        </Card>
      )}
    </div>
  );
}

/** Where a rate came from, once: static, on-demand, spot or EBS, with the raw source in a tooltip when it says more. */
function RateSource({ source }: { source: string }) {
  const label = source === "static" ? "static" : source.endsWith("spot-history") ? "spot" : source.endsWith("ebs-pricing") ? "EBS list price" : "on-demand";
  if (label === source) return <span className="secondary">{label}</span>;
  return (
    <Tooltip content={`price source: ${source}`}>
      <span className="secondary">{label}</span>
    </Tooltip>
  );
}

/** A block-storage period's volumes, with its unit prices in a tooltip. */
function RateDetails({ rate }: { rate: HostCostRate }) {
  const d = rate.details;
  if (rate.family !== BLOCK_STORAGE || !d?.volumes?.length) return null;
  const prices = Object.entries(d.prices ?? {});
  const text = volumeText(d.volumes, true);
  const assumed = d.volumes.some((v) => v.assumed);
  return (
    <Tooltip
      content={
        <span className="stack stack-tight">
          {prices.map(([type, p]) => (
            <span key={type}>
              {type}: <Money amount={p.perGBMonth} currency={p.currency} /> per GB-month
              {p.perIOPSMonth ? <> · <Money amount={p.perIOPSMonth} currency={p.currency} /> per IOPS-month</> : null}
              {p.perGiBpsMonth ? <> · <Money amount={p.perGiBpsMonth} currency={p.currency} /> per GiB/s-month</> : null}
            </span>
          ))}
          {d.hoursPerMonth ? <span>{d.hoursPerMonth} hours a month</span> : null}
        </span>
      }
    >
      <span className="rate-details secondary" data-rate-details>
        {text}
        {assumed ? " · assumed" : ""}
      </span>
    </Tooltip>
  );
}

const RATE_COLS: Column<HostCostRate>[] = [
  { key: "family", header: "Family", cell: (r) => <ColorKey color={familyColor(r.family)}>{familyLabel(r.family)}</ColorKey>, sortValue: (r) => r.family, width: 150 },
  { key: "window", header: "Period", cell: (r) => `${formatTimestamp(r.from, { seconds: false })} – ${r.to ? formatTimestamp(r.to, { seconds: false }) : "now"}`, sortValue: (r) => Date.parse(r.from), sortKind: "time", lead: true },
  {
    key: "rate",
    header: "Price",
    cell: (r) => (
      <span className="mono">
        <Money amount={r.perHour} currency={r.currency} />/h
      </span>
    ),
    sortValue: (r) => Number(r.perHour),
    align: "right",
    width: 120,
  },
  {
    key: "source",
    header: "Source",
    cell: (r) => (
      <span className="row">
        <RateSource source={r.source} />
        <RateDetails rate={r} />
      </span>
    ),
    sortValue: (r) => r.source,
  },
];

function RatesTable({ rates }: { rates: HostCostRate[] }) {
  return <Table columns={RATE_COLS} rows={rates} rowKey={(r) => `${r.family}:${r.from}`} dense />;
}

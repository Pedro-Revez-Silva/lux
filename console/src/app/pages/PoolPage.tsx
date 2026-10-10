import { useMemo } from "react";
import { Badge, Callout, Card, CENTS, compareMoney, EmptyState, formatBytes, formatClock, formatCores, formatCount, formatElapsed, KeyValue, Meter, Money, MoneyList, PageHeader, rangeText, StatTile, Table, Tabs, TimeSeriesChart, useNow, type Column } from "@lux/design-system";
import { api, type Pool, type PoolCost, type PoolMetrics, type PoolOwner } from "../../api/index.ts";
import { costInterval, costRange, historyRes, stepNote, stepOfRes } from "../every.ts";
import { go, Link, setSearchParams, useSearchParams } from "../router.tsx";
import { useScope, useScopedQuery } from "../scope.tsx";
import { DASH, ErrorBlock, ErrorStrip, hostPath, labelsText, PageSkeleton, RunLink, RunNameLink, runPath } from "./common.tsx";
import { HostCostChart, HostCostTiles, pick, WhoPaidCard } from "./HostCostParts.tsx";
import { hostCharts, hostCostRows, hostHours, sumPerCurrency, volumeSummary, whoPaid, type HostChart, type HostCostRow, type WhoPaid } from "./hostCostView.ts";
import { HostsList } from "./Hosts.tsx";
import { PagedEvents } from "./PagedEvents.tsx";

/** The Pools page's poll: this page's too. */
const POLL = 15_000;
const TABS = ["metrics", "cost", "hosts", "events"] as const;
type Tab = (typeof TABS)[number];

export function PoolPage({ name }: { name: string }) {
  const scope = useScope();
  const params = useSearchParams();
  const ownerParam = params.get("owner");
  const owner: PoolOwner | undefined = ownerParam === "platform" || ownerParam === "tenant" ? ownerParam : undefined;
  const tabParam = params.get("tab");
  const tab: Tab = TABS.includes(tabParam as Tab) ? (tabParam as Tab) : "metrics";
  const pools = useScopedQuery("pools", api.pools, { interval: POLL });

  // ?owner= picks the platform's pool or a tenant's where both have the
  // name; without it a tenant's own pool shadows the platform's, as it does
  // for its Runs (and for this page's reads).
  const matches = (pools.data ?? [])
    .filter((p) => p.name === name && (owner == null || p.platform === (owner === "platform")))
    .sort((a, b) => Number(a.platform) - Number(b.platform));
  const pool: Pool | undefined = matches[0];
  const ambiguous = scope.showTenant && matches.length > 1;
  const poolOwner: PoolOwner | undefined = pool ? (pool.platform ? "platform" : "tenant") : owner;
  const trend = scope.step("trend");
  const res = historyRes(trend);
  const metrics = useScopedQuery(`pool-metrics:${name}:${poolOwner}:${scope.range}:${res}`, (t, s) => api.poolMetrics(name, t, poolOwner, scope.range, s, res), { interval: 30_000, enabled: pool != null && !ambiguous });

  if (pools.error && !pools.data) {
    return (
      <div className="page">
        <ErrorBlock error={pools.error} onRetry={pools.refetch} />
      </div>
    );
  }
  if (!pools.data) return <PageSkeleton />;
  if (!pool) {
    return (
      <div className="page">
        <ErrorBlock error={`No ${owner === "platform" ? "platform " : ""}pool named ${name}${scope.apiTenant ? " for this tenant" : ""}.`} />
      </div>
    );
  }
  if (ambiguous) {
    return (
      <div className="page">
        <PageHeader title={name} />
        <ErrorBlock error={`${matches.length} pools are named ${name} (${matches.map((p) => (p.platform ? "platform" : p.tenant)).join(", ")}): pick a tenant, or open one from the Pools list.`} />
      </div>
    );
  }

  const now = metrics.data?.now;
  const hostTotal = now ? Object.values(now.hosts).reduce((a, b) => a + b, 0) : 0;
  // A platform pool's events name other tenants' Runs: the operators', not narrowed to a tenant.
  const showEvents = !pool.platform || (scope.operator && !scope.apiTenant);
  return (
    <div className="page page-wide">
      <PageHeader
        title={pool.name}
        badges={
          <>
            <Badge outline>{pool.provider}</Badge>
            {pool.platform && <Badge outline>platform</Badge>}
            {pool.shared && <Badge tone="info">shared</Badge>}
            {pool.isDefault && <Badge tone="accent">Default</Badge>}
          </>
        }
        description={
          <>
            {pool.tenant ? <span>tenant {pool.tenant}</span> : <span>platform pool</span>}
            {now && (
              <span>
                {now.hosts.ready ?? 0} ready / {hostTotal} hosts
              </span>
            )}
            <span>charts over the {rangeText(scope.range)}</span>
          </>
        }
        actions={
          <Tabs
            value={tab}
            onChange={(t) => setSearchParams({ tab: t === "metrics" ? null : t })}
            items={[
              { key: "metrics", label: "Metrics" },
              { key: "cost", label: "Cost" },
              { key: "hosts", label: "Hosts", count: hostTotal || undefined },
              { key: "events", label: "Events", disabled: !showEvents },
            ]}
          />
        }
      />
      <ErrorStrip error={pools.error ?? metrics.error} />
      <PoolTiles pool={pool} metrics={metrics.data} loading={metrics.loading} />
      {tab === "metrics" && <PoolCharts metrics={metrics.data} step={stepNote(trend, stepOfRes(metrics.data?.resolution))} />}
      {tab === "cost" && <PoolCostTab name={name} owner={poolOwner} />}
      {tab === "hosts" && <PoolHostsTab pool={pool} />}
      {tab === "events" && showEvents && <PoolEvents name={name} owner={poolOwner} />}
      <p className="page-note">
        Pool charts come from per-pool samples, kept by the pool's id: a rename keeps its history; a removed pool and a new one of its name stay apart. History begins when luxd started taking them; older ranges are empty, not zero.
        {pool.platform && !scope.operator ? " Runs, allocation and cost here are your own; hosts and capacity are the pool's." : ""}
      </p>
    </div>
  );
}

function PoolTiles({ pool, metrics, loading }: { pool: Pool; metrics?: PoolMetrics; loading: boolean }) {
  const scope = useScope();
  const clock = useNow();
  const n = metrics?.now;
  const since = costRange(scope.range);
  const cost = useScopedQuery(`pool-cost-tile:${pool.id}:${since}`, (t, s) => api.poolCost(pool.name, t, pool.platform ? "platform" : "tenant", since, "hour", s), { interval: 60_000 });
  return (
    <div className="grid grid-stats">
      <StatTile label="Hosts ready" loading={loading} value={formatCount(n ? (n.hosts.ready ?? 0) : null)} unit={n ? `${n.hosts.provisioning ?? 0} provisioning · ${n.hosts.draining ?? 0} draining` : undefined} />
      <StatTile label="CPU allocated" loading={loading} value={formatCores(n?.allocatedCpus)} unit={n ? `of ${formatCores(n.capacityCpus)}` : undefined} />
      <StatTile label="Memory allocated" loading={loading} value={formatBytes(n?.allocatedMemory)} unit={n ? `of ${formatBytes(n.capacityMemory)}` : undefined} />
      <StatTile label="Runs running" loading={loading} value={formatCount(n?.running)} unit={n ? `${n.queued} queued${n.oldestQueuedAt ? ` · oldest ${formatElapsed(n.oldestQueuedAt, clock)}` : ""}` : undefined} />
      <StatTile
        label={`Launch failures (${scope.range})`}
        loading={loading}
        value={formatCount(n?.launchFailures)}
        tone={(n?.launchFailures ?? 0) > 0 ? "danger" : "default"}
        unit={n?.lastLaunchFailure ? `last ${formatClock(n.lastLaunchFailure)}${n.lastLaunchError ? ` · ${n.lastLaunchError.slice(0, 40)}` : ""}` : pool.provider === "static" ? "static pool" : undefined}
      />
      <PoolCostTile since={since} cost={cost.data} loading={cost.loading} />
    </div>
  );
}

/**
 * The pool's cost over the range: its host cost (compute and block storage,
 * Runs' and unallocated) and the unallocated part, one figure each per
 * currency. A reader who may not see unallocated gets its Runs' cost alone.
 */
function PoolCostTile({ since, cost, loading }: { since: string; cost?: PoolCost; loading: boolean }) {
  const visible = cost?.idle != null;
  if (!visible) return <StatTile label={`Cost (${since})`} loading={loading} value={<MoneyList amounts={cost?.totals} decimals={CENTS} />} unit={cost ? <>list price · charged to your Runs</> : "list price"} />;
  // The same figure as the Cost tab's lead tile: whoPaid's host cost per currency.
  const host = pick(whoPaid(cost.hostSeries ?? []), (p) => p.all.total);
  const idle = sumPerCurrency(cost.idle ?? []);
  return (
    <StatTile
      label={`Host cost (${since})`}
      loading={loading}
      value={<MoneyList amounts={host} decimals={CENTS} />}
      unit={idle.length ? <>list price · <MoneyList amounts={idle} decimals={CENTS} /> unallocated</> : "list price"}
    />
  );
}

/** Aligned series from pool samples. */
function poolSeries(m: PoolMetrics | undefined, pick: ((s: PoolMetrics["samples"][number]) => number | null | undefined)[]) {
  const x: number[] = [];
  const ys: (number | null)[][] = pick.map(() => []);
  for (const s of m?.samples ?? []) {
    x.push(Math.floor(Date.parse(s.at) / 1000));
    pick.forEach((p, i) => ys[i]!.push(p(s) ?? null));
  }
  return { x, ys };
}

function PoolCharts({ metrics, step }: { metrics?: PoolMetrics; step: string }) {
  const m = metrics;
  const s = useMemo(
    () => ({
      cpu: poolSeries(m, [(x) => x.allocatedCpus, (x) => x.capacityCpus]),
      mem: poolSeries(m, [(x) => x.allocatedMemory, (x) => x.capacityMemory]),
      hosts: poolSeries(m, [(x) => x.hosts?.ready ?? 0, (x) => x.hosts?.provisioning ?? 0, (x) => x.hosts?.draining ?? 0, (x) => x.hosts?.lost ?? 0]),
      runs: poolSeries(m, [(x) => x.running, (x) => x.queued]),
      flow: poolSeries(m, [(x) => x.started, (x) => x.finished]),
      launch: poolSeries(m, [(x) => x.launches - x.launchFailures, (x) => x.launchFailures]),
    }),
    [m],
  );
  if (m && m.samples.length === 0) {
    return (
      <Card>
        <EmptyState compact title="No history for this range yet" description={m.historyFrom ? `This pool's samples start ${new Date(m.historyFrom).toLocaleString()}.` : "Samples start when luxd takes its first one; nothing before that is known, so nothing is drawn."} />
      </Card>
    );
  }
  const cut = m?.historyFrom && Date.parse(m.historyFrom) > Date.parse(m.from) ? ` · history since ${formatClock(m.historyFrom)}` : "";
  return (
    <div className="grid grid-charts">
      <Card title="CPU" subtitle={`allocated vs capacity · ${step}${cut}`}>
        <TimeSeriesChart x={s.cpu.x} ys={s.cpu.ys} series={[{ label: "Allocated", color: 1, area: true }, { label: "Capacity", color: "var(--fg-faint)", dashed: true, step: true }]} unit="cores" />
      </Card>
      <Card title="Memory" subtitle={`allocated vs capacity · ${step}${cut}`}>
        <TimeSeriesChart x={s.mem.x} ys={s.mem.ys} series={[{ label: "Allocated", color: 7, area: true }, { label: "Capacity", color: "var(--fg-faint)", dashed: true, step: true }]} unit="bytes" />
      </Card>
      <Card title="Hosts" subtitle={`by state · ${step}`}>
        <TimeSeriesChart x={s.hosts.x} ys={s.hosts.ys} series={[{ label: "Ready", color: 3, step: true, area: true }, { label: "Provisioning", color: 1, step: true }, { label: "Draining", color: 4, step: true }, { label: "Lost", color: 8, step: true }]} unit="count" />
      </Card>
      <Card title="Runs" subtitle={`running and queued · ${step}`}>
        <TimeSeriesChart x={s.runs.x} ys={s.runs.ys} series={[{ label: "Running", color: 1, area: true }, { label: "Queued", color: 2 }]} unit="count" />
      </Card>
      <Card title="Started / finished" subtitle={step}>
        <TimeSeriesChart x={s.flow.x} ys={s.flow.ys} series={[{ label: "Started", color: 1, step: true }, { label: "Finished", color: 3, step: true }]} unit="count" />
      </Card>
      <Card title="Launches" subtitle={`launched vs failed · ${step}`}>
        <TimeSeriesChart x={s.launch.x} ys={s.launch.ys} series={[{ label: "Launched", color: 3, step: true }, { label: "Launch failed", color: 8, step: true }]} unit="count" />
      </Card>
    </div>
  );
}

interface TopRun {
  id: string;
  name?: string;
  currency: string;
  amount: string;
  estimate: boolean;
}

/**
 * The pool's Cost tab: its machines only (compute and block storage, apart).
 * Host time is the pool's whole cost, charged to Runs or unallocated; a
 * reader who may not see unallocated (a tenant on a platform pool: luxd
 * omits idle, hostSeries and hosts) gets its own Runs' figures alone.
 */
function PoolCostTab({ name, owner }: { name: string; owner?: PoolOwner }) {
  const scope = useScope();
  const step = scope.step("cost");
  const since = costRange(scope.range);
  const interval = costInterval(step);
  const q = useScopedQuery(`pool-cost:${name}:${owner}:${since}:${interval}`, (t, s) => api.poolCost(name, t, owner, since, interval, s), { interval: 60_000 });
  const v = useMemo(() => poolCostView(q.data), [q.data]);
  const topCols = useMemo<Column<TopRun>[]>(
    () => [
      { key: "name", header: "Run", cell: (r) => <RunNameLink id={r.id} name={r.name} />, sortValue: (r) => r.name || r.id, lead: true },
      { key: "id", header: "Id", cell: (r) => <RunLink id={r.id} />, sortValue: (r) => r.id, mono: true, width: 170 },
      { key: "cost", header: "Cost", cell: (r) => <span>{r.estimate ? "~" : ""}<Money amount={r.amount} currency={r.currency} /></span>, sortValue: (r) => Number(r.amount), align: "right", mono: true, width: 120 },
    ],
    [],
  );
  const top = (q.data?.topRuns ?? []).slice().sort((a, b) => a.currency.localeCompare(b.currency) || compareMoney(b.amount, a.amount));
  const loading = q.loading && !q.data;
  const per = interval === "hour" ? "hour" : "day";
  if (q.error && !q.data) return <ErrorBlock error={q.error} onRetry={q.refetch} />;
  return (
    <div className="stack pool-cost">
      <ErrorStrip error={q.error} />
      <HostCostTiles since={since} paid={v.paid} hours={v.hours} volumes={v.volumes} unallocated={v.visible} loading={loading} />
      {v.visible ? (
        <div className="grid grid-2-1">
          <div className="stack">
            <HostCostChart charts={v.charts} title={`Host cost per ${per}`} subtitle="compute and block storage, each split into what Runs reserved and what they did not" loading={loading} step={interval} />
          </div>
          <WhoPaidCard paid={v.paid} loading={loading} />
        </div>
      ) : (
        <HostCostChart charts={v.charts} title={`Charged to your Runs per ${per}`} subtitle="your Runs' share of the machines: compute and block storage" loading={loading} step={interval} />
      )}
      {v.visible && (
        <Card title="Cost by host" subtitle="costliest first · utilisation = charged to Runs ÷ host cost" flush>
          <HostCostTable rows={v.hosts} loading={loading} />
        </Card>
      )}
      <Card title="Top runs in this pool" subtitle={`by the cost of its machines, over the last ${since}`} flush>
        <Table columns={topCols} rows={top} rowKey={(r) => `${r.currency}:${r.id}`} defaultSort={{ key: "cost", dir: "desc" }} onRowClick={(r) => go(runPath(r.id))} loading={loading} loadingRows={3} empty="No Run has a cost here in this range." dense />
      </Card>
      <Callout>Pool cost is the machines only — instance and disk. AI and other external costs belong to Runs.</Callout>
    </div>
  );
}

/** Everything the pool Cost tab shows, from one poolCost answer. */
export function poolCostView(d: PoolCost | undefined) {
  // idle is present (maybe empty) exactly when luxd lets the reader see host time.
  const visible = d?.idle != null;
  const step = d?.interval === "day" ? 86400 : 3600;
  if (!d) return { visible: false, paid: [] as WhoPaid[], charts: [] as HostChart[], hosts: [] as HostCostRow[], hours: null, volumes: null };
  if (!visible) {
    const runs = d.series.map((r) => ({ t: Date.parse(r.at) / 1000, family: r.family, currency: r.currency, allocated: r.amount }));
    return { visible, paid: whoPaid(runs), charts: hostCharts(runs, d.from, d.to, step), hosts: [], hours: null, volumes: null };
  }
  const series = (d.hostSeries ?? []).map((r) => ({ t: Date.parse(r.at!) / 1000, family: r.family, currency: r.currency, allocated: r.allocated, unallocated: r.unallocated }));
  const hostRows = d.hosts ?? [];
  const byHost = new Map(hostRows.filter((h) => h.hostId).map((h) => [h.hostId!, h]));
  return {
    visible,
    paid: whoPaid(series),
    charts: hostCharts(series, d.from, d.to, step),
    hosts: hostCostRows(hostRows),
    hours: hostHours(hostRows),
    volumes: volumeSummary([...byHost.values()]),
  };
}

const money = (a: string | null, currency: string) => (a == null ? DASH : <Money amount={a} currency={currency} />);

function HostCostTable({ rows, loading }: { rows: HostCostRow[]; loading: boolean }) {
  const multi = new Set(rows.map((r) => r.currency)).size > 1;
  const cols = useMemo<Column<HostCostRow>[]>(
    () => [
      { key: "name", header: "Host", cell: (h) => <Link to={`${hostPath(h.hostId)}?tab=cost`} className="name-link">{h.hostName}</Link>, sortValue: (h) => h.hostName, lead: true },
      ...(multi ? [{ key: "currency", header: "Currency", cell: (h: HostCostRow) => h.currency, sortValue: (h: HostCostRow) => h.currency, width: 90 }] : []),
      { key: "up", header: "Up", cell: (h) => (h.hours == null ? DASH : `${h.hours.toFixed(1)} h`), sortValue: (h) => h.hours, align: "right", mono: true, width: 90 },
      { key: "compute", header: "Compute", cell: (h) => money(h.compute, h.currency), sortValue: (h) => (h.compute == null ? null : Number(h.compute)), align: "right", mono: true, width: 120 },
      { key: "bs", header: "Block storage", cell: (h) => money(h.blockStorage, h.currency), sortValue: (h) => (h.blockStorage == null ? null : Number(h.blockStorage)), align: "right", mono: true, width: 130 },
      { key: "total", header: "Total", cell: (h) => <Money amount={h.total} currency={h.currency} />, sortValue: (h) => Number(h.total), align: "right", mono: true, width: 120 },
      { key: "unallocated", header: "Unallocated", cell: (h) => <span className="muted"><Money amount={h.unallocated} currency={h.currency} /></span>, sortValue: (h) => Number(h.unallocated), align: "right", mono: true, width: 120 },
      { key: "util", header: "Utilisation", cell: (h) => <Meter value={h.utilisation} />, sortValue: (h) => h.utilisation, align: "right", width: 130 },
    ],
    [multi],
  );
  return <Table columns={cols} rows={rows} rowKey={(h) => `${h.currency}:${h.hostId}`} defaultSort={{ key: "total", dir: "desc" }} onRowClick={(h) => go(`${hostPath(h.hostId)}?tab=cost`)} loading={loading} loadingRows={3} empty="No host time recorded here in this range." dense />;
}

function PoolHostsTab({ pool }: { pool: Pool }) {
  const template = pool.template && Object.keys(pool.template).length > 0 ? <span className="mono">{labelsText(pool.template)}</span> : DASH;
  return (
    <div className="stack">
      <Card title="Settings">
        <KeyValue
          columns={2}
          items={[
            { key: "Hosts", value: `min ${pool.minHosts} · warm ${pool.warmHosts}${pool.warmWhileActive ? " (while in use)" : ""} · max ${pool.maxHosts || "unlimited"}` },
            { key: "Scale down after", value: pool.scaleDownAfter || "luxd's default" },
            { key: "Hourly price", value: pool.hourlyPrice ? `${pool.hourlyPrice} ${pool.currency ?? ""}` : DASH },
            { key: "Template", value: template },
          ]}
        />
      </Card>
      <HostsList pool={pool.name} poolId={pool.id} embedded />
    </div>
  );
}

/** A pool's events, a page at a time in the sort chosen (server-side). */
function PoolEvents({ name, owner }: { name: string; owner?: PoolOwner }) {
  const scope = useScope();
  return (
    <PagedEvents
      prefix="pool-events"
      view={`${scope.tenant}|${name}|${owner}`}
      fetch={(req, s) => api.poolEventsPage(name, scope.apiTenant, owner, req, s)}
      interval={POLL}
      subtitle="scale-ups, launches, placements and releases"
    />
  );
}

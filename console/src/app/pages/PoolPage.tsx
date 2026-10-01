import { useMemo } from "react";
import { Badge, Card, compareMoney, EmptyState, familyDisplay, formatBytes, formatClock, formatCores, formatCount, formatElapsed, KeyValue, ListPriceNote, Money, MoneyList, PageHeader, rangeText, SectionHeader, StatTile, Table, Tabs, TimeSeriesChart, useNow, type Column } from "@lux/design-system";
import { api, type Pool, type PoolCost, type PoolMetrics, type PoolOwner } from "../../api/index.ts";
import { go, setSearchParams, useSearchParams } from "../router.tsx";
import { useScope, useScopedQuery } from "../scope.tsx";
import { DASH, ErrorBlock, ErrorStrip, hostPath, labelsText, PageSkeleton, RunLink, RunNameLink, runPath } from "./common.tsx";
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
  const metrics = useScopedQuery(`pool-metrics:${name}:${poolOwner}:${scope.range}`, (t, s) => api.poolMetrics(name, t, poolOwner, scope.range, s), { interval: 30_000, enabled: pool != null && !ambiguous });

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
            size="sm"
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
      {tab === "metrics" && <PoolCharts metrics={metrics.data} />}
      {tab === "cost" && <PoolCostTab name={name} owner={poolOwner} operatorView={scope.operator && !scope.apiTenant} />}
      {tab === "hosts" && <PoolHostsTab pool={pool} />}
      {tab === "events" && showEvents && <PoolEvents name={name} owner={poolOwner} />}
      <p className="page-note">
        Pool charts come from per-pool samples, kept by the pool's id: a rename keeps its history; a removed pool and a new one of its name stay apart. History begins when luxd started taking them; older ranges are empty, not zero.
        {pool.platform && !scope.operator ? " Runs, allocation and cost here are your own; hosts and capacity are the pool's." : ""}
      </p>
    </div>
  );
}

/** Costs are hourly: the 1h range reads 6h. */
function costSince(range: string): string {
  return range === "1h" ? "6h" : range;
}

function PoolTiles({ pool, metrics, loading }: { pool: Pool; metrics?: PoolMetrics; loading: boolean }) {
  const scope = useScope();
  const clock = useNow();
  const n = metrics?.now;
  const since = costSince(scope.range);
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
      <StatTile label={`Cost (${since})`} loading={cost.loading} value={<MoneyList amounts={cost.data?.totals} />} unit={cost.data?.idle?.length ? <>list price · <MoneyList amounts={cost.data.idle} /> idle</> : "list price"} />
    </div>
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

function PoolCharts({ metrics }: { metrics?: PoolMetrics }) {
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
  const every = m?.resolution === 0 ? "sample" : m?.resolution === 60 ? "minute" : m?.resolution === 3600 ? "hour" : "sample";
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
      <Card title="CPU" subtitle={`allocated vs capacity${cut}`}>
        <TimeSeriesChart x={s.cpu.x} ys={s.cpu.ys} series={[{ label: "Allocated", color: 1, area: true }, { label: "Capacity", color: "var(--fg-faint)", dashed: true, step: true }]} unit="cores" />
      </Card>
      <Card title="Memory" subtitle={`allocated vs capacity${cut}`}>
        <TimeSeriesChart x={s.mem.x} ys={s.mem.ys} series={[{ label: "Allocated", color: 7, area: true }, { label: "Capacity", color: "var(--fg-faint)", dashed: true, step: true }]} unit="bytes" />
      </Card>
      <Card title="Hosts" subtitle="by state">
        <TimeSeriesChart x={s.hosts.x} ys={s.hosts.ys} series={[{ label: "Ready", color: 3, step: true, area: true }, { label: "Provisioning", color: 1, step: true }, { label: "Draining", color: 4, step: true }, { label: "Lost", color: 8, step: true }]} unit="count" />
      </Card>
      <Card title="Runs" subtitle="running and queued">
        <TimeSeriesChart x={s.runs.x} ys={s.runs.ys} series={[{ label: "Running", color: 1, area: true }, { label: "Queued", color: 2 }]} unit="count" />
      </Card>
      <Card title="Started / finished" subtitle={`per ${every}`}>
        <TimeSeriesChart x={s.flow.x} ys={s.flow.ys} series={[{ label: "Started", color: 1, step: true }, { label: "Finished", color: 3, step: true }]} unit="count" />
      </Card>
      <Card title="Launches" subtitle={`launched vs failed, per ${every}`}>
        <TimeSeriesChart x={s.launch.x} ys={s.launch.ys} series={[{ label: "Launched", color: 3, step: true }, { label: "Launch failed", color: 8, step: true }]} unit="count" />
      </Card>
    </div>
  );
}

interface FamilyChart {
  currency: string;
  x: number[];
  ys: (number | null)[][];
  series: { label: string; color: string }[];
}

/** Every bucket of the range, in epoch seconds, and each one's position. */
function bucketAxis(d: PoolCost): { x: number[]; index: Map<number, number> } {
  const step = d.interval === "hour" ? 3600 : 86400;
  const start = Math.floor(Date.parse(d.from) / 1000 / step) * step;
  const end = Math.floor(Date.parse(d.to) / 1000);
  const x: number[] = [];
  for (let t = start; t < end; t += step) x.push(t);
  return { x, index: new Map(x.map((t, i) => [t, i])) };
}

/** One stacked chart per currency: a series per family over every bucket (a bucket without a row is a gap, not zero). */
function familyCharts(d: PoolCost | undefined): FamilyChart[] {
  if (!d?.series.length) return [];
  const { x, index } = bucketAxis(d);
  const meta = new Map((d.families ?? []).map((f) => [f.family, f]));
  return [...new Set(d.series.map((r) => r.currency))].sort().map((currency) => {
    const rows = d.series.filter((r) => r.currency === currency);
    const families = [...new Set(rows.map((r) => r.family))].sort((a, b) => (a === "compute" ? -1 : b === "compute" ? 1 : a.localeCompare(b)));
    const display = familyDisplay(families.map((f) => meta.get(f) ?? { family: f }));
    const ys = families.map(() => x.map((): number | null => null));
    for (const r of rows) {
      const i = index.get(Math.floor(Date.parse(r.at) / 1000));
      // Chart geometry only: figures in text come from the strings.
      if (i != null) ys[families.indexOf(r.family)]![i] = Number(r.amount);
    }
    return { currency, x, ys, series: families.map((f) => display.get(f)!) };
  });
}

/** Host time per bucket, allocated and idle, per currency. */
function hostTimeCharts(d: PoolCost | undefined): FamilyChart[] {
  if (!d?.hostSeries?.length) return [];
  const { x, index } = bucketAxis(d);
  return [...new Set(d.hostSeries.map((r) => r.currency))].sort().map((currency) => {
    const ys = [x.map((): number | null => null), x.map((): number | null => null)];
    for (const r of d.hostSeries!.filter((h) => h.currency === currency)) {
      const i = index.get(Math.floor(Date.parse(r.at!) / 1000));
      if (i != null) {
        ys[0]![i] = Number(r.allocated);
        ys[1]![i] = Number(r.unallocated);
      }
    }
    return { currency, x, ys, series: [{ label: "Allocated to runs", color: "var(--chart-1)" }, { label: "Idle (unallocated)", color: "var(--st-neutral-dot)" }] };
  });
}

interface TopRun {
  id: string;
  name?: string;
  currency: string;
  amount: string;
  estimate: boolean;
}

interface HostTimeRow {
  hostId: string;
  hostName: string;
  currency: string;
  allocated: string;
  unallocated: string;
}

function PoolCostTab({ name, owner, operatorView }: { name: string; owner?: PoolOwner; operatorView: boolean }) {
  const scope = useScope();
  // 30d reads daily.
  const since = costSince(scope.range);
  const interval: "hour" | "day" = since === "30d" ? "day" : "hour";
  const q = useScopedQuery(`pool-cost:${name}:${owner}:${since}:${interval}`, (t, s) => api.poolCost(name, t, owner, since, interval, s), { interval: 60_000 });
  const charts = useMemo(() => familyCharts(q.data), [q.data]);
  const idle = useMemo(() => hostTimeCharts(q.data), [q.data]);
  const topCols = useMemo<Column<TopRun>[]>(
    () => [
      { key: "name", header: "Run", cell: (r) => <RunNameLink id={r.id} name={r.name} />, sortValue: (r) => r.name || r.id, lead: true },
      { key: "id", header: "Id", cell: (r) => <RunLink id={r.id} />, sortValue: (r) => r.id, mono: true, width: 170 },
      { key: "cost", header: "Cost", cell: (r) => <span>{r.estimate ? "~" : ""}<Money amount={r.amount} currency={r.currency} /></span>, sortValue: (r) => Number(r.amount), align: "right", mono: true, width: 120 },
    ],
    [],
  );
  const hostCols = useMemo<Column<HostTimeRow>[]>(
    () => [
      { key: "name", header: "Host", cell: (h) => h.hostName, sortValue: (h) => h.hostName, lead: true },
      { key: "allocated", header: "Allocated", cell: (h) => <Money amount={h.allocated} currency={h.currency} />, sortValue: (h) => Number(h.allocated), align: "right", mono: true, width: 120 },
      { key: "idle", header: "Idle", cell: (h) => <span className="muted"><Money amount={h.unallocated} currency={h.currency} /></span>, sortValue: (h) => Number(h.unallocated), align: "right", mono: true, width: 120 },
    ],
    [],
  );
  const top = (q.data?.topRuns ?? []).slice().sort((a, b) => a.currency.localeCompare(b.currency) || compareMoney(b.amount, a.amount));
  return (
    <div className="stack">
      <SectionHeader title="Cost" note={<span className="row">{interval === "hour" ? "hourly" : "daily"} over the last {since}<ListPriceNote /></span>} />
      <ErrorStrip error={q.error} />
      <div className="grid grid-2">
        {charts.length === 0 ? (
          <Card title="Cost by family" subtitle={`over the last ${since}`}>
            <EmptyState compact title={q.loading ? "Loading…" : "No cost recorded in this range"} description={q.loading ? undefined : "No figure is not a zero: nothing has been costed yet."} />
          </Card>
        ) : (
          charts.map((c) => (
            <Card key={c.currency} title={charts.length > 1 ? `Cost by family · ${c.currency}` : "Cost by family"} subtitle={`stacked, per ${interval}`}>
              <TimeSeriesChart x={c.x} ys={c.ys} series={c.series} unit="money" currency={c.currency} stacked legend />
            </Card>
          ))
        )}
        {operatorView &&
          (idle.length === 0 ? (
            <Card title="Host time" subtitle="allocated to runs vs idle">
              <EmptyState compact title={q.loading ? "Loading…" : "No host time recorded in this range"} />
            </Card>
          ) : (
            idle.map((c) => (
              <Card key={c.currency} title={idle.length > 1 ? `Host time · ${c.currency}` : "Host time"} subtitle="allocated to runs vs idle · idle is not in the runs' cost">
                <TimeSeriesChart x={c.x} ys={c.ys} series={c.series} unit="money" currency={c.currency} stacked legend />
              </Card>
            ))
          ))}
      </div>
      <div className="grid grid-2">
        <Card title="Top runs in this pool" subtitle={`by cost, over the last ${since}`} flush>
          <Table columns={topCols} rows={top} rowKey={(r) => `${r.currency}:${r.id}`} defaultSort={{ key: "cost", dir: "desc" }} onRowClick={(r) => go(runPath(r.id))} loading={q.loading} loadingRows={3} empty="No Run has a cost here in this range." dense />
        </Card>
        {operatorView && (
          <Card title="Cost by host" subtitle="operators only · allocated / idle" flush>
            <Table
              columns={hostCols}
              rows={(q.data?.hosts ?? []).map((h) => ({ hostId: h.hostId ?? "", hostName: h.hostName ?? h.hostId ?? "", currency: h.currency, allocated: h.allocated, unallocated: h.unallocated }))}
              rowKey={(h) => `${h.currency}:${h.hostId}`}
              defaultSort={{ key: "allocated", dir: "desc" }}
              onRowClick={(h) => go(hostPath(h.hostId))}
              loading={q.loading}
              loadingRows={3}
              empty="No host time recorded here in this range."
              dense
            />
          </Card>
        )}
      </div>
    </div>
  );
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

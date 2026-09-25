import { Card, formatBytes, formatCores, formatCount, formatDuration, formatElapsed, PageHeader, SectionHeader, StatTile, TimeSeriesChart } from "@lux/design-system";
import { api, useNow } from "../../api/index.ts";
import { go } from "../router.tsx";
import { useScope, useScopedQuery } from "../scope.tsx";
import { ErrorBlock, ErrorStrip, useSeries } from "./common.tsx";
import { ActivityFeed } from "./ActivityFeed.tsx";

/** What the Queued tile counts. */
const QUEUED = ["submitted", "resuming", "provisioning"];

const RANGE_LABEL: Record<string, string> = { "1h": "the last hour", "6h": "the last 6 hours", "24h": "the last 24 hours", "7d": "the last 7 days", "30d": "the last 30 days" };

export function Overview() {
  const scope = useScope();
  const now = useNow();
  // Status counts hosts too, which have no events: it keeps polling.
  const status = useScopedQuery("status", api.status, { interval: 5000 });
  const history = useScopedQuery(`history:${scope.range}`, (t, s) => api.history(t, scope.range, s), { interval: 30_000 });
  const st = status.data;
  const runs = st?.runs ?? {};
  const hosts = st?.hosts ?? {};

  const h = history.data?.samples;
  const runSeries = useSeries(h, [(s) => s.runs?.running, (s) => s.queued]);
  const flow = useSeries(h, [(s) => s.started, (s) => s.finished]);
  const latency = useSeries(h, [(s) => s.startP50, (s) => s.startP95]);
  const cpu = useSeries(h, [(s) => s.allocatedCpus, (s) => s.capacityCpus]);
  const mem = useSeries(h, [(s) => s.allocatedMemory, (s) => s.capacityMemory]);
  const hostSeries = useSeries(h, [(s) => s.hosts?.ready, (s) => s.hosts?.draining, (s) => s.hosts?.lost]);

  const loading = status.loading;
  const lostHosts = hosts.lost ?? 0;
  const resolution = history.data?.resolution;
  const interval = resolution === 0 ? "10s" : resolution === 60 ? "1m" : resolution === 3600 ? "1h" : undefined;
  const where = scope.apiTenant ? `tenant ${scope.apiTenant}` : scope.operator ? "all tenants" : "your runs and hosts";

  return (
    <div className="page page-wide">
      <PageHeader title="Overview" description={<span>{where} · charts over {RANGE_LABEL[scope.range] ?? scope.range}</span>} />
      {status.error && !st && <ErrorBlock error={status.error} onRetry={status.refetch} />}
      {status.error && st && <ErrorStrip error={`Showing stale numbers: the last refresh failed (${status.error}).`} />}
      <div className="grid grid-stats">
        <StatTile label="Running" onClick={() => go("/runs?state=running")} loading={loading} value={formatCount(runs.running ?? 0)} unit={st ? `${st.busy} busy · ${st.idle} idle` : undefined} />
        <StatTile label="Queued" onClick={() => go(`/runs?state=${QUEUED.join(",")}`)} loading={loading} value={formatCount(st?.queued ?? 0)} unit={st?.oldestQueuedAt ? `oldest ${formatElapsed(st.oldestQueuedAt, now)}` : undefined} tone={(st?.queued ?? 0) > 0 && st?.oldestQueuedAt && now - Date.parse(st.oldestQueuedAt) > 300_000 ? "warn" : "default"} />
        <StatTile label="Start latency (1h)" loading={loading} value={formatDuration(st?.startLatency.p50)} unit={st?.startLatency.n ? `p95 ${formatDuration(st.startLatency.p95)} · n=${st.startLatency.n}` : "no starts"} />
        <StatTile label="Hosts ready" onClick={() => go("/hosts")} loading={loading} value={formatCount(hosts.ready ?? 0)} unit={`${hosts.draining ?? 0} draining · ${lostHosts} lost`} />
        {lostHosts > 0 && <StatTile label="Hosts lost" onClick={() => go("/hosts?state=lost")} value={formatCount(lostHosts)} unit="stopped heartbeating" tone="danger" />}
        <StatTile label="CPU allocated" loading={loading} value={formatCores(st?.allocated.cpus)} unit={`of ${formatCores(st?.capacity.cpus)}`} />
        <StatTile label="Memory allocated" loading={loading} value={formatBytes(st?.allocated.memory)} unit={`of ${formatBytes(st?.capacity.memory)}`} />
      </div>
      <div className="overview-grid">
        <div className="stack overview-charts">
          <SectionHeader title="Trends" note={interval ? `${interval} samples` : undefined} />
          <ErrorStrip error={history.error} />
          <div className="grid grid-charts">
            <Card title="Runs" subtitle="running and queued">
              <TimeSeriesChart x={runSeries.x} ys={runSeries.ys} series={[{ label: "Running", color: 1, area: true }, { label: "Queued", color: 2 }]} unit="count" />
            </Card>
            <Card title="Started / finished" subtitle={interval ? `per ${interval} interval` : "per interval"}>
              <TimeSeriesChart x={flow.x} ys={flow.ys} series={[{ label: "Started", color: 1, step: true }, { label: "Finished", color: 3, step: true }]} unit="count" />
            </Card>
            <Card title="Start latency" subtitle="submit to first workload start">
              <TimeSeriesChart x={latency.x} ys={latency.ys} series={[{ label: "p50", color: 1 }, { label: "p95", color: 2, dashed: true }]} unit="duration" />
            </Card>
            <Card title="Hosts" subtitle="by state">
              <TimeSeriesChart x={hostSeries.x} ys={hostSeries.ys} series={[{ label: "Ready", color: 3, step: true, area: true }, { label: "Draining", color: 4, step: true }, { label: "Lost", color: 8, step: true }]} unit="count" />
            </Card>
            <Card title="CPU" subtitle="allocated vs capacity">
              <TimeSeriesChart x={cpu.x} ys={cpu.ys} series={[{ label: "Allocated", color: 1, area: true }, { label: "Capacity", color: "var(--fg-faint)", dashed: true }]} unit="cores" />
            </Card>
            <Card title="Memory" subtitle="allocated vs capacity">
              <TimeSeriesChart x={mem.x} ys={mem.ys} series={[{ label: "Allocated", color: 7, area: true }, { label: "Capacity", color: "var(--fg-faint)", dashed: true }]} unit="bytes" />
            </Card>
          </div>
        </div>
        <ActivityFeed />
      </div>
    </div>
  );
}

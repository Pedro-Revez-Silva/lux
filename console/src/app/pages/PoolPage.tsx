import { useMemo } from "react";
import { Badge, Card, formatBytes, formatCores, KeyValue, PageHeader, StatePill, Table, type Column } from "@lux/design-system";
import { api, type Host, type Pool } from "../../api/index.ts";
import { go } from "../router.tsx";
import { useScope, useScopedQuery } from "../scope.tsx";
import { DASH, ErrorBlock, ErrorStrip, hostPath, labelsText, PageSkeleton, RelativeTime } from "./common.tsx";
import { InfraEvents } from "./InfraEvents.tsx";

/** The Pools page's poll: this page's too. */
const POLL = 15_000;

export function PoolPage({ name }: { name: string }) {
  const scope = useScope();
  const pools = useScopedQuery("pools", api.pools, { interval: POLL });
  const hosts = useScopedQuery(`pool-hosts:${name}`, (t, s) => api.hosts(t, { pool: name }, s), { interval: POLL });

  // A tenant's own pool shadows the platform's of the same name, as it does
  // for its Runs (and for this page's events).
  const matches = (pools.data ?? []).filter((p) => p.name === name).sort((a, b) => Number(a.platform) - Number(b.platform));
  const pool: Pool | undefined = matches[0];
  const ambiguous = scope.showTenant && matches.length > 1;
  const own = useMemo(() => (hosts.data ?? []).filter((h) => pool && h.platform === pool.platform && (pool.platform || h.tenant === pool.tenant)), [hosts.data, pool]);

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
        <ErrorBlock error={`No pool named ${name}${scope.apiTenant ? " for this tenant" : ""}.`} />
      </div>
    );
  }
  if (ambiguous) {
    return (
      <div className="page">
        <PageHeader title={name} />
        <ErrorBlock error={`${matches.length} pools are named ${name} (${matches.map((p) => (p.platform ? "platform" : p.tenant)).join(", ")}): pick a tenant.`} />
      </div>
    );
  }

  const template = pool.template && Object.keys(pool.template).length > 0 ? <span className="mono">{labelsText(pool.template)}</span> : DASH;
  return (
    <div className="page">
      <PageHeader
        title={pool.name}
        badges={
          <>
            <Badge outline>{pool.provider}</Badge>
            {pool.platform && <Badge outline>platform</Badge>}
            {pool.shared && <Badge tone="info">shared</Badge>}
          </>
        }
        description={
          <>
            {pool.tenant && <span>tenant {pool.tenant}</span>}
            <span>
              {own.filter((h) => h.state === "ready").length} ready / {own.length} hosts
            </span>
          </>
        }
      />
      <ErrorStrip error={pools.error} />

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

      <Card flush title="Hosts" subtitle={`${own.length} not terminated`}>
        <ErrorStrip error={hosts.error} />
        <PoolHosts hosts={own} loading={hosts.loading} />
      </Card>

      {/* A platform pool's events name other tenants' Runs: the operators'. */}
      {(scope.operator || !pool.platform) && (
        <InfraEvents
          queryKey={`pool-events:${name}@${scope.tenant}`}
          page={(before, s) => api.poolEvents(name, scope.apiTenant, before, s)}
          interval={POLL}
          subtitle="scale-ups, launches, placements and releases"
        />
      )}
    </div>
  );
}

function PoolHosts({ hosts, loading }: { hosts: Host[]; loading: boolean }) {
  const cols = useMemo<Column<Host>[]>(
    () => [
      { key: "name", header: "Host", cell: (h) => h.name, sortValue: (h) => h.name, lead: true },
      { key: "state", header: "State", cell: (h) => <StatePill kind="host" state={h.state} />, sortValue: (h) => h.state, width: 140 },
      { key: "runs", header: "Runs", cell: (h) => h.liveRuns, sortValue: (h) => h.liveRuns, align: "right", mono: true, width: 70 },
      { key: "alloc", header: "Allocated", cell: (h) => `${formatCores(h.allocated.cpus ?? 0)} · ${formatBytes(h.allocated.memory ?? 0)}`, mono: true, width: 170, optional: true },
      { key: "type", header: "Instance", cell: (h) => h.instanceType ?? DASH, mono: true, width: 130, optional: true },
      { key: "heartbeat", header: "Heartbeat", cell: (h) => <RelativeTime at={h.lastHeartbeat} />, align: "right", width: 110 },
    ],
    [],
  );
  return <Table columns={cols} rows={hosts} rowKey={(h) => h.id} loading={loading} onRowClick={(h) => go(hostPath(h.id))} defaultSort={{ key: "name", dir: "asc" }} empty="No hosts." dense />;
}

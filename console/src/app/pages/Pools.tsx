import { useMemo } from "react";
import { Badge, Card, PageHeader, Table, type Column } from "@lux/design-system";
import { api, type Pool } from "../../api/index.ts";
import { useScope, useScopedQuery } from "../scope.tsx";
import { DASH, ErrorBlock, ErrorStrip, labelsText } from "./common.tsx";

interface PoolRow extends Pool {
  key: string;
  hostCount: number;
  readyCount: number;
}

export function Pools() {
  const { showTenant } = useScope();
  const pools = useScopedQuery("pools", api.pools, { interval: 15_000 });
  const hosts = useScopedQuery("hosts", (t, s) => api.hosts(t, {}, s), { interval: 15_000 });

  const rows = useMemo<PoolRow[]>(() => {
    const counts = new Map<string, { n: number; ready: number }>();
    for (const h of hosts.data ?? []) {
      // Host rows carry the tenant name (empty for platform), like pools do.
      const k = `${h.platform ? "" : h.tenant ?? ""}/${h.pool}`;
      const c = counts.get(k) ?? { n: 0, ready: 0 };
      c.n++;
      if (h.state === "ready") c.ready++;
      counts.set(k, c);
    }
    return (pools.data ?? []).map((p) => {
      const k = `${p.platform ? "" : p.tenant ?? ""}/${p.name}`;
      const c = counts.get(k);
      return { ...p, key: k, hostCount: c?.n ?? 0, readyCount: c?.ready ?? 0 };
    });
  }, [pools.data, hosts.data]);

  const cols = useMemo<Column<PoolRow>[]>(() => {
    const c: Column<PoolRow>[] = [{ key: "name", header: "Pool", cell: (p) => p.name, sortValue: (p) => p.name, lead: true, width: 180 }];
    if (showTenant) c.push({ key: "tenant", header: "Tenant", cell: (p) => (p.platform ? <span className="muted">platform</span> : p.tenant || DASH), sortValue: (p) => (p.platform ? "" : p.tenant), width: 130 });
    c.push(
      { key: "provider", header: "Provider", cell: (p) => <span className="secondary">{p.provider}</span>, sortValue: (p) => p.provider, width: 110 },
      { key: "hosts", header: "Hosts", cell: (p) => `${p.readyCount} ready / ${p.hostCount}`, sortValue: (p) => p.hostCount, align: "right", mono: true, width: 140 },
      { key: "min", header: "Min", cell: (p) => p.minHosts, sortValue: (p) => p.minHosts, align: "right", mono: true, width: 70 },
      { key: "warm", header: "Warm", cell: (p) => p.warmHosts, sortValue: (p) => p.warmHosts, align: "right", mono: true, width: 70 },
      { key: "max", header: "Max", cell: (p) => p.maxHosts, sortValue: (p) => p.maxHosts, align: "right", mono: true, width: 70 },
      { key: "shared", header: "Shared", cell: (p) => (p.shared ? <Badge tone="info">shared</Badge> : DASH), sortValue: (p) => (p.shared ? 1 : 0), width: 100 },
      { key: "template", header: "Template", cell: (p) => (p.template && Object.keys(p.template).length > 0 ? <span className="mono muted">{labelsText(p.template)}</span> : DASH), optional: true },
    );
    return c;
  }, [showTenant]);

  return (
    <div className="page page-list">
      <PageHeader title="Pools" description={`${rows.length} pools · host counts from the hosts list (terminated excluded)`} />
      <Card flush>
        <ErrorStrip error={rows.length > 0 ? pools.error ?? hosts.error : hosts.error} />
        {pools.error && rows.length === 0 && !pools.loading ? (
          <ErrorBlock error={pools.error} onRetry={pools.refetch} />
        ) : (
          <Table columns={cols} rows={rows} rowKey={(p) => p.key} loading={pools.loading} defaultSort={{ key: "name", dir: "asc" }} empty="No pools." />
        )}
      </Card>
    </div>
  );
}

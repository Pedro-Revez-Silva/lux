import { useMemo, useState } from "react";
import { Badge, Button, Card, ConfirmDialog, PageHeader, Table, useToast, type Column } from "@lux/design-system";
import { api, errorText, type Pool } from "../../api/index.ts";
import { useScope, useScopedQuery } from "../scope.tsx";
import { DASH, ErrorBlock, ErrorStrip, labelsText } from "./common.tsx";

interface PoolRow extends Pool {
  key: string;
  hostCount: number;
  readyCount: number;
}

/** The owner a pool's default mark is among: its tenant's name, or "" for the platform. */
function owner(p: Pool): string {
  return p.platform ? "" : p.tenant ?? "";
}

export function Pools() {
  const { showTenant, operator } = useScope();
  const toast = useToast();
  const pools = useScopedQuery("pools", api.pools, { interval: 15_000 });
  const [marking, setMarking] = useState<PoolRow | null>(null);
  const [busy, setBusy] = useState(false);
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
    const c: Column<PoolRow>[] = [
      {
        key: "name",
        header: "Pool",
        cell: (p) => (
          <>
            {p.name} {p.isDefault && <Badge tone="accent">Default</Badge>}
          </>
        ),
        sortValue: (p) => p.name,
        lead: true,
        width: 200,
      },
    ];
    if (showTenant) c.push({ key: "tenant", header: "Tenant", cell: (p) => (p.platform ? <span className="muted">platform</span> : p.tenant || DASH), sortValue: (p) => (p.platform ? "" : p.tenant), width: 130 });
    c.push(
      { key: "provider", header: "Provider", cell: (p) => <span className="secondary">{p.provider}</span>, sortValue: (p) => p.provider, width: 110 },
      { key: "hosts", header: "Hosts", cell: (p) => `${p.readyCount} ready / ${p.hostCount}`, sortValue: (p) => p.hostCount, align: "right", mono: true, width: 140 },
      { key: "min", header: "Min", cell: (p) => p.minHosts, sortValue: (p) => p.minHosts, align: "right", mono: true, width: 70 },
      { key: "warm", header: "Warm", cell: (p) => p.warmHosts, sortValue: (p) => p.warmHosts, align: "right", mono: true, width: 70 },
      { key: "max", header: "Max", cell: (p) => p.maxHosts, sortValue: (p) => p.maxHosts, align: "right", mono: true, width: 70 },
      { key: "shared", header: "Shared", cell: (p) => (p.shared ? <Badge tone="info">shared</Badge> : DASH), sortValue: (p) => (p.shared ? 1 : 0), width: 100 },
      { key: "template", header: "Template", cell: (p) => (p.template && Object.keys(p.template).length > 0 ? <span className="mono muted">{labelsText(p.template)}</span> : DASH), optional: true },
      {
        key: "actions",
        header: "",
        // Operators mark any pool (a platform pool as the platform's default); a tenant, its own pools.
        cell: (p) =>
          !p.isDefault && (operator || !p.platform) ? (
            <Button size="sm" variant="ghost" onClick={() => setMarking(p)}>
              Make default
            </Button>
          ) : null,
        align: "right",
        width: 140,
      },
    );
    return c;
  }, [showTenant, operator]);

  // The pool whose mark the new one takes: the same tenant's, or the platform's.
  const current = marking ? rows.find((p) => p.isDefault && p.platform === marking.platform && owner(p) === owner(marking)) : undefined;
  const whose = marking?.platform ? "the platform's" : showTenant || operator ? `tenant ${marking?.tenant}'s` : "your";
  const makeDefault = async () => {
    if (!marking) return;
    setBusy(true);
    try {
      await api.makePoolDefault(marking.platform ? undefined : marking.tenant, marking.name);
      toast({ title: `${marking.name} is ${whose} default pool`, tone: "success" });
      setMarking(null);
      await pools.refetch();
    } catch (e) {
      toast({ title: "Make default failed", description: errorText(e), tone: "danger" });
    } finally {
      setBusy(false);
    }
  };

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
      <ConfirmDialog
        open={marking != null}
        title={`Make ${marking?.name} ${whose} default pool?`}
        description={
          <>
            {current ? `${current.name} is ${whose} default pool now.` : `There is no default pool now: Runs naming no pool go to a pool named "default".`}{" "}
            {marking?.platform
              ? `From now on, Runs that name no pool go to ${marking?.name}, for tenants without a default pool of their own.`
              : `From now on, Runs that name no pool go to ${marking?.name}.`}{" "}
            Runs already submitted keep their pool.
          </>
        }
        confirmLabel="Make default"
        loading={busy}
        onConfirm={() => void makeDefault()}
        onCancel={() => setMarking(null)}
      />
    </div>
  );
}

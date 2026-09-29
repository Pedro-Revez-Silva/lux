import { useMemo, useState } from "react";
import { Badge, Button, Card, ConfirmDialog, PageHeader, Table, useToast, type Column } from "@lux/design-system";
import { api, errorText, type Pool } from "../../api/index.ts";
import { go, Link } from "../router.tsx";
import { useScope, useScopedQuery } from "../scope.tsx";
import { DASH, ErrorBlock, ErrorStrip, labelsText, poolPath } from "./common.tsx";
import { currentDefaultText } from "./defaultPool.ts";

interface PoolRow extends Pool {
  key: string;
  hostCount: number;
  readyCount: number;
}

export function Pools() {
  const { showTenant, operator } = useScope();
  const toast = useToast();
  const pools = useScopedQuery("pools", api.pools, { interval: 15_000 });
  const [marking, setMarking] = useState<PoolRow | null>(null);
  const [renaming, setRenaming] = useState<PoolRow | null>(null);
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
            <Link to={poolLink(p, showTenant)}>{p.name}</Link> {p.isDefault && <Badge tone="accent">Default</Badge>}
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
        // Operators mark and rename any pool (a platform pool as the
        // platform's default); a tenant, its own pools. The row opens the
        // pool's page: the buttons' click and Enter stay here.
        cell: (p) =>
          operator || !p.platform ? (
            <>
              {!p.isDefault && (
                <Button
                  size="sm"
                  variant="ghost"
                  onClick={(e) => {
                    e.stopPropagation();
                    setMarking(p);
                  }}
                  onKeyDown={(e) => e.stopPropagation()}
                >
                  Make default
                </Button>
              )}{" "}
              <Button
                size="sm"
                variant="ghost"
                onClick={(e) => {
                  e.stopPropagation();
                  setRenaming(p);
                }}
                onKeyDown={(e) => e.stopPropagation()}
              >
                Rename
              </Button>
            </>
          ) : null,
        align: "right",
        width: 220,
      },
    );
    return c;
  }, [showTenant, operator]);

  let whose = "your";
  if (marking?.platform) {
    whose = "the platform's";
  } else if (operator) {
    whose = `tenant ${marking?.tenant}'s`;
  }
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

  const rename = async (p: PoolRow, newName: string) => {
    setBusy(true);
    try {
      const r = await api.renamePool(p.platform ? undefined : p.tenant, p.name, newName.trim(), p.platform);
      toast({ title: `Renamed ${p.name} to ${r.name}`, tone: "success" });
      setRenaming(null);
      await Promise.all([pools.refetch(), hosts.refetch()]);
    } catch (e) {
      toast({ title: "Rename failed", description: errorText(e), tone: "danger" });
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
          <Table columns={cols} rows={rows} rowKey={(p) => p.key} onRowClick={(p) => go(poolLink(p, showTenant))} loading={pools.loading} defaultSort={{ key: "name", dir: "asc" }} empty="No pools." />
        )}
      </Card>
      <ConfirmDialog
        open={renaming != null}
        title={`Rename ${renaming?.name ?? ""}?`}
        description="Only the name changes: its hosts, instances and Runs stay with the pool, and a default pool stays the default. The old name is free at once; Runs naming it no longer find this pool."
        confirmLabel="Rename pool"
        input={{ label: "New name", placeholder: renaming?.name, required: true }}
        loading={busy}
        onConfirm={(name) => renaming && void rename(renaming, name ?? "")}
        onCancel={() => setRenaming(null)}
      />
      <ConfirmDialog
        open={marking != null}
        title={`Make ${marking?.name} ${whose} default pool?`}
        description={
          <>
            {marking && currentDefaultText(rows, marking, whose)}{" "}
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

/** A platform pool's page is the platform's; across tenants, a tenant's pool is linked in its tenant's scope (names repeat across tenants). */
function poolLink(p: Pool, acrossTenants: boolean): string {
  return poolPath(p.name, { platform: p.platform, tenant: acrossTenants ? p.tenant : undefined });
}

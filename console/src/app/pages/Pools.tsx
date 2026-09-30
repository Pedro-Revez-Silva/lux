import { useMemo, useState } from "react";
import { Badge, Card, ConfirmDialog, IconButton, ListPriceNote, MoneyList, PageHeader, Sparkline, Table, useToast, type Column } from "@lux/design-system";
import { IconPencil, IconStar } from "@lux/design-system/icons";
import { api, errorText, type Pool, type PoolStats } from "../../api/index.ts";
import { go, Link } from "../router.tsx";
import { useScope, useScopedQuery } from "../scope.tsx";
import { DASH, ErrorBlock, ErrorStrip, poolPath, UsageBar } from "./common.tsx";
import { currentDefaultText } from "./defaultPool.ts";

interface PoolRow extends Pool {
  key: string;
  hostCount: number;
  readyCount: number;
  stats?: PoolStats;
}

/** A pool's settings in a line: its provider, and its size bounds or that it is static. */
function settingsText(p: Pool): string {
  if (p.provider === "static") return p.hourlyPrice ? `static hosts · ${p.hourlyPrice} ${p.currency ?? ""}/h` : "static hosts";
  const spot = p.template?.spot ? " · spot" : "";
  return `${p.provider} · min ${p.minHosts} · warm ${p.warmHosts}${p.warmWhileActive ? " (in use)" : ""} · max ${p.maxHosts || "∞"}${spot}`;
}

/** Cost figures sort by their first currency's amount (ordering only). */
const costValue = (p: PoolRow) => (p.stats?.cost[0] ? Number(p.stats.cost[0].amount) : null);

export function Pools() {
  const { showTenant, operator, range } = useScope();
  const toast = useToast();
  const pools = useScopedQuery("pools", api.pools, { interval: 15_000 });
  const [marking, setMarking] = useState<PoolRow | null>(null);
  const [renaming, setRenaming] = useState<PoolRow | null>(null);
  const [busy, setBusy] = useState(false);
  // Every pool's figures in one read, by pool id: never a request per pool.
  const stats = useScopedQuery(`pool-stats:${range}`, (t, s) => api.poolStats(t, range, s), { interval: 30_000 });

  const rows = useMemo<PoolRow[]>(() => {
    const byId = new Map((stats.data ?? []).map((x) => [x.id, x]));
    return (pools.data ?? []).map((p) => {
      const st = byId.get(p.id);
      const hostCount = st ? Object.values(st.hosts).reduce((a, b) => a + b, 0) : 0;
      return { ...p, key: p.id, hostCount, readyCount: st?.hosts.ready ?? 0, stats: st };
    });
  }, [pools.data, stats.data]);

  const cols = useMemo<Column<PoolRow>[]>(() => {
    const c: Column<PoolRow>[] = [
      {
        key: "name",
        header: "Pool",
        cell: (p) => (
          <>
            <Link to={poolLink(p, showTenant)}>{p.name}</Link> {p.isDefault && <Badge tone="accent">Default</Badge>} {p.shared && <Badge tone="info">shared</Badge>}
          </>
        ),
        sortValue: (p) => p.name,
        lead: true,
        width: 168,
      },
    ];
    if (showTenant) c.push({ key: "tenant", header: "Tenant", cell: (p) => (p.platform ? <span className="muted">platform</span> : p.tenant || DASH), sortValue: (p) => (p.platform ? "" : p.tenant), width: 96 });
    c.push(
      { key: "hosts", header: "Hosts", cell: (p) => `${p.readyCount} ready / ${p.hostCount}`, sortValue: (p) => p.readyCount * 1e6 + p.hostCount, align: "right", mono: true, width: 132 },
      {
        key: "cpu",
        header: "CPU allocated",
        cell: (p) => (p.stats && p.stats.capacityCpus > 0 ? <UsageBar used={p.stats.allocatedCpus} total={p.stats.capacityCpus} unit="cores" /> : DASH),
        sortValue: (p) => (p.stats && p.stats.capacityCpus > 0 ? p.stats.allocatedCpus / p.stats.capacityCpus : null),
        sortFirst: "desc",
        width: 150,
      },
      {
        key: "runs",
        header: `Runs (${range})`,
        cell: (p) =>
          p.stats ? (
            <span className="spark-row">
              <Sparkline values={p.stats.runsHourly} width={56} height={20} color="var(--chart-1)" endDot={false} title={`${p.stats.runsStarted} Runs started, per hour`} />
              <span className="mono muted">{p.stats.runsStarted}</span>
            </span>
          ) : (
            DASH
          ),
        sortValue: (p) => p.stats?.runsStarted,
        sortFirst: "desc",
        width: 128,
      },
      {
        key: "fails",
        header: "Launch fails",
        cell: (p) => (p.stats == null ? DASH : p.stats.launchFailures > 0 ? <span className="text-danger">{p.stats.launchFailures}</span> : <span className="muted">0</span>),
        sortValue: (p) => p.stats?.launchFailures,
        align: "right",
        mono: true,
        width: 120,
      },
      { key: "cost", header: `Cost (${range})`, cell: (p) => (p.stats ? <MoneyList amounts={p.stats.cost} /> : DASH), sortValue: costValue, align: "right", mono: true, width: 116 },
      // The one column without a width: it takes what is left, and wraps
      // rather than clip. The provider leads it (the pool page's header
      // shows it as a badge).
      { key: "settings", header: "Settings", cell: (p) => <span className="muted">{settingsText(p)}</span>, sortValue: (p) => settingsText(p), wrap: true },
      {
        key: "actions",
        header: "",
        // Operators mark and rename any pool (a platform pool as the
        // platform's default); a tenant, its own pools. The row opens the
        // pool's page: the buttons' click and Enter stay here.
        cell: (p) =>
          operator || !p.platform ? (
            <span className="row-actions">
              {!p.isDefault && (
                <IconButton
                  size="sm"
                  label="Make default"
                  onClick={(e) => {
                    e.stopPropagation();
                    setMarking(p);
                  }}
                  onKeyDown={(e) => e.stopPropagation()}
                >
                  <IconStar size={14} />
                </IconButton>
              )}
              <IconButton
                size="sm"
                label="Rename"
                onClick={(e) => {
                  e.stopPropagation();
                  setRenaming(p);
                }}
                onKeyDown={(e) => e.stopPropagation()}
              >
                <IconPencil size={14} />
              </IconButton>
            </span>
          ) : null,
        align: "right",
        width: 84,
      },
    );
    return c;
  }, [showTenant, operator, range]);

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
      await Promise.all([pools.refetch(), stats.refetch()]);
    } catch (e) {
      toast({ title: "Rename failed", description: errorText(e), tone: "danger" });
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="page page-list">
      <PageHeader
        title="Pools"
        description={
          <>
            <span>{rows.length} pools · Runs, launch failures and cost over the last {range}; hosts now (terminated excluded)</span>
            <ListPriceNote>costs are list prices</ListPriceNote>
          </>
        }
      />
      <Card flush>
        <ErrorStrip error={rows.length > 0 ? pools.error ?? stats.error : stats.error} />
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

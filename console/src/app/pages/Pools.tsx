import { useMemo, useRef, useState } from "react";
import { Badge, Button, Card, ConfirmDialog, PageHeader, Table, useToast, type Column } from "@lux/design-system";
import { api, errorText, invalidate, isApiError, type Pool, type PoolRenamed } from "../../api/index.ts";
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
  const [busy, setBusy] = useState(false);
  const hosts = useScopedQuery("hosts", (t, s) => api.hosts(t, {}, s), { interval: 15_000 });
  const [renaming, setRenaming] = useState<PoolRow | null>(null);
  // What would follow the rename, counted by luxd when the dialog opens.
  const [preview, setPreview] = useState<PoolRenamed | null>(null);
  // Which dialog is current: a count answered for one opened earlier (or
  // closed) is dropped.
  const dialog = useRef(0);

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

  // An operator names the owning tenant; a platform pool has none. A tenant
  // key acts on its own pools only.
  const owner = (p: Pool) => (operator && !p.platform ? p.tenant : undefined);

  const count = (p: PoolRow) => {
    const id = dialog.current;
    api.renamePool(owner(p), p.name, "", undefined, true).then(
      (r) => {
        if (dialog.current === id) setPreview(r);
      },
      (e) => {
        if (dialog.current !== id) return;
        toast({ title: `Cannot rename ${p.name}`, description: errorText(e), tone: "danger", duration: 8000 });
        closeRename();
      },
    );
  };

  const openRename = (p: PoolRow) => {
    dialog.current++;
    setRenaming(p);
    setPreview(null);
    count(p);
  };

  const closeRename = () => {
    dialog.current++;
    setRenaming(null);
  };

  // confirmed: the current name was typed. luxd requires it while the pool
  // has hosts, counted when it renames, not when the dialog opened.
  const rename = async (p: PoolRow, newName: string, confirmed: boolean) => {
    const id = dialog.current;
    setBusy(true);
    try {
      const r = await api.renamePool(owner(p), p.name, newName.trim(), confirmed ? p.name : undefined);
      const retag = r.instances > 0 ? `; its ${plural(r.instances, "instance")} keep running` : "";
      toast({ title: `Renamed ${p.name} to ${r.pool.name}`, description: `${plural(r.hosts, "host")} and ${plural(r.runs, "Run")} followed${retag}.`, tone: "success" });
      if (dialog.current === id) closeRename();
      invalidate((k) => k.startsWith("pools@") || k.startsWith("hosts@"));
    } catch (e) {
      if (isApiError(e) && e.code === "confirm_required" && dialog.current === id) {
        // Hosts joined since the count: count again, and ask for the name.
        setPreview(null);
        count(p);
      }
      toast({ title: "Rename failed", description: errorText(e), tone: "danger", duration: 8000 });
    } finally {
      setBusy(false);
    }
  };

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
        // Operators act on any pool (mark a platform pool as the platform's
        // default, rename it); a tenant, on its own pools. The row opens
        // the pool's page: the buttons' click and Enter stay here.
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
                onClick={(e) => {
                  e.stopPropagation();
                  openRename(p);
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
    // openRename reads only state setters and the scope's operator flag.
  }, [showTenant, operator]);

  const r = renaming;
  // Until luxd has counted, the name is asked for as if there were hosts.
  const mustType = r != null && (preview == null || preview.hosts > 0);
  let description = "Counting what follows the rename…";
  if (r && preview) {
    description = `${plural(preview.hosts, "host")} and ${plural(preview.runs, "Run")} not yet finished will follow the rename, as will finished Runs, should they be resumed.`;
    if (preview.instances > 0)
      description += ` Its ${plural(preview.instances, "instance")} keep running; their name tag follows in the background.`;
  }

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
        open={r != null}
        title={`Rename ${r?.name ?? ""}?`}
        description={description}
        confirmLabel="Rename pool"
        input={{ label: "New name", placeholder: r?.name, required: true }}
        // A pool with live hosts: the current name, typed, confirms.
        confirmText={mustType ? r.name : undefined}
        loading={busy}
        onConfirm={(name) => r && void rename(r, name ?? "", mustType)}
        onCancel={closeRename}
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

function plural(n: number, word: string): string {
  return `${n} ${word}${n === 1 ? "" : "s"}`;
}

/** A platform pool's page is the platform's; across tenants, a tenant's pool is linked in its tenant's scope (names repeat across tenants). */
function poolLink(p: Pool, acrossTenants: boolean): string {
  return poolPath(p.name, { platform: p.platform, tenant: acrossTenants ? p.tenant : undefined });
}

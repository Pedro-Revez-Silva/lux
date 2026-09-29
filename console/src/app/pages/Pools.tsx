import { useMemo, useRef, useState } from "react";
import { Badge, Button, Card, ConfirmDialog, PageHeader, Table, Tooltip, useToast, type Column } from "@lux/design-system";
import { api, errorText, invalidate, isApiError, type Pool, type PoolRenamed } from "../../api/index.ts";
import { useScope, useScopedQuery } from "../scope.tsx";
import { DASH, ErrorBlock, ErrorStrip, labelsText } from "./common.tsx";

interface PoolRow extends Pool {
  key: string;
  hostCount: number;
  readyCount: number;
}

export function Pools() {
  const { showTenant, operator } = useScope();
  const toast = useToast();
  const pools = useScopedQuery("pools", api.pools, { interval: 15_000 });
  const hosts = useScopedQuery("hosts", (t, s) => api.hosts(t, {}, s), { interval: 15_000 });
  const [renaming, setRenaming] = useState<PoolRow | null>(null);
  // What would follow the rename, counted by luxd when the dialog opens.
  const [preview, setPreview] = useState<PoolRenamed | null>(null);
  const [busy, setBusy] = useState(false);
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
      const retag = r.instances > 0 ? `; its ${plural(r.instances, "instance")} are being re-tagged` : "";
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
        cell: (p) =>
          p.renamedFrom ? (
            <>
              {p.name}{" "}
              <Tooltip content={`Renamed from ${p.renamedFrom}: its instances are being re-tagged, and it cannot be renamed again until that is done. No other pool can take that name until no instance has carried it for a while.`}>
                <Badge tone="warn">renaming</Badge>
              </Tooltip>
            </>
          ) : (
            p.name
          ),
        sortValue: (p) => p.name,
        lead: true,
        width: 220,
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
        // A platform pool is the operators' to rename.
        cell: (p) =>
          operator || !p.platform ? (
            <Button size="sm" disabled={!!p.renamedFrom} title={p.renamedFrom ? "Its previous rename is still re-tagging its instances" : undefined} onClick={() => openRename(p)}>
              Rename
            </Button>
          ) : null,
        align: "right",
        width: 100,
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
    description = `${plural(preview.hosts, "host")} and ${plural(preview.runs, "Run")} not yet finished will follow the rename; finished Runs keep the name they ran with.`;
    if (preview.instances > 0)
      description += ` Its ${plural(preview.instances, "instance")} keep running and are re-tagged with the new name; no other pool can take the name ${r.name} until no instance has carried it for a while.`;
  }

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
    </div>
  );
}

function plural(n: number, word: string): string {
  return `${n} ${word}${n === 1 ? "" : "s"}`;
}

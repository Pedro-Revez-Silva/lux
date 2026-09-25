import { useMemo } from "react";
import { Badge, Button, Card, formatBytes, formatCores, HOST_STATE_LIST, PageHeader, Select, Table, type Column } from "@lux/design-system";
import { api, type Host } from "../../api/index.ts";
import { go, Link, setSearchParams, useSearchParams } from "../router.tsx";
import { useScope, useScopedQuery } from "../scope.tsx";
import { DASH, ErrorBlock, ErrorStrip, HostLink, hostPath, hostRunsPath, IdLink, RelativeTime, StateCell, UsageBar } from "./common.tsx";

export function Hosts() {
  const { showTenant } = useScope();
  // Filters live in the URL, so a filtered list is a link.
  const params = useSearchParams();
  const pool = params.get("pool") ?? "";
  const state = params.get("state") ?? "";
  const all = params.get("all") === "true";
  const setPool = (v: string) => setSearchParams({ pool: v || null });
  const setState = (v: string) => setSearchParams({ state: v || null });
  const setAll = (v: boolean) => setSearchParams({ all: v ? "true" : null });
  const q = useScopedQuery(`hosts:${pool}:${state}:${all}`, (t, s) => api.hosts(t, { pool: pool || undefined, state: state || undefined, all }, s), { interval: 5000 });
  const pools = useScopedQuery("pools", api.pools, { interval: 60_000 });

  const poolOptions = useMemo(() => {
    const names = new Set<string>((pools.data ?? []).map((p) => p.name));
    for (const h of q.data ?? []) names.add(h.pool);
    return [{ value: "", label: "Any pool", text: "Any pool" }, ...[...names].sort().map((n) => ({ value: n, label: n, text: n }))];
  }, [pools.data, q.data]);

  const cols = useMemo<Column<Host>[]>(() => {
    const c: Column<Host>[] = [
      { key: "name", header: "Host", cell: (h) => <HostLink id={h.id} name={h.name} />, sortValue: (h) => h.name, lead: true, width: 170 },
    ];
    if (showTenant) c.push({ key: "tenant", header: "Tenant", cell: (h) => (h.platform ? <span className="muted">platform</span> : h.tenant || DASH), sortValue: (h) => (h.platform ? "" : h.tenant), width: 120, optional: true });
    c.push(
      { key: "pool", header: "Pool", cell: (h) => h.pool, sortValue: (h) => h.pool, width: 110 },
      {
        key: "state",
        header: "State",
        cell: (h) => (
          <StateCell kind="host" state={h.state} reason={h.stateReason}>
            {h.stateReason?.startsWith("evicting") ? <Badge tone="danger">evicting</Badge> : h.draining && h.state !== "draining" && <Badge tone="warn">draining</Badge>}
          </StateCell>
        ),
        sortValue: (h) => h.state,
      },
      { key: "runs", header: "Live runs", cell: (h) => <Link to={hostRunsPath(h.id)} title="Runs placed on this host">{`${h.liveRuns} / ${h.capacity.runs}`}</Link>, sortValue: (h) => h.liveRuns, align: "right", mono: true, width: 96 },
      { key: "cpu", header: "CPU", cell: (h) => <UsageBar used={h.allocated.cpus ?? 0} total={h.capacity.cpus} unit="cores" />, sortValue: (h) => (h.capacity.cpus ? (h.allocated.cpus ?? 0) / h.capacity.cpus : 0), width: 150 },
      { key: "mem", header: "Memory", cell: (h) => <UsageBar used={h.allocated.memory ?? 0} total={h.capacity.memory} unit="bytes" />, sortValue: (h) => (h.capacity.memory ? (h.allocated.memory ?? 0) / h.capacity.memory : 0), width: 150 },
      { key: "id", header: "Id", cell: (h) => <IdLink value={h.id} to={hostPath(h.id)} />, sortValue: (h) => h.id, mono: true, width: 210, optional: true },
      { key: "hb", header: "Heartbeat", cell: (h) => <RelativeTime at={h.lastHeartbeat} />, sortValue: (h) => (h.lastHeartbeat ? Date.parse(h.lastHeartbeat) : null), align: "right", width: 100 },
    );
    return c;
  }, [showTenant]);

  const hosts = q.data ?? [];
  const totals = useMemo(() => {
    const t = { cpus: 0, capCpus: 0, mem: 0, capMem: 0 };
    for (const h of hosts) {
      if (h.state !== "ready" && h.state !== "draining") continue;
      t.cpus += h.allocated.cpus ?? 0;
      t.capCpus += h.capacity.cpus;
      t.mem += h.allocated.memory ?? 0;
      t.capMem += h.capacity.memory;
    }
    return t;
  }, [hosts]);

  return (
    <div className="page page-list">
      <PageHeader
        title="Hosts"
        description={<span>{hosts.length} hosts · ready and draining: {formatCores(totals.cpus)} of {formatCores(totals.capCpus)} CPU, {formatBytes(totals.mem)} of {formatBytes(totals.capMem)} memory allocated</span>}
      />
      <div className="filters">
        <Select size="sm" prefix="Pool" value={pool} onChange={setPool} options={poolOptions} width={150} searchable={poolOptions.length > 8} />
        <Select size="sm" prefix="State" value={state} onChange={setState} options={[{ value: "", label: "Any state", text: "Any state" }, ...HOST_STATE_LIST.map((s) => ({ value: s, label: s, text: s }))]} width={150} />
        <label className="check">
          <input type="checkbox" checked={all} onChange={(e) => setAll(e.target.checked)} />
          Include terminated
        </label>
        {(pool || state || all) && (
          <Button size="sm" variant="ghost" onClick={() => setSearchParams({ pool: null, state: null, all: null })}>
            Clear
          </Button>
        )}
      </div>
      <Card flush>
        <ErrorStrip error={hosts.length > 0 ? q.error : null} />
        {q.error && hosts.length === 0 && !q.loading ? (
          <ErrorBlock error={q.error} onRetry={q.refetch} />
        ) : (
          <Table columns={cols} rows={hosts} rowKey={(h) => h.id} loading={q.loading} defaultSort={{ key: "name", dir: "asc" }} onRowClick={(h) => go(hostPath(h.id))} empty="No hosts match these filters." />
        )}
      </Card>
    </div>
  );
}

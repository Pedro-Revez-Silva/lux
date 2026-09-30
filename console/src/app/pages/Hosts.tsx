import { useMemo } from "react";
import { Badge, Button, Card, DurationCell, formatBytes, formatCores, HOST_STATE_LIST, hostDisplayState, PageHeader, Pagination, RelativeTime, SegmentedControl, Select, Table, Tooltip, useNow, type Column } from "@lux/design-system";
import { api, type Host } from "../../api/index.ts";
import { usePaged } from "../paged.ts";
import { go, Link, setSearchParams, useSearchParams } from "../router.tsx";
import { useScope, useScopedQuery } from "../scope.tsx";
import { DASH, ErrorBlock, ErrorStrip, HostLink, hostPath, hostRunsPath, IdLink, StateCell, UsageBar } from "./common.tsx";

/**
 * A host's live Run count. The cap (lux-runner --max-runs) is rarely what
 * limits a host, so it shows only once the count nears it. On a platform
 * host a tenant's count is its own share only while the cap counts every
 * tenant's Runs, so no near-cap warning is drawn from it.
 */
function LiveRuns({ host: h, wholeHost }: { host: Host; wholeHost: boolean }) {
  const cap = h.capacity.runs;
  const near = wholeHost && cap > 0 && h.liveRuns >= 0.75 * cap;
  const tip = wholeHost ? `${h.liveRuns} live · max ${cap} runs on this host` : `${h.liveRuns} of yours live · the host runs at most ${cap}, across tenants`;
  return (
    <Tooltip content={tip}>
      <Link to={hostRunsPath(h.id)} className="live-runs">{near ? <Badge tone="warn" mono>{`${h.liveRuns} / ${cap}`}</Badge> : h.liveRuns}</Link>
    </Tooltip>
  );
}

const up = (h: Host) => h.state === "ready" || h.state === "draining";
const launchFailed = (h: Host) => hostDisplayState(h) === "launch_failed";

/**
 * Uptime: created to terminated, or to now while the host is up; none for a
 * launch that failed (no instance ever ran).
 */
export function HostUptime({ host: h }: { host: Host }) {
  const now = useNow();
  if (launchFailed(h)) return <DurationCell seconds={null} missing="Never launched: no instance ran" />;
  const created = Date.parse(h.times.created ?? "");
  if (!Number.isFinite(created)) return DASH;
  const ended = h.times.terminated ? Date.parse(h.times.terminated) : null;
  const secs = ((ended ?? now) - created) / 1000;
  return <DurationCell seconds={Math.max(0, secs)} live={ended == null} tip={ended == null ? "Created → now (still up)" : "Created → terminated"} />;
}

const STATES = [...HOST_STATE_LIST];
const LIFECYCLE = [
  { value: "live", label: "Live", title: "Hosts not terminated" },
  { value: "all", label: "All" },
  { value: "ended", label: "Ended", title: "Terminated hosts, launch failures included" },
] as const;
type Lifecycle = (typeof LIFECYCLE)[number]["value"];

export function Hosts() {
  const { showTenant, tenant, apiTenant } = useScope();
  // Filters live in the URL, so a filtered list is a link.
  const params = useSearchParams();
  const pool = params.get("pool") ?? "";
  const state = params.get("state") ?? "";
  const lifecycle: Lifecycle = params.get("lifecycle") === "all" || params.get("all") === "true" ? "all" : params.get("lifecycle") === "ended" ? "ended" : "live";
  const setPool = (v: string) => setSearchParams({ pool: v || null });
  const setState = (v: string) => setSearchParams({ state: v || null });
  const setLifecycle = (v: Lifecycle) => setSearchParams({ lifecycle: v === "live" ? null : v, all: null });
  const view = `${tenant}|${pool}|${state}|${lifecycle}`;
  const q = usePaged<Host>(
    view,
    (req, s) =>
      api.hostsPage(apiTenant, { ...req, pool: pool || undefined, state: state || undefined, all: lifecycle === "all" || undefined, lifecycle: lifecycle === "ended" ? "ended" : lifecycle === "live" && !state ? "live" : undefined }, s),
    { defaultSort: { key: "created", dir: "desc" }, defaultSize: 25, interval: 5000 },
  );
  const pools = useScopedQuery("pools", api.pools, { interval: 60_000 });
  // The allocation summary is over every live host, not the page.
  const live = useScopedQuery("hosts-live", (t, s) => api.hosts(t, {}, s), { interval: 15_000 });

  const poolOptions = useMemo(() => {
    const names = new Set<string>((pools.data ?? []).map((p) => p.name));
    return [{ value: "", label: "Any pool", text: "Any pool" }, ...[...names].sort().map((n) => ({ value: n, label: n, text: n }))];
  }, [pools.data]);

  const cols = useMemo<Column<Host>[]>(() => {
    const c: Column<Host>[] = [{ key: "name", header: "Host", cell: (h) => <HostLink id={h.id} name={h.name} />, sortable: true, lead: true, width: 180 }];
    if (showTenant) c.push({ key: "tenant", header: "Tenant", cell: (h) => (h.platform ? <span className="muted">platform</span> : h.tenant || DASH), sortable: true, width: 120, optional: true });
    c.push(
      { key: "pool", header: "Pool", cell: (h) => h.pool, sortable: true, width: 110 },
      {
        key: "state",
        header: "State",
        cell: (h) => (
          <StateCell kind="host" state={hostDisplayState(h)} reason={launchFailed(h) ? h.launch?.error : h.stateReason}>
            {h.stateReason?.startsWith("evicting") ? <Badge tone="danger">evicting</Badge> : h.draining && h.state !== "draining" && <Badge tone="warn">draining</Badge>}
          </StateCell>
        ),
        sortable: true,
      },
      { key: "runs", header: "Live runs", cell: (h) => (up(h) ? <LiveRuns host={h} wholeHost={!h.platform || showTenant} /> : DASH), sortable: true, align: "right", mono: true, width: 100 },
      { key: "cpu", header: "CPU", cell: (h) => (up(h) ? <UsageBar used={h.allocated.cpus ?? 0} total={h.capacity.cpus} unit="cores" /> : DASH), sortable: true, sortFirst: "desc", width: 150, optional: true },
      { key: "memory", header: "Memory", cell: (h) => (up(h) ? <UsageBar used={h.allocated.memory ?? 0} total={h.capacity.memory} unit="bytes" /> : DASH), sortable: true, sortFirst: "desc", width: 160, optional: true },
      { key: "id", header: "Id", cell: (h) => <IdLink value={h.id} to={hostPath(h.id)} />, sortable: true, mono: true, width: 210, optional: true },
      { key: "created", header: "Created", cell: (h) => <RelativeTime at={h.times.created} label="Created" />, sortable: true, sortFirst: "desc", width: 104 },
      { key: "terminated", header: "Terminated", cell: (h) => <RelativeTime at={h.times.terminated} label="Terminated" />, sortable: true, sortFirst: "desc", width: 116 },
      { key: "uptime", header: "Uptime", cell: (h) => <HostUptime host={h} />, sortable: true, align: "right", mono: true, width: 96 },
      { key: "heartbeat", header: "Heartbeat", cell: (h) => <RelativeTime at={h.lastHeartbeat} label="Last heartbeat" />, sortable: true, sortFirst: "desc", align: "right", width: 104 },
    );
    return c;
  }, [showTenant]);

  const totals = useMemo(() => {
    const t = { n: 0, cpus: 0, capCpus: 0, mem: 0, capMem: 0 };
    for (const h of live.data ?? []) {
      t.n++;
      if (!up(h)) continue;
      t.cpus += h.allocated.cpus ?? 0;
      t.capCpus += h.capacity.cpus;
      t.mem += h.allocated.memory ?? 0;
      t.capMem += h.capacity.memory;
    }
    return t;
  }, [live.data]);

  const filtered = pool !== "" || state !== "" || lifecycle !== "live";
  return (
    <div className="page page-list">
      <PageHeader
        title="Hosts"
        description={
          <span>
            {q.total != null ? `${q.total.toLocaleString()} ${filtered ? "matching " : ""}hosts · ` : ""}
            {totals.n} live · ready and draining: {formatCores(totals.cpus)} of {formatCores(totals.capCpus)} CPU, {formatBytes(totals.mem)} of {formatBytes(totals.capMem)} memory allocated
          </span>
        }
      />
      <div className="filters">
        <Select size="sm" prefix="Pool" value={pool} onChange={setPool} options={poolOptions} width={150} searchable={poolOptions.length > 8} />
        <Select size="sm" prefix="State" value={state} onChange={setState} options={[{ value: "", label: "Any state", text: "Any state" }, ...STATES.map((s) => ({ value: s, label: s === "launch_failed" ? "launch failed" : s, text: s }))]} width={170} />
        <SegmentedControl label="Lifecycle" value={lifecycle} onChange={setLifecycle} options={LIFECYCLE} />
        {filtered && (
          <Button size="sm" variant="ghost" onClick={() => setSearchParams({ pool: null, state: null, all: null, lifecycle: null })}>
            Clear
          </Button>
        )}
      </div>
      <Card flush>
        <ErrorStrip error={q.rows.length > 0 ? q.error : null} />
        {q.error && q.rows.length === 0 && !q.loading ? (
          <ErrorBlock error={q.error} onRetry={q.refetch} />
        ) : (
          <Table
            columns={cols}
            rows={q.rows}
            rowKey={(h) => h.id}
            loading={q.loading}
            sortMode="server"
            sort={q.sort}
            onSortChange={q.setSort}
            onRowClick={(h) => go(hostPath(h.id))}
            empty="No hosts match these filters."
            footer={q.total != null && q.total > 0 ? <Pagination mode="count" page={q.page} pageSize={q.size} total={q.total} noun="hosts" onPage={q.goto} onPageSize={q.setSize} /> : undefined}
          />
        )}
      </Card>
    </div>
  );
}

import { useMemo, useState } from "react";
import { Card, PageHeader, RelativeTime, SegmentedControl, ServedStateMark, StatTile, Table, useNow, type Column } from "@lux/design-system";
import { api, type TenantServer, type TenantServerState } from "../../api/index.ts";
import { go, Link, useSearchParams } from "../router.tsx";
import { useScope, useScopedQuery } from "../scope.tsx";
import { DASH, ErrorBlock, ErrorStrip, RunLink } from "./common.tsx";
import { idleText, serverPath } from "./serverText.ts";

type Filter = "all" | "up" | "waking" | "asleep";

const FILTERS: { value: Filter; label: string; states?: TenantServerState[] }[] = [
  { value: "all", label: "All" },
  { value: "up", label: "Up", states: ["ready", "unreachable"] },
  { value: "waking", label: "Waking", states: ["waking", "no answer"] },
  { value: "asleep", label: "Asleep", states: ["asleep", "stopped", "exited"] },
];

/**
 * /servers: the tenant's servers, named URLs that reach a port in a Run.
 * Counts by state on top, a state filter, one row per server.
 */
export function Servers() {
  const { showTenant } = useScope();
  const params = useSearchParams();
  const [filter, setFilter] = useState<Filter>((params.get("state") as Filter) || "all");
  const now = useNow(1000);
  const q = useScopedQuery("servers", (tenant, signal) => api.servers(tenant, {}, signal), { interval: 5000, live: 15_000 });
  const all = q.data?.servers ?? [];
  const counts = q.data?.counts ?? {};
  const states = FILTERS.find((f) => f.value === filter)?.states;
  const rows = useMemo(() => (states ? all.filter((s) => states.includes(s.state)) : all), [all, states]);
  const n = (...st: TenantServerState[]) => st.reduce((a, s) => a + (counts[s] ?? 0), 0);
  const longestWake = all.filter((s) => s.state === "waking" && s.wakeRequestedAt).reduce((m, s) => Math.max(m, now - Date.parse(s.wakeRequestedAt!)), 0);

  const cols = useMemo<Column<TenantServer>[]>(
    () => [
      {
        key: "name",
        header: "Server",
        lead: true,
        cell: (s) => (
          <span className="stack stack-tight">
            <Link to={serverPath(s.id)} className="mono">
              {s.name}
            </Link>
            <span className="muted mono">{s.hostname ?? s.id}</span>
          </span>
        ),
        sortValue: (s) => s.name,
        width: 260,
      },
      { key: "state", header: "State", cell: (s) => <ServedStateMark state={s.state} exitCode={s.exitCode} />, sortValue: (s) => s.state, width: 130 },
      { key: "wake", header: "Wake", cell: (s) => (s.wake === "request" ? "on request" : <span className="muted">with its run</span>), sortValue: (s) => s.wake, width: 110 },
      { key: "run", header: "Run", cell: (s) => (s.runId ? <RunLink id={s.runId} /> : DASH), sortValue: (s) => s.runId ?? "", width: 190 },
      { key: "owner", header: "Owner", cell: (s) => <span className="mono">{s.owner}</span>, sortValue: (s) => s.owner, width: 150, optional: true },
      { key: "last", header: "Last request", cell: (s) => (s.lastRequestAt ? <RelativeTime at={s.lastRequestAt} /> : DASH), sortValue: (s) => (s.lastRequestAt ? Date.parse(s.lastRequestAt) : null), sortKind: "time", width: 130 },
      { key: "idle", header: "Idle", cell: (s) => idleText(s, now) ?? DASH, sortValue: (s) => (s.idleAt ? Date.parse(s.idleAt) : null), width: 100, mono: true },
      { key: "wakes", header: "Wakes", cell: (s) => s.wakes, sortValue: (s) => s.wakes, align: "right", mono: true, width: 80, optional: true },
      { key: "created", header: "Created", cell: (s) => <RelativeTime at={s.createdAt} />, sortValue: (s) => Date.parse(s.createdAt), sortKind: "time", width: 120, optional: true },
    ],
    [now],
  );

  return (
    <div className="page page-list">
      <PageHeader
        title="Servers"
        description="Named URLs that reach a port in a Run. A server outlives the Runs that serve it; its owner is told when someone wants it and when it goes idle."
      />
      <div className="grid grid-stats">
        <StatTile label="Up" value={n("ready", "unreachable")} unit="ready on a running run" onClick={() => setFilter("up")} />
        <StatTile label="Waking" value={n("waking", "no answer")} unit={longestWake > 0 ? `longest ${Math.round(longestWake / 1000)}s` : undefined} tone={n("no answer") > 0 ? "warn" : "default"} onClick={() => setFilter("waking")} />
        <StatTile label="Asleep" value={n("asleep")} unit="wake on request" onClick={() => setFilter("asleep")} />
        <StatTile label="Stopped" value={n("stopped", "exited")} />
      </div>
      <Card
        flush
        actions={
          <SegmentedControl
            label="State"
            value={filter}
            onChange={setFilter}
            options={FILTERS.map((f) => ({ value: f.value, label: `${f.label} ${f.states ? n(...f.states) : all.length}` }))}
          />
        }
      >
        <ErrorStrip error={all.length > 0 ? q.error : null} />
        {q.error && all.length === 0 && !q.loading ? (
          <ErrorBlock error={q.error} onRetry={q.refetch} />
        ) : (
          <Table
            columns={cols}
            rows={rows}
            rowKey={(s) => s.id}
            onRowClick={(s) => go(serverPath(s.id))}
            loading={q.loading}
            defaultSort={{ key: "created", dir: "desc" }}
            empty={showTenant ? "No servers in any tenant." : "No servers. Create one with lux server create, or add one to a run."}
          />
        )}
      </Card>
    </div>
  );
}

import { useMemo, useState } from "react";
import { Badge, Button, Card, Code, ConfirmDialog, formatBytes, formatCores, formatDuration, formatRelative, formatTimestamp, formatTimestampZone, hostDisplayState, IdChip, KeyValue, PageHeader, RelativeTime, StatePill, Table, TimeSeriesChart, Timeline, useToast, type Column, type TimelineStage } from "@lux/design-system";
import { api, errorText, useNow, useQuery, type Host, type HostPlacement, type HostTimeKey, type Run } from "../../api/index.ts";
import { go, Link } from "../router.tsx";
import { useScope, useScopedQuery } from "../scope.tsx";
import { DASH, ErrorBlock, ErrorStrip, hostRunsPath, labelsText, PageSkeleton, poolPath, RunLink, RunNameLink, runColumns, runPath, useSeries } from "./common.tsx";
import { HostCost } from "./HostCost.tsx";
import { InfraEvents } from "./InfraEvents.tsx";
import { ProcessCards, runnerProcesses } from "./ProcessCards.tsx";

export function HostPage({ id }: { id: string }) {
  const scope = useScope();
  const now = useNow();
  const toast = useToast();
  const host = useQuery(`host:${id}`, (s) => api.host(id, s), { interval: 5000 });
  const history = useQuery(`host-history:${id}:${scope.range}`, (s) => api.hostHistory(id, scope.range, s), { interval: 30_000 });
  // By host id: tenant-independent, and an operator sees every tenant's runs there.
  const recent = useScopedQuery(`host-runs:${id}`, (t, s) => api.runs(t, { host: id, limit: 50 }, s), { interval: 15_000, live: 60_000 });
  const [drainOpen, setDrainOpen] = useState(false);
  const [draining, setDraining] = useState(false);

  const h = host.data;
  const samples = history.data?.samples;
  const cap = h?.capacity;
  const cpu = useSeries(samples, [(s) => s.cpuCores, cap?.cpus, (s) => s.allocCpus]);
  const mem = useSeries(samples, [(s) => s.memoryBytes, cap?.memory, (s) => s.allocMemory]);
  const disk = useSeries(samples, [(s) => s.diskBytes, cap?.disk]);
  const placements = useSeries(samples, [(s) => s.placements, cap?.runs]);
  const runners = useMemo(() => runnerProcesses(samples), [samples]);

  const drain = async (forceEvict: boolean) => {
    setDraining(true);
    try {
      await api.drainHost(id, forceEvict);
      let description: string | undefined = "New placements are refused; live runs finish where they are";
      if (forceEvict) description = h?.liveRuns ? `${h.liveRuns} live runs will be moved` : undefined;
      toast({ title: `Draining ${h?.name ?? id}`, description, tone: "warn" });
      setDrainOpen(false);
      await host.refetch();
    } catch (e) {
      toast({ title: "Drain failed", description: errorText(e), tone: "danger" });
    } finally {
      setDraining(false);
    }
  };

  if (host.error && !h) {
    return (
      <div className="page">
        <ErrorBlock error={host.error} onRetry={host.refetch} />
      </div>
    );
  }

  if (!h) return <PageSkeleton />;
  if (hostDisplayState(h) === "launch_failed") return <LaunchFailedHost host={h} />;

  // A draining host with a live run can still be force-evicted; one with
  // none has nothing left for a drain action to do (a plain drain only
  // cordons, which it already is, and a force evict would stop 0 runs).
  const canAct = h.state !== "terminated" && (!h.draining || h.liveRuns > 0);
  const forceOnly = h.draining;
  const oneRun = h.liveRuns === 1;
  const liveRunsText = `${h.liveRuns} live run${oneRun ? "" : "s"}`;
  let dialogDescription = "No new placements will be assigned. It has no live runs. A provisioned host is terminated once empty.";
  if (forceOnly) dialogDescription = `Its ${liveRunsText} will be stopped and resumed elsewhere.`;
  else if (h.liveRuns)
    dialogDescription = `No new placements will be assigned. Its ${liveRunsText} ${oneRun ? "finishes where it is" : "finish where they are"}, unless forced. A provisioned host is terminated once empty.`;
  return (
    <div className="page">
      <PageHeader
        title={h.name}
        badges={
          <>
            <StatePill kind="host" state={h.state} />
            {h.draining && h.state !== "draining" && <Badge tone="warn">draining</Badge>}
            {h.platform && <Badge outline>platform</Badge>}
          </>
        }
        description={
          <>
            <IdChip value={h.id} />
            {h.tenant && <span>tenant {h.tenant}</span>}
            <span>
              pool <Link to={poolPath(h.pool, { platform: h.platform, tenant: scope.showTenant ? h.tenant : undefined })}>{h.pool}</Link>
            </span>
            <Link to={hostRunsPath(h.id)}>
              {h.liveRuns} live run{h.liveRuns === 1 ? "" : "s"} · all runs on this host
            </Link>
          </>
        }
        note={h.stateReason}
        actions={
          <Button variant="danger" disabled={!canAct} onClick={() => setDrainOpen(true)}>
            {forceOnly ? "Force evict" : "Drain"}
          </Button>
        }
      />
      <ErrorStrip error={host.error} />

      <div className="grid grid-2">
        <Card title="Details">
          <KeyValue
            columns={2}
            items={[
              { key: "Provider id", value: h.providerId ? <IdChip value={h.providerId} /> : DASH },
              { key: "Instance type", value: h.instanceType ? <span className="mono">{h.instanceType}</span> : DASH },
              { key: "Heartbeat", value: h.lastHeartbeat ? `${formatRelative(h.lastHeartbeat, now)} (${formatTimestamp(h.lastHeartbeat)})` : DASH },
              { key: "Capacity", value: `${formatCores(h.capacity.cpus)} · ${formatBytes(h.capacity.memory)} · ${formatBytes(h.capacity.disk)} disk · ${h.capacity.runs} runs` },
              { key: "Allocated", value: `${formatCores(h.allocated.cpus ?? 0)} · ${formatBytes(h.allocated.memory ?? 0)} · ${formatBytes(h.allocated.disk ?? 0)} disk · ${h.liveRuns} live` },
              { key: "Labels", value: Object.keys(h.labels).length ? <span className="mono">{labelsText(h.labels)}</span> : DASH },
              { key: "Versions", value: Object.keys(h.versions).length ? <span className="mono">{labelsText(h.versions)}</span> : DASH },
            ]}
          />
        </Card>
        <Card title="Lifecycle">
          <Timeline stages={hostStages(h)} now={now} />
        </Card>
      </div>

      <Card flush title="Live placements" subtitle={`${h.placements?.length ?? 0} on this host`}>
        <PlacementsTable placements={h.placements ?? []} loading={host.loading} tenant={scope.operator} />
      </Card>

      <ErrorStrip error={history.error} />
      <div className="grid grid-charts">
        <Card title="CPU" subtitle="used cores vs capacity, and allocated">
          <TimeSeriesChart x={cpu.x} ys={cpu.ys} series={[{ label: "Used", color: 1, area: true }, { label: "Capacity", color: "var(--fg-faint)", dashed: true }, { label: "Allocated", color: 2 }]} unit="cores" />
        </Card>
        <Card title="Memory" subtitle="used vs capacity, and allocated">
          <TimeSeriesChart x={mem.x} ys={mem.ys} series={[{ label: "Used", color: 7, area: true }, { label: "Capacity", color: "var(--fg-faint)", dashed: true }, { label: "Allocated", color: 2 }]} unit="bytes" />
        </Card>
        <Card title="Disk" subtitle="used vs capacity">
          <TimeSeriesChart x={disk.x} ys={disk.ys} series={[{ label: "Used", color: 4, area: true }, { label: "Capacity", color: "var(--fg-faint)", dashed: true }]} unit="bytes" />
        </Card>
        <Card title="Placements" subtitle="live placements vs run capacity">
          <TimeSeriesChart x={placements.x} ys={placements.ys} series={[{ label: "Placements", color: 3, step: true, area: true }, { label: "Capacity", color: "var(--fg-faint)", dashed: true }]} unit="count" />
        </Card>
        <ProcessCards title="Runner" what="the lux-runner process, not podman or its containers" processes={runners} />
      </div>

      {/* The host history rule: operators, and a tenant for its own host (a tenant sees no other non-platform host). */}
      {(scope.operator || !h.platform) && <HostCost id={h.id} range={scope.range} operator={scope.operator} />}

      {/* A platform host's events are the operators', not narrowed to a tenant (they name other tenants' Runs). */}
      {(!h.platform || (scope.operator && !scope.apiTenant)) && (
        <InfraEvents queryKey={`host-events:${id}@${scope.tenant}`} page={(q, s) => api.hostEvents(id, scope.apiTenant, q, s)} interval={5000} subtitle="registration, placements, drains, termination" />
      )}

      <Card flush title="Recent runs on this host" subtitle="any epoch, newest first, up to 50" actions={<Link to={hostRunsPath(id)}>All runs on this host</Link>}>
        <ErrorStrip error={recent.error} />
        <RecentRuns runs={recent.data ?? []} loading={recent.loading} tenant={scope.showTenant} />
      </Card>

      <ConfirmDialog
        open={drainOpen}
        title={forceOnly ? `Force evict ${h.name}?` : `Drain ${h.name}?`}
        description={dialogDescription}
        confirmLabel={forceOnly ? "Force evict" : "Drain host"}
        tone="danger"
        confirmText={h.name}
        checkbox={{
          label: "Force evict running Runs",
          help: "Stops its live runs now: they are snapshotted and resumed elsewhere, instead of finishing on this host.",
          checked: forceOnly,
          locked: forceOnly,
        }}
        loading={draining}
        onConfirm={(_input, checked) => void drain(!!checked)}
        onCancel={() => setDrainOpen(false)}
      />
    </div>
  );
}

/**
 * A host whose launch the provider refused: no instance ran, so there is
 * no terminate time, no usage and no host cost; what there is, is the
 * launch and its error.
 */
function LaunchFailedHost({ host: h }: { host: Host }) {
  const scope = useScope();
  const l = h.launch!;
  const took = l.requestedAt && l.finishedAt ? (Date.parse(l.finishedAt) - Date.parse(l.requestedAt)) / 1000 : null;
  return (
    <div className="page">
      <PageHeader
        title={h.name}
        badges={
          <>
            <StatePill kind="host" state="launch_failed" />
            {h.platform && <Badge outline>platform</Badge>}
          </>
        }
        description={
          <>
            <IdChip value={h.id} />
            {h.tenant && <span>tenant {h.tenant}</span>}
            <span>
              pool <Link to={poolPath(h.pool, { platform: h.platform, tenant: scope.showTenant ? h.tenant : undefined })}>{h.pool}</Link>
            </span>
            <span>created {formatTimestamp(h.times.created)}</span>
            <span>no instance was started, so there is no terminate time and no host cost</span>
          </>
        }
      />
      <Card title="Launch" subtitle="its recorded outcome, not parsed from the state reason">
        <div className="stack">
          <KeyValue
            columns={3}
            items={[
              { key: "Outcome", value: "Launch failed" },
              { key: "Requested", value: l.requestedAt ? <span title={formatTimestampZone(l.requestedAt)}>{formatTimestamp(l.requestedAt)}</span> : DASH },
              { key: "Failed", value: l.finishedAt ? <span title={formatTimestampZone(l.finishedAt)}>{formatTimestamp(l.finishedAt)}{took != null ? ` (${formatDuration(took)})` : ""}</span> : DASH },
              { key: "Provider instance", value: <span className="muted">– none</span> },
            ]}
          />
          {l.error && (
            <div className="field">
              <span className="field-label">Provider error</span>
              <Code>{l.error}</Code>
            </div>
          )}
        </div>
      </Card>
      <Card title="Timeline">
        <Timeline
          stages={[
            { key: "requested", label: "Launch requested", start: l.requestedAt ? Date.parse(l.requestedAt) : null, end: l.finishedAt ? Date.parse(l.finishedAt) : null, tone: "accent" },
            { key: "failed", label: "Launch failed", note: "host row closed, its one-use token revoked", start: l.finishedAt ? Date.parse(l.finishedAt) : null, point: true, tone: "red" },
          ]}
        />
      </Card>
      {(!h.platform || (scope.operator && !scope.apiTenant)) && (
        <InfraEvents queryKey={`host-events:${h.id}@${scope.tenant}`} page={(q, s) => api.hostEvents(h.id, scope.apiTenant, q, s)} interval={15_000} subtitle="what was recorded on the host" />
      )}
    </div>
  );
}

const HOST_TIMES: { key: HostTimeKey; label: string; tone: TimelineStage["tone"] }[] = [
  { key: "created", label: "Created", tone: "neutral" },
  { key: "provisionRequested", label: "Provision requested", tone: "neutral" },
  { key: "provisioned", label: "Provisioned", tone: "accent" },
  { key: "registered", label: "Registered", tone: "accent" },
  { key: "firstPlacement", label: "First placement", tone: "teal" },
  { key: "lastPlacementEnded", label: "Last placement ended", tone: "teal" },
  { key: "drainRequested", label: "Drain requested", tone: "amber" },
  { key: "terminateRequested", label: "Terminate requested", tone: "amber" },
  { key: "lost", label: "Lost", tone: "red" },
  { key: "terminated", label: "Terminated", tone: "neutral" },
];

/** Host times are instants; each stage runs from its stamp to the next one that happened. */
function hostStages(h: Host): TimelineStage[] {
  const stamped = HOST_TIMES.map((t) => ({ ...t, at: h.times[t.key] ? Date.parse(h.times[t.key]!) : null })).filter((t) => t.at != null && Number.isFinite(t.at));
  stamped.sort((a, b) => a.at! - b.at!);
  const ended = h.state === "terminated" || h.state === "lost";
  return stamped.map((t, i) => {
    const next = stamped[i + 1];
    return { key: t.key, label: t.label, start: t.at, end: next ? next.at : ended ? t.at! + 1000 : null, tone: t.tone };
  });
}

function PlacementsTable({ placements, loading, tenant }: { placements: HostPlacement[]; loading: boolean; tenant: boolean }) {
  const cols = useMemo<Column<HostPlacement>[]>(() => {
    const c: Column<HostPlacement>[] = [{ key: "name", header: "Run", cell: (p) => <RunNameLink id={p.runId} name={p.runName} />, sortValue: (p) => p.runName || p.runId, lead: true, width: "24%" }];
    c.push({ key: "run", header: "Id", cell: (p) => <RunLink id={p.runId} />, sortValue: (p) => p.runId, mono: true, width: 190, optional: true });
    if (tenant) c.push({ key: "tenant", header: "Tenant", cell: (p) => p.tenant, sortValue: (p) => p.tenant, width: 120 });
    c.push(
      { key: "epoch", header: "Epoch", cell: (p) => p.epoch, sortValue: (p) => p.epoch, align: "right", mono: true, width: 72 },
      { key: "state", header: "Placement", cell: (p) => <span className="secondary">{p.state}</span>, sortValue: (p) => p.state, width: 120 },
      { key: "res", header: "Resources", cell: (p) => `${formatCores(p.resources.cpus ?? 0)} · ${formatBytes(p.resources.memory ?? 0)}`, sortValue: (p) => p.resources.cpus, sortFirst: "desc", mono: true },
      { key: "since", header: "Since", cell: (p) => <RelativeTime at={p.since} />, sortValue: (p) => Date.parse(p.since), align: "right", width: 110 },
    );
    return c;
  }, [tenant]);
  return <Table columns={cols} rows={placements} rowKey={(p) => `${p.runId}:${p.epoch}`} loading={loading} onRowClick={(p) => go(runPath(p.runId))} empty="No live placements." dense />;
}

function RecentRuns({ runs, loading, tenant }: { runs: Run[]; loading: boolean; tenant: boolean }) {
  const cols = useMemo<Column<Run>[]>(() => runColumns({ tenant, host: false, adapter: false }), [tenant]);
  return <Table columns={cols} rows={runs} rowKey={(r) => r.id} loading={loading} onRowClick={(r) => go(runPath(r.id))} empty="No runs have been placed here." dense />;
}

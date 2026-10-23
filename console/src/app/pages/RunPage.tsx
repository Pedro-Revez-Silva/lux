import { Badge, FactRow, formatBytes, formatDuration, formatRelative, formatTimestamp, IdChip, ImageRef, KeyValue, LabelsFact, PageHeader, StatePill, Tabs } from "@lux/design-system";
import { isRunActive, useNow, type Run, type RunSubmitter } from "../../api/index.ts";
import { setSearchParams, useSearchParams } from "../router.tsx";
import { useScope } from "../scope.tsx";
import { DASH, ErrorBlock, ErrorStrip, HostLink, PageSkeleton, useRun } from "./common.tsx";
import { RunActions } from "./RunActions.tsx";
import { RunOutput } from "./RunOutput.tsx";
import { RunCost } from "./RunCost.tsx";
import { RunResources } from "./RunResources.tsx";
import { RunServers } from "./RunServers.tsx";
import { RunEvents, RunSnapshots, RunSpecView } from "./RunTabs.tsx";
import { RunTimeline } from "./RunTimeline.tsx";
import { RunSession, RunStdinSession } from "./session/RunSession.tsx";
import { hasSession } from "./session/view.ts";

const TABS = ["session", "output", "servers", "timeline", "resources", "events", "snapshots", "spec"] as const;
type Tab = (typeof TABS)[number];

export function RunPage({ id }: { id: string }) {
  const { operator } = useScope();
  const now = useNow(5000);
  // The tab lives in the URL (?tab=), so links can open one; the first tab has none.
  const params = useSearchParams();
  const t = params.get("tab");
  const q = useRun(id, { active: 3000, settled: 15_000, liveActive: 15_000, liveSettled: 60_000 });
  const run = q.data;
  // An agent adapter's Session tab is first (the conversation); a generic Run's comes after Output.
  const session = hasSession(run?.spec.workload.adapter);
  const first: Tab = session ? "session" : "output";
  const tab: Tab = TABS.includes(t as Tab) ? (t as Tab) : first;
  const setTab = (next: Tab) => setSearchParams({ tab: next === first ? null : next });

  if (q.error && !run) {
    return (
      <div className="page">
        <ErrorBlock error={q.error} onRetry={q.refetch} />
      </div>
    );
  }
  if (!run) return <PageSkeleton />;

  const onChanged = (r: Run) => {
    // Action responses carry no placements or usage; keep what we have and refetch.
    q.setData({ ...run, ...r, placements: run.placements, usage: run.usage, resume: undefined });
    q.setActive();
    void q.refetch();
  };
  const live = isRunActive(run.state);

  return (
    <div className="page">
      <PageHeader
        title={run.name || <span className="mono">{run.id}</span>}
        badges={
          <>
            <StatePill kind="run" state={run.state} activity={run.activity} />
            <Badge outline>{run.spec.workload.adapter}</Badge>
            <Badge outline>epoch {run.epoch}</Badge>
            {run.spec.placement?.preemptible === false && (
              <Badge outline title="placement.preemptible false: lux never takes this Run away on its own initiative">
                non-preemptible
              </Badge>
            )}
          </>
        }
        description={
          <>
            {run.name && <IdChip value={run.id} />}
            {operator && <span>tenant {run.tenant}</span>}
            {run.hostId ? (
              <span>
                on <HostLink id={run.hostId} name={run.host} />
              </span>
            ) : null}
            {run.exitCode != null && !run.stateReason?.includes(`exit code ${run.exitCode}`) && <span>exit {run.exitCode}</span>}
          </>
        }
        note={run.stateReason}
        actions={<RunActions run={run} operator={operator} onChanged={onChanged} />}
      />
      <ErrorStrip error={q.error} />

      <KeyValue
        columns={3}
        items={[
          { key: "Created", value: `${formatTimestamp(run.createdAt)} · ${formatRelative(run.createdAt, now)}` },
          { key: "First started", value: run.firstStartedAt ? `${formatTimestamp(run.firstStartedAt)} · queued ${formatDuration(run.usage?.queueSeconds)}` : DASH },
          { key: "Finished", value: run.finishedAt ? `${formatTimestamp(run.finishedAt)} · ${formatRelative(run.finishedAt, now)}` : DASH },
          { key: "Resources", value: `${run.spec.resources.cpus ?? "–"} cpus · ${formatBytes(run.spec.resources.memory)} · ${formatBytes(run.spec.resources.disk)} disk` },
          { key: "Usage", value: run.usage ? `CPU ${formatDuration(run.usage.cpuSeconds)} · peak mem ${formatBytes(run.usage.peakMemoryBytes)} · ${run.usage.placements} placement${run.usage.placements === 1 ? "" : "s"}` : DASH },
          { key: "Submitted by", value: <SubmittedBy by={run.submittedBy} /> },
          { key: "Session", value: run.sessionId ? <IdChip value={run.sessionId} truncate={24} /> : DASH },
          { key: "Snapshot", value: run.snapshotId ? <IdChip value={run.snapshotId} truncate={24} /> : DASH },
        ]}
      >
        <FactRow label="Image">
          {run.spec.image.ref ? <ImageRef value={run.spec.image.ref} /> : <span className="mono">{run.spec.image.build ? `built from ${run.spec.image.build.containerfile}` : DASH}</span>}
        </FactRow>
        <LabelsFact labels={run.labels} first={["app"]} />
      </KeyValue>

      <Tabs<Tab>
        value={tab}
        onChange={setTab}
        items={[
          ...(session ? [{ key: "session" as const, label: "Session" }] : []),
          { key: "output", label: "Output" },
          ...(session ? [] : [{ key: "session" as const, label: "Session" }]),
          { key: "servers", label: "Servers", count: run.servers?.length || undefined },
          { key: "timeline", label: "Timeline", count: run.placements?.length },
          { key: "resources", label: "Resources & cost" },
          { key: "events", label: "Events" },
          { key: "snapshots", label: "Snapshots & artifacts" },
          { key: "spec", label: "Spec" },
        ]}
      />
      {tab === "session" && (session ? <RunSession run={run} /> : <RunStdinSession run={run} />)}
      {tab === "output" && <RunOutput run={run} />}
      {tab === "servers" && <RunServers run={run} refetch={q.refetch} fetching={q.fetching} error={q.error} />}
      {tab === "timeline" && <RunTimeline run={run} now={now} />}
      {tab === "resources" && (
        <div className="stack">
          <RunCost run={run} />
          <RunResources run={run} />
        </div>
      )}
      {tab === "events" && <RunEvents run={run} live={live} />}
      {tab === "snapshots" && <RunSnapshots run={run} live={live} />}
      {tab === "spec" && <RunSpecView run={run} />}
    </div>
  );
}

/** Who submitted the Run: the key by name (mono; "revoked" after one that is), a person by email, or an operator's key a tenant may not name. */
function SubmittedBy({ by }: { by: RunSubmitter | undefined }) {
  if (!by) return <span className="muted">not recorded (before key tracking)</span>;
  if (by.email) return <span>{by.email}</span>;
  return (
    <span className="row">
      {by.keyName ? <span className="mono">{by.keyName}</span> : <span>Operator key</span>}
      {by.revoked && <Badge tone="neutral">revoked</Badge>}
    </span>
  );
}

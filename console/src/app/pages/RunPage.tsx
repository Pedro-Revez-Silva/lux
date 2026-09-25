import { useState } from "react";
import { Badge, formatBytes, formatDuration, formatRelative, formatTimestamp, IdChip, KeyValue, PageHeader, StatePill, Tabs } from "@lux/design-system";
import { api, isRunActive, useNow, useQuery, type Run } from "../../api/index.ts";
import { useScope } from "../scope.tsx";
import { DASH, ErrorBlock, ErrorStrip, HostLink, labelsText, PageSkeleton } from "./common.tsx";
import { RunActions } from "./RunActions.tsx";
import { RunOutput } from "./RunOutput.tsx";
import { RunResources } from "./RunResources.tsx";
import { RunEvents, RunSnapshots, RunSpecView } from "./RunTabs.tsx";
import { RunTimeline } from "./RunTimeline.tsx";

type Tab = "output" | "timeline" | "resources" | "events" | "snapshots" | "spec";

export function RunPage({ id }: { id: string }) {
  const { operator } = useScope();
  const now = useNow(5000);
  const [tab, setTab] = useState<Tab>("output");
  const [active, setActive] = useState(true);
  const q = useQuery(
    `run:${id}`,
    async (s) => {
      const r = await api.run(id, s);
      setActive(isRunActive(r.state));
      return r;
    },
    { interval: active ? 3000 : 15_000, live: active ? 15_000 : 60_000 },
  );
  const run = q.data;

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
    setActive(true);
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
        className="run-facts"
        items={[
          { key: "Created", value: `${formatTimestamp(run.createdAt)} · ${formatRelative(run.createdAt, now)}` },
          { key: "First started", value: run.firstStartedAt ? `${formatTimestamp(run.firstStartedAt)} · queued ${formatDuration(run.usage?.queueSeconds)}` : DASH },
          { key: "Finished", value: run.finishedAt ? `${formatTimestamp(run.finishedAt)} · ${formatRelative(run.finishedAt, now)}` : DASH },
          { key: "Image", value: run.spec.image.ref ?? (run.spec.image.build ? `built from ${run.spec.image.build.containerfile}` : DASH), mono: true },
          { key: "Resources", value: `${run.spec.resources.cpus ?? "–"} cpus · ${formatBytes(run.spec.resources.memory)} · ${formatBytes(run.spec.resources.disk)} disk` },
          { key: "Usage", value: run.usage ? `CPU ${formatDuration(run.usage.cpuSeconds)} · peak mem ${formatBytes(run.usage.peakMemoryBytes)} · ${run.usage.placements} placement${run.usage.placements === 1 ? "" : "s"}` : DASH },
          { key: "Session", value: run.sessionId ? <IdChip value={run.sessionId} truncate={24} /> : DASH },
          { key: "Snapshot", value: run.snapshotId ? <IdChip value={run.snapshotId} truncate={24} /> : DASH },
          { key: "Labels", value: Object.keys(run.labels ?? {}).length ? <span className="mono">{labelsText(run.labels)}</span> : DASH },
        ]}
      />

      <Tabs<Tab>
        value={tab}
        onChange={setTab}
        items={[
          { key: "output", label: "Output" },
          { key: "timeline", label: "Timeline", count: run.placements?.length },
          { key: "resources", label: "Resources" },
          { key: "events", label: "Events" },
          { key: "snapshots", label: "Snapshots & artifacts" },
          { key: "spec", label: "Spec" },
        ]}
      />
      {tab === "output" && <RunOutput run={run} />}
      {tab === "timeline" && <RunTimeline run={run} now={now} />}
      {tab === "resources" && <RunResources run={run} />}
      {tab === "events" && <RunEvents run={run} live={live} />}
      {tab === "snapshots" && <RunSnapshots run={run} live={live} />}
      {tab === "spec" && <RunSpecView run={run} />}
    </div>
  );
}

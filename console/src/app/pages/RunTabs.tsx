import { useMemo, useRef, useState } from "react";
import { Badge, Button, Card, formatBytes, formatTimestamp, IconButton, IdChip, Table, useToast, type Column } from "@lux/design-system";
import { IconDownload, IconRefresh } from "@lux/design-system/icons";
import { api, errorText, useQuery, type Artifact, type Event, type Run, type Snapshot } from "../../api/index.ts";
import { DASH, ErrorBlock, ErrorStrip, JsonBlock } from "./common.tsx";
import { eventSummary } from "./events.ts";

/** The server's page size for /runs/{id}/events: a full page means there is more. */
const EVENTS_PAGE = 1000;

/**
 * All of a Run's events, paged with the `after` cursor. Each poll only asks
 * for what is new. Events and cursor advance together in a ref, so an
 * aborted poll loses nothing.
 */
function useRunEvents(id: string, live: boolean) {
  const acc = useRef<{ id: string; after: number; events: Event[] }>({ id, after: 0, events: [] });
  return useQuery(
    `run-events:${id}`,
    async (s) => {
      if (acc.current.id !== id) acc.current = { id, after: 0, events: [] };
      const a = acc.current;
      for (;;) {
        const page = await api.runEvents(id, a.after, s);
        if (page.length > 0) {
          a.events = [...a.events, ...page];
          a.after = page[page.length - 1]!.id;
        }
        if (page.length < EVENTS_PAGE) break;
      }
      return a.events;
    },
    { interval: live ? 5000 : 0, live: 60_000 },
  );
}

export function RunEvents({ run, live }: { run: Run; live: boolean }) {
  const q = useRunEvents(run.id, live);
  const [open, setOpen] = useState<number | null>(null);
  const cols = useMemo<Column<Event>[]>(
    () => [
      { key: "id", header: "#", cell: (e) => e.id, sortValue: (e) => e.id, align: "right", mono: true, width: 76 },
      { key: "time", header: "Time", cell: (e) => formatTimestamp(e.time), sortValue: (e) => Date.parse(e.time), mono: true, width: 180 },
      { key: "epoch", header: "Epoch", cell: (e) => (e.epoch ? e.epoch : DASH), sortValue: (e) => e.epoch ?? 0, align: "right", mono: true, width: 72, optional: true },
      { key: "type", header: "Type", cell: (e) => <span className="secondary">{e.type}</span>, sortValue: (e) => e.type, width: 150 },
      { key: "summary", header: "Details", cell: (e) => (open === e.id ? <JsonBlock value={e.data} /> : eventSummary(e)), wrap: true },
    ],
    [open],
  );
  if (q.error && !q.data) return <ErrorBlock error={q.error} onRetry={q.refetch} />;
  return (
    <Card flush title="Events" subtitle={`${q.data?.length ?? 0} events · click a row to expand its data`} actions={<IconButton size="sm" label="Refresh" onClick={() => void q.refetch()}><IconRefresh size={14} /></IconButton>}>
      <ErrorStrip error={q.error} />
      <Table columns={cols} rows={q.data ?? []} rowKey={(e) => String(e.id)} loading={q.loading} defaultSort={{ key: "id", dir: "desc" }} onRowClick={(e) => setOpen((o) => (o === e.id ? null : e.id))} selected={open != null ? String(open) : null} empty="No events." dense minWidth={640} />
    </Card>
  );
}

export function RunSnapshots({ run, live }: { run: Run; live: boolean }) {
  const snaps = useQuery(`run-snapshots:${run.id}`, (s) => api.snapshots(run.id, s), { interval: live ? 10_000 : 0, live: 60_000 });
  const arts = useQuery(`run-artifacts:${run.id}`, (s) => api.artifacts(run.id, s), { interval: live ? 10_000 : 0, live: 60_000 });
  const toast = useToast();
  const [downloading, setDownloading] = useState<string | null>(null);

  const dl = async (a: Artifact) => {
    setDownloading(a.id);
    try {
      await api.downloadArtifact(a);
    } catch (e) {
      toast({ title: "Download failed", description: errorText(e), tone: "danger" });
    } finally {
      setDownloading(null);
    }
  };

  const snapCols = useMemo<Column<Snapshot>[]>(
    () => [
      { key: "id", header: "Snapshot", cell: (s) => <IdChip value={s.id} truncate={18} />, mono: true, width: 210 },
      { key: "epoch", header: "Epoch", cell: (s) => s.epoch, align: "right", mono: true, width: 72 },
      { key: "current", header: "", cell: (s) => (run.snapshotId === s.id ? <Badge tone="accent">current</Badge> : null), width: 90 },
      { key: "volumes", header: "Volumes", cell: (s) => s.manifest.volumes.map((v) => `${v.name} (${formatBytes(v.size)})`).join(", ") || DASH },
      { key: "size", header: "Size", cell: (s) => formatBytes(s.manifest.volumes.reduce((n, v) => n + v.size, 0)), align: "right", mono: true, width: 96 },
      { key: "where", header: "Where", cell: (s) => (s.available ? <span className="row" style={{ gap: 4 }}>{s.uploaded && <Badge tone="success">uploaded</Badge>}{s.onHost && <Badge outline>on {s.onHost}</Badge>}{!s.uploaded && !s.onHost && <Badge tone="warn">nowhere</Badge>}</span> : <Badge tone="danger">deleted</Badge>), width: 220 },
      { key: "created", header: "Created", cell: (s) => formatTimestamp(s.createdAt), align: "right", mono: true, width: 180, optional: true },
    ],
    [run.snapshotId],
  );
  const artCols = useMemo<Column<Artifact>[]>(
    () => [
      { key: "path", header: "Path", cell: (a) => <span className="mono">{a.path}</span>, sortValue: (a) => a.path, lead: true },
      { key: "epoch", header: "Epoch", cell: (a) => a.epoch, sortValue: (a) => a.epoch, align: "right", mono: true, width: 72, optional: true },
      { key: "type", header: "Type", cell: (a) => <span className="muted">{a.contentType}</span>, width: 170, optional: true },
      { key: "size", header: "Size", cell: (a) => formatBytes(a.size), sortValue: (a) => a.size, align: "right", mono: true, width: 96 },
      { key: "sha", header: "SHA-256", cell: (a) => <IdChip value={a.sha256} truncate={12} />, mono: true, width: 150, optional: true },
      { key: "created", header: "Created", cell: (a) => formatTimestamp(a.createdAt), sortValue: (a) => Date.parse(a.createdAt), align: "right", mono: true, width: 180, optional: true },
      {
        key: "dl",
        header: "",
        cell: (a) =>
          a.available ? (
            <Button size="sm" icon={<IconDownload size={13} />} loading={downloading === a.id} onClick={() => void dl(a)}>
              Download
            </Button>
          ) : (
            <Badge tone="warn">not uploaded</Badge>
          ),
        align: "right",
        width: 130,
      },
    ],
    [downloading],
  );

  return (
    <div className="stack">
      <Card flush title="Snapshots" subtitle="state volumes captured at each exit">
        {snaps.error && !snaps.data ? <ErrorBlock compact error={snaps.error} onRetry={snaps.refetch} /> : <Table columns={snapCols} rows={snaps.data ?? []} rowKey={(s) => s.id} loading={snaps.loading} empty="No snapshots." dense />}
      </Card>
      <Card flush title="Artifacts" subtitle="files collected from the spec's artifact paths">
        {arts.error && !arts.data ? <ErrorBlock compact error={arts.error} onRetry={arts.refetch} /> : <Table columns={artCols} rows={arts.data ?? []} rowKey={(a) => a.id} loading={arts.loading} defaultSort={{ key: "path", dir: "asc" }} empty="No artifacts." dense />}
      </Card>
    </div>
  );
}

export function RunSpecView({ run }: { run: Run }) {
  return (
    <div className="stack">
      <Card title="Spec" flush>
        <JsonBlock value={run.spec} />
      </Card>
      {run.image && (
        <Card title="Image resolution" subtitle="how the built image was resolved">
          <JsonBlock value={run.image} />
        </Card>
      )}
      {run.secrets.length > 0 && (
        <Card title="Secrets" subtitle="names and fingerprints only; values are never stored">
          <ul className="plain-list mono">
            {run.secrets.map((s) => (
              <li key={s.name}>
                {s.name} <span className="muted">{s.fingerprint}</span>
              </li>
            ))}
          </ul>
        </Card>
      )}
    </div>
  );
}

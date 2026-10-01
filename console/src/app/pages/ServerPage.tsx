import { useRef, useState } from "react";
import { Button, Card, ConfirmDialog, EmptyState, EventTable, IdleCountdown, IconButton, isServerUp, KeyValue, LogView, PageHeader, ServedStateMark, Tabs, useCopy, useNow, useToast, type LogLine } from "@lux/design-system";
import { IconCheck, IconCopy, IconExternal, IconRefresh, IconStop, IconTrash } from "@lux/design-system/icons";
import { api, errorText, useQuery, type TenantServer } from "../../api/index.ts";
import { go, setSearchParams, useSearchParams } from "../router.tsx";
import { DASH, ErrorBlock, ErrorStrip, JsonBlock, labelsText, PageSkeleton, RunLink } from "./common.tsx";
import { eventSummary } from "./events.ts";
import { durationWords, lifetimeText, wakeText } from "./serverText.ts";

const TABS = ["overview", "events", "log"] as const;
type Tab = (typeof TABS)[number];

/**
 * /servers/:id: one server. Its URL, the Run that serves it, how long until
 * it goes idle, its configuration, its events and its log. Deferred: the
 * 7-day uptime chart, the cost tiles and the table of Runs that served it.
 */
export function ServerPage({ id }: { id: string }) {
  const now = useNow(1000);
  const toast = useToast();
  const params = useSearchParams();
  const t = params.get("tab");
  const tab: Tab = TABS.includes(t as Tab) ? (t as Tab) : "overview";
  const q = useQuery(`server:${id}`, (signal) => api.server(id, signal), { interval: 3000, live: 10_000, keep: true });
  const sv = q.data;
  const [busy, setBusy] = useState(false);
  const [deleting, setDeleting] = useState(false);
  const { copied, copy } = useCopy(sv?.url ?? "");

  if (q.error && !sv) {
    return (
      <div className="page">
        <ErrorBlock error={q.error} onRetry={q.refetch} />
      </div>
    );
  }
  if (!sv) return <PageSkeleton />;

  const act = async (label: string, fn: () => Promise<unknown>) => {
    setBusy(true);
    try {
      await fn();
      toast({ title: `${label} ${sv.name}`, tone: "success" });
      await q.refetch();
    } catch (e) {
      toast({ title: `${label} ${sv.name} failed`, description: errorText(e), tone: "danger", duration: 8000 });
    } finally {
      setBusy(false);
    }
  };
  const up = isServerUp(sv.process);

  return (
    <div className="page">
      <PageHeader
        title={<span className="mono">{sv.name}</span>}
        badges={
          <>
            <ServedStateMark state={sv.state} exitCode={sv.exitCode} />
            <span className="server-tag">{sv.wake === "request" ? "wakes on request" : "runs with its run"}</span>
          </>
        }
        description={
          <>
            {sv.url ? (
              <span className="server-url">
                <a href={sv.url} target="_blank" rel="noreferrer" className="mono">
                  {sv.url}
                </a>
                <IconButton size="sm" label={copied ? "Copied" : "Copy URL"} onClick={copy}>
                  {copied ? <IconCheck size={13} /> : <IconCopy size={13} />}
                </IconButton>
              </span>
            ) : (
              <span className="muted">no preview URL: previews are not configured on this luxd</span>
            )}
            <span className="mono">port {sv.port}</span>
            <span>owned by {sv.owner}</span>
            <span className="mono muted">{sv.id}</span>
          </>
        }
        actions={
          <>
            {sv.url && (
              <Button size="sm" variant="ghost" icon={<IconExternal size={13} />} onClick={() => window.open(sv.url ?? "", "_blank", "noopener")}>
                Open
              </Button>
            )}
            {sv.runId && (
              <Button size="sm" variant="ghost" icon={<IconRefresh size={13} />} loading={busy} onClick={() => void act("Restarted", () => api.serverAction(sv.id, "restart"))}>
                Restart
              </Button>
            )}
            {sv.runId && up && (
              <Button size="sm" variant="ghost" icon={<IconStop size={12} />} loading={busy} onClick={() => void act("Stopped", () => api.serverAction(sv.id, "stop"))}>
                Stop
              </Button>
            )}
            {sv.runId && sv.lifetime === "owner" && (
              <Button size="sm" variant="ghost" loading={busy} onClick={() => void act("Detached", () => api.detachServer(sv.id))}>
                Detach
              </Button>
            )}
            <Button size="sm" variant="danger" icon={<IconTrash size={13} />} onClick={() => setDeleting(true)}>
              Delete
            </Button>
          </>
        }
      />
      <ErrorStrip error={q.error} />
      <ServedBy sv={sv} now={now} />
      <Tabs<Tab>
        items={TABS.map((k) => ({ key: k, label: k[0]!.toUpperCase() + k.slice(1) }))}
        value={tab}
        onChange={(next) => setSearchParams({ tab: next === "overview" ? null : next })}
      />
      {tab === "overview" && <ServerConfig sv={sv} />}
      {tab === "events" && <ServerEvents id={sv.id} />}
      {tab === "log" && <ServerLog sv={sv} />}
      <ConfirmDialog
        open={deleting}
        title={`Delete ${sv.name}?`}
        description={`It is detached first (its command stops; its run is untouched), then ${sv.hostname ?? "its URL"} answers "This preview is gone".`}
        confirmLabel="Delete"
        tone="danger"
        loading={busy}
        onConfirm={() =>
          void act("Deleted", () => api.deleteServer(sv.id)).then(() => {
            setDeleting(false);
            go("/servers");
          })
        }
        onCancel={() => setDeleting(false)}
      />
    </div>
  );
}

/** The Run serving it and the idle countdown: the server's live status. */
function ServedBy({ sv, now }: { sv: TenantServer; now: number }) {
  return (
    <Card className="served-by">
      <div className="row" style={{ justifyContent: "space-between", alignItems: "center", gap: 24 }}>
        <div className="stack stack-tight">
          <span className="muted">Served by</span>
          {sv.runId ? (
            <span>
              {sv.runName ? <strong>{sv.runName} </strong> : null}
              <RunLink id={sv.runId} /> <span className="muted">{sv.runState}</span>
              {sv.epoch != null && <span className="muted"> · placement {sv.epoch}</span>}
            </span>
          ) : (
            <span className="muted">no run: {sv.wake === "request" ? "its owner is asked for one on the next request" : "attach it to a run to serve it"}</span>
          )}
          {sv.wakeRequestedAt && (
            <span className="muted">
              wake asked {Math.round((now - Date.parse(sv.wakeRequestedAt)) / 1000)}s ago{sv.state === "no answer" ? ", no answer" : ""}
            </span>
          )}
        </div>
        <IdleCountdown idleAt={sv.idleAt} idleAfter={durationWords(sv.idleAfter)} now={now} />
      </div>
    </Card>
  );
}

function ServerConfig({ sv }: { sv: TenantServer }) {
  return (
    <Card title="Configuration" subtitle="command, port, workdir, env and after sync apply at its next start">
      <KeyValue
        columns={2}
        items={[
          { key: "Port", value: sv.port, mono: true },
          { key: "Command", value: sv.command?.join(" ") ?? <span className="muted">none: only the port is exposed</span>, mono: true },
          { key: "Workdir", value: sv.workdir || DASH, mono: true },
          { key: "After sync", value: sv.afterSync?.join(" ") ?? DASH, mono: true },
          { key: "Wake", value: wakeText(sv) },
          { key: "Idle after", value: `${durationWords(sv.idleAfter)} without a request` },
          { key: "Wake timeout", value: `${durationWords(sv.wakeTimeout)}, then the page says its owner did not answer` },
          { key: "Lifetime", value: lifetimeText(sv) },
          { key: "Labels", value: labelsText(sv.labels) || DASH, mono: true },
          { key: "Desired", value: sv.desired === "down" ? "down: stopped by request, stays stopped on new placements" : "up" },
          { key: "Last request", value: sv.lastRequestAt ?? DASH, mono: true },
          { key: "Wakes", value: sv.wakes, mono: true },
        ]}
      />
    </Card>
  );
}

function ServerEvents({ id }: { id: string }) {
  const q = useQuery(`server-events:${id}`, (signal) => api.serverEvents(id, signal), { interval: 5000, live: 15_000 });
  if (q.error && !q.data) return <ErrorBlock error={q.error} onRetry={q.refetch} />;
  return (
    <Card flush title="Events" subtitle="also on GET /v1/events, by server">
      <ErrorStrip error={q.error} />
      <EventTable events={[...(q.data ?? [])].reverse()} summary={eventSummary} detail={(e) => <JsonBlock value={e.data} />} epoch loading={q.loading} empty="No events yet." />
    </Card>
  );
}

/** Its command's output, across placements of its Run. */
function ServerLog({ sv }: { sv: TenantServer }) {
  const last = useRef<LogLine[] | null>(null);
  const q = useQuery(
    `server-log:${sv.id}`,
    async (signal) => {
      const lines = (await api.tenantServerLog(sv.id, 500, signal)).map<LogLine>((l) => ({ ts: l.t, stream: l.stream, text: l.text }));
      const prev = last.current;
      const same = prev && prev.length === lines.length && (lines.length === 0 || prev.at(-1)!.text === lines.at(-1)!.text);
      return same ? prev : (last.current = lines);
    },
    { interval: 5000, keep: true },
  );
  if (!sv.runId) return <EmptyState compact title="No log" description="It is attached to no run." />;
  return (
    <Card flush title="Log" subtitle="stdout and stderr of its command, across placements">
      <ErrorStrip error={q.error} />
      <LogView lines={q.data ?? []} height={420} timestamps emptyText={q.loading ? "Loading…" : "No output yet."} />
    </Card>
  );
}

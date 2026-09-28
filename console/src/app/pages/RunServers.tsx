import { useEffect, useRef, useState } from "react";
import { Button, Card, ConfirmDialog, Dialog, EmptyState, ServerList, useToast, type LogLine, type ServerInfo, type ServerLogState } from "@lux/design-system";
import { IconPlus, IconRefresh } from "@lux/design-system/icons";
import { api, errorText, invalidate, isApiError, onLiveEvent, useNow, useQuery, type Run, type Server, type ServerInput } from "../../api/index.ts";
import { ErrorBlock, ErrorStrip } from "./common.tsx";

const LOG_TAIL = 200;
/** How often an open log refetches while its server is up. */
const LOG_REFRESH = 3000;

/** Servers tab: the Run's servers, live through the events stream, with an Add dialog and per-server logs. */
export function RunServers({ run }: { run: Run }) {
  const now = useNow(5000);
  const toast = useToast();
  const running = run.state === "running";
  const q = useQuery(`run-servers:${run.id}`, (s) => api.servers(run.id, s), { interval: running ? 10_000 : 30_000, live: 60_000, keep: true });
  const servers = q.data ?? run.servers ?? [];
  const [busy, setBusy] = useState<string[]>([]);
  const [adding, setAdding] = useState(false);
  const [removing, setRemoving] = useState<Server | null>(null);
  const [logs, setLogs] = useState<Record<string, ServerLogState>>({});
  const openLogs = useRef(new Set<string>());

  // server.* events refetch the list (live.ts) and the logs that are open.
  useEffect(
    () =>
      onLiveEvent((e) => {
        if (e.runId !== run.id || !e.type.startsWith("server.")) return;
        const name = typeof e.data.name === "string" ? e.data.name : null;
        if (name && openLogs.current.has(name)) void loadLog(name);
        if (e.type === "server.removed" && name) {
          openLogs.current.delete(name);
          setLogs((l) => {
            const { [name]: _gone, ...rest } = l;
            return rest;
          });
        }
      }),
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [run.id],
  );

  const loadLog = async (name: string) => {
    setLogs((l) => ({ ...l, [name]: { lines: l[name]?.lines ?? [], loading: !l[name], error: null } }));
    try {
      const lines = await api.serverLog(run.id, name, LOG_TAIL);
      setLogs((l) => ({ ...l, [name]: { lines: lines.map(toLine), loading: false, error: null } }));
    } catch (e) {
      setLogs((l) => ({ ...l, [name]: { lines: l[name]?.lines ?? [], loading: false, error: errorText(e) } }));
    }
  };
  // Open logs of servers still producing output refresh on a timer too:
  // output does not come through events.
  const liveNames = servers.filter((s) => openLogs.current.has(s.name) && (s.state === "starting" || s.state === "ready" || s.state === "unreachable")).map((s) => s.name);
  const liveKey = liveNames.join(",");
  useEffect(() => {
    if (!liveKey) return;
    const t = setInterval(() => {
      if (!document.hidden) liveKey.split(",").forEach((n) => void loadLog(n));
    }, LOG_REFRESH);
    return () => clearInterval(t);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [liveKey, run.id]);

  const onOpenLog = (s: ServerInfo, open: boolean) => {
    if (open) {
      openLogs.current.add(s.name);
      void loadLog(s.name);
    } else openLogs.current.delete(s.name);
  };

  const act = async (label: string, s: ServerInfo, fn: () => Promise<Server | void>) => {
    setBusy((b) => [...b, s.name]);
    try {
      await fn();
      toast({ title: `${label} ${s.name}`, tone: "success" });
      invalidate(`run-servers:${run.id}`);
      invalidate(`run:${run.id}`);
    } catch (e) {
      toast({ title: `${label} ${s.name} failed`, description: errorText(e), tone: "danger", duration: 8000 });
    } finally {
      setBusy((b) => b.filter((n) => n !== s.name));
    }
  };

  if (q.error && !q.data && !run.servers) return <ErrorBlock error={q.error} onRetry={q.refetch} />;

  const note = (
    <>
      Servers stop when the run stops or moves host; spec servers start again with it, added ones do not. Output streams into the run's output as <span className="mono">server:&lt;name&gt;</span>.
      {servers.some((s) => s.url) ? " URLs open through the preview domain, for people allowed to read this run." : servers.length > 0 ? " Previews are not configured on this luxd: reach a server from a shell, or with lux port-forward." : ""}
    </>
  );

  return (
    <>
      <Card
        flush
        title="Servers"
        subtitle={servers.length === 0 ? "named ports of this run" : `${servers.filter((s) => s.state === "ready").length} of ${servers.length} ready`}
        actions={
          <>
            <Button size="sm" variant="ghost" icon={<IconRefresh size={13} />} loading={q.fetching} onClick={() => void q.refetch()}>
              Refresh
            </Button>
            <Button size="sm" icon={<IconPlus size={13} />} onClick={() => setAdding(true)} disabled={run.state === "succeeded" || run.state === "failed" || run.state === "cancelled"}>
              Add server
            </Button>
          </>
        }
      >
        <ErrorStrip error={q.error} />
        <ServerList
          servers={servers}
          logs={logs}
          busy={busy}
          runRunning={running}
          now={now}
          onStart={(s) => void act("Started", s, () => api.startServer(run.id, s.name))}
          onStop={(s) => void act("Stopped", s, () => api.stopServer(run.id, s.name))}
          onRestart={(s) => void act("Restarted", s, () => api.restartServer(run.id, s.name))}
          onRemove={(s) => setRemoving(servers.find((x) => x.name === s.name) ?? null)}
          onOpenLog={onOpenLog}
          note={note}
          empty={<EmptyState compact title="No servers" description="Add one to expose a port of this run, with a command lux starts for you, or declare them in the spec under workload.servers." action={<Button size="sm" icon={<IconPlus size={13} />} onClick={() => setAdding(true)}>Add server</Button>} />}
        />
      </Card>
      {adding && (
        <AddServerDialog
          run={run}
          taken={servers.map((s) => s.name)}
          onDone={(s) => {
            setAdding(false);
            toast({ title: `Added ${s.name}`, description: s.state === "starting" ? "starting" : undefined, tone: "success" });
            invalidate(`run-servers:${run.id}`);
            invalidate(`run:${run.id}`);
          }}
          onCancel={() => setAdding(false)}
        />
      )}
      <ConfirmDialog
        open={removing != null}
        title={`Remove ${removing?.name ?? "server"}?`}
        description={removing?.fromSpec ? "It is declared in the spec: it comes back on the run's next start unless the spec changes. A running process is stopped first." : "Its record and URL go away; a running process is stopped first."}
        confirmLabel="Remove"
        tone="danger"
        loading={removing != null && busy.includes(removing.name)}
        onConfirm={() => {
          const s = removing;
          if (!s) return;
          void act("Removed", s, () => api.removeServer(run.id, s.name)).then(() => setRemoving(null));
        }}
        onCancel={() => setRemoving(null)}
      />
    </>
  );
}

function toLine(l: { t: number; stream: "stdout" | "stderr"; text: string }): LogLine {
  return { ts: l.t, stream: l.stream, text: l.text };
}

const NAME_RE = /^[a-z][a-z0-9-]{0,29}$/;

/** Parse "K=V" lines into an env map; a line without "=" is an error. */
function parseEnv(text: string): { env: Record<string, string>; error?: string } {
  const env: Record<string, string> = {};
  for (const raw of text.split("\n")) {
    const line = raw.trim();
    if (!line || line.startsWith("#")) continue;
    const i = line.indexOf("=");
    if (i <= 0) return { env, error: `"${line}" is not NAME=value` };
    env[line.slice(0, i).trim()] = line.slice(i + 1);
  }
  return { env };
}

function AddServerDialog({ run, taken, onDone, onCancel }: { run: Run; taken: string[]; onDone: (s: Server) => void; onCancel: () => void }) {
  const [name, setName] = useState("");
  const [port, setPort] = useState("");
  const [command, setCommand] = useState("");
  const [workdir, setWorkdir] = useState("");
  const [env, setEnv] = useState("");
  const [start, setStart] = useState(true);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  useEffect(() => setError(null), [name, port, command, workdir, env, start]);

  const portN = Number(port);
  const nameErr = name === "" ? null : !NAME_RE.test(name) || name.endsWith("-") ? "a lowercase name: letters, digits and dashes, up to 30, not ending in a dash" : taken.includes(name) ? "this run already has a server with that name" : null;
  const portErr = port === "" ? null : !Number.isInteger(portN) || portN < 1 || portN > 65535 ? "a port from 1 to 65535" : null;
  const envParsed = parseEnv(env);
  const canSubmit = name !== "" && port !== "" && !nameErr && !portErr && !envParsed.error && !busy;
  const willStart = command.trim() !== "" && start && run.state === "running";

  const submit = async () => {
    const body: ServerInput = { name, port: portN };
    if (command.trim()) body.command = ["sh", "-c", command.trim()];
    if (workdir.trim()) body.workdir = workdir.trim();
    if (Object.keys(envParsed.env).length) body.env = envParsed.env;
    if (command.trim()) body.start = start;
    setBusy(true);
    try {
      onDone(await api.addServer(run.id, body));
    } catch (e) {
      setError(isApiError(e) && e.code === "name_taken" ? "This run already has a server with that name." : errorText(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Dialog open title="Add a server to this run" description="A port of the container, and a command lux starts there for you; or just the port, for something you start from a shell." confirmLabel={willStart ? "Add and start" : "Add"} loading={busy} disabled={!canSubmit} onConfirm={() => void submit()} onCancel={onCancel} width={520}>
      <div className="row" style={{ alignItems: "flex-start" }}>
        <label className="field" style={{ flex: "1 1 200px" }}>
          <span className="field-label">Name</span>
          <input className="input mono" value={name} onChange={(e) => setName(e.target.value.trim())} placeholder="web" autoFocus autoComplete="off" spellCheck={false} />
          {nameErr && <span className="muted">{nameErr}</span>}
        </label>
        <label className="field" style={{ flex: "0 1 120px" }}>
          <span className="field-label">Port</span>
          <input className="input mono" inputMode="numeric" value={port} onChange={(e) => setPort(e.target.value.trim())} placeholder="3000" autoComplete="off" />
          {portErr && <span className="muted">{portErr}</span>}
        </label>
      </div>
      <label className="field">
        <span className="field-label">Command (optional; run with sh -c, as the workload's user)</span>
        <input className="input mono" value={command} onChange={(e) => setCommand(e.target.value)} placeholder="npm run dev -- --host 0.0.0.0 --port 3000" autoComplete="off" spellCheck={false} />
      </label>
      <label className="field">
        <span className="field-label">Working directory (optional; relative to the workload's)</span>
        <input className="input mono" value={workdir} onChange={(e) => setWorkdir(e.target.value)} placeholder="apps/web" autoComplete="off" spellCheck={false} />
      </label>
      <label className="field">
        <span className="field-label">Environment (optional; NAME=value per line, not for secrets)</span>
        <textarea className="input textarea mono" rows={2} value={env} onChange={(e) => setEnv(e.target.value)} placeholder={"VITE_API_URL=http://localhost:8080"} spellCheck={false} />
        {envParsed.error && <span className="muted">{envParsed.error}</span>}
      </label>
      {command.trim() !== "" && (
        <label className="check">
          <input type="checkbox" checked={start} onChange={(e) => setStart(e.target.checked)} />
          Start it now{run.state !== "running" ? " (the run is not running: it starts with it)" : ""}
        </label>
      )}
      {error && <div className="error-strip">{error}</div>}
    </Dialog>
  );
}

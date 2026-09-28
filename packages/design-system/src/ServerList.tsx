import { useEffect, useState, type ReactNode } from "react";
import { Button, IconButton } from "./Button.tsx";
import { ServerStateMark } from "./Badge.tsx";
import { LogView, type LogLine } from "./LogView.tsx";
import { EmptyState } from "./EmptyState.tsx";
import { formatClock, formatElapsed } from "./format.ts";
import { IconCheck, IconChevronDown, IconChevronUp, IconCopy, IconExternal, IconPlay, IconRefresh, IconStop, IconTrash } from "./icons.tsx";
import type { ServerState } from "./states.ts";

/** A Run's server as the API reports it: the fields the row shows. */
export interface ServerInfo {
  name: string;
  port: number;
  command?: string[] | null;
  state: ServerState | string;
  exitCode?: number | null;
  /** The last stderr line on exit. */
  error?: string | null;
  /** When the state last changed. */
  since?: string | null;
  readySince?: string | null;
  stopReason?: string | null;
  url?: string | null;
}

export interface ServerLogState {
  lines: LogLine[];
  loading?: boolean;
  error?: string | null;
}

export interface ServerRowProps {
  server: ServerInfo;
  /** The Run is running: start, stop and restart are possible. */
  runRunning: boolean;
  now?: number;
  /** An action in flight on this server (its buttons wait). */
  busy?: boolean;
  onStart?: (s: ServerInfo) => void;
  onStop?: (s: ServerInfo) => void;
  onRestart?: (s: ServerInfo) => void;
  onRemove?: (s: ServerInfo) => void;
  /** The log pane, once opened; the row asks for it with onOpenLog. */
  log?: ServerLogState;
  onOpenLog?: (s: ServerInfo, open: boolean) => void;
  /** Start with the log open. */
  defaultOpen?: boolean;
  /** Extra actions at the end of the row. */
  extra?: ReactNode;
}

const LOG_H = 240;

/** "ready for 12m", "starting · 9s", "stopped at 14:32 · was ready for 41m", "exited 14:29". */
export function serverSinceText(s: ServerInfo, now: number): string {
  switch (s.state) {
    case "ready":
      return s.readySince ? `ready for ${formatElapsed(s.readySince, now)}` : "ready";
    case "starting":
      return s.since ? `starting · ${formatElapsed(s.since, now)}` : "starting";
    case "unreachable":
      return s.since ? `unreachable for ${formatElapsed(s.since, now)}` : "unreachable";
    case "exited":
      return s.since ? `exited ${formatClock(s.since).slice(0, 5)}` : "exited";
    case "stopped": {
      if (!s.since && !s.stopReason) return "not started";
      const parts: string[] = [];
      if (s.since) parts.push(`stopped ${s.stopReason === "run stopped" || s.stopReason === "migrated" || s.stopReason === "host lost" ? `at ${formatClock(s.since).slice(0, 5)}` : formatClock(s.since).slice(0, 5)}`);
      else parts.push("stopped");
      if (s.stopReason && s.stopReason !== "stopped") parts.push(s.stopReason);
      return parts.join(" · ");
    }
    default:
      return s.state;
  }
}

/** One server: name and port, state with how long, its URL to copy or open, and what can be done to it. */
export function ServerRow({ server: s, runRunning, now = Date.now(), busy, onStart, onStop, onRestart, onRemove, log, onOpenLog, defaultOpen = false, extra }: ServerRowProps) {
  const [open, setOpen] = useState(defaultOpen);
  const [copied, setCopied] = useState(false);
  useEffect(() => {
    if (!copied) return;
    const t = setTimeout(() => setCopied(false), 1200);
    return () => clearTimeout(t);
  }, [copied]);
  const live = s.state === "ready";
  const up = s.state === "ready" || s.state === "starting" || s.state === "unreachable";
  const canStart = runRunning && !up && !!s.command?.length;
  const toggleLog = () => {
    const next = !open;
    setOpen(next);
    onOpenLog?.(s, next);
  };
  const copyUrl = () => {
    if (!s.url) return;
    void navigator.clipboard?.writeText(s.url).then(() => setCopied(true));
  };
  const host = s.url?.replace(/^https?:\/\//, "");
  return (
    <li className={["server-row", open ? "is-open" : ""].join(" ").trim()} data-state={s.state}>
      <div className="server-main">
        <div className="server-name">
          <span className="server-name-text mono">{s.name}</span>
          <span className="server-port mono">:{s.port}</span>
        </div>
        <div className="server-mid">
          <div className="server-state">
            <ServerStateMark state={s.state} exitCode={s.exitCode} />
            <span className="server-since">{serverSinceText(s, now)}</span>
            {s.state === "exited" && s.error && (
              <span className="server-err" title={s.error}>
                {s.error}
              </span>
            )}
          </div>
          {s.url ? (
            <div className={["server-url", live ? "" : "is-off"].join(" ").trim()}>
              <a href={s.url} target="_blank" rel="noreferrer" className="mono" title={s.url}>
                {host}
              </a>
              <IconButton size="sm" label={copied ? "Copied" : "Copy URL"} onClick={copyUrl}>
                {copied ? <IconCheck size={13} /> : <IconCopy size={13} />}
              </IconButton>
              {live && (
                <IconButton size="sm" label="Open in a new tab" onClick={() => window.open(s.url ?? "", "_blank", "noopener")}>
                  <IconExternal size={13} />
                </IconButton>
              )}
            </div>
          ) : (
            <div className="server-url is-off muted">{s.command?.length ? <span className="mono" title={s.command.join(" ")}>{s.command.join(" ")}</span> : "no command: started by hand"}</div>
          )}
        </div>
        <div className="server-actions">
          {onOpenLog && (
            <Button size="sm" variant="ghost" onClick={toggleLog} aria-expanded={open}>
              Logs {open ? <IconChevronUp size={12} /> : <IconChevronDown size={12} />}
            </Button>
          )}
          {up ? (
            <>
              {onRestart && live && (
                <IconButton size="sm" label={`Restart ${s.name}`} disabled={busy || !runRunning} onClick={() => onRestart(s)}>
                  <IconRefresh size={13} />
                </IconButton>
              )}
              {onStop && (
                <Button size="sm" variant="ghost" icon={<IconStop size={12} />} loading={busy} disabled={!runRunning} onClick={() => onStop(s)}>
                  Stop
                </Button>
              )}
            </>
          ) : (
            onStart && (
              <Button size="sm" icon={<IconPlay size={12} />} loading={busy} disabled={!canStart} title={!runRunning ? "The run is not running" : !s.command?.length ? "No command: start it from a shell" : undefined} onClick={() => onStart(s)}>
                Start
              </Button>
            )
          )}
          {onRemove && (
            <IconButton size="sm" label={`Remove ${s.name}`} disabled={busy} onClick={() => onRemove(s)}>
              <IconTrash size={13} />
            </IconButton>
          )}
          {extra}
        </div>
      </div>
      {open && (
        <div className="server-log">
          <div className="server-log-head">
            <span className="mono">server:{s.name}</span>
            {log?.error && <span className="server-log-err">{log.error}</span>}
          </div>
          <LogView lines={log?.lines ?? []} height={LOG_H} timestamps emptyText={log?.loading ? "Loading…" : "No output yet."} />
        </div>
      )}
    </li>
  );
}

export interface ServerListProps extends Omit<ServerRowProps, "server" | "log" | "defaultOpen" | "busy"> {
  servers: ServerInfo[];
  /** Names with an action in flight. */
  busy?: string[];
  /** Log panes by server name. */
  logs?: Record<string, ServerLogState>;
  /** Names whose log starts open. */
  open?: string[];
  empty?: ReactNode;
  /** A line under the list. */
  note?: ReactNode;
}

/** The Run's servers, one row each, in the order given. */
export function ServerList({ servers, busy, logs, open, empty, note, ...row }: ServerListProps) {
  if (servers.length === 0) {
    return typeof empty === "string" || empty == null ? <EmptyState compact title={empty ?? "No servers"} /> : <>{empty}</>;
  }
  return (
    <div className="server-list-wrap">
      <ul className="server-list">
        {servers.map((s) => (
          <ServerRow key={s.name} server={s} busy={busy?.includes(s.name)} log={logs?.[s.name]} defaultOpen={open?.includes(s.name)} {...row} />
        ))}
      </ul>
      {note && <p className="server-note">{note}</p>}
    </div>
  );
}

import { useState, type ReactNode } from "react";
import { Button, IconButton } from "./Button.tsx";
import { ServerStateMark } from "./Badge.tsx";
import { EmptyState } from "./EmptyState.tsx";
import { useCopy } from "./IdChip.tsx";
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
  /** Why it is stopped. */
  stopReason?: string | null;
  /** The placement it stopped in; null if it never started. */
  stoppedEpoch?: number | null;
  url?: string | null;
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
  /** The log pane under the row, once opened (the row owns the toggle; the caller owns the fetch). */
  renderLog?: (s: ServerInfo) => ReactNode;
  /** Start with the log open. */
  defaultOpen?: boolean;
}

/** "ready for 12m", "starting · 9s", "stopped at 14:32 · migrated", "exited 14:29", "not started". */
function sinceText(s: ServerInfo, now: number): string {
  const clock = (at: string) => formatClock(at).slice(0, 5);
  switch (s.state) {
    case "ready":
      return s.readySince ? `ready for ${formatElapsed(s.readySince, now)}` : "ready";
    case "starting":
      return s.since ? `starting · ${formatElapsed(s.since, now)}` : "starting";
    case "unreachable":
      return s.since ? `unreachable for ${formatElapsed(s.since, now)}` : "unreachable";
    case "exited":
      return s.since ? `exited ${clock(s.since)}` : "exited";
    case "stopped": {
      if (s.stoppedEpoch == null && !s.stopReason) return "not started";
      const when = s.since ? `stopped at ${clock(s.since)}` : "stopped";
      return s.stopReason && s.stopReason !== "stopped" ? `${when} · ${s.stopReason}` : when;
    }
    default:
      return s.state;
  }
}

/** One server: name and port, state with how long, its URL to copy or open, and what can be done to it. */
export function ServerRow({ server: s, runRunning, now = Date.now(), busy, onStart, onStop, onRestart, onRemove, renderLog, defaultOpen = false }: ServerRowProps) {
  const [open, setOpen] = useState(defaultOpen);
  const { copied, copy } = useCopy(s.url ?? "");
  const live = s.state === "ready";
  const up = s.state === "ready" || s.state === "starting" || s.state === "unreachable";
  const canStart = runRunning && !up && !!s.command?.length;
  const host = s.url?.replace(/^https?:\/\//, "");
  return (
    <li className={["server-row", open ? "is-open" : ""].join(" ").trim()}>
      <div className="server-main">
        <div className="server-name">
          <span className="server-name-text mono">{s.name}</span>
          <span className="server-port mono">:{s.port}</span>
        </div>
        <div className="server-mid">
          <div className="server-state">
            <ServerStateMark state={s.state} exitCode={s.exitCode} />
            <span className="server-since">{sinceText(s, now)}</span>
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
              <IconButton size="sm" label={copied ? "Copied" : "Copy URL"} onClick={copy}>
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
          {renderLog && (
            <Button size="sm" variant="ghost" onClick={() => setOpen(!open)} aria-expanded={open}>
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
        </div>
      </div>
      {open && renderLog && (
        <div className="server-log">
          <div className="server-log-head">
            <span className="mono">server:{s.name}</span>
          </div>
          {renderLog(s)}
        </div>
      )}
    </li>
  );
}

export interface ServerListProps extends Omit<ServerRowProps, "server" | "defaultOpen" | "busy"> {
  servers: ServerInfo[];
  /** Names with an action in flight. */
  busy?: string[];
  /** Names whose log starts open. */
  open?: string[];
  /** Shown instead of the list when there are no servers. */
  empty?: ReactNode;
  /** A line under the list. */
  note?: ReactNode;
}

/** The Run's servers, one row each, in the order given. */
export function ServerList({ servers, busy, open, empty, note, ...row }: ServerListProps) {
  if (servers.length === 0) return <>{empty ?? <EmptyState compact title="No servers" />}</>;
  return (
    <div className="server-list-wrap">
      <ul className="server-list">
        {servers.map((s) => (
          <ServerRow key={s.name} server={s} busy={busy?.includes(s.name)} defaultOpen={open?.includes(s.name)} {...row} />
        ))}
      </ul>
      {note && <p className="server-note">{note}</p>}
    </div>
  );
}

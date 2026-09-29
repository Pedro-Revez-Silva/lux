import { useCallback, useEffect, useLayoutEffect, useRef, useState } from "react";
import { Badge, Button, Card, ConnectionBadge, CrumbSep, DEFAULT_FONT_SIZE, EmptyState, formatDuration, IconButton, IdChip, LinkButton, PageHeader, StatePill, Terminal, TerminalOverlay, useTheme, type ConnectionStatus, type TerminalHandle, type TerminalSize } from "@lux/design-system";
import { IconChevronLeft, IconExternal, IconMinus, IconMoon, IconPlus, IconRefresh, IconSun, IconTerminal, IconWarning } from "@lux/design-system/icons";
import { errorText, EXEC_RUN_STATES, isApiError, openExec, SHELL_COMMAND, TERMINAL_RUN_STATES, useSession, type ExecSession, type Run } from "../../api/index.ts";
import { href, Link, scoped, useSearch } from "../router.tsx";
import { ButtonLink, ErrorBlock, HostLink, PageSkeleton, RunLink, runPath, useRun } from "./common.tsx";

const FONT_KEY = "lux.terminal.font";
const FONT_MIN = 10;
const FONT_MAX = 20;

function readFont(): number {
  try {
    const n = Number(localStorage.getItem(FONT_KEY));
    if (Number.isInteger(n) && n >= FONT_MIN && n <= FONT_MAX) return n;
  } catch {}
  return DEFAULT_FONT_SIZE;
}

/** The shell as the connection sees it: status, and why it ended. */
interface Shell {
  status: ConnectionStatus;
  exitCode?: number;
  /** The socket's close code (1006: no close frame, the host or luxd went away). */
  closeCode?: number;
  error?: string;
  /** The shell could not be opened at all (the pre-check refused: not allowed, not running); Reconnect will not help. */
  refused?: boolean;
  openedAt?: number;
  endedAt?: number;
}

export const terminalPath = (id: string) => `${runPath(id)}/terminal`;

/**
 * /runs/:id/terminal: a shell in the Run's container, over the exec
 * WebSocket. The header says which run, on which host, as whom; the
 * terminal takes the rest of the height. Every open is one shell: a
 * reload, Reconnect or a new tab starts another; closing the tab ends it.
 */
export function TerminalPage({ id }: { id: string }) {
  const session = useSession();
  const { resolved, toggle } = useTheme();
  const search = useSearch();
  const q = useRun(id, { active: 5000, settled: 15_000, liveActive: 30_000, liveSettled: 60_000 });
  const run = q.data;
  const running = run != null && EXEC_RUN_STATES.has(run.state);

  const term = useRef<TerminalHandle>(null);
  const exec = useRef<ExecSession | null>(null);
  const [shell, setShell] = useState<Shell>({ status: "connecting" });
  const [round, setRound] = useState(0);
  const [size, setSize] = useState<TerminalSize>({ cols: 80, rows: 24 });
  const [fontSize, setFontSize] = useState(readFont);
  const setFont = (n: number) => {
    const v = Math.min(FONT_MAX, Math.max(FONT_MIN, n));
    setFontSize(v);
    try {
      localStorage.setItem(FONT_KEY, String(v));
    } catch {}
  };

  // One shell per round: connect once the terminal is open and the Run is
  // running; Reconnect (a new round) ends the shell and opens another.
  const [ready, setReady] = useState(false);
  useEffect(() => {
    if (!ready || !running) return;
    const t = term.current;
    if (!t) return;
    const ctrl = new AbortController();
    t.reset();
    t.write(`\x1b[2mOpening a shell on ${run?.host ?? "the run's host"} (bash -l, falling back to sh)…\x1b[0m\r\n`);
    setShell({ status: "connecting" });
    let opened = 0;
    openExec(id, {
      command: SHELL_COMMAND,
      size: () => t.size(),
      signal: ctrl.signal,
      onOpen: () => {
        opened = Date.now();
        t.reset();
        t.focus();
        setShell({ status: "connected", openedAt: opened });
      },
      onData: (b) => t.write(b),
      onExit: (code) => setShell({ status: "exited", exitCode: code, openedAt: opened, endedAt: Date.now() }),
      onError: (m) => setShell({ status: "disconnected", error: m, openedAt: opened, endedAt: Date.now() }),
      onClose: (code, reason) =>
        setShell((s) => (s.status === "exited" || s.status === "disconnected" ? { ...s, closeCode: code } : { status: "disconnected", closeCode: code, error: reason || undefined, openedAt: opened, endedAt: Date.now() })),
    })
      .then((s) => {
        if (ctrl.signal.aborted) s.close();
        else exec.current = s;
      })
      .catch((e: unknown) => {
        if (ctrl.signal.aborted) return;
        // The pre-check refused (not allowed, not running): not a lost host.
        setShell({ status: "disconnected", error: errorText(e), refused: isApiError(e), endedAt: Date.now() });
      });
    return () => {
      ctrl.abort();
      exec.current = null;
    };
  }, [id, ready, running, round]);

  // The Run stopped under the shell: the socket closes on its own, but say
  // why. Running again: back to connecting before the Terminal paints, so
  // the last shell's overlay does not show over the new screen.
  useLayoutEffect(() => {
    if (!run) return;
    if (!running) setShell((s) => (s.status === "connected" || s.status === "connecting" ? { ...s, status: "disconnected", error: `the run is ${run.state}`, endedAt: Date.now() } : s));
    else setShell((s) => (s.status === "disconnected" && !s.closeCode && !s.refused ? { status: "connecting" } : s));
  }, [run, running]);

  const onData = useCallback((d: string) => exec.current?.send(d), []);
  const onResize = useCallback((s: TerminalSize) => {
    setSize(s);
    exec.current?.resize(s.cols, s.rows);
  }, []);
  const onReady = useCallback((s: TerminalSize) => {
    setSize(s);
    setReady(true);
  }, []);
  const reconnect = () => setRound((r) => r + 1);
  const connected = shell.status === "connected";
  const who = session.user?.email ?? (session.role === "operator" ? "an operator key" : "an API key");

  if (q.error && !run) {
    return (
      <div className="page">
        <ErrorBlock error={q.error} onRetry={q.refetch} />
      </div>
    );
  }
  if (!run) return <PageSkeleton />;

  return (
    <div className="page page-fill">
      <PageHeader
        className="term-head"
        crumbs={
          <>
            <Link to="/runs">Runs</Link>
            <CrumbSep />
            <RunLink id={run.id} />
            <CrumbSep />
            <span className="crumbs-here">Terminal</span>
          </>
        }
        title="Terminal"
        badges={
          <>
            <StatePill kind="run" state={run.state} activity={run.activity} />
            {run.name && <Badge outline>{run.name}</Badge>}
            {running && <ConnectionBadge status={shell.status} exitCode={shell.exitCode} />}
          </>
        }
        description={
          <>
            {running && (
              <>
                <span>
                  <span className="mono">bash -l</span> as <span className="mono">{run.spec.workload.user || "the workload's user"}</span>
                </span>
                {run.spec.workload.workdir && (
                  <span>
                    in <span className="mono">{run.spec.workload.workdir}</span>
                  </span>
                )}
              </>
            )}
            {run.hostId && (
              <span>
                on <HostLink id={run.hostId} name={run.host} /> <IdChip value={run.hostId} />
              </span>
            )}
            <span>epoch {run.epoch}</span>
            {!running && run.stateReason && <span>{run.stateReason}</span>}
          </>
        }
        actions={
          running ? (
            <div className="row term-actions">
              <div className="btn-group" role="group" aria-label="Font size">
                <IconButton label="Smaller text" onClick={() => setFont(fontSize - 1)} disabled={fontSize <= FONT_MIN}>
                  <IconMinus size={15} />
                </IconButton>
                <span className="btn-group-val">{fontSize} px</span>
                <IconButton label="Larger text" onClick={() => setFont(fontSize + 1)} disabled={fontSize >= FONT_MAX}>
                  <IconPlus size={15} />
                </IconButton>
              </div>
              <IconButton label={resolved === "dark" ? "Switch to light theme" : "Switch to dark theme"} title="Switch theme (the terminal follows: Solarized dark / light)" onClick={toggle}>
                {resolved === "dark" ? <IconSun size={16} /> : <IconMoon size={16} />}
              </IconButton>
              <span className="btn-sep" aria-hidden="true" />
              <Button icon={<IconRefresh size={15} />} onClick={reconnect} disabled={shell.status === "connecting"} title="Ends this shell and opens a new one">
                Reconnect
              </Button>
              <LinkButton href={href(scoped(terminalPath(run.id), search))} target="_blank" rel="noopener" icon={<IconExternal size={15} />} title={`Opens ${terminalPath(run.id)} in a new tab: a separate shell`}>
                Open in new tab
              </LinkButton>
            </div>
          ) : (
            <ButtonLink to={runPath(run.id)} icon={<IconChevronLeft size={15} />}>
              Back to run
            </ButtonLink>
          )
        }
      />

      {running ? (
        <Terminal
          ref={term}
          fontSize={fontSize}
          disabled={!connected}
          onData={onData}
          onResize={onResize}
          onReady={onReady}
          overlay={shell.status === "exited" || shell.status === "disconnected" ? <ShellOverlay run={run} shell={shell} onReconnect={reconnect} /> : undefined}
          bar={
            <>
              <div className="term-bar-group">
                <span className="num">
                  {size.cols} × {size.rows}
                </span>
                <span className="sep">·</span>
                <span>
                  <kbd>Ctrl</kbd>+<kbd>Shift</kbd>+<kbd>C</kbd> / <kbd>V</kbd> to copy and paste
                </span>
                <span className="sep">·</span>
                <span>Closing this tab ends the shell</span>
              </div>
              <div className="term-bar-group is-audit" title={`Opened by ${who}; keystrokes are not recorded`}>
                Opened by <span className="mono">{who}</span> · recorded as a run event
              </div>
            </>
          }
        />
      ) : (
        <NotRunning run={run} />
      )}
    </div>
  );
}

function ShellOverlay({ run, shell, onReconnect }: { run: Run; shell: Shell; onReconnect: () => void }) {
  const lasted = shell.openedAt && shell.endedAt ? formatDuration((shell.endedAt - shell.openedAt) / 1000) : null;
  const back = (
    <ButtonLink to={runPath(run.id)} variant="ghost">
      Back to run
    </ButtonLink>
  );
  if (shell.status === "exited") {
    return (
      <TerminalOverlay
        icon={<IconTerminal size={18} />}
        title={
          <>
            Shell exited <Badge mono>code {shell.exitCode ?? 0}</Badge>
          </>
        }
        description={
          <>
            The shell ended (you typed <span className="mono">exit</span>, or it was killed). The run is still running; a new shell starts fresh{run.spec.workload.workdir ? <> in <span className="mono">{run.spec.workload.workdir}</span></> : null}.
          </>
        }
        actions={
          <>
            <Button variant="primary" icon={<IconRefresh size={15} />} onClick={onReconnect}>
              Start a new shell
            </Button>
            {back}
          </>
        }
        meta={lasted ? `Lasted ${lasted} · recorded on the run.` : "Recorded on the run."}
      />
    );
  }
  const meta = [shell.closeCode != null ? `Socket closed (${shell.closeCode})` : null, shell.error].filter(Boolean).join(" · ");
  if (shell.refused) {
    return (
      <TerminalOverlay
        warn
        icon={<IconWarning size={18} />}
        title="No shell could be opened"
        description={shell.error ?? "luxd refused the stream."}
        actions={
          <>
            <Button variant="primary" icon={<IconRefresh size={15} />} onClick={onReconnect}>
              Try again
            </Button>
            {back}
          </>
        }
      />
    );
  }
  return (
    <TerminalOverlay
      warn
      icon={<IconWarning size={18} />}
      title="Connection to the host was lost"
      description="The run may be moving to another host. This shell is gone; when the run is running again, Reconnect opens a new one."
      actions={
        <>
          <Button variant="primary" icon={<IconRefresh size={15} />} onClick={onReconnect}>
            Reconnect
          </Button>
          {back}
        </>
      }
      meta={meta ? `${meta}.` : undefined}
    />
  );
}

/** Why there is no shell to open, in the run's own words. */
function NotRunning({ run }: { run: Run }) {
  const last = run.placements?.at(-1);
  const state = run.state;
  const ended = TERMINAL_RUN_STATES.has(state);
  const parked = state === "stopped" || state === "lost";
  const title =
    state === "stopped"
      ? "This run is stopped — there is no container to open a shell in."
      : state === "lost"
        ? "This run is lost — its host stopped answering."
        : ended
          ? `This run is ${state} — there is no container to open a shell in.`
          : `This run is ${state} — its container is not up yet.`;
  const desc =
    state === "stopped" && last?.stopReason === "migrate"
      ? `Stopped by migration: its placement on ${last.hostName || last.host} was snapshotted and the run is parked until a ready host takes it. Once it is running again, this page opens a shell.`
      : parked
        ? `${run.stateReason ? run.stateReason + ". " : ""}Resume it from the run page; once it is running again, this page opens a shell.`
        : ended
          ? run.stateReason || "The run has ended for good."
          : "This page opens a shell as soon as the run is running.";
  return (
    <div className="term-none">
      <Card>
        <EmptyState
          icon={<IconTerminal size={24} />}
          title={title}
          description={desc}
          action={
            <span className="row" style={{ justifyContent: "center" }}>
              <ButtonLink to={runPath(run.id)} variant="primary">
                Back to run
              </ButtonLink>
              <ButtonLink to={`${runPath(run.id)}?tab=timeline`}>Timeline</ButtonLink>
            </span>
          }
        />
      </Card>
    </div>
  );
}

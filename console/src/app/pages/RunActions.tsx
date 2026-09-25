import { useEffect, useMemo, useState } from "react";
import { Badge, Button, ConfirmDialog, Dialog, formatBytes, IdChip, Select, useToast } from "@lux/design-system";
import { api, errorText, isApiError, RESUMABLE_RUN_STATES, TERMINAL_RUN_STATES, useQuery, type MigrateRequest, type ResumeRequest, type Run, type Snapshot } from "../../api/index.ts";

export interface RunActionsProps {
  run: Run;
  operator: boolean;
  /** Called with the Run returned by the action. */
  onChanged: (run: Run) => void;
}

type Open = "stop" | "cancel" | "resume" | "migrate" | null;

export function RunActions({ run, operator, onChanged }: RunActionsProps) {
  const toast = useToast();
  const [open, setOpen] = useState<Open>(null);
  const [busy, setBusy] = useState(false);

  const terminal = TERMINAL_RUN_STATES.has(run.state);
  const canStop = !terminal && run.state !== "stopped" && run.state !== "lost";
  const canCancel = !terminal;
  const canResume = RESUMABLE_RUN_STATES.has(run.state);
  const canMigrate = operator && run.state === "running";

  /** Runs an action; returns the error (already toasted) or null on success. */
  const act = async (label: string, fn: () => Promise<Run>): Promise<unknown> => {
    setBusy(true);
    try {
      const r = await fn();
      toast({ title: `${label}: ${r.state}`, description: r.stateReason, tone: "success" });
      onChanged(r);
      setOpen(null);
      return null;
    } catch (e) {
      toast({ title: `${label} failed`, description: errorText(e), tone: "danger", duration: 8000 });
      return e;
    } finally {
      setBusy(false);
    }
  };

  return (
    <>
      <Button disabled={!canStop} onClick={() => setOpen("stop")}>
        Stop
      </Button>
      <Button variant="danger" disabled={!canCancel} onClick={() => setOpen("cancel")}>
        Cancel
      </Button>
      <Button variant="primary" disabled={!canResume} onClick={() => setOpen("resume")}>
        Resume
      </Button>
      {operator && (
        <Button disabled={!canMigrate} onClick={() => setOpen("migrate")} title={run.state !== "running" ? "Only a running run can be migrated" : undefined}>
          Migrate
        </Button>
      )}

      <ConfirmDialog
        open={open === "stop"}
        title="Stop run?"
        description="The workload gets its grace period, its state volumes are snapshotted, and the run can be resumed later."
        confirmLabel="Stop run"
        loading={busy}
        onConfirm={() => void act("Stop", () => api.stopRun(run.id))}
        onCancel={() => setOpen(null)}
      />
      <ConfirmDialog
        open={open === "cancel"}
        title="Cancel run?"
        description="The run ends for good: it cannot be resumed, and its held secrets are dropped. Its snapshots and artifacts stay until retention."
        confirmLabel="Cancel run"
        tone="danger"
        confirmText={run.id}
        loading={busy}
        onConfirm={() => void act("Cancel", () => api.cancelRun(run.id))}
        onCancel={() => setOpen(null)}
      />
      {open === "resume" && <ResumeDialog run={run} operator={operator} busy={busy} onConfirm={(body) => act("Resume", () => api.resumeRun(run.id, body))} onCancel={() => setOpen(null)} />}
      {open === "migrate" && <MigrateDialog run={run} busy={busy} onConfirm={(body) => act("Migrate", () => api.migrateRun(run.id, body))} onCancel={() => setOpen(null)} />}
    </>
  );
}

const ANY = "";

/** Ready hosts the run's tenant can use (its own and platform hosts), minus `exclude` (a host id). */
function useReadyHosts(run: Run, enabled: boolean, exclude?: string) {
  const q = useQuery(`hosts:ready:${run.tenant}`, (s) => api.hosts(run.tenant, { state: "ready" }, s), { enabled });
  const options = useMemo(
    () => (q.data ?? []).filter((h) => h.id !== exclude).map((h) => ({ value: h.id, text: h.name, label: h.name, description: `${h.pool} · ${h.liveRuns}/${h.capacity.runs} runs${h.tenant ? ` · ${h.tenant}` : " · platform"}` })),
    [q.data, exclude],
  );
  return { options, error: q.error };
}

function ResumeDialog({ run, operator, busy, onConfirm, onCancel }: { run: Run; operator: boolean; busy: boolean; onConfirm: (body: ResumeRequest) => Promise<unknown>; onCancel: () => void }) {
  const rs = run.resume;
  const [input, setInput] = useState("");
  const [to, setTo] = useState(ANY);
  const [from, setFrom] = useState(ANY);
  // Stopped over its disk limit: offer twice the limit it had.
  const overDisk = run.placements?.at(-1)?.stopReason === "disk";
  const diskLimit = run.spec.resources.disk;
  const [disk, setDisk] = useState(overDisk && diskLimit ? formatBytes(diskLimit * 2).replace(/\s|B$/g, "") : "");
  const [secretValues, setSecretValues] = useState<Record<string, string>>({});
  const [needSecrets, setNeedSecrets] = useState(!operator || (rs != null && (rs.secrets?.length ?? 0) > 0 && !rs.secretsHeld));
  const [lastError, setLastError] = useState<string | null>(null);
  const hosts = useReadyHosts(run, operator);
  const snaps = useQuery(`run-snapshots:${run.id}`, (s) => api.snapshots(run.id, s));
  const secretNames = rs?.secrets ?? run.secrets.map((s) => s.name);

  useEffect(() => {
    setLastError(null);
  }, [input, to, from, secretValues, disk]);

  const missing = needSecrets ? secretNames.filter((n) => !secretValues[n]) : [];
  // Warnings, not a gate: the server decides. A chosen snapshot makes the
  // "latest snapshot unavailable" blocker moot.
  const blockers = (rs?.blockers ?? []).filter((b) => !(from && b.startsWith("its snapshot")));
  const submit = async () => {
    const body: ResumeRequest = {};
    if (input.trim()) body.input = { text: input.trim() };
    if (to) body.to = to;
    if (from) body.fromSnapshot = from;
    if (disk.trim()) body.resources = { disk: disk.trim() };
    if (needSecrets && secretNames.length > 0) body.secrets = secretNames.map((name) => ({ name, value: secretValues[name] ?? "" }));
    const err = await onConfirm(body);
    if (err == null) return;
    // luxd no longer holds the values: ask for them and let the user retry.
    if (isApiError(err) && err.code === "secrets_required") setNeedSecrets(true);
    setLastError(errorText(err));
  };

  const snapOptions = [{ value: ANY, text: "Latest (default)", label: "Latest (default)" }, ...(snaps.data ?? []).filter((s: Snapshot) => s.available).map((s) => ({ value: s.id, text: s.id, label: <span className="mono">{s.id}</span>, description: `epoch ${s.epoch}${s.uploaded ? " · uploaded" : ""}${s.onHost ? ` · on ${s.onHost}` : ""}` }))];

  return (
    <Dialog open title="Resume run" confirmLabel="Resume" loading={busy} disabled={missing.length > 0} onConfirm={() => void submit()} onCancel={onCancel} width={520}>
      {rs && (
        <div className="resume-report">
          <div className="resume-row">
            <span className="muted">Snapshot</span>
            <span>{rs.snapshot ? <IdChip value={rs.snapshot} truncate={20} /> : <span className="muted">none: starts from scratch</span>}</span>
          </div>
          {rs.snapshot && (
            <div className="resume-row">
              <span className="muted">Lives</span>
              <span className="row" style={{ gap: 4 }}>
                {rs.uploaded && <Badge tone="success">uploaded</Badge>}
                {(rs.onHosts ?? []).map((h) => (
                  <Badge key={h} outline>
                    on {h}
                  </Badge>
                ))}
                {!rs.uploaded && (rs.onHosts?.length ?? 0) === 0 && <Badge tone="danger">nowhere</Badge>}
              </span>
            </div>
          )}
          <div className="resume-row">
            <span className="muted">Secrets</span>
            <span>{secretNames.length === 0 ? <span className="muted">none</span> : rs.secretsHeld ? <Badge tone="success">held by luxd ({secretNames.join(", ")})</Badge> : <Badge tone="warn">values needed: {secretNames.join(", ")}</Badge>}</span>
          </div>
          {blockers.map((b) => (
            <div key={b} className="warn-strip">
              {b}
            </div>
          ))}
          {blockers.length > 0 && <div className="muted">luxd checks again when you resume, and says why if it refuses.</div>}
        </div>
      )}
      <label className="field">
        <span className="field-label">Input text (optional, delivered on start)</span>
        <textarea className="input textarea" rows={3} value={input} onChange={(e) => setInput(e.target.value)} placeholder="e.g. continue where you left off" />
      </label>
      {operator && (
        <div className="field">
          <span className="field-label">Target host (optional)</span>
          <Select value={to} onChange={setTo} searchable options={[{ value: ANY, text: "Any ready host", label: "Any ready host" }, ...hosts.options]} placeholder="Any ready host" />
          {hosts.error && <span className="muted">{hosts.error}</span>}
        </div>
      )}
      <div className="field">
        <span className="field-label">From snapshot (optional)</span>
        <Select value={from} onChange={setFrom} options={snapOptions} />
      </div>
      <label className="field">
        <span className="field-label">Disk limit (optional; now {diskLimit ? formatBytes(diskLimit) : "the default"})</span>
        {overDisk && <div className="warn-strip">It stopped over its disk limit: give it more, or it will stop again.</div>}
        <input className="input mono" value={disk} onChange={(e) => setDisk(e.target.value)} placeholder="e.g. 40Gi" spellCheck={false} />
      </label>
      {needSecrets && secretNames.length > 0 && (
        <div className="field">
          <span className="field-label">Secret values</span>
          {secretNames.map((n) => (
            <label key={n} className="secret-row">
              <span className="mono secret-name">{n}</span>
              <input className="input mono" type="password" value={secretValues[n] ?? ""} onChange={(e) => setSecretValues((v) => ({ ...v, [n]: e.target.value }))} autoComplete="off" />
            </label>
          ))}
        </div>
      )}
      {lastError && <div className="error-strip">{lastError}</div>}
    </Dialog>
  );
}

function MigrateDialog({ run, busy, onConfirm, onCancel }: { run: Run; busy: boolean; onConfirm: (body: MigrateRequest) => Promise<unknown>; onCancel: () => void }) {
  const [to, setTo] = useState(ANY);
  const [input, setInput] = useState("");
  const hosts = useReadyHosts(run, true, run.hostId);
  const submit = () => {
    const body: MigrateRequest = {};
    if (to) body.to = to;
    if (input.trim()) body.input = { text: input.trim() };
    void onConfirm(body);
  };
  return (
    <Dialog open title="Migrate run" description={`Stops the placement on ${run.host || "its host"}, snapshots it, and resumes it elsewhere.`} confirmLabel="Migrate" loading={busy} onConfirm={submit} onCancel={onCancel} width={520}>
      <div className="field">
        <span className="field-label">Target host</span>
        <Select value={to} onChange={setTo} searchable options={[{ value: ANY, text: "Any other ready host", label: "Any other ready host" }, ...hosts.options]} />
        {hosts.error && <span className="muted">{hosts.error}</span>}
      </div>
      <label className="field">
        <span className="field-label">Input text (optional, delivered after the move)</span>
        <textarea className="input textarea" rows={3} value={input} onChange={(e) => setInput(e.target.value)} />
      </label>
    </Dialog>
  );
}

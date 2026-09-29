import { useMemo, useState, type FormEvent } from "react";
import { Badge, Button, LogView, Spinner, Tabs, useToast, type LogLine } from "@lux/design-system";
import { IconSend } from "@lux/design-system/icons";
import { api, errorText, INPUT_RUN_STATES, type Run } from "../../api/index.ts";
import { setSearchParams, useSearchParams } from "../router.tsx";
import { useRunOutput } from "./useRunOutput.ts";

/** all: every line; output: the workload's stdout and stderr; lux: Lux's own (system) lines. */
type OutputView = "all" | "output" | "lux";

/** The lines the view shows, and each view's count, in one pass. */
function filterOutput(lines: LogLine[], view: OutputView): { lines: LogLine[]; counts: Record<OutputView, number> } {
  const counts = { all: lines.length, output: 0, lux: 0 };
  const visibleLines: LogLine[] = [];
  for (const line of lines) {
    const category = line.stream === "system" ? "lux" : "output";
    counts[category]++;
    if (view === category) visibleLines.push(line);
  }
  return { lines: view === "all" ? lines : visibleLines, counts };
}

/** Output tab: streamed log, and an input box to steer a running agent. */
export function RunOutput({ run }: { run: Run }) {
  const out = useRunOutput(run.id, run.epoch);
  // The filter lives in the URL (?output=), so a filtered view is a link.
  const requested = useSearchParams().get("output");
  const view: OutputView = requested === "output" || requested === "lux" ? requested : "all";
  const setView = (v: OutputView) => setSearchParams({ output: v === "all" ? null : v });
  const shown = useMemo(() => filterOutput(out.lines, view), [out.lines, view]);
  let emptyText = "No output.";
  if (out.status === "connecting") emptyText = "Connecting…";
  else if (view === "lux") emptyText = "No lines from Lux.";
  const toast = useToast();
  const [text, setText] = useState("");
  const [sending, setSending] = useState(false);
  const [interrupting, setInterrupting] = useState(false);
  const canInput = INPUT_RUN_STATES.has(run.state);

  const interrupt = async () => {
    setInterrupting(true);
    try {
      await api.interruptRun(run.id);
      toast({ title: "Interrupt sent", tone: "success" });
    } catch (err) {
      toast({ title: "Interrupt failed", description: errorText(err), tone: "danger" });
    } finally {
      setInterrupting(false);
    }
  };

  const send = async (e: FormEvent) => {
    e.preventDefault();
    const t = text.trim();
    if (!t) return;
    setSending(true);
    try {
      await api.inputRun(run.id, t);
      setText("");
      toast({ title: "Input sent", description: t.length > 80 ? t.slice(0, 80) + "…" : t, tone: "success" });
    } catch (err) {
      toast({ title: "Input failed", description: errorText(err), tone: "danger" });
    } finally {
      setSending(false);
    }
  };

  return (
    <div className="stack stack-tight">
      <div className="row output-status">
        {out.status === "streaming" && <Badge tone="success">streaming</Badge>}
        {out.status === "connecting" && (
          <Badge tone="info">
            <Spinner size={10} /> connecting
          </Badge>
        )}
        {out.status === "ended" && <Badge>ended</Badge>}
        {out.status === "error" && <Badge tone="danger">disconnected</Badge>}
        {out.error && <span className="muted">{out.error}</span>}
        {out.cursor && <span className="muted mono">cursor {out.cursor}</span>}
      </div>
      <Tabs<OutputView>
        size="sm"
        value={view}
        onChange={setView}
        items={[
          { key: "all", label: "All", count: shown.counts.all },
          { key: "output", label: "Output", count: shown.counts.output },
          { key: "lux", label: "Lux", count: shown.counts.lux },
        ]}
      />
      <LogView lines={shown.lines} height="clamp(320px, calc(100vh - 470px), 720px)" lineNumbers emptyText={emptyText} />
      <form className="output-form" onSubmit={send}>
        <input className="input mono" placeholder={canInput ? "Send input to the agent…" : "Input needs a starting or running run"} value={text} onChange={(e) => setText(e.target.value)} disabled={!canInput || sending} />
        <Button type="submit" icon={<IconSend size={14} />} disabled={!canInput || text.trim() === ""} loading={sending}>
          Send
        </Button>
        <Button disabled={!canInput} loading={interrupting} onClick={() => void interrupt()} title="Interrupt the agent's current turn">
          Interrupt
        </Button>
      </form>
    </div>
  );
}

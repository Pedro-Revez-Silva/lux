import { useState, type FormEvent } from "react";
import { Badge, Button, LogView, Spinner, useToast } from "@lux/design-system";
import { IconSend } from "@lux/design-system/icons";
import { api, errorText, INPUT_RUN_STATES, type Run } from "../../api/index.ts";
import { useRunOutput } from "./useRunOutput.ts";

/** Output tab: streamed log, and an input box to steer a running agent. */
export function RunOutput({ run }: { run: Run }) {
  const out = useRunOutput(run.id, run.epoch);
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
      <LogView lines={out.lines} height="clamp(320px, calc(100vh - 470px), 720px)" lineNumbers emptyText={out.status === "connecting" ? "Connecting…" : "No output."} />
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

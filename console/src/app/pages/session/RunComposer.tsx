import { SessionComposer, useToast } from "@lux/design-system";
import { api, errorText, INPUT_RUN_STATES, type Run } from "../../../api/index.ts";

/** Why a Run cannot take input now, for the composer's placeholder and hint. */
function closedReason(run: Run): { placeholder: string; hint: string } {
  if (run.spec.workload.steerable === false) return { placeholder: "This Run takes no messages", hint: "Not steerable · its spec sets workload.steerable false" };
  if (run.resumable) return { placeholder: "Resume this Run to send a message", hint: `Run ${run.state} · Resume is in the page actions` };
  return { placeholder: "Input needs a starting or running Run", hint: `Run ${run.state} · it takes no input` };
}

/** Where steered input lands, in the Run's own words when the adapter reports it. */
function landsHint(run: Run, stdin: boolean): string {
  if (stdin) return "Written to stdin with a newline · read when the workload next reads";
  if (run.steer?.lands === "next_turn") return "Root session · read when the agent's current turn ends";
  return "Root session · read at the agent's next step";
}

/**
 * The Session tab's input: always to the Run (its root session) through
 * POST /v1/runs/{id}/input, whichever subagent is open. `stdin` labels it for
 * a generic workload, whose input is a line on its stdin.
 */
export function RunComposer({ run, stdin = false }: { run: Run; stdin?: boolean }) {
  const toast = useToast();
  const open = INPUT_RUN_STATES.has(run.state) && run.spec.workload.steerable !== false;
  const closed = open ? undefined : closedReason(run);
  const interrupt = async () => {
    try {
      await api.interruptRun(run.id);
      toast({ title: "Interrupt sent", tone: "success" });
    } catch (err) {
      toast({ title: "Interrupt failed", description: errorText(err), tone: "danger" });
    }
  };
  return (
    <SessionComposer
      label={stdin ? "Send to stdin" : "Message the agent"}
      placeholder={closed?.placeholder ?? (stdin ? "A line for the workload's stdin…" : "Send a message to this Run…")}
      hint={closed?.hint ?? landsHint(run, stdin)}
      disabled={!open}
      onSend={(text) => api.inputRun(run.id, text)}
      onInterrupt={interrupt}
      interruptTitle={stdin ? "Send the workload SIGINT" : undefined}
      errorText={errorText}
    />
  );
}

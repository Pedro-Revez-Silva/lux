import { afterAll, beforeAll, expect, test } from "bun:test";
import { GlobalRegistrator } from "@happy-dom/global-registrator";
import { act } from "react";
import { createRoot } from "react-dom/client";
import { useRunOutput, type OutputState } from "./useRunOutput.ts";

beforeAll(() => {
  GlobalRegistrator.register({ url: "http://localhost" });
  (globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;
});
afterAll(() => GlobalRegistrator.unregister());

const record = (ch: string, data: string, t = 1, cursor = "c1") => ["record", { ch, data, t, cursor }] as const;
type Message = readonly [string, unknown];
const frame = (messages: Message[]) => messages.map(([event, data]) => `event: ${event}\ndata: ${JSON.stringify(data)}\n\n`).join("");

async function harness(run: (h: { send: (messages: Message[]) => Promise<void>; state: () => OutputState; switchRun: () => Promise<void>; disconnect: () => Promise<void>; urls: string[] }) => Promise<void>) {
  const original = globalThis.fetch;
  const urls: string[] = [];
  let controller: ReadableStreamDefaultController<Uint8Array>;
  globalThis.fetch = (async (url) => {
    urls.push(String(url));
    return new Response(new ReadableStream<Uint8Array>({ start(c) { controller = c; } }), { headers: { "Content-Type": "text/event-stream" } });
  }) as typeof fetch;
  let state!: OutputState;
  function Probe({ id }: { id: string }) {
    state = useRunOutput(id, 1);
    return null;
  }
  const root = createRoot(document.createElement("div"));
  try {
    await act(async () => root.render(<Probe id="a" />));
    await run({
      send: async (messages) => { await act(async () => { controller.enqueue(new TextEncoder().encode(frame(messages))); await Bun.sleep(70); }); },
      state: () => state,
      switchRun: async () => { await act(async () => root.render(<Probe id="b" />)); },
      disconnect: async () => { await act(async () => { controller.close(); await Bun.sleep(1100); }); },
      urls,
    });
  } finally {
    await act(async () => root.unmount());
    controller!.close();
    globalThis.fetch = original;
  }
}

test("gap flushes pre-gap partials and resets string, CSI and style state", async () => {
  await harness(async ({ send, state }) => {
    await send([record("stdout", "\x1b]0;unfinished-title"), record("stderr", "\x1b[31mbefore\x1b[3"), ["gap", { reason: "lost-host" }], record("stdout", "new workload output\n", 2), record("stderr", "plain stderr\n", 2), ["end", {}]]);
    expect(state().lines.filter((l) => l.stream === "stdout").map((l) => l.text)).toEqual(["", "new workload output"]);
    const err = state().lines.filter((l) => l.stream === "stderr");
    expect(err.map((l) => l.text)).toEqual(["before", "plain stderr"]);
    expect(err[1]!.spans).toBeUndefined();
    expect(state().status).toBe("ended");
  });
});

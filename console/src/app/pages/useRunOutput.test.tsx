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

test("tied timestamp partials flush in arrival order, in either channel order", async () => {
  for (const order of [["stderr", "stdout"], ["stdout", "stderr"]] as const) {
    await harness(async ({ send, state }) => {
      await send([...order.map((ch) => record(ch, ch, 10)), ["end", {}]]);
      expect(state().lines.map((l) => l.stream)).toEqual([...order]);
      expect(state().lines.map((l) => l.text)).toEqual([...order]);
    });
  }
});

test("a completed partial rejoins arrival order behind outstanding channels", async () => {
  await harness(async ({ send, state }) => {
    await send([record("stdout", "one", 10), record("stderr", "err", 10), record("stdout", "\ntwo", 10), ["end", {}]]);
    expect(state().lines.map((l) => l.text)).toEqual(["one", "err", "two"]);
  });
});

test("lifecycle flush preserves fragmented ESC for SGR and OSC", async () => {
  await harness(async ({ send, state }) => {
    await send([record("stdout", "before\x1b"), record("stderr", "err\x1b"), ["lux", { id: 1, type: "activity", time: new Date(1).toISOString(), data: {} }], record("stdout", "[31mred\x1b[0m\n", 2), record("stderr", "]0;title\x07visible\n", 2), ["end", {}]]);
    const out = state().lines.filter((l) => l.stream === "stdout");
    expect(out.map((l) => l.text)).toEqual(["before", "red"]);
    expect(out[1]!.spans).toEqual([{ text: "red", fg: 1 }]);
    expect(state().lines.filter((l) => l.stream === "stderr").map((l) => l.text)).toEqual(["err", "visible"]);
  });
});

test("transport reconnect preserves fragmented CSI, timestamp and cursor", async () => {
  await harness(async ({ send, state, disconnect, urls }) => {
    await send([record("stdout", "\x1b[3", 11, "resume-here")]);
    await disconnect();
    expect(urls[1]).toContain("since=resume-here");
    await send([record("stdout", "1mred\x1b[0m\n", 22, "next"), ["end", {}]]);
    expect(state().lines).toEqual([{ ts: 11, stream: "stdout", text: "red", spans: [{ text: "red", fg: 1 }] }]);
    expect(state().cursor).toBe("next");
  });
});

test("switching run ID clears decoder style and cursor", async () => {
  await harness(async ({ send, state, switchRun, urls }) => {
    await send([record("stdout", "\x1b[31mold\n", 1, "old-cursor")]);
    await switchRun();
    await send([record("stdout", "new\n", 2, "new-cursor"), ["end", {}]]);
    expect(urls[1]).not.toContain("since=");
    expect(state().lines).toEqual([{ ts: 2, stream: "stdout", text: "new" }]);
  });
});

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

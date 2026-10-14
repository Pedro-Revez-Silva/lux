import { afterAll, beforeAll, expect, test } from "bun:test";
import { GlobalRegistrator } from "@happy-dom/global-registrator";
import { act } from "react";
import { createRoot } from "react-dom/client";
import type { OutputRecord } from "../../../api/index.ts";
import { until } from "../../testing.ts";
import type { MessageItem } from "./types.ts";
import { useRunSession, type SessionState } from "./useRunSession.ts";

beforeAll(() => {
  GlobalRegistrator.register({ url: "http://localhost" });
  (globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;
});
afterAll(() => GlobalRegistrator.unregister());

const chunk = (seq: number, text: string, epoch = 1): OutputRecord => ({ cursor: `${epoch}.${seq}`, epoch, seq, t: seq, ch: "event", event: { type: "acp.agent_message_chunk", data: { messageId: `m${epoch}.${seq}`, content: { text } } } });
const frame = (messages: [string, unknown][]) => messages.map(([event, data]) => `event: ${event}\ndata: ${JSON.stringify(data)}\n\n`).join("");

async function harness(run: (h: { send: (messages: [string, unknown][]) => Promise<void>; disconnect: () => Promise<void>; render: (id: string, restart: number) => Promise<void>; state: () => SessionState; urls: string[] }) => Promise<void>) {
  const original = globalThis.fetch;
  const urls: string[] = [];
  let controller!: ReadableStreamDefaultController<Uint8Array>;
  globalThis.fetch = (async (url) => {
    urls.push(String(url));
    return new Response(new ReadableStream<Uint8Array>({ start(c) { controller = c; } }), { headers: { "Content-Type": "text/event-stream" } });
  }) as typeof fetch;
  let state!: SessionState;
  function Probe({ id, restart }: { id: string; restart: number }) {
    state = useRunSession(id, "acp", restart);
    return null;
  }
  const root = createRoot(document.createElement("div"));
  const render = async (id: string, restart: number) => act(async () => root.render(<Probe id={id} restart={restart} />));
  try {
    await render("a", 1);
    await run({
      send: async (messages) => {
        await act(async () => controller.enqueue(new TextEncoder().encode(frame(messages))));
        await until(() => messages.every(([event, data]) => event !== "record" || messageSeen(state, data as OutputRecord)), "records folded");
      },
      disconnect: async () => {
        const count = urls.length;
        await act(async () => controller.close());
        await until(() => urls.length > count, "reconnect", 2500);
      },
      render,
      state: () => state,
      urls,
    });
  } finally {
    await act(async () => root.unmount());
    globalThis.fetch = original;
  }
}

const messageSeen = (state: SessionState, r: OutputRecord) => {
  const id = (r.event as { data: { messageId?: string } }).data.messageId;
  return !id || state.session.threads.root!.items.some((i) => i.id === `message:${id}`);
};
const texts = (state: SessionState) => state.session.threads.root!.items.filter((i): i is MessageItem => i.kind === "message").map((i) => i.text);

test("transport reconnect resumes after record and lux event cursors without losing items", async () => {
  await harness(async ({ send, disconnect, state, urls }) => {
    await send([["record", chunk(4, "before")], ["lux", { id: 9, type: "state", time: new Date(5).toISOString(), data: {} }]]);
    await disconnect();
    expect(urls[1]).toContain("since=1.4");
    expect(urls[1]).toContain("afterEvent=9");
    await send([["record", chunk(5, "after")]]);
    expect(texts(state())).toEqual(["before", "after"]);
  });
});

test("an epoch restart reopens from the cursor and keeps the session", async () => {
  await harness(async ({ send, render, state, urls }) => {
    await send([["record", chunk(2, "first")]]);
    await render("a", 2);
    await until(() => urls.length === 2, "restart");
    expect(urls[1]).toContain("since=1.2");
    await send([["record", { cursor: "2.0", epoch: 2, seq: 0, t: 0, ch: "event", event: { type: "lux.session", data: { sessionId: "s" } } }], ["record", chunk(1, "second", 2)]]);
    expect(texts(state())).toEqual(["first", "second"]);
  });
});

test("changing Run starts a fresh session and stream", async () => {
  await harness(async ({ send, render, state, urls }) => {
    await send([["record", chunk(2, "old")]]);
    await render("b", 1);
    await until(() => urls.length === 2, "new run stream");
    expect(urls[1]).toContain("/runs/b/output");
    expect(urls[1]).not.toContain("since=");
    expect(texts(state())).toEqual([]);
    await send([["record", chunk(1, "new")]]);
    expect(texts(state())).toEqual(["new"]);
  });
});

import { afterAll, beforeAll, expect, test } from "bun:test";
import { GlobalRegistrator } from "@happy-dom/global-registrator";
import { act } from "react";
import { createRoot } from "react-dom/client";
import type { OutputRecord, Run } from "../../../api/index.ts";
import { fixture } from "./fixtures.ts";

// RunSession's router import touches window at load: register the DOM first.
let RunSession: typeof import("./RunSession.tsx").RunSession;
beforeAll(async () => {
  GlobalRegistrator.register({ url: "http://localhost/runs/r1", width: 1600 });
  ({ RunSession } = await import("./RunSession.tsx"));
  (globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;
});
afterAll(() => GlobalRegistrator.unregister());

const run = (adapter: string, state = "succeeded"): Run => ({ id: "r1", tenant: "t", labels: {}, state, epoch: 1, spec: { workload: { adapter } }, secrets: [], createdAt: new Date().toISOString() }) as unknown as Run;
const frame = (records: OutputRecord[], end = false) => records.map((r) => `event: record\ndata: ${JSON.stringify(r)}\n\n`).join("") + (end ? "event: end\ndata: {}\n\n" : "");

async function waitFor(check: () => boolean, timeout = 2000) {
  const deadline = Date.now() + timeout;
  while (!check()) {
    if (Date.now() > deadline) throw new Error("condition not reached");
    await act(async () => Bun.sleep(10));
  }
}

// Serves records as the output stream the tab reads; send() appends more.
async function render(adapter: string, records: OutputRecord[], end = true, state?: string) {
  const original = globalThis.fetch;
  const urls: string[] = [];
  let controller!: ReadableStreamDefaultController<Uint8Array>;
  globalThis.fetch = (async (url) => {
    urls.push(String(url));
    return new Response(new ReadableStream<Uint8Array>({ start(c) { controller = c; c.enqueue(new TextEncoder().encode(frame(records, end))); } }), { headers: { "Content-Type": "text/event-stream" } });
  }) as typeof fetch;
  const el = document.createElement("div");
  document.body.append(el);
  const root = createRoot(el);
  await act(async () => root.render(<RunSession run={run(adapter, state)} />));
  return {
    el,
    urls,
    send: async (more: OutputRecord[]) => act(async () => controller.enqueue(new TextEncoder().encode(frame(more)))),
    done: async () => {
      await act(async () => root.unmount());
      el.remove();
      globalThis.fetch = original;
    },
  };
}

test("the tab reads the Run's output stream and shows its subagent in the transcript and the rail", async () => {
  const v = await render("claude-code", fixture("claude-2.1.207-agent-subagent.jsonl"));
  try {
    await waitFor(() => (v.el.textContent ?? "").includes("The first line of /etc/hosts is ## (a comment line with two hash marks)."));
    expect(v.urls[0]).toContain("/runs/r1/output");
    expect(v.urls[0]).toContain("events=true");
    const text = v.el.textContent ?? "";
    expect(text).toContain("Read-only · input and interrupt are on Output");
    expect(v.el.querySelector('[role="tree"]')?.textContent).toContain("Read /etc/hosts first line");
    const stub = v.el.querySelector<HTMLElement>('[aria-label="Open subagent Read /etc/hosts first line"]')!;
    await act(async () => stub.click());
    expect(v.el.querySelector('[aria-label="Open subagent Read /etc/hosts first line"]')?.className).toContain("is-selected");
    const panel = v.el.querySelector(".subpanel")!;
    expect(panel.querySelector(".subpanel-title")?.textContent).toBe("Read /etc/hosts first line");
    expect(panel.querySelector(".subpanel-brief")?.textContent).toBe("Read the file /etc/hosts and report only its first line. Do not read anything else.");
    expect(panel.querySelector(".toolcall-name")?.textContent).toBe("Read");
  } finally {
    await v.done();
  }
});

test("an acp Run's rail says its adapter doesn't report subagents", async () => {
  const v = await render("acp", fixture("acp-opencode-1.18.21-todo-task.jsonl"));
  try {
    await waitFor(() => (v.el.textContent ?? "").includes("The acp adapter doesn't report subagents: ACP shows only the Run's own session."));
    expect(v.el.querySelector('[role="tree"]')).toBeNull();
  } finally {
    await v.done();
  }
});

test("finished rows keep their rendered props while live rows and child stubs update", async () => {
  let seq = 0;
  const ev = (type: string, data: Record<string, unknown>): OutputRecord => ({ cursor: `1.${++seq}`, epoch: 1, seq, t: Date.now(), ch: "event", event: { type, data } });
  const v = await render("claude-code", [
    ev("claude.assistant", { message: { content: [{ type: "text", text: "Finished **reply**" }] }, parent_tool_use_id: null }),
    ev("claude.assistant", { message: { content: [{ type: "tool_use", id: "task", name: "Agent", input: { description: "Child", prompt: "work" } }] }, parent_tool_use_id: null }),
  ], false, "running");
  try {
    await waitFor(() => v.el.querySelector(".turn-msg") != null);
    const message = v.el.querySelector(".turn-msg")!;
    // React DOM's internal props key: its identity is unchanged only if the row did not re-render.
    const propsKey = Object.keys(message).find((k) => k.startsWith("__reactProps$"))!;
    const props = (message as unknown as Record<string, unknown>)[propsKey];
    const duration = () => v.el.querySelector(".toolcall-dur")?.textContent;
    const before = duration();
    await v.send([ev("claude.assistant", { message: { content: [{ type: "text", text: "child update" }] }, parent_tool_use_id: "task" })]);
    await waitFor(() => (v.el.querySelector(".subthread-last")?.textContent ?? "") === "child update");
    await waitFor(() => duration() !== before, 2500);
    expect((message as unknown as Record<string, unknown>)[propsKey]).toBe(props);
  } finally {
    await v.done();
  }
});

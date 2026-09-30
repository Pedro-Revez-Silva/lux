import { afterAll, beforeAll, expect, test } from "bun:test";
import { GlobalRegistrator } from "@happy-dom/global-registrator";
import { act } from "react";
import { createRoot } from "react-dom/client";
import type { Page } from "../api/index.ts";
import { pageRequest, type Paged, type PagedOptions, type PagedRequest } from "./paged.ts";

let usePaged: typeof import("./paged.ts").usePaged;
let invalidate: typeof import("../api/index.ts").invalidate;
beforeAll(async () => {
  GlobalRegistrator.register();
  ({ usePaged } = await import("./paged.ts"));
  ({ invalidate } = await import("../api/index.ts"));
  (globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;
});
afterAll(async () => {
  await GlobalRegistrator.unregister();
});

test("pageRequest: a refresh past page 1 re-reads the page on screen from its own cursor; page 1 from the top; a counted page at its offset", () => {
  const sort = { key: "created", dir: "desc" as const };
  expect(pageRequest(sort, 50, { kind: "first" }, undefined, 1)).toEqual({ sort: "created", dir: "desc", limit: 50 });
  expect(pageRequest(sort, 50, { kind: "next", cursor: "c2" }, undefined, 2)).toEqual({ sort: "created", dir: "desc", limit: 50, next: "c2" });
  expect(pageRequest(sort, 50, { kind: "next", cursor: "c2" }, "self2", 2)).toEqual({ sort: "created", dir: "desc", limit: 50, at: "self2" });
  expect(pageRequest(sort, 50, { kind: "first" }, "self1", 1)).toEqual({ sort: "created", dir: "desc", limit: 50 });
  expect(pageRequest(sort, 25, { kind: "offset", offset: 75 }, undefined, 4)).toEqual({ sort: "created", dir: "desc", limit: 25, offset: 75 });
  expect(pageRequest(sort, 25, { kind: "offset", offset: 75 }, "self4", 4)).toEqual({ sort: "created", dir: "desc", limit: 25, offset: 75 });
});

interface Call {
  req: PagedRequest;
  signal: AbortSignal;
  resolve: (p: Page<string>) => void;
}

/** Mounts usePaged with a fetch whose answers the test hands out. */
async function mount(view: () => string, opts: Partial<PagedOptions> & { prefix?: string } = {}) {
  const calls: Call[] = [];
  let state!: Paged<string>;
  const fetch = (req: PagedRequest, signal: AbortSignal) => new Promise<Page<string>>((resolve) => calls.push({ req, signal, resolve }));
  function Probe() {
    state = usePaged(opts.prefix ?? "test", view(), fetch, { defaultSort: { key: "created", dir: "desc" }, defaultSize: 2, interval: 0, ...opts });
    return <div>{state.rows.join(",")}</div>;
  }
  const el = document.createElement("div");
  document.body.appendChild(el);
  const root = createRoot(el);
  await act(async () => root.render(<Probe />));
  return {
    calls,
    get: () => state,
    last: () => calls.at(-1)!,
    text: () => el.textContent,
    rerender: () => act(async () => root.render(<Probe />)),
    unmount: async () => {
      await act(async () => root.unmount());
      el.remove();
    },
  };
}

type Mounted = Awaited<ReturnType<typeof mount>>;

/** Page 1 (a,b), then Next to page 2 (c,d). */
async function toPage2(m: Mounted) {
  await act(async () => m.last().resolve({ rows: ["a", "b"], next: "n1", page: "p1" }));
  await act(async () => m.get().next());
  expect(m.last().req.next).toBe("n1");
  await act(async () => m.last().resolve({ rows: ["c", "d"], next: "n2", prev: "q2", page: "p2" }));
  expect([m.text(), m.get().page]).toEqual(["c,d", 2]);
}

const cursorOf = (r: PagedRequest) => [r.next, r.prev, r.at, r.offset];
const NONE = [undefined, undefined, undefined, undefined];

test("next, then a refresh stays on the page; a sort change goes back to page 1", async () => {
  const m = await mount(() => "v1");
  try {
    await toPage2(m);
    await act(async () => void m.get().refetch());
    expect(m.last().req.at).toBe("p2");
    expect(m.last().req.next).toBeUndefined();
    await act(async () => m.get().setSort({ key: "cost", dir: "desc" }));
    const last = m.last().req;
    expect([last.sort, last.next, last.at, m.get().page]).toEqual(["cost", undefined, undefined, 1]);
  } finally {
    await m.unmount();
  }
});

test("a filter change aborts the old request and drops its answer", async () => {
  let view = "tenant-a";
  const m = await mount(() => view);
  try {
    const first = m.calls[0]!;
    view = "tenant-b";
    await m.rerender();
    expect(first.signal.aborted).toBe(true);
    const second = m.last();
    // The old answer lands late: ignored.
    await act(async () => first.resolve({ rows: ["stale"], page: "x" }));
    expect(m.text()).toBe("");
    await act(async () => second.resolve({ rows: ["fresh"], page: "y" }));
    expect(m.text()).toBe("fresh");
    expect(m.get().page).toBe(1);
  } finally {
    await m.unmount();
  }
});

test("while another view's answer is pending, its rows stay on screen but its cursors are not offered", async () => {
  let view = "tenant-a";
  const m = await mount(() => view);
  try {
    await act(async () => m.last().resolve({ rows: ["a", "b"], next: "n1", page: "p1" }));
    expect(m.get().hasNext).toBe(true);
    view = "tenant-b";
    await m.rerender();
    expect(m.text()).toBe("a,b");
    expect([m.get().hasNext, m.get().loading]).toEqual([false, true]);
    // Next does nothing: tenant-a's cursor would read tenant-a's page 2.
    const n = m.calls.length;
    await act(async () => m.get().next());
    expect(m.calls.length).toBe(n);
  } finally {
    await m.unmount();
  }
});

test("a view change starts over, and so does going back to an earlier view (A → B → A)", async () => {
  let view = "A";
  const m = await mount(() => view);
  try {
    await toPage2(m);
    view = "B";
    await m.rerender();
    expect(cursorOf(m.last().req)).toEqual(NONE);
    expect(m.get().page).toBe(1);
    await act(async () => m.last().resolve({ rows: ["x"], page: "pb" }));
    view = "A";
    await m.rerender();
    expect(cursorOf(m.last().req)).toEqual(NONE);
    expect(m.get().page).toBe(1);
    await act(async () => m.last().resolve({ rows: ["a", "b"], next: "n1", page: "p1" }));
    // A refresh reads the top, not A's old page 2.
    await act(async () => void m.get().refetch());
    expect(cursorOf(m.last().req)).toEqual(NONE);
  } finally {
    await m.unmount();
  }
});

test("a page size change goes back to page 1 at the new size", async () => {
  const m = await mount(() => "v");
  try {
    await toPage2(m);
    await act(async () => m.get().setSize(100));
    expect(m.last().req.limit).toBe(100);
    expect(cursorOf(m.last().req)).toEqual(NONE);
    expect(m.get().page).toBe(1);
  } finally {
    await m.unmount();
  }
});

test("Previous to page 1 reads the top; from page 3 it reads before the page's cursor", async () => {
  const m = await mount(() => "v");
  try {
    await toPage2(m);
    await act(async () => m.get().next());
    await act(async () => m.last().resolve({ rows: ["e", "f"], next: "n3", prev: "q3", page: "p3" }));
    expect(m.get().page).toBe(3);
    await act(async () => m.get().prev());
    expect(m.last().req.prev).toBe("q3");
    await act(async () => m.last().resolve({ rows: ["c", "d"], next: "n2", prev: "q2", page: "p2" }));
    expect(m.get().page).toBe(2);
    await act(async () => m.get().prev());
    expect(cursorOf(m.last().req)).toEqual(NONE);
    expect(m.get().page).toBe(1);
  } finally {
    await m.unmount();
  }
});

test("counted: a numbered page reads its offset, keeps its number, and refreshes at that offset", async () => {
  const m = await mount(() => "v");
  try {
    await act(async () => m.last().resolve({ rows: ["a", "b"], total: 10, offset: 0, next: "n1", page: "p1" }));
    expect([m.get().hasPrev, m.get().total]).toEqual([false, 10]);
    await act(async () => m.get().goto(3));
    expect(m.last().req.offset).toBe(4);
    expect(m.last().req.at).toBeUndefined();
    // Rows inserted ahead do not move the page number.
    await act(async () => m.last().resolve({ rows: ["e", "f"], total: 12, offset: 4, next: "n3", page: "p3" }));
    expect([m.get().page, m.get().hasPrev]).toEqual([3, true]);
    await act(async () => void m.get().refetch());
    expect([m.last().req.offset, m.last().req.at]).toEqual([4, undefined]);
    await act(async () => m.get().goto(1));
    expect(cursorOf(m.last().req)).toEqual(NONE);
  } finally {
    await m.unmount();
  }
});

test("counted: a numbered page that comes back empty (the list shrank) starts over at the top", async () => {
  const m = await mount(() => "v");
  try {
    await act(async () => m.last().resolve({ rows: ["a", "b"], total: 10, offset: 0, page: "p1" }));
    await act(async () => m.get().goto(5));
    expect(m.last().req.offset).toBe(8);
    await act(async () => m.last().resolve({ rows: [], total: 3, offset: 8 }));
    expect(cursorOf(m.last().req)).toEqual(NONE);
    expect(m.get().page).toBe(1);
  } finally {
    await m.unmount();
  }
});

test("a page past 1 that comes back empty, or without a previous page, starts over at the top", async () => {
  const m = await mount(() => "v");
  try {
    await toPage2(m);
    await act(async () => void m.get().refetch());
    expect(m.last().req.at).toBe("p2");
    await act(async () => m.last().resolve({ rows: [] }));
    expect(cursorOf(m.last().req)).toEqual(NONE);
    expect(m.get().page).toBe(1);
    await toPage2(m);
    await act(async () => void m.get().refetch());
    await act(async () => m.last().resolve({ rows: ["c", "d"], next: "n2", page: "p2" }));
    expect(cursorOf(m.last().req)).toEqual(NONE);
    expect(m.get().page).toBe(1);
  } finally {
    await m.unmount();
  }
});

test("invalidating the key prefix refetches a mounted list", async () => {
  const m = await mount(() => "v", { prefix: "runs" });
  try {
    await act(async () => m.last().resolve({ rows: ["a"], page: "p1" }));
    const n = m.calls.length;
    invalidate("other:");
    await act(() => new Promise((r) => setTimeout(r, 400)));
    expect(m.calls.length).toBe(n);
    invalidate((k) => k.startsWith("runs:"));
    await act(() => new Promise((r) => setTimeout(r, 400)));
    expect(m.calls.length).toBe(n + 1);
  } finally {
    await m.unmount();
  }
});

const sleep = (ms: number) => act(() => new Promise<void>((r) => setTimeout(r, ms)));

/** Polls of a list whose fetch answers at once, over `ms`, with the event stream live or not. */
async function pollsOver(ms: number, streaming: boolean, live: number | undefined): Promise<number> {
  const { fakeApi } = await import("./testing.ts");
  const { useLiveState, useLiveStream } = await import("../api/index.ts");
  const fake = fakeApi(() => ({}));
  let calls = 0;
  let status = "";
  function Stream() {
    useLiveStream(undefined);
    const st = useLiveState();
    status = st.status + (st.error ? `: ${st.error}` : "");
    return null;
  }
  function List() {
    usePaged("polls", "v", async () => (calls++, { rows: [] }), { defaultSort: { key: "created", dir: "desc" }, defaultSize: 2, interval: 20, live });
    return null;
  }
  const root = createRoot(document.createElement("div"));
  try {
    if (streaming) {
      await act(async () => root.render(<Stream />));
      for (let i = 0; i < 50 && status !== "live"; i++) await sleep(10);
      expect(status).toBe("live");
    }
    await act(async () => root.render(streaming ? <><Stream /><List /></> : <List />));
    await sleep(ms);
    return calls;
  } finally {
    await act(async () => root.unmount());
    fake.restore();
  }
}

test("live reaches useQuery: while the event stream is live the list polls at live, not interval", async () => {
  expect(await pollsOver(200, true, 10_000)).toBe(1);
});

test("without live, a live stream does not slow the poll", async () => {
  expect(await pollsOver(200, true, undefined)).toBeGreaterThan(2);
});

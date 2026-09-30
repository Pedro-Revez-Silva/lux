import { afterAll, beforeAll, expect, test } from "bun:test";
import { GlobalRegistrator } from "@happy-dom/global-registrator";
import { act } from "react";
import { createRoot } from "react-dom/client";
import type { Page } from "../api/index.ts";
import { pageRequest, type Paged, type PagedRequest } from "./paged.ts";

let usePaged: typeof import("./paged.ts").usePaged;
beforeAll(async () => {
  GlobalRegistrator.register();
  ({ usePaged } = await import("./paged.ts"));
  (globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;
});
afterAll(async () => {
  await GlobalRegistrator.unregister();
});

test("pageRequest: a refresh past page 1 re-reads the page on screen from its own cursor; page 1 from the top", () => {
  const sort = { key: "created", dir: "desc" as const };
  expect(pageRequest(sort, 50, { kind: "first" }, undefined, 1)).toEqual({ sort: "created", dir: "desc", limit: 50 });
  expect(pageRequest(sort, 50, { kind: "next", cursor: "c2" }, undefined, 2)).toEqual({ sort: "created", dir: "desc", limit: 50, next: "c2" });
  expect(pageRequest(sort, 50, { kind: "next", cursor: "c2" }, "self2", 2)).toEqual({ sort: "created", dir: "desc", limit: 50, at: "self2" });
  expect(pageRequest(sort, 50, { kind: "first" }, "self1", 1)).toEqual({ sort: "created", dir: "desc", limit: 50 });
  expect(pageRequest(sort, 25, { kind: "offset", offset: 75 }, undefined, 4)).toEqual({ sort: "created", dir: "desc", limit: 25, offset: 75 });
});

interface Call {
  req: PagedRequest;
  signal: AbortSignal;
  resolve: (p: Page<string>) => void;
}

/** Mounts usePaged with a fetch whose answers the test hands out. */
async function mount(view: () => string) {
  const calls: Call[] = [];
  let state!: Paged<string>;
  const fetch = (req: PagedRequest, signal: AbortSignal) => new Promise<Page<string>>((resolve) => calls.push({ req, signal, resolve }));
  function Probe() {
    state = usePaged(view(), fetch, { defaultSort: { key: "created", dir: "desc" }, defaultSize: 2, interval: 0 });
    return <div>{state.rows.join(",")}</div>;
  }
  const el = document.createElement("div");
  document.body.appendChild(el);
  const root = createRoot(el);
  await act(async () => root.render(<Probe />));
  return {
    calls,
    get: () => state,
    text: () => el.textContent,
    rerender: () => act(async () => root.render(<Probe />)),
    unmount: async () => {
      await act(async () => root.unmount());
      el.remove();
    },
  };
}

test("next, then a refresh stays on the page; a sort change goes back to page 1", async () => {
  const m = await mount(() => "v1");
  try {
    await act(async () => m.calls[0]!.resolve({ rows: ["a", "b"], next: "n1", page: "p1" }));
    expect(m.text()).toBe("a,b");
    await act(async () => m.get().next());
    expect(m.calls.at(-1)!.req.next).toBe("n1");
    await act(async () => m.calls.at(-1)!.resolve({ rows: ["c", "d"], next: "n2", prev: "q2", page: "p2" }));
    expect(m.get().page).toBe(2);
    await act(async () => void m.get().refetch());
    expect(m.calls.at(-1)!.req.at).toBe("p2");
    expect(m.calls.at(-1)!.req.next).toBeUndefined();
    await act(async () => m.get().setSort({ key: "cost", dir: "desc" }));
    const last = m.calls.at(-1)!.req;
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
    const second = m.calls.at(-1)!;
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

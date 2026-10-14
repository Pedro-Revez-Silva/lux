// Test helpers: a stubbed fetch that answers by path, with a /v1/events
// stream that goes live at once and stays open; a bounded wait; typing.
import { act } from "react";

/**
 * Waits, letting React settle between checks, until done() is true; fails
 * naming what after timeoutMs.
 */
export async function until(done: () => boolean, what: string, timeoutMs = 1000): Promise<void> {
  const deadline = Date.now() + timeoutMs;
  while (!done()) {
    if (Date.now() > deadline) throw new Error(`timed out after ${timeoutMs}ms waiting for ${what}`);
    await act(() => new Promise<void>((r) => setTimeout(r, 5)));
  }
}

// A typed change as a browser delivers it: React tracks the value, so set it through the native setter.
export async function type(area: HTMLTextAreaElement, value: string): Promise<void> {
  await act(async () => {
    area.focus();
    area.dispatchEvent(new Event("focusin", { bubbles: true }));
    Object.getOwnPropertyDescriptor(Object.getPrototypeOf(area), "value")!.set!.call(area, value);
    area.dispatchEvent(new Event("input", { bubbles: true }));
    area.dispatchEvent(new KeyboardEvent("keyup", { key: "x", bubbles: true }));
  });
}

export interface FakeApi {
  /** The paths (with query) of every request, in order. */
  calls: string[];
  /** Every non-GET request: its path, method and parsed JSON body. */
  posts: { path: string; method: string; body: unknown }[];
  restore: () => void;
}

/**
 * Replace fetch: /v1/events streams, other paths answer answer(path, method,
 * body) as JSON. An answer may be a promise (a request that is still pending), and
 * a Response is returned as is (an error status).
 */
export function fakeApi(answer: (path: string, method: string, body: unknown) => unknown): FakeApi {
  const calls: string[] = [];
  const posts: FakeApi["posts"] = [];
  // API URLs are built on the page's origin (happy-dom starts at about:blank).
  (window as unknown as { happyDOM: { setURL: (u: string) => void } }).happyDOM.setURL("http://localhost/");
  const real = globalThis.fetch;
  globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
    const path = typeof input === "string" ? input : input instanceof URL ? input.pathname + input.search : input.url;
    const method = init?.method ?? "GET";
    const body = typeof init?.body === "string" ? JSON.parse(init.body) : undefined;
    calls.push(path);
    if (method !== "GET") posts.push({ path, method, body });
    if (path.startsWith("/v1/events")) {
      // A first message: the connection counts as open.
      const stream = new ReadableStream<Uint8Array>({ start: (c) => c.enqueue(new TextEncoder().encode("event: ping\ndata: x\n\n")) });
      return new Response(stream, { status: 200, headers: { "Content-Type": "text/event-stream" } });
    }
    const a = await answer(path, method, body);
    if (a instanceof Response) return a;
    return new Response(JSON.stringify(a), { status: 200, headers: { "Content-Type": "application/json" } });
  }) as typeof fetch;
  return {
    calls,
    posts,
    restore: () => {
      globalThis.fetch = real;
    },
  };
}

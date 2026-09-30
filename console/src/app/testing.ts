// Test helpers: a stubbed fetch that answers by path, with a /v1/events
// stream that goes live at once and stays open.
export interface FakeApi {
  /** The paths (with query) of every request, in order. */
  calls: string[];
  restore: () => void;
}

/** Replace fetch: /v1/events streams, other paths answer answer(path) as JSON. */
export function fakeApi(answer: (path: string) => unknown): FakeApi {
  const calls: string[] = [];
  // API URLs are built on the page's origin (happy-dom starts at about:blank).
  (window as unknown as { happyDOM: { setURL: (u: string) => void } }).happyDOM.setURL("http://localhost/");
  const real = globalThis.fetch;
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    const path = typeof input === "string" ? input : input instanceof URL ? input.pathname + input.search : input.url;
    calls.push(path);
    if (path.startsWith("/v1/events")) {
      // A first message: the connection counts as open.
      const body = new ReadableStream<Uint8Array>({ start: (c) => c.enqueue(new TextEncoder().encode("event: ping\ndata: x\n\n")) });
      return new Response(body, { status: 200, headers: { "Content-Type": "text/event-stream" } });
    }
    return new Response(JSON.stringify(answer(path)), { status: 200, headers: { "Content-Type": "application/json" } });
  }) as typeof fetch;
  return {
    calls,
    restore: () => {
      globalThis.fetch = real;
    },
  };
}

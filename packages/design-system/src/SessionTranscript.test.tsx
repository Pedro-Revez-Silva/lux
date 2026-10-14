import { afterAll, afterEach, beforeAll, describe, expect, test } from "bun:test";
import { GlobalRegistrator } from "@happy-dom/global-registrator";
import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { renderToStaticMarkup } from "react-dom/server";
import { SessionComposer } from "./SessionComposer.tsx";
import { SessionTranscript } from "./SessionTranscript.tsx";

test("SessionTranscript: the read-only note is the default, and a composer replaces it", () => {
  const plain = renderToStaticMarkup(<SessionTranscript itemCount={0} />);
  expect(plain).toContain("Read-only");
  const withComposer = renderToStaticMarkup(<SessionTranscript itemCount={0} composer={<SessionComposer label="Message the agent" onSend={async () => {}} />} />);
  expect(withComposer).not.toContain("Read-only");
  expect(withComposer).toContain('aria-label="Message the agent"');
});

const mounted: { el: HTMLElement; root: Root }[] = [];
async function render(node: React.ReactNode) {
  const el = document.createElement("div");
  document.body.appendChild(el);
  const root = createRoot(el);
  mounted.push({ el, root });
  await act(async () => root.render(node));
  return el;
}

describe("in a DOM", () => {
  beforeAll(() => {
    GlobalRegistrator.register();
    (globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;
  });
  afterEach(async () => {
    for (const { el, root } of mounted.splice(0)) {
      await act(async () => root.unmount());
      el.remove();
    }
  });
  afterAll(async () => {
    await GlobalRegistrator.unregister();
  });

  test("SessionTranscript: while following, a scroller resized by a growing composer stays pinned to the bottom; off, it is left; unmount disconnects", async () => {
    // happy-dom's ResizeObserver never fires: stand one in that the test triggers.
    const observers: { cb: () => void; targets: Element[]; live: boolean }[] = [];
    const Real = globalThis.ResizeObserver;
    globalThis.ResizeObserver = class {
      o: { cb: () => void; targets: Element[]; live: boolean };
      constructor(cb: () => void) {
        this.o = { cb, targets: [], live: true };
        observers.push(this.o);
      }
      observe(t: Element) {
        this.o.targets.push(t);
      }
      unobserve() {}
      disconnect() {
        this.o.live = false;
      }
    } as unknown as typeof ResizeObserver;
    try {
      const el = await render(<SessionTranscript itemCount={3} composer={<SessionComposer label="Message the agent" onSend={async () => {}} />} />);
      const scroller = el.querySelector<HTMLElement>(".transcript-scroll")!;
      Object.defineProperty(scroller, "scrollHeight", { configurable: true, value: 900 });
      const resize = () => act(async () => observers.filter((o) => o.live && o.targets.includes(scroller)).forEach((o) => o.cb()));
      scroller.scrollTop = 500;
      await resize();
      expect(scroller.scrollTop).toBe(900);

      const follow = el.querySelector<HTMLButtonElement>("button[aria-pressed]")!;
      await act(async () => follow.click());
      expect(follow.getAttribute("aria-pressed")).toBe("false");
      scroller.scrollTop = 400;
      await resize();
      expect(scroller.scrollTop).toBe(400);

      await act(async () => follow.click());
      expect(observers.filter((o) => o.live && o.targets.includes(scroller))).toHaveLength(1);
      const { root } = mounted.pop()!;
      await act(async () => root.unmount());
      expect(observers.filter((o) => o.live)).toEqual([]);
    } finally {
      globalThis.ResizeObserver = Real;
    }
  });
});

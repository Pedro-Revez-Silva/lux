import { afterAll, afterEach, beforeAll, describe, expect, test } from "bun:test";
import { GlobalRegistrator } from "@happy-dom/global-registrator";
import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { renderToStaticMarkup } from "react-dom/server";
import { EventTable, repeatNote, type LifecycleEventRow } from "./EventTable.tsx";

const at = (s: number) => new Date(Date.UTC(2026, 8, 29, 10, 0, s)).toISOString();

const events: LifecycleEventRow[] = [
  { id: 3, type: "pool.placement", time: at(30) },
  { id: 7, type: "pool.launch_failed", time: at(1), count: 4, lastTime: at(40) },
  { id: 5, type: "pool.scale_up", time: at(20) },
];

/** The table's body text, one entry per row. */
function rows(html: string): string[] {
  const body = html.slice(html.indexOf("<tbody>"));
  return [...body.matchAll(/<tr[^>]*>(.*?)<\/tr>/g)].map((m) => m[1]!.replace(/<[^>]+>/g, " ").replace(/\s+/g, " ").trim());
}

test("EventTable: newest first, by id, whatever order the events come in", () => {
  const html = renderToStaticMarkup(<EventTable events={events} summary={(e) => `says ${e.type}`} />);
  const types = rows(html).map((r) => /pool\.\w+/.exec(r)?.[0]);
  expect(types).toEqual(["pool.launch_failed", "pool.scale_up", "pool.placement"]);
});

test("EventTable: a repeated event says how often, and when it last happened", () => {
  const html = renderToStaticMarkup(<EventTable events={events} summary={() => "launch failed"} />);
  const failed = rows(html)[0]!;
  expect(failed).toContain("launch failed");
  expect(failed).toContain(`(${repeatNote(4, at(40))})`);
  expect(rows(html)[1]).not.toContain("×");
});

test("repeatNote: only for more than once", () => {
  expect(repeatNote(undefined, undefined)).toBe("");
  expect(repeatNote(1, at(0))).toBe("");
  expect(repeatNote(3, undefined)).toBe("×3");
  expect(repeatNote(2, at(5))).toMatch(/^×2 · last \d\d:\d\d:\d\d$/);
});

test("EventTable: no events reads the empty text", () => {
  expect(renderToStaticMarkup(<EventTable events={[]} summary={() => ""} empty="Nothing happened yet." />)).toContain("Nothing happened yet.");
});

/** Renders into a DOM whose containers are `width` px wide, and returns the rows' types, top to bottom, and the container. */
async function mount(events: LifecycleEventRow[], width: number) {
  Object.defineProperty(HTMLElement.prototype, "clientWidth", { configurable: true, get: () => width });
  const el = document.createElement("div");
  document.body.appendChild(el);
  const root = createRoot(el);
  mounted.push({ el, root });
  await act(async () => root.render(<EventTable events={events} summary={(e) => e.type} />));
  const types = () => [...el.querySelectorAll("tbody tr")].map((tr) => tr.querySelector("td .secondary")?.textContent);
  const headers = () => [...el.querySelectorAll("thead th")].map((th) => th.textContent);
  return { el, types, headers };
}

const mounted: { el: HTMLElement; root: Root }[] = [];

describe("EventTable in a DOM", () => {
  let clientWidth: PropertyDescriptor | undefined;
  beforeAll(() => {
    GlobalRegistrator.register();
    (globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;
    clientWidth = Object.getOwnPropertyDescriptor(HTMLElement.prototype, "clientWidth");
  });
  afterEach(async () => {
    for (const { el, root } of mounted.splice(0)) {
      await act(async () => root.unmount());
      el.remove();
    }
    if (clientWidth) Object.defineProperty(HTMLElement.prototype, "clientWidth", clientWidth);
    else delete (HTMLElement.prototype as { clientWidth?: number }).clientWidth;
  });
  afterAll(async () => {
    await GlobalRegistrator.unregister();
  });

  test("narrow, without its # column, newest first all the same", async () => {
    const { types, headers } = await mount(events, 700);
    expect(headers().some((h) => h?.startsWith("#"))).toBe(false);
    expect(types()).toEqual(["pool.launch_failed", "pool.scale_up", "pool.placement"]);
  });

  test("Time sorts by when each happened, not by id: newest first, then oldest", async () => {
    // Ids and times in different orders: 7 is newest by id, oldest by time.
    const { el, types } = await mount(events, 1400);
    const time = [...el.querySelectorAll("thead th")].find((th) => th.textContent?.startsWith("Time")) as HTMLElement;
    await act(async () => time.click());
    expect(time.getAttribute("aria-sort")).toBe("descending");
    expect(types()).toEqual(["pool.placement", "pool.scale_up", "pool.launch_failed"]);
    await act(async () => time.click());
    expect(time.getAttribute("aria-sort")).toBe("ascending");
    expect(types()).toEqual(["pool.launch_failed", "pool.scale_up", "pool.placement"]);
  });

  test("cleanup leaves no container and the real clientWidth", () => {
    expect(document.body.children.length).toBe(0);
    expect(Object.getOwnPropertyDescriptor(HTMLElement.prototype, "clientWidth")).toEqual(clientWidth);
  });
});

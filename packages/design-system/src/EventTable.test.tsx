import { expect, test } from "bun:test";
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

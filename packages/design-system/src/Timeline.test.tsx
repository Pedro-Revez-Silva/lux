import { afterAll, afterEach, beforeAll, expect, test } from "bun:test";
import { GlobalRegistrator } from "@happy-dom/global-registrator";
import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { formatClock } from "./format.ts";
import { Timeline } from "./Timeline.tsx";

const mounted: { el: HTMLElement; root: Root }[] = [];
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

async function render(node: React.ReactNode) {
  const el = document.createElement("div");
  document.body.appendChild(el);
  const root = createRoot(el);
  mounted.push({ el, root });
  await act(async () => root.render(node));
  return el;
}

const T0 = Date.UTC(2026, 8, 30, 22, 38, 8);

test("a point stage is a dot at its instant: no bar, no duration, and the axis ends there", async () => {
  const el = await render(
    <Timeline
      now={T0 + 3_600_000}
      stages={[
        { key: "requested", label: "Launch requested", start: T0, end: T0 + 1200, tone: "accent" },
        // end is ignored for a point.
        { key: "failed", label: "Launch failed", start: T0 + 1200, end: T0 + 60_000, tone: "red", point: true, note: "host row closed" },
      ]}
    />,
  );
  const rows = [...el.querySelectorAll(".timeline-row")];
  expect(rows[0]!.querySelector(".timeline-bar")).not.toBeNull();
  expect(rows[0]!.querySelector(".timeline-dur")?.textContent).toBe("1.2s");
  const failed = rows[1]!;
  expect(failed.querySelector(".timeline-bar")).toBeNull();
  const dot = failed.querySelector(".timeline-point") as HTMLElement;
  expect(dot.className).toContain("tone-red");
  expect(dot.style.left).toBe("100.000%");
  // The clock time where a bar has its duration.
  expect(failed.querySelector(".timeline-dur")?.textContent).toBe(formatClock(T0 + 1200));
  expect(failed.querySelector(".timeline-label")?.textContent).toBe("Launch failed · host row closed");
  expect(failed.querySelector(".timeline-label")?.getAttribute("title")).toBe("Launch failed · host row closed");
  // The axis spans the 1.2s of the launch, not the point's end nor now.
  expect(el.querySelector(".timeline-axis-label")?.textContent).toContain("→ 1.2s");
});

test("a point that has not happened reads not yet, like any stage", async () => {
  const el = await render(<Timeline now={T0 + 5000} stages={[{ key: "a", label: "A", start: T0, end: T0 + 2000 }, { key: "b", label: "B", point: true }]} />);
  const b = el.querySelectorAll(".timeline-row")[1]!;
  expect(b.className).toContain("is-pending");
  expect(b.querySelector(".timeline-point")).toBeNull();
  expect(b.querySelector(".timeline-dur")?.textContent).toBe("not yet");
});

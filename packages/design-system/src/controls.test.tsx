import { afterAll, afterEach, beforeAll, describe, expect, test } from "bun:test";
import { GlobalRegistrator } from "@happy-dom/global-registrator";
import { act, useState } from "react";
import { createRoot, type Root } from "react-dom/client";
import { formatTimestampZone } from "./format.ts";
import { pageList, Pagination } from "./Pagination.tsx";
import { DurationCell, RelativeTime } from "./RelativeTime.tsx";
import { SegmentedControl } from "./SegmentedControl.tsx";
import { StatePill } from "./Badge.tsx";
import { hostDisplayState } from "./states.ts";
import { readTerminalScheme, setTerminalScheme, TERMINAL_THEME_KEY, useTerminalScheme } from "./theme.ts";

test("pageList: first, last, the current page and its neighbours, gaps as null", () => {
  expect(pageList(1, 1)).toEqual([1]);
  expect(pageList(1, 52)).toEqual([1, 2, null, 52]);
  expect(pageList(10, 52)).toEqual([1, null, 9, 10, 11, null, 52]);
  expect(pageList(52, 52)).toEqual([1, null, 51, 52]);
  expect(pageList(3, 5)).toEqual([1, 2, 3, 4, 5]);
});

test("hostDisplayState: launch_failed only for a terminated host whose launch failed", () => {
  expect(hostDisplayState({ state: "terminated", launch: { outcome: "failed" } })).toBe("launch_failed");
  expect(hostDisplayState({ state: "terminated", launch: { outcome: "launched" } })).toBe("terminated");
  expect(hostDisplayState({ state: "terminated" })).toBe("terminated");
  expect(hostDisplayState({ state: "provisioning", launch: { outcome: "failed" } })).toBe("provisioning");
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
    localStorage.clear();
    delete document.documentElement.dataset.theme;
  });
  afterAll(async () => {
    await GlobalRegistrator.unregister();
  });

  test("Pagination, count mode: the range, numbered pages, page size", async () => {
    const seen: string[] = [];
    function Pager() {
      const [page, setPage] = useState(1);
      const [size, setSize] = useState(25);
      return (
        <Pagination
          mode="count"
          page={page}
          pageSize={size}
          total={1284}
          noun="hosts"
          onPage={(p) => {
            seen.push(`page ${p}`);
            setPage(p);
          }}
          onPageSize={(n) => {
            seen.push(`size ${n}`);
            setSize(n);
          }}
        />
      );
    }
    const el = await render(<Pager />);
    expect(el.querySelector(".pager-range")?.textContent).toBe("1–25 of 1,284 hosts");
    const prev = [...el.querySelectorAll("button")].find((b) => b.textContent?.includes("Previous")) as HTMLButtonElement;
    expect(prev.disabled).toBe(true);
    await act(async () => (el.querySelector('[aria-label="Page 52"]') as HTMLElement).click());
    expect(el.querySelector(".pager-range")?.textContent).toBe("1,276–1,284 of 1,284 hosts");
    expect(el.querySelector('[aria-current="page"]')?.textContent).toBe("52");
    const select = el.querySelector("select") as HTMLSelectElement;
    await act(async () => {
      select.value = "100";
      select.dispatchEvent(new Event("change", { bubbles: true }));
    });
    expect(seen).toEqual(["page 52", "size 100"]);
  });

  test("Pagination, cursor mode: First/Previous off on page 1, Next off on the last", async () => {
    const calls: string[] = [];
    const el = await render(
      <Pagination mode="cursor" page={1} count={50} pageSize={50} hasPrev={false} hasNext noun="runs" sortLabel="Created, newest first" onFirst={() => calls.push("first")} onPrev={() => calls.push("prev")} onNext={() => calls.push("next")} />,
    );
    const btn = (t: string) => [...el.querySelectorAll("button")].find((b) => b.textContent?.includes(t)) as HTMLButtonElement;
    expect(el.textContent).toContain("Page 1 · runs 1–50");
    expect(el.textContent).toContain("Created, newest first");
    expect(btn("First").disabled).toBe(true);
    expect(btn("Previous").disabled).toBe(true);
    await act(async () => btn("Next").click());
    expect(calls).toEqual(["next"]);
  });

  const button = (el: HTMLElement, t: string) => [...el.querySelectorAll("button")].find((b) => b.textContent?.includes(t)) as HTMLButtonElement;

  test("Pagination, cursor mode: past page 1, First stays on even when the page has lost its previous one", async () => {
    const calls: string[] = [];
    const el = await render(<Pagination mode="cursor" page={3} count={0} pageSize={50} hasPrev={false} hasNext={false} onFirst={() => calls.push("first")} onPrev={() => calls.push("prev")} onNext={() => calls.push("next")} />);
    expect([button(el, "First").disabled, button(el, "Previous").disabled, button(el, "Next").disabled]).toEqual([false, true, true]);
    await act(async () => button(el, "First").click());
    expect(calls).toEqual(["first"]);
  });

  test("Pagination, count mode: Next off on the last page; an empty result reads 0–0 of 0", async () => {
    const onPage = () => {};
    const el = await render(
      <>
        <div className="last">
          <Pagination mode="count" page={52} pageSize={25} total={1284} noun="hosts" onPage={onPage} />
        </div>
        <div className="none">
          <Pagination mode="count" page={1} pageSize={25} total={0} noun="hosts" onPage={onPage} />
        </div>
      </>,
    );
    const last = el.querySelector(".last") as HTMLElement;
    expect([button(last, "Previous").disabled, button(last, "Next").disabled]).toEqual([false, true]);
    expect(el.querySelector(".none .pager-range")?.textContent).toBe("0–0 of 0 hosts");
  });

  test("Pagination, count mode: busy, a page click does nothing", async () => {
    const asked: number[] = [];
    const el = await render(<Pagination mode="count" page={1} pageSize={25} total={1284} busy onPage={(p) => asked.push(p)} />);
    await act(async () => (el.querySelector('[aria-label="Page 2"]') as HTMLElement).click());
    await act(async () => button(el, "Next").click());
    expect(asked).toEqual([]);
  });

  test("SegmentedControl: one radio checked; choosing another reports it", async () => {
    const got: string[] = [];
    const el = await render(
      <SegmentedControl
        label="Lifecycle"
        value="all"
        onChange={(v) => got.push(v)}
        options={[
          { value: "live", label: "Live" },
          { value: "all", label: "All" },
          { value: "ended", label: "Ended" },
        ]}
      />,
    );
    const radios = [...el.querySelectorAll('[role="radio"]')] as HTMLElement[];
    expect(radios.map((r) => r.getAttribute("aria-checked"))).toEqual(["false", "true", "false"]);
    await act(async () => radios[2]!.click());
    await act(async () => radios[1]!.click());
    expect(got).toEqual(["ended"]);
  });

  test("RelativeTime: ago text, the exact time with its zone on hover, a dash for none", async () => {
    const at = new Date(Date.UTC(2026, 8, 30, 9, 0, 0)).toISOString();
    const now = Date.UTC(2026, 8, 30, 12, 0, 0);
    const el = await render(
      <>
        <RelativeTime at={at} now={now} label="Created" />
        <RelativeTime at={null} />
      </>,
    );
    const rel = el.querySelector(".rel-time") as HTMLElement;
    expect(rel.textContent).toBe("3h ago");
    await act(async () => rel.parentElement!.dispatchEvent(new MouseEvent("mouseover", { bubbles: true })));
    const tip = el.querySelector('[role="tooltip"]');
    expect(tip?.textContent).toBe(`Created ${formatTimestampZone(at)}`);
    expect(formatTimestampZone(at)).toMatch(/^2026-09-30 \d\d:00:00 \S+/);
    expect(el.textContent).toContain("–");
  });

  test("DurationCell: warn tone, live ellipsis, a dash with its reason", async () => {
    const el = await render(
      <>
        <DurationCell seconds={301} warn tip="slow" />
        <DurationCell seconds={42.9} live ellipsis />
        <DurationCell seconds={null} missing="Never launched" />
      </>,
    );
    const d = [...el.querySelectorAll(".duration")];
    expect(d[0]!.className).toContain("is-warn");
    expect(d[0]!.textContent).toBe("5m 1s");
    expect(d[1]!.textContent).toBe("42s…");
    expect(d[1]!.className).toContain("is-live");
    expect(el.textContent).toContain("–");
  });

  test("launch_failed pill: its own label, outlined", async () => {
    const el = await render(<StatePill kind="host" state="launch_failed" />);
    const pill = el.querySelector(".pill") as HTMLElement;
    expect(pill.textContent).toBe("Launch failed");
    expect(pill.className).toContain("pill-outline");
    expect(pill.className).toContain("pill-red");
  });

  test("terminal scheme: persisted under its own key, validated, never the console theme", async () => {
    localStorage.setItem("lux.theme", "light");
    document.documentElement.dataset.theme = "light";
    localStorage.setItem(TERMINAL_THEME_KEY, "purple");
    expect(readTerminalScheme()).toBe("auto");
    let seen = "";
    function Probe() {
      const t = useTerminalScheme();
      seen = `${t.pref}/${t.resolved}`;
      return null;
    }
    await render(<Probe />);
    expect(seen).toBe("auto/light");
    await act(async () => setTerminalScheme("dark"));
    expect(seen).toBe("dark/dark");
    expect(localStorage.getItem(TERMINAL_THEME_KEY)).toBe("dark");
    expect(localStorage.getItem("lux.theme")).toBe("light");
    expect(document.documentElement.dataset.theme).toBe("light");
    await act(async () => setTerminalScheme("auto"));
    expect(localStorage.getItem(TERMINAL_THEME_KEY)).toBeNull();
    expect(seen).toBe("auto/light");
  });

  test("terminal scheme: another tab's change (a storage event) reaches this one", async () => {
    let seen = "";
    function Probe() {
      seen = useTerminalScheme().pref;
      return null;
    }
    await render(<Probe />);
    expect(seen).toBe("auto");
    await act(async () => {
      localStorage.setItem(TERMINAL_THEME_KEY, "dark");
      window.dispatchEvent(new StorageEvent("storage", { key: "lux.theme" }));
    });
    // Another key: not re-read.
    expect(seen).toBe("auto");
    await act(async () => window.dispatchEvent(new StorageEvent("storage", { key: TERMINAL_THEME_KEY })));
    expect(seen).toBe("dark");
  });
});

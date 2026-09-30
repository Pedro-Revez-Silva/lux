import { afterAll, afterEach, beforeAll, describe, expect, test } from "bun:test";
import { GlobalRegistrator } from "@happy-dom/global-registrator";
import { act, useState } from "react";
import { createRoot, type Root } from "react-dom/client";
import { dropsOptional, firstSortDir, sortRows, Table, type Column, type SortState } from "./Table.tsx";

// The Runs list's widths: name 22%, Id, State flexible, Host, Adapter,
// Runtime, Placements, Cost, Created; Id/Adapter/Placements optional.
const runs = [{ width: "22%" }, { width: 200, optional: true }, {}, { width: 150 }, { width: 110, optional: true }, { width: 90 }, { width: 116, optional: true }, { width: 120 }, { width: 104 }];

test("optional columns drop under 1100px", () => {
  expect(dropsOptional([{}, { width: 100, optional: true }], 1099)).toBe(true);
  expect(dropsOptional([{}, { width: 100, optional: true }], 1100)).toBe(false);
  expect(dropsOptional(runs, 0)).toBe(false);
});

test("optional columns drop when with them a flexible column would get under 140px", () => {
  // 1300px of container: 1300 - 286 - 890 = 124px left for State.
  expect(dropsOptional(runs, 1300)).toBe(true);
  // 1400px: 1400 - 308 - 890 = 202px.
  expect(dropsOptional(runs, 1400)).toBe(false);
  // No optional column: nothing to drop.
  expect(dropsOptional(runs.map(({ width }) => ({ width })), 1300)).toBe(false);
});

interface R {
  id: string;
  name: string;
  n: number | null;
}
const data: R[] = [
  { id: "a", name: "beta", n: 2 },
  { id: "b", name: "alpha", n: null },
  { id: "c", name: "Gamma", n: 10 },
  { id: "d", name: "", n: 1 },
];

test("sortRows: missing values last in both directions, numbers numerically, text case-insensitively", () => {
  const byN = (r: R) => r.n;
  expect(sortRows(data, byN, "desc").map((r) => r.id)).toEqual(["c", "a", "d", "b"]);
  expect(sortRows(data, byN, "asc").map((r) => r.id)).toEqual(["d", "a", "c", "b"]);
  const byName = (r: R) => r.name;
  expect(sortRows(data, byName, "asc").map((r) => r.id)).toEqual(["b", "a", "c", "d"]);
  expect(sortRows(data, byName, "desc").map((r) => r.id)).toEqual(["c", "a", "b", "d"]);
});

test("first click: text A→Z; numbers, times and durations largest first", () => {
  expect(firstSortDir<R>({ key: "name", header: "", cell: () => null, sortValue: (r) => r.name }, data)).toBe("asc");
  expect(firstSortDir<R>({ key: "n", header: "", cell: () => null, sortValue: (r) => r.n }, data)).toBe("desc");
  expect(firstSortDir<R>({ key: "t", header: "", cell: () => null, sortable: true, align: "right" })).toBe("desc");
  expect(firstSortDir<R>({ key: "t", header: "", cell: () => null, sortable: true, sortFirst: "desc" })).toBe("desc");
});

const mounted: { el: HTMLElement; root: Root }[] = [];

async function render(node: React.ReactNode) {
  const el = document.createElement("div");
  document.body.appendChild(el);
  const root = createRoot(el);
  mounted.push({ el, root });
  await act(async () => root.render(node));
  const th = (label: string) => [...el.querySelectorAll("thead th")].find((h) => h.textContent?.replace(/[↑↓↕]$/, "") === label) as HTMLElement;
  const ids = () => [...el.querySelectorAll("tbody tr")].map((tr) => tr.querySelector("td")?.textContent);
  return { el, th, ids };
}

const cols: Column<R>[] = [
  { key: "id", header: "Id", cell: (r) => r.id, sortValue: (r) => r.id },
  { key: "name", header: "Name", cell: (r) => r.name, sortValue: (r) => r.name },
  { key: "n", header: "N", cell: (r) => r.n ?? "–", sortValue: (r) => r.n, align: "right" },
  { key: "static", header: "Static", cell: () => "x" },
];

describe("Table in a DOM", () => {
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

  test("every sortable header is focusable, carries aria-sort and an arrow; others do not", async () => {
    const { th } = await render(<Table columns={cols} rows={data} rowKey={(r) => r.id} defaultSort={{ key: "name", dir: "asc" }} />);
    expect(th("Name").getAttribute("aria-sort")).toBe("ascending");
    expect(th("Name").textContent).toBe("Name↑");
    expect(th("N").getAttribute("aria-sort")).toBe("none");
    expect(th("N").textContent).toBe("N↕");
    expect(th("N").tabIndex).toBe(0);
    expect(th("Static").hasAttribute("aria-sort")).toBe(false);
    expect(th("Static").getAttribute("tabindex")).toBeNull();
  });

  test("client sort: click toggles, Enter and Space sort, missing values stay last", async () => {
    const { th, ids } = await render(<Table columns={cols} rows={data} rowKey={(r) => r.id} />);
    await act(async () => th("N").click());
    expect(th("N").getAttribute("aria-sort")).toBe("descending");
    expect(ids()).toEqual(["c", "a", "d", "b"]);
    await act(async () => th("N").dispatchEvent(new KeyboardEvent("keydown", { key: "Enter", bubbles: true })));
    expect(th("N").getAttribute("aria-sort")).toBe("ascending");
    expect(ids()).toEqual(["d", "a", "c", "b"]);
    await act(async () => th("Name").dispatchEvent(new KeyboardEvent("keydown", { key: " ", bubbles: true })));
    expect(th("Name").getAttribute("aria-sort")).toBe("ascending");
    expect(ids()).toEqual(["b", "a", "c", "d"]);
  });

  test("server sort: rows shown as given, the chosen sort reported, the header follows the controlled sort", async () => {
    const asked: SortState[] = [];
    function Paged() {
      const [sort, setSort] = useState<SortState>({ key: "id", dir: "asc" });
      return (
        <Table
          columns={cols}
          rows={data}
          rowKey={(r) => r.id}
          sortMode="server"
          sort={sort}
          onSortChange={(s) => {
            asked.push(s);
            setSort(s);
          }}
          footer={<div className="the-footer">pages</div>}
        />
      );
    }
    const { el, th, ids } = await render(<Paged />);
    expect(ids()).toEqual(["a", "b", "c", "d"]);
    await act(async () => th("N").click());
    expect(asked).toEqual([{ key: "n", dir: "desc" }]);
    // Not re-sorted locally: the server's order is the page.
    expect(ids()).toEqual(["a", "b", "c", "d"]);
    expect(th("N").getAttribute("aria-sort")).toBe("descending");
    await act(async () => th("N").click());
    expect(asked.at(-1)).toEqual({ key: "n", dir: "asc" });
    expect(el.querySelector(".the-footer")?.textContent).toBe("pages");
  });
});

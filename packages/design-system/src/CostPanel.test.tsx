import { afterAll, afterEach, beforeAll, describe, expect, test } from "bun:test";
import { GlobalRegistrator } from "@happy-dom/global-registrator";
import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { BreakdownTable, LabelChips, LabelFilterPopover, matchValues, SplitBar } from "./CostPanel.tsx";
import { StepPicker } from "./StepPicker.tsx";

const mounted: { el: HTMLElement; root: Root }[] = [];
async function render(node: React.ReactNode) {
  const el = document.createElement("div");
  document.body.appendChild(el);
  const root = createRoot(el);
  mounted.push({ el, root });
  await act(async () => root.render(node));
  return el;
}

describe("cost panel components", () => {
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

  test("LabelChips: the keys asked for first, at most max, the rest counted with their text in the title", async () => {
    const el = await render(<LabelChips labels={{ team: "x", app: "jervasion", "jervasion.repository": "absmartly/abs", z: "1" }} first={["app"]} max={2} />);
    const chips = [...el.querySelectorAll(".label-chip")].map((c) => c.textContent);
    expect(chips).toEqual(["app=jervasion", "jervasion.repository=absmartly/abs", "+2"]);
    expect(el.querySelector(".label-chip.is-more")?.getAttribute("title")).toBe("team=x\nz=1");
    const none = await render(<LabelChips labels={{}} />);
    expect(none.textContent).toBe("–");
  });

  test("SplitBar: each part as long as its share; a faint part is drawn, not dropped; a part with no amount is absent", async () => {
    const el = await render(
      <SplitBar
        currency="USD"
        whole="10"
        parts={[
          { amount: "2.5", color: "red", label: "Compute", faint: true },
          { amount: "7.5", color: "blue", label: "External" },
          { amount: null, color: "green", label: "Other" },
        ]}
      />,
    );
    const parts = [...el.querySelectorAll<HTMLElement>(".split-bar-part")];
    expect(parts.map((p) => [p.style.flexGrow, p.classList.contains("is-faint")])).toEqual([
      ["0.25", true],
      ["0.75", false],
    ]);
    expect(el.querySelector(".split-bar")?.getAttribute("title")).toBe("Compute $2.50 · External $7.50");
  });

  test("BreakdownTable: a row per value with its parts, a revoked pill, a missing part as a dash; a row click reports it", async () => {
    const clicked: string[] = [];
    const el = await render(
      <BreakdownTable
        lead="API key"
        onRowClick={(r) => clicked.push(r.id)}
        rows={[
          { id: "k1", label: "ci-bot", color: "red", currency: "USD", runs: 3, compute: "1", external: null, total: "1", share: 0.5, pill: "revoked" },
          { id: "(none)", label: "Before key tracking", color: "grey", currency: "USD", runs: 1, compute: "1", external: "0", total: "1", share: 0.5, quiet: true },
        ]}
      />,
    );
    const rows = [...el.querySelectorAll("tbody tr")];
    expect(rows[0]!.textContent).toContain("revoked");
    expect([...rows[0]!.querySelectorAll("td")].map((td) => td.textContent)).toEqual(["ci-botrevoked", "3", "$1.00", "–", "$1.00", "50%"]);
    await act(async () => (rows[1] as HTMLElement).click());
    expect(clicked).toEqual(["(none)"]);
  });

  test("BreakdownTable: several currencies say their shares are per currency; SplitBar names its currency", async () => {
    const el = await render(
      <BreakdownTable
        lead="app"
        rows={[
          { id: "a", label: "a", color: "red", currency: "EUR", runs: 1, compute: null, external: "3", total: "3", share: 1 },
          { id: "a", label: "a", color: "red", currency: "USD", runs: 2, compute: "1", external: "1", total: "2", share: 0.4 },
        ]}
      />,
    );
    expect([...el.querySelectorAll("thead th")].map((th) => th.textContent).at(-1)).toBe("Share (per currency)");
    expect([...el.querySelectorAll("tbody tr")].map((tr) => [...tr.querySelectorAll("td")].map((td) => td.textContent))).toEqual([
      ["a", "1", "–", "€3.00", "€3.00", "100%"],
      ["a", "2", "$1.00", "$1.00", "$2.00", "40%"],
    ]);
    const one = await render(<BreakdownTable lead="app" rows={[{ id: "a", label: "a", color: "red", currency: "USD", runs: 1, compute: "1", external: null, total: "1", share: 1 }]} />);
    expect([...one.querySelectorAll("thead th")].map((th) => th.textContent).at(-1)).toBe("Share");
    const bar = await render(<SplitBar currency="EUR" whole="1" label="EUR" parts={[{ amount: "1", color: "red", label: "Compute" }]} />);
    expect(bar.querySelector(".split-bar-label")?.textContent).toBe("EUR");
  });

  describe("LabelFilterPopover", () => {
    const VALUES = [
      { value: "dude", amounts: [{ currency: "USD", amount: "7.39" }] },
      { value: "jervasion", amounts: [{ currency: "USD", amount: "50.6" }] },
      { value: "tiny", amounts: [] },
    ];
    async function popover(keys = [{ key: "app", runs: 3 }, { key: "repo" }]) {
      const applied: unknown[] = [];
      const keysSeen: string[] = [];
      const el = await render(
        <LabelFilterPopover keys={keys} initialKey="app" onKeyChange={(k) => keysSeen.push(k)} values={(k) => (k === "app" ? VALUES : undefined)} notSet={() => [{ currency: "USD", amount: "0.4" }]} onApply={(f) => applied.push(f)} />,
      );
      const add = () => el.querySelector<HTMLButtonElement>('button[aria-haspopup="dialog"]')!;
      const dialog = () => el.querySelector<HTMLElement>('[role="dialog"]');
      const button = (name: string) => [...dialog()!.querySelectorAll("button")].find((b) => b.textContent === name) as HTMLButtonElement;
      // A checkbox by its label's text.
      const box = (name: string) => [...dialog()!.querySelectorAll("label")].find((l) => l.querySelector('input[type="checkbox"]') && l.textContent?.startsWith(name))!.querySelector("input") as HTMLInputElement;
      const names = () => [...dialog()!.querySelectorAll('input[type="checkbox"]')].map((b) => b.closest("label")!.querySelector(".mono")!.textContent);
      const open = () => act(async () => add().click());
      const key = (k: string) => act(async () => document.activeElement!.dispatchEvent(new KeyboardEvent("keydown", { key: k, bubbles: true })));
      return { el, applied, keysSeen, add, dialog, button, box, names, open, key };
    }

    test("values biggest first; Apply adds the values picked", async () => {
      const p = await popover();
      await p.open();
      expect(p.keysSeen).toEqual(["app"]);
      expect(p.names()).toEqual(["jervasion", "dude", "tiny", "(not set)"]);
      expect(p.button("Apply").disabled).toBe(true);
      await act(async () => p.box("dude").click());
      await act(async () => p.box("jervasion").click());
      await act(async () => p.button("Apply").click());
      expect(p.applied).toEqual([{ key: "app", values: ["dude", "jervasion"], notSet: false }]);
      expect(p.dialog()).toBeNull();
    });

    test("(not set) replaces the values and Apply sends notSet", async () => {
      const p = await popover();
      await p.open();
      await act(async () => p.box("dude").click());
      await act(async () => p.box("(not set)").click());
      expect(p.box("dude").disabled).toBe(true);
      expect(p.box("dude").checked).toBe(false);
      await act(async () => p.button("Apply").click());
      expect(p.applied).toEqual([{ key: "app", values: [], notSet: true }]);
    });

    test("Cancel applies nothing and closes", async () => {
      const p = await popover();
      await p.open();
      await act(async () => p.box("dude").click());
      await act(async () => p.button("Cancel").click());
      expect(p.applied).toEqual([]);
      expect(p.dialog()).toBeNull();
    });

    test("the search narrows the values and hides (not set)", async () => {
      const p = await popover();
      await p.open();
      const input = p.dialog()!.querySelector<HTMLInputElement>('input[aria-label="Search values"]')!;
      // A typed change as a browser delivers it: focus, the new value, input and keyup.
      await act(async () => {
        input.focus();
        input.dispatchEvent(new Event("focusin", { bubbles: true }));
        Object.getOwnPropertyDescriptor(Object.getPrototypeOf(input), "value")!.set!.call(input, "JER");
        input.dispatchEvent(new Event("input", { bubbles: true }));
        input.dispatchEvent(new KeyboardEvent("keyup", { key: "R", bubbles: true }));
      });
      expect(input.value).toBe("JER");
      expect(p.names()).toEqual(["jervasion"]);
    });

    test("focus: into the dialog on open, back to the add button on Escape", async () => {
      const p = await popover();
      p.add().focus();
      await p.open();
      expect(p.dialog()!.contains(document.activeElement)).toBe(true);
      expect(document.activeElement!.classList.contains("select-trigger")).toBe(true);
      await p.key("Escape");
      expect(p.dialog()).toBeNull();
      expect(document.activeElement).toBe(p.add());
    });

    test("focus: with no key to pick, the search takes it", async () => {
      const p = await popover([]);
      await p.open();
      expect(document.activeElement?.getAttribute("aria-label")).toBe("Search values");
    });

    test("focus: a click elsewhere closes the dialog and leaves focus where the user put it", async () => {
      const p = await popover();
      const outside = document.createElement("button");
      outside.textContent = "elsewhere";
      document.body.appendChild(outside);
      try {
        await p.open();
        await act(async () => {
          outside.focus();
          outside.dispatchEvent(new MouseEvent("mousedown", { bubbles: true }));
        });
        expect(p.dialog()).toBeNull();
        expect(document.activeElement).toBe(outside);
      } finally {
        outside.remove();
      }
    });

    test("Escape with the key picker open closes the picker only", async () => {
      const p = await popover();
      await p.open();
      await act(async () => (document.activeElement as HTMLElement).click());
      expect(p.dialog()!.querySelector(".select.is-open")).not.toBeNull();
      await p.key("Escape");
      expect(p.dialog()).not.toBeNull();
      expect(p.dialog()!.querySelector(".select.is-open")).toBeNull();
      await p.key("Escape");
      expect(p.dialog()).toBeNull();
    });

    test("matchValues: case-insensitive anywhere, biggest cost first", () => {
      expect(matchValues([{ value: "absmartly/abs", amounts: [] }, { value: "Jervasion", amounts: [{ currency: "USD", amount: "1" }] }, { value: "dude", amounts: [{ currency: "USD", amount: "2" }] }], " JER ").map((v) => v.value)).toEqual(["Jervasion"]);
      expect(matchValues([{ value: "a", amounts: [{ currency: "USD", amount: "1" }] }, { value: "b", amounts: [{ currency: "USD", amount: "2" }] }, { value: "c", amounts: [] }], "").map((v) => v.value)).toEqual(["b", "a", "c"]);
    });
  });

  test("StepPicker: Auto shows what it resolved to; a disabled choice says why and cannot be picked", async () => {
    const picked: string[] = [];
    const el = await render(
      <StepPicker
        value="auto"
        resolved="min/hour"
        onChange={(v) => picked.push(v)}
        options={[{ value: "auto", hint: "each chart picks" }, { value: "minute", hint: "1,440 points" }, { value: "hour" }, { value: "day", disabled: "1 point · too coarse" }]}
        note="what it does"
      />,
    );
    expect(el.querySelector(".select-trigger")?.textContent).toBe("EveryAuto · min/hour");
    await act(async () => (el.querySelector(".select-trigger") as HTMLElement).click());
    const opt = (t: string) => [...el.querySelectorAll('[role="option"]')].find((o) => o.textContent?.startsWith(t)) as HTMLElement;
    expect(opt("Minute").textContent).toBe("Minute1,440 points");
    expect(opt("Day").getAttribute("aria-disabled")).toBe("true");
    await act(async () => opt("Day").click());
    await act(async () => opt("Hour").click());
    expect(picked).toEqual(["hour"]);
    expect(el.querySelector(".select-footer")).toBeNull();
  });
});

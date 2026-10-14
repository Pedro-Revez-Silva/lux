import { afterAll, afterEach, beforeAll, describe, expect, test } from "bun:test";
import { GlobalRegistrator } from "@happy-dom/global-registrator";
import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { ImageRef, LabelsFact, shortDigest, splitImageRef } from "./Facts.tsx";
import { KeyValue } from "./KeyValue.tsx";

const DIGEST = "sha256:2c567002fb22a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f6d76856cf";

test("splitImageRef: registry path, name, tag and digest", () => {
  expect(splitImageRef(`ghcr.io/acme/jervasion-agent@${DIGEST}`)).toEqual({ path: "ghcr.io/acme/", name: "jervasion-agent", tag: "", digest: DIGEST });
  expect(splitImageRef("ghcr.io/acme/agent:1.14")).toEqual({ path: "ghcr.io/acme/", name: "agent", tag: ":1.14", digest: "" });
  expect(splitImageRef("localhost:5000/agent")).toEqual({ path: "localhost:5000/", name: "agent", tag: "", digest: "" });
  expect(splitImageRef("alpine")).toEqual({ path: "", name: "alpine", tag: "", digest: "" });
});

test("shortDigest: sha256 + the first 12 and last 8 hex; a short or odd digest stays whole", () => {
  expect(shortDigest(DIGEST)).toBe("sha256:2c567002fb22…d76856cf");
  expect(shortDigest("sha256:abc123")).toBe("sha256:abc123");
  expect(shortDigest("not-a-digest")).toBe("not-a-digest");
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

  test("ImageRef: one line with the digest shortened; the whole reference is the title and what Copy copies", async () => {
    const copied: string[] = [];
    Object.defineProperty(navigator, "clipboard", { configurable: true, value: { writeText: async (t: string) => void copied.push(t) } });
    const ref = `ghcr.io/acme/jervasion-agent@${DIGEST}`;
    const el = await render(<ImageRef value={ref} />);
    const text = el.querySelector(`[title="${ref}"]`)!;
    expect(text.textContent).toBe("ghcr.io/acme/jervasion-agent@sha256:2c567002fb22…d76856cf");
    await act(async () => el.querySelector<HTMLButtonElement>('button[aria-label="Copy image reference"]')!.click());
    expect(copied).toEqual([ref]);
  });

  test("ImageRef: a tag is shown whole; a bare name is just the name", async () => {
    const el = await render(
      <>
        <ImageRef value="ghcr.io/acme/agent:1.14" />
        <ImageRef value="alpine" />
      </>,
    );
    expect([...el.querySelectorAll("[title]")].filter((n) => n.tagName === "SPAN").map((n) => n.textContent)).toEqual(["ghcr.io/acme/agent:1.14", "alpine"]);
  });

  const LABELS = { "jervasion.target": "pr:PR_kwDOQUrH8Es8AAAABIcOqGg", app: "jervasion", "jervasion.pr": "45", "jervasion.intent-sha256": "c90e477bbaff488659e553c23aea3e860c7e5422c5515df7ac6657bec23e5cde" };
  // The disclosure is found as a user finds it: a button with aria-expanded, named by its visible text.
  const toggleOf = (el: HTMLElement) => el.querySelector<HTMLButtonElement>("button[aria-expanded]")!;

  test("LabelsFact: collapsed shows the count, three chips and +1; the toggle opens every pair and says so in aria-expanded", async () => {
    const el = await render(<LabelsFact labels={LABELS} first={["app"]} />);
    const group = el.querySelector('[role="group"]')!;
    expect(group.getAttribute("aria-labelledby")).toBeTruthy();
    expect(el.querySelector(`#${CSS.escape(group.getAttribute("aria-labelledby")!)}`)?.textContent).toBe("Labels 4");
    const toggle = toggleOf(el);
    expect(toggle.getAttribute("aria-expanded")).toBe("false");
    expect(toggle.textContent).toBe("Show all");
    // Its accessible name is its visible text (WCAG 2.5.3): no aria-label overrides it.
    expect(toggle.hasAttribute("aria-label")).toBe(false);
    expect(toggle.classList.contains("btn-ghost")).toBe(true);
    expect([...el.querySelectorAll(".label-chip")].map((c) => c.textContent)).toEqual(["app=jervasion", "jervasion.intent-sha256=c90e477bbaff488659e553c23aea3e860c7e5422c5515df7ac6657bec23e5cde", "jervasion.pr=45", "+1"]);
    expect(el.querySelector("dl")).toBeNull();

    await act(async () => toggle.click());
    expect(toggle.getAttribute("aria-expanded")).toBe("true");
    expect(toggle.textContent).toBe("Hide");
    const list = el.querySelector(`#${CSS.escape(toggle.getAttribute("aria-controls")!)}`)!;
    const pairs = [...list.querySelectorAll("dt")].map((dt) => [dt.textContent, dt.nextElementSibling?.textContent]);
    expect(pairs).toEqual([
      ["app", "jervasion"],
      ["jervasion.intent-sha256", "c90e477bbaff488659e553c23aea3e860c7e5422c5515df7ac6657bec23e5cde"],
      ["jervasion.pr", "45"],
      ["jervasion.target", "pr:PR_kwDOQUrH8Es8AAAABIcOqGg"],
    ]);
    const copied: string[] = [];
    Object.defineProperty(navigator, "clipboard", { configurable: true, value: { writeText: async (t: string) => void copied.push(t) } });
    await act(async () => list.querySelector<HTMLButtonElement>('button[aria-label="Copy jervasion.target"]')!.click());
    expect(copied).toEqual(["pr:PR_kwDOQUrH8Es8AAAABIcOqGg"]);

    await act(async () => toggle.click());
    expect(toggle.getAttribute("aria-expanded")).toBe("false");
    expect(el.querySelector("dl")).toBeNull();
  });

  test("LabelsFact: `first` leads both the preview chips and the list, even over a key that sorts before it", async () => {
    const el = await render(<LabelsFact labels={{ ...LABELS, "a.b": "x" }} first={["app"]} defaultOpen />);
    expect([...el.querySelectorAll(".label-chip")].map((c) => c.textContent?.split("=")[0])).toEqual(["app", "a.b", "jervasion.intent-sha256", "+2"]);
    expect([...el.querySelectorAll("dl dt")].map((d) => d.textContent)).toEqual(["app", "a.b", "jervasion.intent-sha256", "jervasion.pr", "jervasion.target"]);
  });

  test("LabelsFact: new labels, or a new `first` for the same labels, on re-render redraw the chips and the list", async () => {
    const el = document.createElement("div");
    document.body.appendChild(el);
    const root = createRoot(el);
    mounted.push({ el, root });
    const chips = () => [...el.querySelectorAll(".label-chip")].map((c) => c.textContent);
    const keys = () => [...el.querySelectorAll("dl dt")].map((d) => d.textContent);
    await act(async () => root.render(<LabelsFact labels={{ a: "1" }} defaultOpen />));
    expect(chips()).toEqual(["a=1"]);
    const labels = { a: "1", b: "2" };
    await act(async () => root.render(<LabelsFact labels={labels} first={["b"]} defaultOpen />));
    expect(chips()).toEqual(["b=2", "a=1"]);
    expect(keys()).toEqual(["b", "a"]);
    await act(async () => root.render(<LabelsFact labels={labels} first={["a"]} defaultOpen />));
    expect(chips()).toEqual(["a=1", "b=2"]);
    expect(keys()).toEqual(["a", "b"]);
  });

  test("LabelsFact: `label` names the row, empty or not; the toggle reads Show all / Hide whatever the label", async () => {
    const el = await render(<LabelsFact label="Versions" labels={{ runc: "1.2", kernel: "6.8" }} />);
    const group = el.querySelector('[role="group"]')!;
    expect(el.querySelector(`#${CSS.escape(group.getAttribute("aria-labelledby")!)}`)?.textContent).toBe("Versions 2");
    expect(toggleOf(el).textContent).toBe("Show all");
    await act(async () => toggleOf(el).click());
    expect(toggleOf(el).textContent).toBe("Hide");
    const node = await render(<LabelsFact label={<span>Template</span>} labels={{ a: "1" }} />);
    expect(toggleOf(node).textContent).toBe("Show all");
    await act(async () => toggleOf(node).click());
    expect(toggleOf(node).textContent).toBe("Hide");
    expect((await render(<LabelsFact label="Versions" labels={{}} />)).textContent).toBe("Versions–");
  });

  test("LabelsFact: no labels reads Labels – with no toggle", async () => {
    const el = await render(<LabelsFact labels={{}} />);
    expect(el.textContent).toBe("Labels–");
    expect(el.querySelector("button")).toBeNull();
  });

  test("KeyValue: children are full-width rows after the grid, outside it", async () => {
    const el = await render(
      <KeyValue columns={3} items={[{ key: "Created", value: "now" }]}>
        <LabelsFact labels={{ app: "x" }} />
      </KeyValue>,
    );
    const grid = el.querySelector("dl")!;
    expect([...grid.querySelectorAll("dt")].map((d) => d.textContent)).toEqual(["Created"]);
    expect(grid.contains(el.querySelector('[role="group"]'))).toBe(false);
    expect(grid.nextElementSibling?.getAttribute("role")).toBe("group");
    // className goes on the wrapper with children; the card surface is on by default.
    expect(el.firstElementChild?.className).toBe("kv-facts kv-facts-surface");
  });

  test("KeyValue: surface={false} draws the rows without a card of their own (inside a Card); className still on the wrapper", async () => {
    const el = await render(
      <KeyValue items={[{ key: "Created", value: "now" }]} surface={false} className="host-facts">
        <LabelsFact labels={{ app: "x" }} />
      </KeyValue>,
    );
    expect(el.firstElementChild?.className).toBe("kv-facts host-facts");
    expect(el.querySelector("dl")?.className).toBe("kv kv-cols-1");
  });
});

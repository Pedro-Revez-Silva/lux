import { expect, test } from "bun:test";
import { bandColor, familyColor, familyDisplay, NONE_BAND_COLOR, OTHER_BAND_COLOR, STORAGE_KIND_LIST, storageKindStyle } from "./states.ts";

const color = (families: Parameters<typeof familyDisplay>[0], family: string) => familyDisplay(families).get(family)?.color;

test("compute is always --chart-1, whatever is beside it", () => {
  expect(color([{ family: "compute" }], "compute")).toBe("var(--chart-1)");
  expect(color([{ family: "ai", color: "blue" }, { family: "compute", color: "violet" }, { family: "egress" }], "compute")).toBe("var(--chart-1)");
});

test("a hinted family gets its hint's token", () => {
  expect(color([{ family: "compute" }, { family: "ai", displayName: "AI models", color: "violet" }], "ai")).toBe("var(--chart-7)");
  expect(color([{ family: "video", color: "amber" }], "video")).toBe("var(--chart-4)");
  // Blue is compute's: a blue hint takes another slot.
  expect(color([{ family: "cdn", color: "blue" }], "cdn")).not.toBe("var(--chart-1)");
});

test("distinct families shown together get distinct colours", () => {
  const families = [{ family: "compute" }, { family: "ai", color: "violet" }, { family: "video", color: "amber" }, { family: "storage", color: "teal" }, { family: "network" }];
  const d = familyDisplay(families);
  const colors = families.map((f) => d.get(f.family)!.color);
  expect(new Set(colors).size).toBe(families.length);
  expect(colors.every((c) => /^var\(--chart-[1-8]\)$/.test(c))).toBe(true);
});

test("a family's colour does not depend on its companions", () => {
  const alone = color([{ family: "family0" }], "family0");
  expect(alone).toMatch(/^var\(--chart-[2-8]\)$/);
  const companions = [
    [{ family: "hinted", color: "violet" }],
    [{ family: "compute" }, { family: "ai", color: "violet" }, { family: "video", color: "amber" }],
    [{ family: "red", color: "red" }, { family: "teal", color: "teal" }, { family: "pink", color: "pink" }, { family: "green", color: "green" }, { family: "orange", color: "orange" }, { family: "amber", color: "amber" }, { family: "violet", color: "violet" }],
    [{ family: "egress" }, { family: "llm" }, { family: "storage" }],
  ];
  for (const others of companions) {
    expect(color([...others, { family: "family0" }], "family0")).toBe(alone!);
    expect(color([{ family: "family0" }, ...others], "family0")).toBe(alone!);
  }
  // The same holds for a hinted family.
  expect(color([{ family: "family0" }, { family: "ai", color: "violet" }], "ai")).toBe("var(--chart-7)");
});

test("labels: the displayName, else Compute for compute, else the key", () => {
  const d = familyDisplay([{ family: "compute" }, { family: "ai", displayName: "AI models" }, { family: "egress" }]);
  expect([d.get("compute")!.label, d.get("ai")!.label, d.get("egress")!.label]).toEqual(["Compute", "AI models", "egress"]);
});

test("each storage kind has its own label and categorical slot, never compute's", () => {
  expect(STORAGE_KIND_LIST).toEqual(["volume", "output", "artifact"]);
  expect(STORAGE_KIND_LIST.map((k) => storageKindStyle(k).label)).toEqual(["Snapshots", "Output", "Artifacts"]);
  const colors = STORAGE_KIND_LIST.map((k) => storageKindStyle(k).color);
  expect(colors).toEqual(["var(--chart-3)", "var(--chart-7)", "var(--chart-4)"]);
});

test("an unknown storage kind keeps its key and a neutral colour", () => {
  expect(storageKindStyle("cache")).toEqual({ label: "cache", color: "var(--st-neutral-dot)" });
});

test("breakdown bands never take compute's colour, for any number of bands", () => {
  const compute = familyColor("compute");
  for (let n = 1; n <= 24; n++) {
    expect(Array.from({ length: n }, (_, i) => bandColor(i))).not.toContain(compute);
  }
  expect([OTHER_BAND_COLOR, NONE_BAND_COLOR]).not.toContain(compute);
  // Bands take the remaining slots in rank order, each once before any repeats.
  const slots = [2, 3, 4, 5, 6, 7, 8, 2, 3, 4, 5, 6, 7, 8, 2];
  expect(slots.map((_, i) => bandColor(i))).toEqual(slots.map((s) => `var(--chart-${s})`));
});

test("the Family view keeps compute's colour: compute is --chart-1 there", () => {
  expect(familyColor("compute")).toBe("var(--chart-1)");
  expect(familyDisplay([{ family: "ai" }, { family: "compute" }]).get("compute")!.color).toBe("var(--chart-1)");
});

test("build contexts are not a charted kind", () => {
  expect(STORAGE_KIND_LIST).not.toContain("context");
  expect(storageKindStyle("context")).toEqual({ label: "context", color: "var(--st-neutral-dot)" });
});

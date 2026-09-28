import { expect, test } from "bun:test";
import { familyDisplay } from "./states.ts";

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

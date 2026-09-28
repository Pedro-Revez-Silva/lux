import { expect, test } from "bun:test";
import { dropsOptional } from "./Table.tsx";

// The Runs list's widths: name 22%, State flexible, Id/Adapter/Epoch optional.
const runs = [{ width: "22%" }, { width: 200, optional: true }, {}, { width: 150 }, { width: 110, optional: true }, { width: 76, optional: true }, { width: 120 }, { width: 104 }];

test("optional columns drop under 1100px", () => {
  expect(dropsOptional([{}, { width: 100, optional: true }], 1099)).toBe(true);
  expect(dropsOptional([{}, { width: 100, optional: true }], 1100)).toBe(false);
  expect(dropsOptional(runs, 0)).toBe(false);
});

test("optional columns drop when with them a flexible column would get under 140px", () => {
  // 1102px of container: 1102 - 242 - 760 = 100px left for State.
  expect(dropsOptional(runs, 1102)).toBe(true);
  // 1300px: 1300 - 286 - 760 = 254px.
  expect(dropsOptional(runs, 1300)).toBe(false);
  // No optional column: nothing to drop.
  expect(dropsOptional(runs.map(({ width }) => ({ width })), 1102)).toBe(false);
});

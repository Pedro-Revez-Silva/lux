import { expect, test } from "bun:test";
import { dropsOptional } from "./Table.tsx";

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

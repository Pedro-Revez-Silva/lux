// Records lux wrote for real runs, recorded on this repo's host (testdata/).
import { readFileSync } from "node:fs";
import type { OutputRecord } from "../../../api/index.ts";

export function fixture(name: string): OutputRecord[] {
  return readFileSync(new URL(`./testdata/${name}`, import.meta.url), "utf8")
    .split("\n")
    .filter((l) => l.trim() !== "")
    .map((l) => JSON.parse(l) as OutputRecord);
}

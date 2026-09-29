import { expect, test } from "bun:test";
import { ChannelLines, eventLine, lineText, luxEventLines, systemLines } from "./outputLines.ts";

const T = Date.UTC(2026, 8, 29, 10, 0, 0);

test("a multi-line lux input event is one LogLine per line, all at its time", () => {
  const lines = luxEventLines({ id: 4, type: "input", epoch: 1, time: new Date(T).toISOString(), data: { text: "fix the bug\n\nin scheduler.go\r\nthen run the tests" } });
  expect(lines.map((l) => l.text)).toEqual(["lux: input (epoch 1) input: fix the bug", "", "in scheduler.go", "then run the tests"]);
  expect(lines.every((l) => l.ts === T && l.stream === "system")).toBe(true);
});

test("a multi-line event record is one LogLine per line", () => {
  const lines = systemLines(T, eventLine({ type: "tool", data: { note: "a\nb" } }));
  expect(lines.map((l) => l.text)).toEqual(["[tool] note=a", "b"]);
});

test("\\r\\n leaves no \\r; a bare \\r keeps what was written after it", () => {
  expect(lineText("done\r")).toBe("done");
  expect(lineText("10%\r50%\r100%")).toBe("100%");
  expect(lineText("10%\r100%\r")).toBe("100%");
  const ch = new ChannelLines("stdout");
  expect(ch.push(T, "one\r\ntwo\r").map((l) => l.text)).toEqual(["one"]);
  expect(ch.push(T + 1, "\nthree").map((l) => l.text)).toEqual(["two"]);
  expect(ch.flush().map((l) => l.text)).toEqual(["three"]);
});

test("a line split across records starts at its first record's time", () => {
  const ch = new ChannelLines("stderr");
  expect(ch.push(T, "par")).toEqual([]);
  expect(ch.push(T + 5, "tial\nnext\n")).toEqual([
    { ts: T, stream: "stderr", text: "partial" },
    { ts: T + 5, stream: "stderr", text: "next" },
  ]);
  expect(ch.flush()).toEqual([]);
});

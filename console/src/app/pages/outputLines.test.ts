import { expect, test } from "bun:test";
import { ANSI_DIM } from "@lux/design-system";
import { ChannelLines, eventLine, luxEventLines, systemLines } from "./outputLines.ts";

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
  const texts = (s: string) => systemLines(T, s).map((l) => l.text);
  expect(texts("done\r")).toEqual(["done"]);
  expect(texts("10%\r50%\r100%")).toEqual(["100%"]);
  expect(texts("10%\r100%\r")).toEqual(["100%"]);
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

test("stderr from opencode: plain visible text, and a colour opened in one record reaches the lines of the next", () => {
  const ch = new ChannelLines("stderr");
  const first = ch.push(T, "\x1b[0m\x1b[31mError handling request {\n  \x1b[0mid\x1b[2m:\x1b[0m \x1b[0m\x1b[33m3\x1b[0m,\n  \x1b[36mmethod");
  expect(first.map((l) => l.text)).toEqual(["Error handling request {", "  id: 3,"]);
  expect(first[0]!.spans).toEqual([{ text: "Error handling request {", fg: 1 }]);
  expect(first[1]!.spans).toEqual([{ text: "  ", fg: 1 }, { text: "id" }, { text: ":", flags: ANSI_DIM }, { text: " " }, { text: "3", fg: 3 }, { text: "," }]);
  const second = ch.push(T + 1, ": x\n}\x1b[0m\n");
  expect(second.map((l) => l.text)).toEqual(["  method: x", "}"]);
  expect(second[0]!.spans).toEqual([{ text: "  " }, { text: "method: x", fg: 6 }]);
  expect(second[1]!.spans).toEqual([{ text: "}", fg: 6 }]);
});

test("partial flush preserves fragmented ESC, CSI and OSC without printing their tails", () => {
  for (const [prefix, suffix, fg] of [["\x1b", "[31mred\x1b[0m\n", 1], ["\x1b[3", "1mred\x1b[0m\n", 1], ["\x1b", "]0;title\x07red\n", undefined], ["\x1b]0;ti", "tle\x07red\n", undefined]] as const) {
    const ch = new ChannelLines("stdout");
    ch.push(1, `before${prefix}`);
    expect(ch.flush()).toEqual([{ ts: 1, stream: "stdout", text: "before" }]);
    const next = ch.push(2, suffix);
    expect(next[0]!.text).toBe("red");
    expect(next[0]!.spans).toEqual(fg === undefined ? undefined : [{ text: "red", fg }]);
  }
});

test("a system line's escapes never reach its text", () => {
  const lines = systemLines(T, "\x1b]0;title\x07\x1b[1mbold\nstill bold\x1b[22m plain");
  expect(lines.map((l) => l.text)).toEqual(["bold", "still bold plain"]);
  expect(lines[1]!.spans?.map((s) => s.flags ?? 0)).toEqual([1, 0]);
});

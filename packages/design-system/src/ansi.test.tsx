import { expect, test } from "bun:test";
import { renderToStaticMarkup } from "react-dom/server";
import { ANSI_BOLD, ANSI_DIM, ANSI_INVERSE, ANSI_ITALIC, ANSI_UNDERLINE, AnsiDecoder, ansiColorCss, decodeAnsiLine } from "./ansi.ts";
import { LogView, logLineContent } from "./LogView.tsx";

const SAMPLE = "\x1b[0m\x1b[31mError handling request {\n  \x1b[0mid\x1b[2m:\x1b[0m \x1b[0m\x1b[33m3\x1b[0m...";

function decodeAll(text: string) {
  const d = new AnsiDecoder();
  return text.split("\n").map((l) => d.line(l));
}

test("the opencode sample: visible text only, red header, dim colon, yellow id", () => {
  const [a, b] = decodeAll(SAMPLE);
  expect(a).toEqual({ text: "Error handling request {", spans: [{ text: "Error handling request {", fg: 1 }] });
  expect(b!.text).toBe("  id: 3...");
  expect(b!.spans).toEqual([{ text: "  ", fg: 1 }, { text: "id" }, { text: ":", flags: ANSI_DIM }, { text: " " }, { text: "3", fg: 3 }, { text: "..." }]);
});

test("a style opened on one line and closed on a later one styles every line between", () => {
  const lines = decodeAll("\x1b[1;32mone\ntwo\nthr\x1b[0mee");
  expect(lines.map((l) => l.spans)).toEqual([
    [{ text: "one", fg: 2, flags: ANSI_BOLD }],
    [{ text: "two", fg: 2, flags: ANSI_BOLD }],
    [{ text: "thr", fg: 2, flags: ANSI_BOLD }, { text: "ee" }],
  ]);
});

test("256-colour and truecolor, both layers, semicolon and colon forms", () => {
  expect(decodeAnsiLine("\x1b[38;5;208ma\x1b[48;5;21mb").spans).toEqual([
    { text: "a", fg: 208 },
    { text: "b", fg: 208, bg: 21 },
  ]);
  expect(decodeAnsiLine("\x1b[38;2;255;128;0ma\x1b[48;2;0;0;255mb\x1b[39mc\x1b[49md").spans).toEqual([
    { text: "a", fg: 256 + 0xff8000 },
    { text: "b", fg: 256 + 0xff8000, bg: 256 + 0x0000ff },
    { text: "c", bg: 256 + 0x0000ff },
    { text: "d" },
  ]);
  expect(decodeAnsiLine("\x1b[38:2::1:2:3mx\x1b[38:5:9my").spans).toEqual([
    { text: "x", fg: 256 + 0x010203 },
    { text: "y", fg: 9 },
  ]);
  // A colour's parameters are consumed with it: the 1 in 38;5;1 is not bold.
  expect(decodeAnsiLine("\x1b[38;5;1mz").spans).toEqual([{ text: "z", fg: 1 }]);
});

test("16 colours, bright, and every attribute with its own reset", () => {
  expect(decodeAnsiLine("\x1b[91;104ma\x1b[37;40mb").spans).toEqual([
    { text: "a", fg: 9, bg: 12 },
    { text: "b", fg: 7, bg: 0 },
  ]);
  const all = ANSI_BOLD | ANSI_ITALIC | ANSI_UNDERLINE | ANSI_INVERSE;
  expect(decodeAnsiLine("\x1b[1;3;4;7ma\x1b[22mb\x1b[23mc\x1b[24md\x1b[27me\x1b[2mf\x1b[22mg").spans).toEqual([
    { text: "a", flags: all },
    { text: "b", flags: all & ~ANSI_BOLD },
    { text: "c", flags: ANSI_UNDERLINE | ANSI_INVERSE },
    { text: "d", flags: ANSI_INVERSE },
    { text: "e" },
    { text: "f", flags: ANSI_DIM },
    { text: "g" },
  ]);
  expect(decodeAnsiLine("\x1b[31mred\x1b[mplain").spans).toEqual([{ text: "red", fg: 1 }, { text: "plain" }]);
});

test("a CSI cut by a line break completes on the next line, as in a terminal; neither half prints", () => {
  const lines = decodeAll("before\x1b[3\n1mafter\x1b[0\n;1mbold");
  expect(lines).toEqual([
    { text: "before" },
    { text: "after", spans: [{ text: "after", fg: 1 }] },
    { text: "bold", spans: [{ text: "bold", flags: ANSI_BOLD }] },
  ]);
  // A lone ESC at a line's end is dropped, not joined to the next line's first character.
  expect(decodeAll("a\x1b\nbc").map((l) => l.text)).toEqual(["a", "bc"]);
});

test("non-SGR CSI, OSC and other escapes are stripped, never printed", () => {
  expect(decodeAnsiLine("\x1b[2K\x1b[1Gprogress\x1b[?25l\x1b[3;4H done\x1b[?25h")).toEqual({ text: "progress done" });
  expect(decodeAnsiLine("\x1b]0;my title\x07shell$ \x1b]8;;https://x.test\x1b\\link\x1b]8;;\x1b\\")).toEqual({ text: "shell$ link" });
  expect(decodeAnsiLine("\x1b(Bascii\x1b=keypad\x1bMup\x1bPdcs\x1b\\end")).toEqual({ text: "asciikeypadupend" });
  expect(decodeAll("before\x1b]0;hidden\ntitle\x07after").map((l) => l.text)).toEqual(["before", "after"]);
  expect(decodeAll("a\x1bPsecret\nescape\x1b\n\\b").map((l) => l.text)).toEqual(["a", "", "b"]);
  expect(decodeAnsiLine("bell\x07 back\x08 nul\x00")).toEqual({ text: "bell back nul" });
});

test("\\r: a trailing one is dropped; text after one replaces the line, escapes after one do not", () => {
  expect(decodeAnsiLine("done\r")).toEqual({ text: "done" });
  expect(decodeAnsiLine("\x1b[32m10%\r50%\r\x1b[1m100%")).toEqual({ text: "100%", spans: [{ text: "100%", fg: 2, flags: ANSI_BOLD }] });
  expect(decodeAnsiLine("\x1b[31mfail\r\x1b[0m")).toEqual({ text: "fail", spans: [{ text: "fail", fg: 1 }] });
});

test("a lone or truncated ESC at the end neither throws nor prints", () => {
  for (const tail of ["\x1b", "\x1b[", "\x1b[38;5", "\x1b]0;title", "\x1b(", "\x1b[31"]) {
    expect(decodeAnsiLine(`ok${tail}`)).toEqual({ text: "ok" });
  }
  expect(decodeAnsiLine("\x1b")).toEqual({ text: "" });
  expect(decodeAnsiLine("a\x1b\x1b[31mb").spans).toEqual([{ text: "a" }, { text: "b", fg: 1 }]);
});

test("colours come from theme tokens; explicit RGB is clamped into the theme's band", () => {
  expect(ansiColorCss(1, "fg")).toBe("var(--ansi-red)");
  expect(ansiColorCss(12, "bg")).toBe("var(--ansi-bg-bright-blue)");
  expect(ansiColorCss(196, "fg")).toBe("oklch(from rgb(255 0 0) clamp(var(--ansi-fg-l-min), l, var(--ansi-fg-l-max)) c h)");
  expect(ansiColorCss(244, "bg")).toBe("oklch(from rgb(128 128 128) clamp(var(--ansi-bg-l-min), l, var(--ansi-bg-l-max)) c h)");
  expect(ansiColorCss(256 + 0x102030, "fg")).toContain("rgb(16 32 48)");
});

test("LogView renders escapes as styled spans, and its text copies without them", () => {
  const html = renderToStaticMarkup(<LogView lines={[{ stream: "stderr", text: "\x1b[1;31mfail\x1b[0m: \x1b]0;t\x07done" }]} height={100} />);
  expect(html).not.toContain("\x1b");
  expect(html).not.toContain("[0m");
  expect(html).toContain('<span class="ansi-bold" style="color:var(--ansi-red)">fail</span>');
  const text = html.replace(/<[^>]+>/g, "");
  expect(text).toContain("fail: done");
});

test("LogView uses a line's decoded spans as given", () => {
  const html = renderToStaticMarkup(<>{logLineContent({ stream: "stdout", text: "ab", spans: [{ text: "a", fg: 2 }, { text: "b" }] })}</>);
  expect(html).toBe('<span style="color:var(--ansi-green)">a</span>b');
  const raw = renderToStaticMarkup(<>{logLineContent({ stream: "stdout", text: "first\nsecond" })}</>);
  expect(raw.replace(/<[^>]+>/g, "")).toBe("first\nsecond");
});

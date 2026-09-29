// ANSI escape sequences in log text: SGR styling becomes spans, every other
// escape sequence and C0 control is dropped. Decoding is one linear pass per
// line; an AnsiDecoder carries the style from one line to the next, so a
// colour opened on one line and closed on a later one styles every line
// between.

export const ANSI_BOLD = 1;
export const ANSI_DIM = 2;
export const ANSI_ITALIC = 4;
export const ANSI_UNDERLINE = 8;
export const ANSI_INVERSE = 16;

/**
 * A colour: 0–15 the themed ANSI colours, 16–255 the xterm 256-colour
 * palette, 256 + 0xRRGGBB a truecolor.
 */
export type AnsiColor = number;

export interface AnsiSpan {
  text: string;
  fg?: AnsiColor;
  bg?: AnsiColor;
  /** ANSI_BOLD | ANSI_DIM | … */
  flags?: number;
}

export interface AnsiLine {
  /** The visible text, no escapes. */
  text: string;
  /** Present only when some of the text is styled; their texts concatenate to `text`. */
  spans?: AnsiSpan[];
}

const ESC = 0x1b;
// The start of a CSI with its parameter and intermediate bytes, and the most of it carried to the next line.
const CSI_PREFIX = /^\x1b\[[\x20-\x3f]*$/;
const MAX_CARRY = 64;

/** Stateful decoder for one stream: feed it its lines in order. */
export class AnsiDecoder {
  private fg: AnsiColor | undefined;
  private bg: AnsiColor | undefined;
  private flags = 0;
  private carry = "";
  private discardString = false;
  private stringEsc = false;

  /** Decodes one line (no "\n"). Text after a "\r" replaces what came before it, as a terminal's progress line reads once redrawn. */
  line(raw: string): AnsiLine {
    const s = this.carry ? this.carry + raw : raw;
    this.carry = "";
    const spans: AnsiSpan[] = [];
    let seg = "";
    let styled = false;
    let overwrite = false; // a "\r" was seen and no text has followed it yet
    const add = (t: string) => {
      if (t === "") return;
      if (overwrite) {
        seg = "";
        spans.length = 0;
        styled = false;
        overwrite = false;
      }
      seg += t;
    };
    const close = () => {
      if (seg === "") return;
      const span: AnsiSpan = { text: seg };
      if (this.fg !== undefined) span.fg = this.fg;
      if (this.bg !== undefined) span.bg = this.bg;
      if (this.flags) span.flags = this.flags;
      if (this.fg !== undefined || this.bg !== undefined || this.flags) styled = true;
      spans.push(span);
      seg = "";
    };
    const n = s.length;
    let i = 0;
    let run = 0; // start of the plain text not yet added
    while (i < n) {
      const c = s.charCodeAt(i);
      if (this.discardString) {
        if (c === 0x07 || (this.stringEsc && c === 0x5c)) this.discardString = false;
        this.stringEsc = c === ESC;
        run = ++i;
        continue;
      }
      if (c >= 0x20 && c !== 0x7f) {
        i++;
        continue;
      }
      add(s.slice(run, i));
      if (c === 0x09) add("\t");
      else if (c === 0x0d) overwrite = true;
      else if (c === ESC) {
        const end = this.escape(s, i, close);
        if (end < 0) {
          // Cut by the line break: an unfinished CSI completes on the next line (a
          // terminal runs the LF inside it); anything else is dropped, never printed.
          const rest = s.slice(i);
          if (rest.length <= MAX_CARRY && CSI_PREFIX.test(rest)) this.carry = rest;
          run = i = n;
          break;
        }
        run = i = end;
        continue;
      }
      run = ++i;
    }
    add(s.slice(run, i));
    close();
    const text = spans.length === 1 ? spans[0]!.text : spans.map((sp) => sp.text).join("");
    return styled ? { text, spans } : { text };
  }

  // The index after the escape starting at s[i], or -1 when s ends inside it.
  private escape(s: string, i: number, close: () => void): number {
    const n = s.length;
    if (i + 1 >= n) return -1;
    const k = s.charCodeAt(i + 1);
    if (k === 0x5b) {
      // CSI: parameter bytes 0x30–0x3F, intermediates 0x20–0x2F, one final byte 0x40–0x7E.
      let j = i + 2;
      while (j < n) {
        const b = s.charCodeAt(j);
        if (b >= 0x40 && b <= 0x7e) {
          if (b === 0x6d) {
            close();
            this.sgr(s.slice(i + 2, j));
          }
          return j + 1;
        }
        if (b < 0x20 || b > 0x3f) return j; // malformed: drop what was read, keep the byte
        j++;
      }
      return -1;
    }
    if (k === 0x5d || k === 0x50 || k === 0x58 || k === 0x5e || k === 0x5f) {
      // OSC, DCS, SOS, PM, APC: a string up to BEL or ST (ESC \).
      // Discard state survives a line break; none of the payload becomes text.
      this.discardString = true;
      this.stringEsc = false;
      return i + 2;
    }
    // Any other escape: intermediates 0x20–0x2F, then one final byte; a control character there ends it unconsumed.
    let j = i + 1;
    while (j < n && s.charCodeAt(j) >= 0x20 && s.charCodeAt(j) <= 0x2f) j++;
    if (j >= n) return -1;
    const f = s.charCodeAt(j);
    return f < 0x20 || f === 0x7f ? j : j + 1;
  }

  private sgr(params: string): void {
    if (params === "") {
      this.fg = this.bg = undefined;
      this.flags = 0;
      return;
    }
    const ps = params.split(";");
    for (let p = 0; p < ps.length; p++) {
      const part = ps[p]!;
      if (part.includes(":")) {
        // Colon sub-parameters: 38:5:n, 38:2::r:g:b (or 38:2:r:g:b), 4:n.
        const sub = part.split(":").map((x) => (x === "" ? -1 : Number(x)));
        const code = sub[0];
        if (code === 38 || code === 48) {
          const rgb = sub[1] === 2 ? (sub.length >= 6 ? sub.slice(3, 6) : sub.slice(2, 5)) : null;
          const col = sub[1] === 5 ? palette(sub[2]) : rgb ? truecolor(rgb[0], rgb[1], rgb[2]) : undefined;
          if (col !== undefined) this.setColor(code, col);
        } else if (code === 4) {
          this.flags = sub[1] === 0 ? this.flags & ~ANSI_UNDERLINE : this.flags | ANSI_UNDERLINE;
        }
        continue;
      }
      const code = part === "" ? 0 : Number(part);
      if (code === 38 || code === 48) {
        const mode = Number(ps[p + 1]);
        if (mode === 5) {
          const col = palette(Number(ps[p + 2]));
          if (col !== undefined) this.setColor(code, col);
          p += 2;
        } else if (mode === 2) {
          const col = truecolor(Number(ps[p + 2]), Number(ps[p + 3]), Number(ps[p + 4]));
          if (col !== undefined) this.setColor(code, col);
          p += 4;
        } else p += 1;
        continue;
      }
      this.basic(code);
    }
  }

  private setColor(code: number, col: AnsiColor): void {
    if (code === 38) this.fg = col;
    else this.bg = col;
  }

  private basic(code: number): void {
    if (code === 0) {
      this.fg = this.bg = undefined;
      this.flags = 0;
    } else if (code === 1) this.flags |= ANSI_BOLD;
    else if (code === 2) this.flags |= ANSI_DIM;
    else if (code === 3) this.flags |= ANSI_ITALIC;
    else if (code === 4 || code === 21) this.flags |= ANSI_UNDERLINE;
    else if (code === 7) this.flags |= ANSI_INVERSE;
    else if (code === 22) this.flags &= ~(ANSI_BOLD | ANSI_DIM);
    else if (code === 23) this.flags &= ~ANSI_ITALIC;
    else if (code === 24) this.flags &= ~ANSI_UNDERLINE;
    else if (code === 27) this.flags &= ~ANSI_INVERSE;
    else if (code >= 30 && code <= 37) this.fg = code - 30;
    else if (code === 39) this.fg = undefined;
    else if (code >= 40 && code <= 47) this.bg = code - 40;
    else if (code === 49) this.bg = undefined;
    else if (code >= 90 && code <= 97) this.fg = code - 90 + 8;
    else if (code >= 100 && code <= 107) this.bg = code - 100 + 8;
  }
}

function palette(n: number | undefined): AnsiColor | undefined {
  return n !== undefined && Number.isInteger(n) && n >= 0 && n <= 255 ? n : undefined;
}

function truecolor(r: number | undefined, g: number | undefined, b: number | undefined): AnsiColor | undefined {
  const ok = (v: number | undefined): v is number => v !== undefined && Number.isInteger(v) && v >= 0 && v <= 255;
  return ok(r) && ok(g) && ok(b) ? 256 + ((r << 16) | (g << 8) | b) : undefined;
}

/** One line on its own (no state before it): its visible text and spans. */
export function decodeAnsiLine(raw: string): AnsiLine {
  return new AnsiDecoder().line(raw);
}

/** Whether a line needs decoding at all: it holds an escape or another control character. */
export function hasAnsi(text: string): boolean {
  return /[\x00-\x08\x0a-\x1f\x7f]/.test(text);
}

const NAMES = ["black", "red", "green", "yellow", "blue", "magenta", "cyan", "white"];
const CUBE = [0, 95, 135, 175, 215, 255];

/** 16–255 as xterm draws them. */
function xterm256(n: number): [number, number, number] {
  if (n >= 232) {
    const v = 8 + (n - 232) * 10;
    return [v, v, v];
  }
  const i = n - 16;
  return [CUBE[Math.floor(i / 36)]!, CUBE[Math.floor(i / 6) % 6]!, CUBE[i % 6]!];
}

/**
 * The CSS colour for an ANSI colour, as text ("fg") or background ("bg").
 * 0–15 are theme tokens (--ansi-*, --ansi-bg-*). An explicit RGB keeps its
 * hue and chroma, with its OKLCH lightness clamped into the theme's readable
 * band for that layer (--ansi-{fg,bg}-l-{min,max}).
 */
export function ansiColorCss(c: AnsiColor, layer: "fg" | "bg"): string {
  if (c < 16) return `var(--ansi-${layer === "bg" ? "bg-" : ""}${c >= 8 ? "bright-" : ""}${NAMES[c % 8]})`;
  const [r, g, b] = c < 256 ? xterm256(c) : [(c - 256) >> 16, ((c - 256) >> 8) & 0xff, (c - 256) & 0xff];
  return `oklch(from rgb(${r} ${g} ${b}) clamp(var(--ansi-${layer}-l-min), l, var(--ansi-${layer}-l-max)) c h)`;
}

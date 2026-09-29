// The terminal's own palette: Solarized, exact, one set per console theme.
// Everything else on the page is the design tokens'; the screen inside the
// terminal frame is Solarized so what a shell prints looks as it does in
// any Solarized terminal. The CSS side (frame, scrollbar) is --term-* in
// tokens.css, from the same values.
import type { ITheme } from "@xterm/xterm";

const base03 = "#002b36";
const base02 = "#073642";
const base01 = "#586e75";
const base00 = "#657b83";
const base0 = "#839496";
const base1 = "#93a1a1";
const base2 = "#eee8d5";
const base3 = "#fdf6e3";
const yellow = "#b58900";
const orange = "#cb4b16";
const red = "#dc322f";
const magenta = "#d33682";
const violet = "#6c71c4";
const blue = "#268bd2";
const cyan = "#2aa198";
const green = "#859900";

/* The eight ANSI colours are the same in both; the bright slots carry the
   base tones, as Solarized's terminal mapping has them. */
const ansi = { red, green, yellow, blue, magenta, cyan, brightRed: orange, brightMagenta: violet };

export const terminalThemes: Record<"dark" | "light", ITheme> = {
  dark: {
    ...ansi,
    background: base03,
    foreground: base0,
    cursor: base1,
    cursorAccent: base03,
    selectionBackground: base02,
    selectionForeground: base1,
    selectionInactiveBackground: base02,
    scrollbarSliderBackground: `${base01}66`,
    scrollbarSliderHoverBackground: `${base01}99`,
    scrollbarSliderActiveBackground: `${base01}cc`,
    black: base02,
    white: base2,
    brightBlack: base03,
    brightGreen: base01,
    brightYellow: base00,
    brightBlue: base0,
    brightCyan: base1,
    brightWhite: base3,
  },
  light: {
    ...ansi,
    background: base3,
    foreground: base00,
    cursor: base01,
    cursorAccent: base3,
    selectionBackground: base2,
    selectionForeground: base01,
    selectionInactiveBackground: base2,
    scrollbarSliderBackground: `${base1}66`,
    scrollbarSliderHoverBackground: `${base1}99`,
    scrollbarSliderActiveBackground: `${base1}cc`,
    black: base02,
    white: base2,
    brightBlack: base03,
    brightGreen: base01,
    brightYellow: base00,
    brightBlue: base0,
    brightCyan: base1,
    brightWhite: base3,
  },
};

/** The Solarized values by name, for the gallery and anything drawing beside a terminal. */
export const solarized = { base03, base02, base01, base00, base0, base1, base2, base3, yellow, orange, red, magenta, violet, blue, cyan, green };

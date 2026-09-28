import { useEffect, useImperativeHandle, useLayoutEffect, useRef, type ReactNode, type Ref } from "react";
import { Terminal as XTerm } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";
import { WebLinksAddon } from "@xterm/addon-web-links";
import { Unicode11Addon } from "@xterm/addon-unicode11";
import { WebglAddon } from "@xterm/addon-webgl";
import "@xterm/xterm/css/xterm.css";
import { terminalThemes } from "./terminalThemes.ts";
import { cssVar, useTheme } from "./theme.ts";

export interface TerminalSize {
  cols: number;
  rows: number;
}

/** What the page holds to drive the terminal: the transport-facing half of the API. */
export interface TerminalHandle {
  /** Bytes (UTF-8) or text from the remote end, onto the screen. */
  write: (data: string | Uint8Array) => void;
  /** Wipe the screen and scrollback (a new shell). */
  reset: () => void;
  focus: () => void;
  /** The current grid, for the open message. */
  size: () => TerminalSize;
}

export interface TerminalProps {
  ref?: Ref<TerminalHandle>;
  /** Keystrokes and pastes, as text; the transport encodes them. */
  onData?: (data: string) => void;
  /** The grid changed (the frame or the font size did). */
  onResize?: (size: TerminalSize) => void;
  /** Called once the terminal is open and fitted, with its grid. */
  onReady?: (size: TerminalSize) => void;
  fontSize?: number;
  /** No input, dimmed screen, still cursor: the shell is gone. */
  disabled?: boolean;
  /** A card over the dimmed screen (TerminalOverlay). */
  overlay?: ReactNode;
  /** The status strip under the screen. */
  bar?: ReactNode;
  className?: string;
  "aria-label"?: string;
}

export const DEFAULT_FONT_SIZE = 13;

/**
 * An xterm.js terminal in the console's frame (LogView's inset, hairline
 * and radius; a status bar under the screen), Solarized inside, following
 * the console theme. Transport-agnostic: the page feeds it bytes through
 * the handle and gets keystrokes and resizes back. WebGL rendering when the
 * browser has it, xterm's DOM renderer otherwise.
 */
export function Terminal({ ref, onData, onResize, onReady, fontSize = DEFAULT_FONT_SIZE, disabled = false, overlay, bar, className, "aria-label": ariaLabel = "Terminal" }: TerminalProps) {
  const host = useRef<HTMLDivElement>(null);
  const term = useRef<XTerm | null>(null);
  const fitter = useRef<FitAddon | null>(null);
  const scheme = useTheme().resolved;
  // Callbacks change every render; the terminal is built once.
  const cbs = useRef({ onData, onResize, onReady });
  cbs.current = { onData, onResize, onReady };

  useLayoutEffect(() => {
    const el = host.current;
    if (!el) return;
    const t = new XTerm({
      allowProposedApi: true,
      cursorBlink: true,
      cursorStyle: "block",
      cursorInactiveStyle: "outline",
      fontFamily: cssVar("--font-mono") || "monospace",
      fontSize,
      lineHeight: 1.3,
      scrollback: 5000,
      theme: terminalThemes[scheme],
      // xterm 6 renders mixed-width glyphs itself; Solarized is contrast enough.
      minimumContrastRatio: 1,
      macOptionIsMeta: true,
      scrollOnUserInput: true,
    });
    const fit = new FitAddon();
    t.loadAddon(fit);
    t.loadAddon(new Unicode11Addon());
    t.unicode.activeVersion = "11";
    t.loadAddon(new WebLinksAddon((_e, uri) => window.open(uri, "_blank", "noopener")));
    t.open(el);
    // GPU rendering where it works; on a lost context, back to the DOM renderer.
    try {
      const gl = new WebglAddon();
      gl.onContextLoss(() => gl.dispose());
      t.loadAddon(gl);
    } catch {}
    // Ctrl+Shift+C copies the selection (Ctrl+C is the shell's); the
    // browser handles Ctrl+Shift+V as a paste into xterm's textarea.
    t.attachCustomKeyEventHandler((ev) => {
      if (ev.type === "keydown" && ev.ctrlKey && ev.shiftKey && (ev.key === "C" || ev.key === "c")) {
        // Ours, not the browser's (DevTools) nor xterm's.
        ev.preventDefault();
        ev.stopPropagation();
        const s = t.getSelection();
        if (s) void navigator.clipboard?.writeText(s);
        return false;
      }
      return true;
    });
    const subs = [t.onData((d) => cbs.current.onData?.(d)), t.onResize((s) => cbs.current.onResize?.(s))];
    term.current = t;
    fitter.current = fit;
    fit.fit();
    cbs.current.onReady?.({ cols: t.cols, rows: t.rows });
    // Refit as the frame changes (a sidebar collapse, a window resize).
    const ro = new ResizeObserver(() => fit.fit());
    ro.observe(el);
    return () => {
      ro.disconnect();
      subs.forEach((s) => s.dispose());
      t.dispose();
      term.current = null;
      fitter.current = null;
    };
    // Built once: font size, theme and input are applied as options below.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  useEffect(() => {
    const t = term.current;
    if (!t) return;
    t.options.theme = terminalThemes[scheme];
  }, [scheme]);

  useEffect(() => {
    const t = term.current;
    if (!t) return;
    t.options.fontSize = fontSize;
    fitter.current?.fit();
  }, [fontSize]);

  useEffect(() => {
    const t = term.current;
    if (!t) return;
    t.options.disableStdin = disabled;
    t.options.cursorBlink = !disabled;
    if (!disabled) t.focus();
  }, [disabled]);

  useImperativeHandle(
    ref,
    () => ({
      write: (data) => term.current?.write(data),
      reset: () => term.current?.reset(),
      focus: () => term.current?.focus(),
      size: () => ({ cols: term.current?.cols ?? 80, rows: term.current?.rows ?? 24 }),
    }),
    [],
  );

  return (
    <div className={["term", disabled ? "is-dimmed" : "", className ?? ""].join(" ").trim()} aria-label={ariaLabel}>
      <div ref={host} className="term-screen" />
      {overlay && <div className="term-overlay">{overlay}</div>}
      {bar && <div className="term-bar">{bar}</div>}
    </div>
  );
}

export interface TerminalOverlayProps {
  icon?: ReactNode;
  /** Amber icon: something went wrong, not just ended. */
  warn?: boolean;
  title: ReactNode;
  description?: ReactNode;
  actions?: ReactNode;
  /** A last line in the muted colour: how long it lasted, what was recorded. */
  meta?: ReactNode;
}

/** The card over a dimmed terminal: the shell exited, the connection was lost. */
export function TerminalOverlay({ icon, warn, title, description, actions, meta }: TerminalOverlayProps) {
  return (
    <div className="card" role="status">
      <div className="card-body term-overlay-body">
        <div className="overlay-title">
          {icon && <span className={warn ? "overlay-icon is-warn" : "overlay-icon"}>{icon}</span>}
          {title}
        </div>
        {description && <div className="overlay-desc">{description}</div>}
        {actions && <div className="overlay-actions">{actions}</div>}
        {meta && <div className="overlay-meta">{meta}</div>}
      </div>
    </div>
  );
}

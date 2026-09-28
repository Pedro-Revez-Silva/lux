import { useId, useLayoutEffect, useRef, useState, type CSSProperties, type ReactNode } from "react";

export interface TooltipProps {
  content: ReactNode;
  /** Preferred side; flipped to the opposite one when it would leave the viewport. */
  side?: "top" | "bottom" | "left" | "right";
  children: ReactNode;
}

type Side = NonNullable<TooltipProps["side"]>;

/** Gap kept between a tooltip and the viewport edge, in px. */
const EDGE = 8;
const OPPOSITE: Record<Side, Side> = { top: "bottom", bottom: "top", left: "right", right: "left" };

/**
 * Hover/focus tooltip. CSS-positioned, no portal (keep it off overflow-clipped
 * parents). Measured when it opens: it flips to the opposite side when its
 * side has no room, and shifts along that side to stay inside the viewport.
 */
export function Tooltip({ content, side = "top", children }: TooltipProps) {
  const id = useId();
  const [open, setOpen] = useState(false);
  const [placed, setPlaced] = useState<{ side: Side; shift: number }>({ side, shift: 0 });
  const tip = useRef<HTMLSpanElement>(null);

  useLayoutEffect(() => {
    if (!open || !tip.current) return;
    // Runs before paint, on the preferred side unshifted.
    const r = tip.current.getBoundingClientRect();
    const vw = document.documentElement.clientWidth;
    const vh = document.documentElement.clientHeight;
    const out: Record<Side, boolean> = { top: r.top < EDGE, bottom: r.bottom > vh - EDGE, left: r.left < EDGE, right: r.right > vw - EDGE };
    const flipped = out[side] ? OPPOSITE[side] : side;
    const vertical = side === "top" || side === "bottom";
    const [lo, hi, max] = vertical ? [r.left, r.right, vw] : [r.top, r.bottom, vh];
    let shift = 0;
    if (hi > max - EDGE) shift = max - EDGE - hi;
    if (lo + shift < EDGE) shift = EDGE - lo;
    setPlaced({ side: flipped, shift });
  }, [open, side]);

  const show = (v: boolean) => {
    if (v) setPlaced({ side, shift: 0 });
    setOpen(v);
  };

  return (
    <span className="tip-anchor" onMouseEnter={() => show(true)} onMouseLeave={() => show(false)} onFocus={() => show(true)} onBlur={() => show(false)} aria-describedby={open ? id : undefined}>
      {children}
      {open && (
        <span ref={tip} role="tooltip" id={id} className={`tip tip-${placed.side}`} data-side={placed.side} style={{ "--tip-shift": `${placed.shift}px` } as CSSProperties}>
          {content}
        </span>
      )}
    </span>
  );
}

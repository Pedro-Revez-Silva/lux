import { useId, useState, type ReactNode } from "react";

export interface TooltipProps {
  content: ReactNode;
  side?: "top" | "bottom" | "left" | "right";
  children: ReactNode;
}

/** Hover/focus tooltip. CSS-positioned; no portal, so keep it off overflow-clipped parents. */
export function Tooltip({ content, side = "top", children }: TooltipProps) {
  const id = useId();
  const [open, setOpen] = useState(false);
  return (
    <span
      className="tip-anchor"
      onMouseEnter={() => setOpen(true)}
      onMouseLeave={() => setOpen(false)}
      onFocus={() => setOpen(true)}
      onBlur={() => setOpen(false)}
      aria-describedby={open ? id : undefined}
    >
      {children}
      {open && (
        <span role="tooltip" id={id} className={`tip tip-${side}`}>
          {content}
        </span>
      )}
    </span>
  );
}

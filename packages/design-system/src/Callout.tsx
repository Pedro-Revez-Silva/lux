import type { ReactNode } from "react";

export interface CalloutProps {
  children: ReactNode;
  /** An icon before the sentence (`<IconInfo size={14} />`). */
  icon?: ReactNode;
  className?: string;
}

/**
 * One sentence that explains a page ("Pool cost is the machines only…"):
 * a soft info-tinted box between cards. Not a toast, not an error strip,
 * and not InfoStrip, which qualifies figures inside one card.
 */
export function Callout({ children, icon, className }: CalloutProps) {
  return (
    <div className={["callout", className ?? ""].join(" ").trim()} role="note">
      {icon && <span className="callout-icon">{icon}</span>}
      <span className="callout-text">{children}</span>
    </div>
  );
}

import type { ReactNode } from "react";
import { IconInbox } from "./icons.tsx";

export interface EmptyStateProps {
  title: ReactNode;
  description?: ReactNode;
  icon?: ReactNode;
  action?: ReactNode;
  /** Small variant for table bodies and panels. */
  compact?: boolean;
}

export function EmptyState({ title, description, icon, action, compact }: EmptyStateProps) {
  return (
    <div className={compact ? "empty empty-compact" : "empty"}>
      <div className="empty-icon">{icon ?? <IconInbox size={compact ? 18 : 24} />}</div>
      <div className="empty-title">{title}</div>
      {description && <div className="empty-desc">{description}</div>}
      {action && <div className="empty-action">{action}</div>}
    </div>
  );
}

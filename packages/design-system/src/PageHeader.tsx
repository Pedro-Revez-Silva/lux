import type { ReactNode } from "react";
import { IconChevronRight } from "./icons.tsx";

export interface PageHeaderProps {
  /** A breadcrumb row above the title, for a page under an object (a run's terminal). */
  crumbs?: ReactNode;
  /** The page or object name. Plain text for pages; a name (or a mono id when unnamed) for objects. */
  title: ReactNode;
  /** Pills and badges shown beside the title. */
  badges?: ReactNode;
  /** One line of context under the title: ids, links, counts. */
  description?: ReactNode;
  /** A longer note (e.g. a state reason), set off from the description. */
  note?: ReactNode;
  /** Primary actions, on the right (below the title on phones). */
  actions?: ReactNode;
  className?: string;
}

/** Every page opens with one: a clear title, one line of context, actions on the right. */
export function PageHeader({ crumbs, title, badges, description, note, actions, className }: PageHeaderProps) {
  return (
    <header className={["page-head", className ?? ""].join(" ").trim()}>
      <div className="page-head-main">
        {crumbs && (
          <nav className="crumbs" aria-label="Breadcrumb">
            {crumbs}
          </nav>
        )}
        <div className="page-head-title">
          <h1 className="page-title">{title}</h1>
          {badges}
        </div>
        {description && <div className="page-desc">{description}</div>}
        {note && <div className="page-note">{note}</div>}
      </div>
      {actions && <div className="page-head-actions">{actions}</div>}
    </header>
  );
}

/** The separator between breadcrumbs. */
export function CrumbSep() {
  return (
    <span className="crumbs-sep" aria-hidden="true">
      <IconChevronRight size={12} />
    </span>
  );
}

/** A quiet section heading between cards (uppercase label, optional note on the right). */
export function SectionHeader({ title, note, actions }: { title: ReactNode; note?: ReactNode; actions?: ReactNode }) {
  return (
    <div className="section-head">
      <h2 className="section-title">{title}</h2>
      {note && <span className="section-note">{note}</span>}
      {actions}
    </div>
  );
}

import type { ReactNode } from "react";

export interface PageHeaderProps {
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
}

/** Every page opens with one: a clear title, one line of context, actions on the right. */
export function PageHeader({ title, badges, description, note, actions }: PageHeaderProps) {
  return (
    <header className="page-head">
      <div className="page-head-main">
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

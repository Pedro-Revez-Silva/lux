import type { HTMLAttributes, ReactNode } from "react";

export interface CardProps extends Omit<HTMLAttributes<HTMLElement>, "title"> {
  title?: ReactNode;
  subtitle?: ReactNode;
  actions?: ReactNode;
  /** No body padding (tables, logs, charts that manage their own). */
  flush?: boolean;
  footer?: ReactNode;
  children?: ReactNode;
}

export function Card({ title, subtitle, actions, flush, footer, className, children, ...rest }: CardProps) {
  return (
    <section className={["card", className ?? ""].join(" ").trim()} {...rest}>
      {(title || actions) && (
        <header className="card-head">
          <div className="card-titles">
            {title && <h2 className="card-title">{title}</h2>}
            {subtitle && <p className="card-subtitle">{subtitle}</p>}
          </div>
          {actions && <div className="card-actions">{actions}</div>}
        </header>
      )}
      <div className={flush ? "card-body card-body-flush" : "card-body"}>{children}</div>
      {footer && <footer className="card-foot">{footer}</footer>}
    </section>
  );
}

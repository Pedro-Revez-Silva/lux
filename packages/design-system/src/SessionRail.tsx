import type { ReactNode } from "react";

/** The column beside a transcript; under 1000px of content it is not drawn (a subagent opens in a Dialog). */
export function SessionRail({ children }: { children: ReactNode }) {
  return <div className="session-rail">{children}</div>;
}

export interface RailCardProps {
  title: ReactNode;
  count?: ReactNode;
  /** Right of the title (a "2 running" pill). */
  aside?: ReactNode;
  /** No body padding (a tree that pads its own rows). */
  flush?: boolean;
  children: ReactNode;
}

/** One card of the rail: session facts, subagents, tools used. */
export function RailCard({ title, count, aside, flush, children }: RailCardProps) {
  return (
    <section className="rail-card">
      <div className="rail-card-head">
        {title}
        {count != null && <span className="muted num">{count}</span>}
        {aside && <span className="rail-card-aside">{aside}</span>}
      </div>
      {flush ? children : <div className="rail-card-body">{children}</div>}
    </section>
  );
}

/** Label/value pairs in the rail, one per line; values truncate with the full text in a title. */
export function RailFacts({ items }: { items: { key: string; value: ReactNode; mono?: boolean; title?: string }[] }) {
  return (
    <dl className="rail-facts">
      {items.map((i) => (
        <div key={i.key} className="rail-fact">
          <dt>{i.key}</dt>
          <dd className={i.mono ? "mono" : undefined} title={i.title}>
            {i.value}
          </dd>
        </div>
      ))}
    </dl>
  );
}

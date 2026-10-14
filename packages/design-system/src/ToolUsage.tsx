export interface ToolUsageProps {
  /** Calls per tool name. */
  counts: { name: string; count: number }[];
}

/** Tool calls per tool, most used first, one bar each in --chart-1 with its count beside it. */
export function ToolUsage({ counts }: ToolUsageProps) {
  const rows = [...counts].sort((a, b) => b.count - a.count || a.name.localeCompare(b.name));
  const max = rows[0]?.count || 1;
  return (
    <div className="toolusage">
      {rows.map((r) => (
        <div key={r.name} className="toolusage-row">
          <span className="toolusage-name" title={r.name}>
            {r.name}
          </span>
          <span className="toolusage-track" aria-hidden="true">
            <span className="toolusage-fill" style={{ width: `${(r.count / max) * 100}%` }} />
          </span>
          <span className="toolusage-n">{r.count}</span>
        </div>
      ))}
    </div>
  );
}

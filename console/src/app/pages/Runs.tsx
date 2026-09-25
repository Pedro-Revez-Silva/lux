import { useEffect, useMemo, useState, type FormEvent } from "react";
import { Button, Card, PageHeader, RUN_STATE_LIST, runStateStyle, Table } from "@lux/design-system";
import { api, errorText, useQuery, type Run, type RunListParams } from "../../api/index.ts";
import { go, Link, setSearchParams, useSearchParams } from "../router.tsx";
import { useScope, useScopedQuery } from "../scope.tsx";
import { ErrorBlock, ErrorStrip, hostPath, runColumns, runPath } from "./common.tsx";

const PAGE = 100;
/** The most rows loaded at once: past it, narrow the filters. */
const MAX = 1000;

/**
 * Runs list. Every filter lives in the URL (?state=a,b&resumable=true&host=&label=),
 * so "runs on host X" or "queued runs" are links. State chips select any of
 * those states; Resumable narrows to what resume accepts; all filters AND.
 */
export function Runs() {
  const scope = useScope();
  const params = useSearchParams();
  const states = useMemo(() => (params.get("state") ?? "").split(",").filter(Boolean), [params]);
  const resumable = params.get("resumable") === "true";
  const host = params.get("host") ?? "";
  const label = params.get("label") ?? "";

  const [hostDraft, setHostDraft] = useState(host);
  const [labelDraft, setLabelDraft] = useState(label);
  useEffect(() => setHostDraft(host), [host]);
  useEffect(() => setLabelDraft(label), [label]);

  const filter = useMemo<RunListParams>(
    () => ({ state: states.length ? states : undefined, resumable: resumable || undefined, host: host || undefined, label: label || undefined }),
    [states, resumable, host, label],
  );
  const filterKey = `${states.join(",")}|${resumable}|${host}|${label}`;

  // Only the newest page is polled. "Load more" pages further back (by
  // creation time) and keeps what it loaded, merged under the fresh first
  // page (which wins). A filter or tenant change starts over.
  const q = useScopedQuery(`runs:${filterKey}`, (t, s) => api.runs(t, { ...filter, limit: PAGE }, s), { interval: 5000, live: 60_000 });
  const olderKey = `${scope.tenant}|${filterKey}`;
  const [older, setOlder] = useState<{ key: string; runs: Run[]; full: boolean; loading: boolean; error: string | null }>({ key: olderKey, runs: [], full: false, loading: false, error: null });
  const olderHere = older.key === olderKey ? older : null;

  const runs = useMemo(() => {
    const first = q.data ?? [];
    if (!olderHere || olderHere.runs.length === 0) return first;
    // The fresh first page wins. A loaded row within its time span that it
    // no longer has left the filter (e.g. changed state): drop it.
    const seen = new Set(first.map((r) => r.id));
    const cutoff = first.length >= PAGE ? Date.parse(first[first.length - 1]!.createdAt) : -Infinity;
    return [...first, ...olderHere.runs.filter((r) => !seen.has(r.id) && Date.parse(r.createdAt) < cutoff)];
  }, [q.data, olderHere]);
  // A short first page is the whole list.
  const full = (q.data?.length ?? 0) >= PAGE && (olderHere && olderHere.runs.length > 0 ? olderHere.full : true);
  const more = full && runs.length < MAX;

  const loadMore = async () => {
    const last = runs[runs.length - 1];
    if (!last) return;
    const key = olderKey;
    const loaded = runs;
    setOlder((o) => ({ ...(o.key === key ? o : { runs: [], full: false }), key, loading: true, error: null }));
    try {
      const page = await api.runs(scope.apiTenant, { ...filter, before: last.createdAt, limit: PAGE });
      setOlder((o) => (o.key !== key ? o : { key, runs: [...loaded, ...page], full: page.length >= PAGE, loading: false, error: null }));
    } catch (e) {
      setOlder((o) => (o.key !== key ? o : { ...o, loading: false, error: errorText(e) }));
    }
  };

  // Show a host id filter by name (a name filter is shown as typed).
  const hostInfo = useQuery(`host-name:${host}`, (s) => api.host(host, s), { enabled: host.startsWith("host_") });

  const toggleState = (s: string) => {
    const next = states.includes(s) ? states.filter((x) => x !== s) : [...states, s];
    setSearchParams({ state: next.length ? next.join(",") : null });
  };
  const submitText = (e: FormEvent) => {
    e.preventDefault();
    setSearchParams({ host: hostDraft.trim() || null, label: labelDraft.trim() || null });
  };
  const clear = () => setSearchParams({ state: null, resumable: null, host: null, label: null });
  const filtered = states.length > 0 || resumable || host !== "" || label !== "";

  const cols = useMemo(() => runColumns({ tenant: scope.showTenant }), [scope.showTenant]);

  return (
    <div className="page page-list">
      <PageHeader title="Runs" description={<span>{runs.length}{full ? "+" : ""} {filtered ? "matching" : ""} runs · newest first{scope.showTenant ? " · all tenants" : ""}</span>} />
      <div className="filters-bar">
        <div className="filters">
          <div className="chips" role="group" aria-label="States">
            {RUN_STATE_LIST.map((s) => {
              const st = runStateStyle(s);
              const on = states.includes(s);
              return (
                <button key={s} type="button" className={["chip", `chip-${st.hue}`, on ? "is-on" : ""].join(" ").trim()} onClick={() => toggleState(s)} aria-pressed={on}>
                  {st.label}
                </button>
              );
            })}
          </div>
          <label className="check" title="Stopped, lost or failed: what resume accepts">
            <input type="checkbox" checked={resumable} onChange={(e) => setSearchParams({ resumable: e.target.checked ? "true" : null })} />
            Resumable
          </label>
        </div>
        <form className="filters" onSubmit={submitText}>
          <input className="input input-sm mono" placeholder="host id or name" value={hostDraft} onChange={(e) => setHostDraft(e.target.value)} style={{ width: 200 }} />
          <input className="input input-sm mono" placeholder="label k=v" value={labelDraft} onChange={(e) => setLabelDraft(e.target.value)} style={{ width: 160 }} />
          <Button size="sm" type="submit">
            Apply
          </Button>
          {filtered && (
            <Button size="sm" variant="ghost" onClick={clear}>
              Clear
            </Button>
          )}
          {filtered && (
            <span className="filter-summary">
              Showing runs
              {states.length > 0 && (
                <>
                  {" "}in state <span className="mono">{states.join(" or ")}</span>
                </>
              )}
              {resumable && <>{states.length > 0 ? " and" : ""} resumable (stopped, lost or failed)</>}
              {host && (
                <>
                  {" "}placed on{" "}
                  {hostInfo.data ? (
                    <Link to={hostPath(hostInfo.data.id)}>
                      {hostInfo.data.name}
                    </Link>
                  ) : (
                    <span className="mono">{host}</span>
                  )}
                </>
              )}
              {label && (
                <>
                  {" "}labelled <span className="mono">{label}</span>
                </>
              )}
              .
            </span>
          )}
        </form>
      </div>
      <Card flush>
        <ErrorStrip error={runs.length > 0 ? q.error ?? olderHere?.error ?? null : null} />
        {q.error && runs.length === 0 && !q.loading ? (
          <ErrorBlock error={q.error} onRetry={q.refetch} />
        ) : (
          <Table columns={cols} rows={runs} rowKey={(r) => r.id} loading={q.loading} onRowClick={(r) => go(runPath(r.id))} empty="No runs match these filters." />
        )}
        {full && (
          <div className="table-foot">
            {more ? (
              <Button size="sm" onClick={() => void loadMore()} loading={olderHere?.loading ?? false}>
                Load more
              </Button>
            ) : (
              <span className="muted">Showing the newest {runs.length}. Narrow the filters to see older runs.</span>
            )}
          </div>
        )}
      </Card>
    </div>
  );
}

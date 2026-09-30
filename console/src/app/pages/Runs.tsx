import { useEffect, useMemo, useState, type FormEvent } from "react";
import { Button, Card, ListPriceNote, PageHeader, Pagination, rangeText, RUN_STATE_LIST, runStateStyle, sortInWords, Table, TimeSeriesChart } from "@lux/design-system";
import { api, useQuery, type RunListParams } from "../../api/index.ts";
import { usePaged } from "../paged.ts";
import { go, Link, setSearchParams, useSearchParams } from "../router.tsx";
import { useScope, useScopedQuery } from "../scope.tsx";
import { ErrorBlock, ErrorStrip, hostPath, runColumns, runPath, useSeries } from "./common.tsx";


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

  // A page of the whole result, in the sort chosen, read by the server.
  // A filter, tenant, sort or size change starts at the first page; a
  // refresh re-reads the page on screen in place.
  // Keyed under runs: so Run events refetch it (live.ts).
  const q = usePaged("runs", `${scope.tenant}|${filterKey}`, (req, sig) => api.runsPage(scope.apiTenant, { ...filter, ...req }, sig), {
    defaultSort: { key: "created", dir: "desc" },
    defaultSize: 50,
    interval: 5000,
    live: 60_000,
  });
  const runs = q.rows;

  // The charts: the tenant scope's history over the global range, not the
  // table's rows (its filters do not narrow them).
  const history = useScopedQuery(`history:${scope.range}`, (t, sig) => api.history(t, scope.range, sig), { interval: 30_000 });
  const h = history.data?.samples;
  const level = useSeries(h, [(x) => x.runs?.running, (x) => x.queued]);
  const flow = useSeries(h, [(x) => x.started, (x) => x.finished]);
  const res = history.data?.resolution;
  const every = res === 0 ? "10s" : res === 60 ? "1m" : res === 3600 ? "1h" : undefined;
  const range = rangeText(scope.range);

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

  const cols = useMemo(() => runColumns({ tenant: scope.showTenant, cost: true, placement: true }).map((c) => ({ ...c, sortable: true })), [scope.showTenant]);

  return (
    <div className="page page-list">
      <PageHeader
        title="Runs"
        description={
          <>
            <span>{scope.apiTenant ? `tenant ${scope.apiTenant}` : scope.showTenant ? "all tenants" : "your runs"} · every column sorts, across every matching run</span>
            <ListPriceNote>costs are list prices</ListPriceNote>
          </>
        }
      />
      <ErrorStrip error={history.error} />
      <div className="grid grid-2">
        <Card title="Runs over time" subtitle={`running and queued · ${range}${every ? ` · ${every} samples` : ""}${filtered ? " · all runs in scope, not filtered" : ""}`}>
          <TimeSeriesChart x={level.x} ys={level.ys} series={[{ label: "Running", color: 1, area: true }, { label: "Queued", color: 2 }]} unit="count" />
        </Card>
        <Card title="Started / finished" subtitle={`per ${every ?? "sample"} · ${range}${filtered ? " · all runs in scope, not filtered" : ""}`}>
          <TimeSeriesChart x={flow.x} ys={flow.ys} series={[{ label: "Started", color: 1, step: true }, { label: "Finished", color: 3, step: true }]} unit="count" />
        </Card>
      </div>
      <p className="page-note">Charts follow the tenant and the time range; table filters do not narrow them. History starts with luxd's samples: a range older than them is empty, not zero.</p>
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
        <ErrorStrip error={runs.length > 0 ? q.error : null} />
        {q.error && runs.length === 0 && !q.loading ? (
          <ErrorBlock error={q.error} onRetry={q.refetch} />
        ) : (
          <Table
            columns={cols}
            rows={runs}
            rowKey={(r) => r.id}
            loading={q.loading}
            sortMode="server"
            sort={q.sort}
            onSortChange={q.setSort}
            onRowClick={(r) => go(runPath(r.id))}
            empty="No runs match these filters."
            footer={
              runs.length > 0 || q.page > 1 ? (
                <Pagination
                  mode="cursor"
                  page={q.page}
                  count={runs.length}
                  pageSize={q.size}
                  pageSizes={[50, 100, 200]}
                  onPageSize={q.setSize}
                  hasPrev={q.hasPrev}
                  hasNext={q.hasNext}
                  onFirst={q.first}
                  onPrev={q.prev}
                  onNext={q.next}
                  noun="runs"
                  sortLabel={sortInWords(cols, q.sort)}
                />
              ) : undefined
            }
          />
        )}
      </Card>
    </div>
  );
}

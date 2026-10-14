import { memo, useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState, type ReactNode } from "react";
import {
  Badge,
  EmptyState,
  IdChip,
  InputTurn,
  MessageTurn,
  Note,
  PlanTurn,
  RailCard,
  RailFacts,
  SegmentedControl,
  SessionMarker,
  SessionRail,
  SessionTranscript,
  Spinner,
  StatePill,
  SubagentPanel,
  SubagentStub,
  SubagentTree,
  ThoughtTurn,
  ToolCall,
  ToolUsage,
  useNow,
} from "@lux/design-system";
import { isRunActive, type Run } from "../../../api/index.ts";
import { setSearchParams } from "../../router.tsx";
import { agentOf, subagentTree } from "./projection.ts";
import { RunComposer } from "./RunComposer.tsx";
import type { Session, SessionItem, SessionThread, ToolItem } from "./types.ts";
import { useRunSession } from "./useRunSession.ts";
import { agentColors, briefFor, lastActivity, noSubagentsText, subagentCounts, subagentFootnote, summaryIsProse, toolCounts, toolCutNote, toolInputText, toolSummary, usageText, visible } from "./view.ts";

type Filter = "all" | "messages";

// Under this content width the rail is not drawn (components.css): a subagent opens in a dialog.
const RAIL_MIN = 1000;

function useNarrow(el: React.RefObject<HTMLElement | null>): boolean {
  const [narrow, setNarrow] = useState(false);
  useLayoutEffect(() => {
    const n = el.current;
    if (!n) return;
    const ro = new ResizeObserver(() => setNarrow(n.clientWidth < RAIL_MIN));
    ro.observe(n);
    setNarrow(n.clientWidth < RAIL_MIN);
    return () => ro.disconnect();
  }, [el]);
  return narrow;
}

interface Ctx {
  s: Session;
  colors: Map<string, string>;
  selected?: string;
  open: (id: string) => void;
  now: number;
  filter: Filter;
}

function summaryOf(ctx: Pick<Ctx, "s" | "colors">, th: SessionThread) {
  return childSummary(th, agentOf(ctx.s, th), ctx.colors.get(th.id));
}

function childSummary(th: SessionThread, agent: string | undefined, color: string | undefined) {
  return {
    id: th.id,
    title: th.title,
    agent,
    model: th.model,
    color: color ?? "var(--fg-faint)",
    status: th.status,
    startedAt: th.startedAt,
    endedAt: th.endedAt,
    tools: th.tools,
    subagents: th.children.length,
  };
}

// Output cannot scroll to a record yet: the link opens the tab and names the record.
const openOutput = () => setSearchParams({ tab: "output" }, { replace: false });

interface ItemProps {
  item: SessionItem;
  cutNote?: string;
  child?: SessionThread;
  agent?: string;
  color?: string;
  selected: boolean;
  // Defined only for rows whose duration still moves.
  now?: number;
  open: (id: string) => void;
}

function Tool({ t, cutNote, child, agent, color, selected, now, open }: ItemProps & { t: ToolItem }) {
  const record = useMemo(() => ({ label: `record ${t.cursor} in Output`, onClick: openOutput }), [t.cursor]);
  const onOpen = useCallback(() => child && open(child.id), [child, open]);
  return (
    <ToolCall
      name={t.name}
      summary={toolSummary(t)}
      summaryMono={!summaryIsProse(t)}
      status={t.status}
      start={t.t}
      end={t.end}
      now={now}
      input={toolInputText(t)}
      diff={t.diff}
      output={t.output}
      error={t.error}
      cutNote={cutNote}
      record={record}
    >
      {child && <SubagentStub {...childSummary(child, agent, color)} last={lastActivity(child)} selected={selected} now={now} onOpen={onOpen} />}
    </ToolCall>
  );
}

const Item = memo(function Item(props: ItemProps) {
  const { item } = props;
  switch (item.kind) {
    case "input":
      return <InputTurn t={item.t} text={item.text} sender={item.sender} steer={item.steer} phase={item.phase} error={item.error} truncated={item.truncated} attachments={item.attachments} />;
    case "message":
      return <MessageTurn t={item.t} text={item.text} streaming={item.streaming} cutNote={item.truncated ? "cut at 2 KiB by lux" : undefined} />;
    case "thought":
      return <ThoughtTurn t={item.t} text={item.text} seconds={(item.end - item.t) / 1000} />;
    case "plan":
      return <PlanTurn t={item.t} steps={item.steps} />;
    case "tool":
      return <Tool {...props} t={item} />;
    case "note":
      return (
        <Note t={item.t} tone={item.tone} detail={item.detail}>
          {item.text}
        </Note>
      );
    case "marker": {
      const tokens = item.tokens && (item.tokens.pre != null || item.tokens.post != null) ? ` · ${fmtTokens(item.tokens.pre)} → ${fmtTokens(item.tokens.post)} tokens` : "";
      return (
        <SessionMarker type={item.type} t={item.t} summary={item.summary} summaryNote={item.summaryTruncated ? "cut at 64 KiB by lux" : undefined}>
          {item.type === "compacted" ? (
            <>
              <b>{item.text}</b>
              {tokens}
              {item.trigger ? ` · ${item.trigger}` : ""}
              {!item.summary ? " · no summary" : ""}
            </>
          ) : (
            item.text
          )}
        </SessionMarker>
      );
    }
  }
});

function fmtTokens(n: number | undefined): string {
  if (n == null) return "–";
  if (n >= 1000) return `${(n / 1000).toFixed(1)}k`;
  return String(n);
}

function Items({ ctx, thread }: { ctx: Ctx; thread: SessionThread }): ReactNode {
  return visible(thread.items, ctx.filter).map((item) => {
    const child = item.kind === "tool" && item.child ? ctx.s.threads[item.child] : undefined;
    const live = item.kind === "tool" && (item.status === "running" || item.status === "pending" || child?.status === "running");
    return (
      <Item
        key={item.id}
        item={item}
        cutNote={item.kind === "tool" ? toolCutNote(ctx.s.adapter, thread, item) : undefined}
        child={child}
        agent={child ? agentOf(ctx.s, child) : undefined}
        color={child ? ctx.colors.get(child.id) : undefined}
        selected={!!child && ctx.selected === child.id}
        now={live ? ctx.now : undefined}
        open={ctx.open}
      />
    );
  });
}

function Panel({ ctx, id, bare, onClose }: { ctx: Ctx; id: string; bare?: boolean; onClose: () => void }) {
  const th = ctx.s.threads[id];
  if (!th) return null;
  const parent = th.parent && th.parent !== ctx.s.root ? ctx.s.threads[th.parent] : undefined;
  return (
    <SubagentPanel
      {...summaryOf(ctx, th)}
      bare={bare}
      now={ctx.now}
      back={parent ? { label: parent.title, onClick: () => ctx.open(parent.id) } : undefined}
      onClose={onClose}
      brief={briefFor(ctx.s, th)}
      usage={usageText(th)}
      footnote={subagentFootnote(ctx.s.adapter)}
    >
      {th.items.length ? <Items ctx={{ ...ctx, filter: "all" }} thread={th} /> : <span className="muted">Nothing yet.</span>}
    </SubagentPanel>
  );
}

function Rail({ ctx, run }: { ctx: Ctx; run: Run }) {
  const s = ctx.s;
  const counts = subagentCounts(s);
  const tools = toolCounts(s);
  const rows = subagentTree(s).map((r) => ({ ...summaryOf(ctx, r.thread), depth: r.depth }));
  const live = isRunActive(run.state);
  return (
    <>
      <RailCard title="Session">
        <RailFacts
          items={[
            { key: "Adapter", value: s.adapter },
            { key: "Model", value: s.model ?? "–", mono: !!s.model, title: s.model },
            { key: "Session", value: s.sessionId ? <IdChip value={s.sessionId} truncate={22} /> : <span className="muted">–</span> },
            { key: "Activity", value: live && run.activity ? <StatePill kind="run" state={run.state} activity={run.activity} /> : <StatePill kind="run" state={run.state} /> },
            { key: "Turns", value: <span className="num">{s.turns || "–"}</span> },
            { key: "Compactions", value: <span className="num">{s.compactions}</span> },
          ]}
        />
      </RailCard>
      <RailCard title="Subagents" count={s.subagents === "reported" ? counts.total : undefined} aside={counts.running ? <Badge tone="info">{counts.running} running</Badge> : undefined} flush={rows.length > 0}>
        {s.subagents === "unsupported" ? (
          <span className="muted">{noSubagentsText(s.adapter)}</span>
        ) : rows.length ? (
          <SubagentTree rows={rows} selected={ctx.selected} now={ctx.now} onOpen={ctx.open} />
        ) : (
          <span className="muted">None yet. A subagent appears here as it starts.</span>
        )}
      </RailCard>
      {tools.length > 0 && (
        <RailCard title="Tools used" count={tools.reduce((n, t) => n + t.count, 0)}>
          <ToolUsage counts={tools} />
        </RailCard>
      )}
    </>
  );
}

/** The Run page's Session tab: the Run's output read as a conversation, with its subagents, and the input to the Run. */
export function RunSession({ run }: { run: Run }) {
  const adapter = run.spec.workload.adapter;
  const hosts = useMemo(() => Object.fromEntries((run.placements ?? []).map((p) => [p.host, p.hostName])), [run.placements]);
  const { session: s, status, error } = useRunSession(run.id, adapter, run.epoch, hosts);
  const [filter, setFilter] = useState<Filter>("all");
  const [selected, setSelected] = useState<string | undefined>();
  const slots = useRef(new Map<string, number>());
  const layout = useRef<HTMLDivElement>(null);
  const dialog = useRef<HTMLDialogElement>(null);
  const narrow = useNarrow(layout);
  const live = isRunActive(run.state);
  const now = useNow(1000);
  const colors = useMemo(() => agentColors(s, slots.current), [s]);
  const open = useCallback((id: string) => setSelected(id), []);
  const close = useCallback(() => setSelected(undefined), []);
  const ctx: Ctx = { s, colors, selected, open, now, filter };

  useEffect(() => {
    if (!selected) return;
    const onKey = (e: KeyboardEvent) => e.key === "Escape" && close();
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [selected, close]);

  useEffect(() => {
    const d = dialog.current;
    if (!d) return;
    const want = narrow && !!selected;
    if (want && !d.open) d.showModal();
    else if (!want && d.open) d.close();
  }, [narrow, selected]);

  const root = s.threads[s.root]!;
  const shown = visible(root.items, filter);
  const lastItem = root.items.at(-1);
  const activeTool = [...root.items].reverse().find((i): i is ToolItem => i.kind === "tool" && i.status === "running");
  const tools = toolCounts(s).reduce((n, t) => n + t.count, 0);
  const empty =
    root.items.length === 0 ? (
      <div className="transcript-empty">
        <EmptyState
          compact
          icon={status === "connecting" || live ? <Spinner size={18} /> : undefined}
          title={status === "error" ? "Disconnected" : "No session yet"}
          description={error ?? (live ? "The agent's conversation appears here once lux has its first record." : "This Run wrote no agent records.")}
        />
      </div>
    ) : undefined;

  return (
    <div ref={layout} className="session-layout">
      <SessionTranscript
        className="session-pane-h"
        subtitle={
          <>
            {adapter}
            {s.model && (
              <>
                {" "}
                · <span className="mono">{s.model}</span>
              </>
            )}
          </>
        }
        actions={<SegmentedControl<Filter> label="Show" value={filter} onChange={setFilter} options={[{ value: "all", label: "Everything" }, { value: "messages", label: "Messages" }]} />}
        itemCount={shown.length}
        contentKey={lastItem}
        composer={<RunComposer key={run.id} run={run} />}
        status={
          <span className="num">
            {s.turns} {s.turns === 1 ? "turn" : "turns"} · {tools} tool {tools === 1 ? "call" : "calls"}
            {s.cursor && <span className="mono"> · cursor {s.cursor}</span>}
          </span>
        }
        empty={empty}
      >
        <Items ctx={ctx} thread={root} />
        {live && root.status === "running" && (
          <div className="activity-tail">
            <Spinner size={12} />
            {activeTool ? (
              <>
                running <span className="mono">{activeTool.name}</span>
              </>
            ) : (
              "working…"
            )}
          </div>
        )}
      </SessionTranscript>
      <SessionRail>{selected && !narrow ? <div className="session-pane-h" style={{ display: "flex" }}><Panel ctx={ctx} id={selected} onClose={close} /></div> : <Rail ctx={ctx} run={run} />}</SessionRail>
      <dialog
        ref={dialog}
        className="dialog session-dialog"
        aria-label="Subagent"
        onCancel={(e) => {
          e.preventDefault();
          close();
        }}
      >
        {narrow && selected && <Panel ctx={ctx} id={selected} bare onClose={close} />}
      </dialog>
    </div>
  );
}

/** A generic workload's Session tab: no conversation to read, only input to its stdin. */
export function RunStdinSession({ run }: { run: Run }) {
  return (
    <SessionTranscript
      subtitle={run.spec.workload.adapter}
      itemCount={0}
      empty={
        <div className="transcript-empty">
          <EmptyState compact title="No agent conversation" description="This workload is not an agent: its output is on the Output tab and input goes to its stdin." />
        </div>
      }
      composer={<RunComposer key={run.id} run={run} stdin />}
    />
  );
}

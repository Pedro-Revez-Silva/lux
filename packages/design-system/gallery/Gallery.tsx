import { useMemo, useState, type ReactNode } from "react";
import {
  Badge,
  Button,
  Card,
  Code,
  ConfirmDialog,
  EmptyState,
  formatBytes,
  formatCores,
  formatCount,
  formatDuration,
  formatPercent,
  formatRelative,
  formatTimestamp,
  HOST_STATE_LIST,
  IconButton,
  IdChip,
  KeyValue,
  LiveDot,
  LogView,
  PageHeader,
  RUN_STATE_LIST,
  SectionHeader,
  Select,
  Skeleton,
  SkeletonLines,
  Sparkline,
  Spinner,
  StatePill,
  StatTile,
  TenantPicker,
  Table,
  Tabs,
  TimeRangePicker,
  TimeSeriesChart,
  Timeline,
  Tooltip,
  useDensity,
  useTheme,
  useToast,
  type Column,
  type TimeRange,
} from "../src/index.ts";
import { IconDots, IconInfo, IconMoon, IconRefresh, IconRows, IconRowsLoose, IconSun } from "../src/icons.tsx";
import { fakeHosts, fakeLogs, fakePlacementStages, fakeRuns, fakeSeries, fakeTenants, NOW, type FakeHost, type FakeRun } from "./fake.ts";

function Section({ id, title, children, note }: { id: string; title: string; note?: ReactNode; children: ReactNode }) {
  return (
    <section className="sg-section" id={id}>
      <div className="sg-section-head">
        <h2 className="sg-h2">{title}</h2>
        {note && <p className="sg-note">{note}</p>}
      </div>
      {children}
    </section>
  );
}

const SECTIONS = ["colors", "type", "spacing", "layout", "buttons", "badges", "states", "stats", "cards", "tables", "tabs", "selects", "charts", "timeline", "logs", "keyvalue", "dialogs", "feedback", "format"];

/** The gallery: a slim bar (brand, theme and density) over the sections. */
export function Gallery() {
  const { resolved, toggle } = useTheme();
  const { density, toggle: toggleDensity } = useDensity();
  return (
    <>
      <header className="gallery-bar">
        <span className="gallery-brand">
          <span className="gallery-mark" aria-hidden="true" />
          <span className="gallery-name">lux</span>
          <span className="gallery-sub">design system</span>
        </span>
        <span className="gallery-controls">
          <IconButton size="sm" label={density === "compact" ? "Comfortable density" : "Compact density"} onClick={toggleDensity}>
            {density === "compact" ? <IconRowsLoose size={15} /> : <IconRows size={15} />}
          </IconButton>
          <IconButton size="sm" label={resolved === "dark" ? "Switch to light theme" : "Switch to dark theme"} onClick={toggle}>
            {resolved === "dark" ? <IconSun size={15} /> : <IconMoon size={15} />}
          </IconButton>
        </span>
      </header>
      <main className="gallery-content">
        <Sections />
      </main>
    </>
  );
}

function Sections() {
  return (
    <div className="sg">
      <nav className="sg-toc" aria-label="Sections">
        {SECTIONS.map((s) => (
          <a key={s} href={`#${s}`}>
            {s}
          </a>
        ))}
      </nav>
      <div className="sg-body">
        <PageHeader title="Design system" description="Every token and component the lux console is built from, with fake data. Toggle the theme and the density in the bar above." />
        <Colors />
        <Type />
        <Spacing />
        <Layout />
        <Buttons />
        <Badges />
        <States />
        <Stats />
        <Cards />
        <Tables />
        <TabsDemo />
        <Selects />
        <Charts />
        <TimelineDemo />
        <Logs />
        <KeyValueDemo />
        <Dialogs />
        <Feedback />
        <Formatting />
      </div>
    </div>
  );
}

/* ---------- tokens ---------- */

function Swatch({ name, text }: { name: string; text?: boolean }) {
  return (
    <div className="sg-swatch">
      <div className="sg-swatch-chip" style={text ? { background: "var(--bg-surface)", color: `var(${name})` } : { background: `var(${name})` }}>
        {text ? "Aa" : ""}
      </div>
      <code className="sg-swatch-name">{name}</code>
    </div>
  );
}

function SwatchRow({ names, text }: { names: string[]; text?: boolean }) {
  return (
    <div className="sg-swatches">
      {names.map((n) => (
        <Swatch key={n} name={n} text={text} />
      ))}
    </div>
  );
}

function Colors() {
  return (
    <Section id="colors" title="Color" note="Neutrals do the layout; the accent is reserved for interaction; semantic colors carry meaning with an icon or label beside them. Chart slots are a fixed, validated order (dataviz method): assigned in sequence, never cycled; slots 3–5 are below 3:1 on the light surface, so charts always ship a legend and a tooltip.">
      <h3 className="sg-h3">Neutrals</h3>
      <SwatchRow names={["--bg-page", "--bg-surface", "--bg-raised", "--bg-subtle", "--bg-muted", "--bg-inset", "--border", "--border-strong"]} />
      <SwatchRow names={["--fg", "--fg-secondary", "--fg-muted", "--fg-faint"]} text />
      <h3 className="sg-h3">Accent</h3>
      <SwatchRow names={["--accent", "--accent-hover", "--accent-active", "--accent-subtle"]} />
      <SwatchRow names={["--accent-text"]} text />
      <h3 className="sg-h3">Semantic</h3>
      <SwatchRow names={["--success-dot", "--success-bg", "--warn-dot", "--warn-bg", "--danger-dot", "--danger-bg", "--info-dot", "--info-bg"]} />
      <SwatchRow names={["--success-fg", "--warn-fg", "--danger-fg", "--info-fg"]} text />
      <h3 className="sg-h3">State hue families</h3>
      <SwatchRow names={["--st-neutral-dot", "--st-blue-dot", "--st-teal-dot", "--st-green-dot", "--st-amber-dot", "--st-red-dot", "--st-violet-dot"]} />
      <h3 className="sg-h3">Categorical chart palette (fixed order)</h3>
      <SwatchRow names={["--chart-1", "--chart-2", "--chart-3", "--chart-4", "--chart-5", "--chart-6", "--chart-7", "--chart-8"]} />
      <SwatchRow names={["--chart-grid", "--chart-axis", "--chart-cursor"]} />
    </Section>
  );
}

function Type() {
  const sizes: [string, string][] = [
    ["--text-xs", "12 / 11px · meta, tick labels"],
    ["--text-sm", "13 / 12px · secondary, pills, table headers"],
    ["--text-md", "14 / 13px · body, tables, controls"],
    ["--text-lg", "15 / 14px · brand"],
    ["--text-xl", "17 / 16px · dialog titles"],
    ["--text-2xl", "22 / 20px · page titles"],
    ["--text-3xl", "30 / 28px · stat values (proportional figures)"],
  ];
  return (
    <Section id="type" title="Type" note="System sans everywhere, names lead. Monospace (tabular numerals) is kept for ids, logs and numeric columns; host names, labels and adapters are set in the sans. Sizes are comfortable / compact.">
      <div className="sg-type">
        {sizes.map(([v, d]) => (
          <div className="sg-type-row" key={v}>
            <code className="sg-type-token">{v}</code>
            <span style={{ fontSize: `var(${v})` }}>Fleet of 14 hosts, 121 placements</span>
            <span className="muted">{d}</span>
          </div>
        ))}
        <div className="sg-type-row">
          <code className="sg-type-token">--font-mono</code>
          <span className="mono">run_4h2kq7m3xw5ybzta i-0a1b2c3d4e5f60718 1,284.50</span>
          <span className="muted">ids, logs, numbers (tabular-nums)</span>
        </div>
        <div className="sg-type-row">
          <code className="sg-type-token">weights</code>
          <span>
            <span style={{ fontWeight: 400 }}>Regular 400</span> · <span style={{ fontWeight: 500 }}>Medium 500</span> · <span style={{ fontWeight: 600 }}>Semibold 600</span>
          </span>
          <span className="muted">no bold above 600</span>
        </div>
      </div>
    </Section>
  );
}

function Spacing() {
  return (
    <Section id="spacing" title="Spacing, radius, elevation, motion">
      <div className="sg-row">
        {["--sp-1", "--sp-2", "--sp-3", "--sp-4", "--sp-5", "--sp-6", "--sp-7", "--sp-8", "--sp-9"].map((s) => (
          <div className="sg-space" key={s}>
            <div className="sg-space-bar" style={{ width: `var(${s})` }} />
            <code>{s.replace("--sp-", "")}</code>
          </div>
        ))}
      </div>
      <div className="sg-row" style={{ marginTop: 16 }}>
        {["--radius-sm", "--radius-md", "--radius-lg", "--radius-pill"].map((r) => (
          <div className="sg-radius" key={r} style={{ borderRadius: `var(${r})` }}>
            <code>{r.replace("--radius-", "")}</code>
          </div>
        ))}
        {["--shadow-sm", "--shadow-md", "--shadow-lg"].map((r) => (
          <div className="sg-shadow" key={r} style={{ boxShadow: `var(${r})` }}>
            <code>{r.replace("--shadow-", "")}</code>
          </div>
        ))}
      </div>
      <p className="sg-note" style={{ marginTop: 12 }}>
        Motion: <Code>--dur-fast</Code> 100ms for hover, <Code>--dur-normal</Code> 180ms for enter/exit, <Code>--dur-slow</Code> 300ms for layout; all zero under <Code>prefers-reduced-motion</Code>.
      </p>
    </Section>
  );
}

function Layout() {
  const { density, set } = useDensity();
  const rows: [string, string, string][] = [
    ["body text", "14px", "13px"],
    ["table row", "40px (dense 34px)", "32px (dense 28px)"],
    ["control", "32px (sm 26px)", "28px (sm 24px)"],
    ["card padding", "20px", "16px"],
    ["grid gap", "20px / 28px", "16px / 20px"],
    ["chart height", "200 · 240 · 280px", "180 · 210 · 240px"],
  ];
  return (
    <Section id="layout" title="Density, breakpoints, shell" note="Comfortable is the default; compact is the old dense tuning. In the console the setting lives in the top bar; it persists like the theme (localStorage lux.density, applied before first paint as <html data-density>). It switches a handful of tokens; every component reads them.">
      <div className="sg-row">
        <Button variant={density === "comfortable" ? "primary" : "default"} onClick={() => set("comfortable")}>
          Comfortable
        </Button>
        <Button variant={density === "compact" ? "primary" : "default"} onClick={() => set("compact")}>
          Compact
        </Button>
        <span className="muted">currently {density}</span>
      </div>
      <table className="table sg-fmt">
        <thead>
          <tr>
            <th>Token</th>
            <th>Comfortable</th>
            <th>Compact</th>
          </tr>
        </thead>
        <tbody>
          {rows.map(([k, a, b]) => (
            <tr key={k}>
              <td>{k}</td>
              <td className="mono">{a}</td>
              <td className="mono">{b}</td>
            </tr>
          ))}
        </tbody>
      </table>
      <h3 className="sg-h3">Breakpoints</h3>
      <table className="table sg-fmt sg-fmt-wide">
        <thead>
          <tr>
            <th>Viewport</th>
            <th>Sidebar</th>
            <th>Top bar</th>
            <th>Content</th>
          </tr>
        </thead>
        <tbody>
          <tr>
            <td className="mono">&lt; 768</td>
            <td>off-canvas drawer behind a menu button</td>
            <td>brand, live dot, one scope menu (tenant, range, density, theme, session)</td>
            <td>one column; state chips scroll sideways; tables scroll inside their card with the first column pinned; header actions drop below the title</td>
          </tr>
          <tr>
            <td className="mono">768–1279</td>
            <td>icon rail (56px)</td>
            <td>full controls</td>
            <td>charts 2 across; optional table columns (ids, adapter, epoch) drop out under ~1100px of content</td>
          </tr>
          <tr>
            <td className="mono">1280–1919</td>
            <td>full (232px), collapsible to the rail (persisted)</td>
            <td>full controls</td>
            <td>overview feed becomes a side column at 1200px of content; charts 2–3 across</td>
          </tr>
          <tr>
            <td className="mono">1920, 2560</td>
            <td>full</td>
            <td>full controls</td>
            <td>charts grow (240, 280px) and go 3–4 across; the feed widens; lists cap at 1760px, detail pages at 1920px and centre</td>
          </tr>
        </tbody>
      </table>
      <p className="sg-note">
        Inside the content area, grids react to the container (<Code>@container content</Code>), not the viewport, so a rail and a full sidebar both get the right layout. Page widths: <Code>.page</Code> (detail, 1920px), <Code>.page-list</Code> (tables, 1760px), <Code>.page-wide</Code> (dashboards, unbounded).
      </p>
      <h3 className="sg-h3">Page header</h3>
      <Card>
        <PageHeader
          title="web-build"
          badges={
            <>
              <StatePill kind="run" state="running" activity="busy" />
              <Badge outline>generic</Badge>
              <Badge outline>epoch 2</Badge>
            </>
          }
          description={
            <>
              <IdChip value="run_4h2kq7m3xw5ybzta" />
              <span>tenant acme</span>
              <span>
                on <a className="name-link" href="#layout">i-0a1b2c3d4e5f60718</a>
              </span>
            </>
          }
          note="waiting for capacity: no ready host in pool gpu-a10"
          actions={
            <>
              <Button>Stop</Button>
              <Button variant="danger">Cancel</Button>
              <Button variant="primary" disabled>
                Resume
              </Button>
            </>
          }
        />
        <SectionHeader title="Section header" note="a quiet label between groups of cards" />
      </Card>
    </Section>
  );
}

/* ---------- components ---------- */

function Buttons() {
  return (
    <Section id="buttons" title="Button, IconButton">
      <div className="sg-row">
        <Button>Default</Button>
        <Button variant="primary">Primary</Button>
        <Button variant="danger">Drain host</Button>
        <Button variant="ghost">Ghost</Button>
        <Button icon={<IconRefresh size={14} />}>Refresh</Button>
        <Button loading>Saving</Button>
        <Button disabled>Disabled</Button>
      </div>
      <div className="sg-row">
        <Button size="sm">Small</Button>
        <Button size="md">Medium</Button>
        <Button size="lg" variant="primary">
          Large
        </Button>
        <IconButton label="More" size="sm">
          <IconDots size={14} />
        </IconButton>
        <IconButton label="Refresh">
          <IconRefresh size={15} />
        </IconButton>
        <IconButton label="Info" variant="default">
          <IconInfo size={15} />
        </IconButton>
        <IconButton label="Active" active>
          <IconDots size={15} />
        </IconButton>
      </div>
    </Section>
  );
}

function Badges() {
  return (
    <Section id="badges" title="Badge">
      <div className="sg-row">
        <Badge>neutral</Badge>
        <Badge tone="accent">accent</Badge>
        <Badge tone="success">success</Badge>
        <Badge tone="warn">warn</Badge>
        <Badge tone="danger">danger</Badge>
        <Badge tone="info">info</Badge>
        <Badge outline>outline</Badge>
        <Badge outline>epoch 3</Badge>
        <Badge mono>e2</Badge>
      </div>
    </Section>
  );
}

function States() {
  return (
    <Section id="states" title="StatePill" note="Run and host states map to seven hue families. Live states pulse; the label is always present, and compact pills keep it in the title and for screen readers.">
      <h3 className="sg-h3">Run states</h3>
      <div className="sg-row">
        {RUN_STATE_LIST.map((s) => (
          <StatePill key={s} kind="run" state={s} />
        ))}
      </div>
      <div className="sg-row">
        <StatePill kind="run" state="running" activity="busy" />
        <StatePill kind="run" state="running" activity="idle" />
        <StatePill kind="run" state="running" compact />
        <StatePill kind="run" state="failed" compact />
        <StatePill kind="run" state="weird" />
        <LiveDot />
      </div>
      <h3 className="sg-h3">Host states</h3>
      <div className="sg-row">
        {HOST_STATE_LIST.map((s) => (
          <StatePill key={s} kind="host" state={s} />
        ))}
      </div>
    </Section>
  );
}

function Stats() {
  const series = useMemo(() => fakeSeries(48, 1800), []);
  return (
    <Section id="stats" title="StatTile" note="Label, proportional-figure value, optional unit, signed delta vs a named period, 12–48 point sparkline in the de-emphasis hue with the current point in accent.">
      <div className="grid grid-stats">
        <StatTile label="Running" value={formatCount(series.running.at(-1))} delta={12.5} deltaLabel="yesterday" trend={series.running} />
        <StatTile label="Queued" value={formatCount(series.queued.at(-1))} delta={-38} deltaUnit="" deltaLabel="1h ago" upIsGood={false} trend={series.queued} />
        <StatTile label="Hosts ready" value="14" unit="of 16" delta={0} deltaLabel="1h ago" />
        <StatTile label="Hosts lost" value="2" tone="danger" delta={2} deltaUnit="" deltaLabel="1h ago" upIsGood={false} />
        <StatTile label="Peak memory" value="1.4" unit="GiB" trend={series.mem} />
        <StatTile label="p50 queue time" value={formatDuration(83)} delta={-14.2} deltaLabel="7d" upIsGood={false} />
        <StatTile label="Loading" value="" loading />
      </div>
    </Section>
  );
}

function Cards() {
  return (
    <Section id="cards" title="Card">
      <div className="grid grid-2">
        <Card title="Placement" subtitle="epoch 3 on i-0a1b2c3d4e5f60718" actions={<Button size="sm">Migrate</Button>}>
          <p className="secondary">Body content with default padding. Cards are the only elevated surface; nesting cards is not a thing.</p>
        </Card>
        <Card title="Flush body" flush footer="Footer: 4 placements, last ended 12m ago">
          <div style={{ padding: 16 }}>
            <SkeletonLines lines={4} />
          </div>
        </Card>
      </div>
    </Section>
  );
}

function RunsTable() {
  const [selected, setSelected] = useState<string | null>(null);
  const cols: Column<FakeRun>[] = [
    { key: "name", header: "Run", cell: (r) => <a className="name-link" href={`#run-${r.id}`}>{r.name}</a>, sortValue: (r) => r.name, lead: true, width: "20%" },
    { key: "id", header: "Id", cell: (r) => <IdChip value={r.id} truncate={14} href={`#run-${r.id}`} />, sortValue: (r) => r.id, mono: true, width: 170, optional: true },
    { key: "state", header: "State", cell: (r) => <StatePill kind="run" state={r.state} activity={r.activity} />, sortValue: (r) => r.state, width: 150 },
    { key: "tenant", header: "Tenant", cell: (r) => r.tenant, sortValue: (r) => r.tenant, width: 100 },
    { key: "adapter", header: "Adapter", cell: (r) => <span className="secondary">{r.adapter}</span>, sortValue: (r) => r.adapter, width: 110, optional: true },
    { key: "image", header: "Image", cell: (r) => r.image, sortValue: (r) => r.image, mono: true },
    { key: "host", header: "Host", cell: (r) => r.host ?? <span className="muted">–</span>, sortValue: (r) => r.host, width: 170 },
    { key: "epoch", header: "Epoch", cell: (r) => r.epoch, sortValue: (r) => r.epoch, align: "right", mono: true, width: 76, optional: true },
    { key: "cpu", header: "CPU", cell: (r) => formatDuration(r.cpuSeconds), sortValue: (r) => r.cpuSeconds, align: "right", mono: true, width: 90 },
    { key: "mem", header: "Peak mem", cell: (r) => formatBytes(r.peakMemoryBytes), sortValue: (r) => r.peakMemoryBytes, align: "right", mono: true, width: 100 },
    { key: "age", header: "Created", cell: (r) => <Tooltip content={formatTimestamp(r.createdAt)}><span>{formatRelative(r.createdAt, NOW)}</span></Tooltip>, sortValue: (r) => r.createdAt, align: "right", width: 100 },
  ];
  return <Table columns={cols} rows={fakeRuns} rowKey={(r) => r.id} onRowClick={(r) => setSelected(r.id)} selected={selected} defaultSort={{ key: "age", dir: "desc" }} maxHeight={360} />;
}

function HostsTable() {
  const cols: Column<FakeHost>[] = [
    { key: "id", header: "Host", cell: (h) => h.id, sortValue: (h) => h.id, lead: true, width: 210 },
    { key: "state", header: "State", cell: (h) => <StatePill kind="host" state={h.state} />, sortValue: (h) => h.state, width: 130 },
    { key: "pool", header: "Pool", cell: (h) => h.pool, sortValue: (h) => h.pool, width: 100 },
    { key: "pl", header: "Placements", cell: (h) => `${h.placements} / ${h.capacity}`, sortValue: (h) => h.placements, align: "right", mono: true, width: 100 },
    { key: "cpu", header: "CPU", cell: (h) => `${formatCores(h.cpuUsed)} / ${h.cpuCores}`, sortValue: (h) => h.cpuUsed / h.cpuCores, align: "right", mono: true, width: 130 },
    { key: "trend", header: "24h", cell: (h) => <Sparkline values={h.cpuTrend} width={72} height={18} min={0} max={1} />, width: 90 },
    { key: "mem", header: "Memory", cell: (h) => `${formatBytes(h.memUsed)} / ${formatBytes(h.memBytes)}`, sortValue: (h) => h.memUsed / h.memBytes, align: "right", mono: true, width: 170 },
    { key: "hb", header: "Heartbeat", cell: (h) => formatRelative(h.lastHeartbeat, NOW), sortValue: (h) => h.lastHeartbeat, align: "right", width: 100 },
  ];
  return <Table columns={cols} rows={fakeHosts} rowKey={(h) => h.id} defaultSort={{ key: "state", dir: "asc" }} dense />;
}

function Tables() {
  const cols: Column<FakeRun>[] = [
    { key: "id", header: "Run", cell: (r) => r.id, mono: true },
    { key: "state", header: "State", cell: (r) => r.state },
  ];
  return (
    <Section id="tables" title="Table" note="Fixed layout: columns with a width keep it, the rest share what is left, so wide screens stretch names rather than gaps. Sticky header, sortable columns, a lead column (the name) in the foreground weight, quiet mono ids, optional columns that drop out in narrow content, sideways scroll with the first column pinned when there is no room, row click and selection, skeleton and empty states.">
      <Card title="Runs" subtitle="24 fake runs · click a row to select · sortable" flush>
        <RunsTable />
      </Card>
      <Card title="Hosts" subtitle="dense rows, inline sparklines, names in the sans" flush>
        <HostsTable />
      </Card>
      <div className="grid grid-2">
        <Card title="Loading" flush>
          <Table columns={cols} rows={[]} rowKey={(r) => r.id} loading loadingRows={4} />
        </Card>
        <Card title="Empty" flush>
          <Table columns={cols} rows={[]} rowKey={(r) => r.id} empty={<EmptyState compact title="No runs in the last 24 hours" description="Widen the time range or pick another tenant." />} />
        </Card>
      </div>
    </Section>
  );
}

function TabsDemo() {
  const [v, setV] = useState("logs");
  const [w, setW] = useState("all");
  return (
    <Section id="tabs" title="Tabs">
      <Tabs
        value={v}
        onChange={setV}
        items={[
          { key: "overview", label: "Overview" },
          { key: "logs", label: "Logs" },
          { key: "events", label: "Events", count: 42 },
          { key: "placements", label: "Placements", count: 3 },
          { key: "artifacts", label: "Artifacts", disabled: true },
        ]}
      />
      <Tabs
        size="sm"
        value={w}
        onChange={setW}
        items={[
          { key: "all", label: "All", count: 61 },
          { key: "live", label: "Live", count: 12 },
          { key: "failed", label: "Failed", count: 3 },
        ]}
      />
    </Section>
  );
}

function Selects() {
  const [pool, setPool] = useState("default");
  const [range, setRange] = useState<TimeRange>("24h");
  const [tenant, setTenant] = useState("*");
  return (
    <Section id="selects" title="Select, TenantPicker, TimeRangePicker" note="Presets as rows, selection marked by a bold check, hover as a ghost wash. In the console the tenant and range pickers sit in the top bar and scope every page.">
      <div className="sg-row">
        <TenantPicker tenants={fakeTenants} value={tenant} onChange={setTenant} />
        <Select
          value={pool}
          onChange={setPool}
          prefix="Pool"
          options={[
            { value: "default", label: "default", description: "m6i.xlarge · 8 warm" },
            { value: "gpu-a10", label: "gpu-a10", description: "g5.2xlarge · 0 warm" },
            { value: "spot-large", label: "spot-large", description: "spot · 2 warm" },
            { value: "static", label: "static", description: "lab hosts" },
          ]}
        />
        <Select
          value={pool}
          onChange={setPool}
          size="sm"
          searchable
          options={["default", "gpu-a10", "spot-large", "static", "eu-west", "us-east", "ci", "nightly"].map((v) => ({ value: v, label: v, group: v.includes("-") ? "Regions" : "Pools" }))}
        />
        <TimeRangePicker value={range} onChange={setRange} />
        <TimeRangePicker value={range} onChange={setRange} size="md" />
      </div>
    </Section>
  );
}

function Charts() {
  const s = useMemo(() => fakeSeries(), []);
  return (
    <Section id="charts" title="TimeSeriesChart" note="uPlot line charts: 2px lines, hairline grid, one y axis (never two), crosshair with one tooltip listing every series, click a legend entry to hide a series. Units format axes and tooltips. Series slots are fixed to the entity, so filtering never repaints survivors. Height comes from --chart-h (200 / 240 / 280px as the screen grows; less when compact); the chart grid adds columns as the content widens.">
      <div className="grid grid-charts">
        <Card title="Runs" subtitle="last 24 hours, 1-minute samples">
          <TimeSeriesChart x={s.x} ys={[s.running, s.idle, s.queued]} series={[{ label: "Running", color: 1, area: true }, { label: "Idle", color: 3 }, { label: "Queued", color: 2 }]} unit="count" />
        </Card>
        <Card title="Hosts" subtitle="stepped: count changes only on scale events">
          <TimeSeriesChart x={s.x} ys={[s.hosts]} series={[{ label: "Hosts", color: 1, step: true, area: true }]} unit="count" />
        </Card>
        <Card title="CPU" subtitle="fleet-wide used cores, with a capacity line">
          <TimeSeriesChart x={s.x} ys={[s.cpu, s.hosts.map((h) => h * 8)]} series={[{ label: "Used", color: 1, area: true }, { label: "Capacity", color: "var(--fg-faint)", dashed: true }]} unit="cores" />
        </Card>
        <Card title="Memory" subtitle="bytes formatting on axis and tooltip">
          <TimeSeriesChart x={s.x} ys={[s.mem]} series={[{ label: "Peak memory", color: 7, area: true }]} unit="bytes" />
        </Card>
      </div>
      <h3 className="sg-h3">Sparkline</h3>
      <div className="sg-row">
        <Sparkline values={s.running.filter((_, i) => i % 60 === 0)} />
        <Sparkline values={s.queued.filter((_, i) => i % 60 === 0)} area color="var(--chart-2)" width={140} height={32} />
        <Sparkline values={s.hosts.filter((_, i) => i % 60 === 0)} color="var(--chart-1)" endDot={false} width={200} height={40} />
      </div>
    </Section>
  );
}

function TimelineDemo() {
  return (
    <Section id="timeline" title="Timeline (placement waterfall)" note="Ten lifecycle stages of one placement on a shared time axis. Setup is neutral/accent, the workload teal, teardown amber; a striped bar is still in progress. Hover a bar for exact timestamps.">
      <Card title="Placement epoch 3" subtitle="i-0a1b2c3d4e5f60718 · stopped by drain">
        <Timeline stages={fakePlacementStages} now={NOW} />
      </Card>
      <Card title="Fresh placement" subtitle="only the first two stages have happened">
        <Timeline stages={fakePlacementStages.map((st, i) => (i < 2 ? st : i === 2 ? { ...st, end: null } : { ...st, start: null, end: null }))} now={fakePlacementStages[2]!.start! + 9_000} />
      </Card>
    </Section>
  );
}

function Logs() {
  const lines = useMemo(() => fakeLogs(50_000), []);
  return (
    <Section id="logs" title="LogView" note="50,000 fake lines, windowed rendering with fixed 18px rows. stderr lines are tinted, system lines are italic. Follow-tail sticks to the bottom and switches off when you scroll up.">
      <LogView lines={lines} height={320} lineNumbers />
      <LogView lines={lines.slice(0, 6)} height={160} timestamps={false} follow={false} />
      <LogView lines={[]} height={100} />
    </Section>
  );
}

function KeyValueDemo() {
  return (
    <Section id="keyvalue" title="KeyValue, IdChip, Code" note="Ids are quiet: plain mono in the muted colour, with a copy affordance on hover or focus. Names lead everywhere; ids support.">
      <Card title="Run" subtitle="detail header">
        <KeyValue
          columns={2}
          items={[
            { key: "Run", value: <IdChip value="run_4h2kq7m3xw5ybzta" /> },
            { key: "State", value: <StatePill kind="run" state="running" activity="busy" /> },
            { key: "Tenant", value: "acme" },
            { key: "Adapter", value: <Badge outline>acp</Badge> },
            { key: "Image", value: "ghcr.io/acme/agent:1.14", mono: true },
            { key: "Host", value: <IdChip value="i-0a1b2c3d4e5f60718" href="#host" /> },
            { key: "Epoch", value: "3", mono: true },
            { key: "Created", value: `${formatTimestamp(NOW - 3_600_000)} (${formatRelative(NOW - 3_600_000, NOW)})` },
            { key: "State reason", value: <Code>waiting for capacity</Code> },
            { key: "Snapshot", value: null },
          ]}
        />
      </Card>
      <div className="sg-row">
        <IdChip value="run_4h2kq7m3xw5ybzta" />
        <IdChip value="run_4h2kq7m3xw5ybzta" truncate={10} prefix="run" />
        <IdChip value="sha256:8b1f2c9e4d7a6b5c3f2e1d0c9b8a7f6e5d4c3b2a1f0e9d8c7b6a5f4e3d2c1b0a" truncate={22} />
        <Code>--userns=auto</Code>
      </div>
    </Section>
  );
}

function Dialogs() {
  const [a, setA] = useState(false);
  const [b, setB] = useState(false);
  const [c, setC] = useState(false);
  const toast = useToast();
  return (
    <Section id="dialogs" title="ConfirmDialog" note="Native <dialog>; destructive actions use the danger tone and may require typing the target id.">
      <div className="sg-row">
        <Button onClick={() => setA(true)}>Cancel run…</Button>
        <Button variant="danger" onClick={() => setB(true)}>
          Drain host…
        </Button>
        <Button onClick={() => setC(true)}>Migrate with reason…</Button>
      </div>
      <ConfirmDialog open={a} title="Cancel run?" description="The run stops after the grace period. Its state volumes are snapshotted; a cancelled run can be resumed." confirmLabel="Cancel run" tone="danger" onConfirm={() => { setA(false); toast({ title: "Run cancelled", tone: "success" }); }} onCancel={() => setA(false)} />
      <ConfirmDialog open={b} title="Drain i-0a1b2c3d4e5f60718?" description="No new placements will be assigned. Its 3 live placements finish where they are, unless forced. The host is terminated when empty." confirmLabel="Drain host" tone="danger" confirmText="i-0a1b2c3d4e5f60718" checkbox={{ label: "Force evict running Runs", help: "Stops its live runs now: they are snapshotted and resumed elsewhere." }} onConfirm={(_input, forceEvict) => { setB(false); toast({ title: "Draining i-0a1b2c3d4e5f60718", description: forceEvict ? "3 placements to move" : undefined, tone: "warn" }); }} onCancel={() => setB(false)} />
      <ConfirmDialog open={c} title="Migrate run" description="Stops this placement and resumes on another host in the same pool." confirmLabel="Migrate" input={{ label: "Reason (recorded on the event)", placeholder: "e.g. host degraded", required: true }} onConfirm={(reason) => { setC(false); toast({ title: "Migration requested", description: reason }); }} onCancel={() => setC(false)} />
    </Section>
  );
}

function Feedback() {
  const toast = useToast();
  return (
    <Section id="feedback" title="Toast, Tooltip, EmptyState, Spinner, Skeleton">
      <div className="sg-row">
        <Button onClick={() => toast({ title: "Snapshot uploaded", description: "412 MiB in 41s", tone: "success" })}>Success toast</Button>
        <Button onClick={() => toast({ title: "Host lost", description: "i-0f9e8d7c6b5a49382 missed 3 heartbeats", tone: "danger", duration: 0 })}>Danger toast (sticky)</Button>
        <Button onClick={() => toast({ title: "Scaling pool default", description: "+2 hosts", tone: "info" })}>Info toast</Button>
        <Button onClick={() => toast({ title: "Draining", tone: "warn" })}>Warn toast</Button>
      </div>
      <div className="sg-row">
        <Tooltip content="Last heartbeat 2026-09-24 14:03:09">
          <span className="mono">3s ago</span>
        </Tooltip>
        <Tooltip content="Below" side="bottom">
          <Button size="sm">bottom</Button>
        </Tooltip>
        <Tooltip content="Right" side="right">
          <Button size="sm">right</Button>
        </Tooltip>
        <Spinner />
        <Spinner size={20} />
        <Skeleton width={120} />
        <Skeleton width={60} height={20} round />
      </div>
      <Card>
        <EmptyState title="No hosts in pool gpu-a10" description="Hosts appear here once the provider reports them running. Check the pool's scaling settings." action={<Button variant="primary" size="sm">Add host</Button>} />
      </Card>
    </Section>
  );
}

function Formatting() {
  const rows: [string, string][] = [
    ["formatBytes(1536)", formatBytes(1536)],
    ["formatBytes(6.4e9)", formatBytes(6.4e9)],
    ["formatDuration(0.45)", formatDuration(0.45)],
    ["formatDuration(83)", formatDuration(83)],
    ["formatDuration(4321)", formatDuration(4321)],
    ["formatDuration(200000)", formatDuration(200000)],
    ["formatRelative(now - 90s)", formatRelative(NOW - 90_000, NOW)],
    ["formatRelative(now + 2h)", formatRelative(NOW + 7_200_000, NOW)],
    ["formatTimestamp(now)", formatTimestamp(NOW)],
    ["formatCores(0.25)", formatCores(0.25)],
    ["formatCores(2.5)", formatCores(2.5)],
    ["formatPercent(0.4213)", formatPercent(0.4213)],
    ["formatCount(12900, compact)", formatCount(12900, { compact: true })],
    ["formatCount(1284)", formatCount(1284)],
    ["formatBytes(null)", formatBytes(null)],
  ];
  return (
    <Section id="format" title="Formatting helpers" note="src/ds/format.ts. Missing values render as an en dash, never as 0.">
      <table className="table sg-fmt">
        <thead>
          <tr>
            <th>Call</th>
            <th>Output</th>
          </tr>
        </thead>
        <tbody>
          {rows.map(([k, v]) => (
            <tr key={k}>
              <td className="mono">{k}</td>
              <td className="mono">{v}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </Section>
  );
}

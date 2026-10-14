# @lux/design-system

The tokens, styles and React components the lux console is built from, and
a gallery that shows every one of them with fake data, in either theme and
density.

```bash
bun install                                   # once, at the repository root
cd packages/design-system
bun run gallery        # http://localhost:5198/ (Bun HTML-import server, HMR)
bun run gallery:build  # static gallery in dist/, opens from any directory
bun run typecheck
bun run test           # bun test: idle countdown and server states, money rounding, y scale, y axis width, chart stacking, bars, day ticks and faded colours, family colours, storage kind colours, CostFigure, PartBar shares, Meter clamping and text, Table sort, columns and sort in words, EventTable, Pagination, Timeline point stages, durations, SegmentedControl, RelativeTime, terminal scheme, agent colour slots, ToolUsage, Markdown escaping, ToolCall, SessionComposer (send, draft kept on failure, keys, IME, disabled, interrupt) and the transcript's composer slot and resize follow, ImageRef digest shortening, LabelsFact disclosure, KeyValue full-width rows and surface (src/*.test.ts*)
```

## Using it

The package is a Bun workspace (the root `package.json`); an app depends on
`"@lux/design-system": "workspace:*"` and imports source, no build step:

```ts
import { Button, Table, useTheme } from "@lux/design-system";
import { IconPlay } from "@lux/design-system/icons";
```

and loads the stylesheets, in this order, before its own:

```html
<link rel="stylesheet" href="../packages/design-system/src/tokens.css" />
<link rel="stylesheet" href="../packages/design-system/src/base.css" />
<link rel="stylesheet" href="../packages/design-system/src/components.css" />
<link rel="stylesheet" href="../packages/design-system/src/layout.css" />
```

Grids in `layout.css` react to the nearest container named `content`: give
the app's scrolling content area `container-type: inline-size;
container-name: content` (the console's `.content`, the gallery's
`.gallery-content`).

## Layout

```
src/
  tokens.css        every custom property, light + dark, both densities
  base.css          reset, global text
  components.css    component styles (one section per component)
  layout.css        page widths, grids, stacks, name links
  format.ts         bytes, durations, relative time, cores, percentages, money
  states.ts         run/host/server state -> hue family + label (a server's process: ServerStateMark;
                    a server as the tenant's list shows it, asleep/waking/no answer: ServedStateMark);
                    cost status; cost family -> chart slot; stored blob kind -> label + chart slot
  theme.ts          theme and density: toggles, persistence, cssVar()
  icons.tsx         the icon set
  *.tsx             components
gallery/            the gallery app (index.html, Gallery.tsx, fake data)
```

## Principles

- Clean, calm, comfortable. One elevated surface (the card, a soft hairline,
  no shadow), quiet headers, whitespace instead of rules. Every page opens
  with a `PageHeader` (title, one line of context, primary actions on the
  right). Nothing animates except live state dots (a pill's, a running
  subagent's `AgentMark`), spinners and toasts.
- One control for "pick one of a few": the grey pill track (`Tabs` for
  what a page or card shows, `SegmentedControl` for a value; both draw
  alike). A page with more than one view puts its `Tabs` in the
  `PageHeader` actions and keeps the choice in `?tab=` (none for the first
  tab); a tab a principal cannot read is disabled, not hidden.
- Names lead, ids support. Ids are plain mono in the muted colour with a
  copy affordance on hover or focus (`IdChip`), never boxed. On touch screens
  (no hover) the copy affordance is always shown, with a target of at least
  24px. Monospace is
  kept for ids, logs and numeric columns; host names, labels and adapters
  are set in the sans.
- Comfortable by default, compact on request (see Density).
- Wide screens are used deliberately: tables stretch their text columns
  (fixed layout, sensible widths), dashboards add chart columns and a side
  feed, and detail pages cap and centre (see Breakpoints).
- Color carries meaning only next to a label. State pills always have text,
  status colors always have an icon or label, charts always have a legend
  (for 2+ series) and a tooltip.
- Charts follow the dataviz method: one y axis, 2px lines, hairline solid
  grid, area fills at 10%, a categorical palette in a fixed order that was run
  through the palette validator on both surfaces (`#ffffff` and `#1b1c1f`).
  Series colors follow the entity, never the row number.
- Dark mode is its own set of steps, not an inverted light mode. It applies by
  `prefers-color-scheme` unless `<html data-theme="light|dark">` overrides
  (the toggle in the top bar, persisted in `localStorage["lux.theme"]`).
- Missing values render as an en dash, never as `0`. A cost nothing has
  reported yet is an empty state ("no cost reported yet"), never `$0.00`.
- Money is exact: amounts stay decimal strings end to end (`formatMoney`,
  `sumMoney`, `compareMoney` work on scaled integers, never floats), one
  figure per currency, never added across currencies. Every page that shows
  money labels it "list price" once, explained in a Tooltip
  (`ListPriceNote`). Amounts show at most 4 decimals; the exact value is one
  hover away (`Money`), and in the API.
- A cost's status is a labelled `CostStatusBadge` where there is room for
  the label. In a table cell there is not, so a figure that may still change
  gets a `~` prefix, never a colour, with the status spelled out in its
  Tooltip (`CostFigure`); `~` is what `lux ls` prints too.
- An agent Run reads as a conversation first and a log second; both are
  the same records, and each view links to the other (the Session
  transcript's tool calls name their record in Output). Input and interrupt
  are sent from the Session tab's composer, to the Run's root session; the
  Output tab is read-only logs.
- A value that can be long (an image reference, labels, hashes) never sits
  in a multi-column `KeyValue` grid cell, where it stretches its whole row:
  it gets a full-width row under the grid (`FactRow`), on one line, with the
  full value on hover and copy (`ImageRef`) or behind a disclosure
  (`LabelsFact`). Host (Labels, Versions), Pool (Template) and Server
  (Labels) still use grid cells; they move to `LabelsFact`. A full-width
  row's value follows its key, not the grid's value column (accepted: the
  Image and Labels values start where their key ends).

## Terminal scheme

The terminal's colours are its own. `useTerminalScheme()` reads and sets
the choice, "auto" (Match console: the console's resolved theme), "light"
or "dark" (Solarized), persisted in `localStorage["lux.terminal.theme"]`
(`TERMINAL_THEME_KEY`; anything else reads as auto). Outside React,
`readTerminalScheme()` reads it and `setTerminalScheme(pref)` stores it
and updates every mounted hook; a change in another tab (a `storage`
event on that key) reaches this one. Pass its `resolved` to `Terminal`'s
`scheme`: the terminal updates xterm's theme in place (no remount, so the
connection, scrollback and selection stay) and sets its frame colours on
`.term[data-term-scheme]`. It never writes `lux.theme` or
`html[data-theme]`, and never changes a global token: the console theme
stays the top bar's.

## Density

Two settings, switched from the console's top bar (the rows icon) and persisted in
`localStorage["lux.density"]`, applied before first paint as
`<html data-density="compact">`. Only tokens change; every component reads
them.

| Token | Comfortable (default) | Compact |
| --- | --- | --- |
| `--text-md` (body) | 14px | 13px |
| `--text-{xs,sm,lg,xl,2xl,3xl}` | 12, 13, 15, 17, 22, 30px | 11, 12, 14, 16, 20, 28px |
| `StatTile` value / label | 22 / 12px (`--text-2xl` / `--text-xs`) | 20 / 11px |
| `--row-h` / `--row-h-dense` | 40 / 34px | 32 / 28px |
| `--control-h` / `-sm` / `-lg` | 32 / 26 / 38px | 28 / 24 / 34px |
| `--pad-card`, `--pad-cell` | 20, 14px | 16, 12px |
| `--gap`, `--gap-lg`, `--pad-page` | 20, 28, 32px | 16, 20, 24px |
| `--sidebar-w`, `--topbar-h` | 232, 52px | 208, 44px |
| `--chart-h` | 200px (240 at 1920, 280 at 2560) | 180px (210, 240) |
| `--session-rail-w` | 360px (420 at 1920, 480 at 2560) | 320px (380, 440) |
| `--measure` | 760px (880 at 1920) | 720px (840 at 1920) |
| `--turn-gap`, `--tool-row-h` | 12, 30px | 8, 26px |

## Breakpoints

The shell reacts to the viewport; layout inside the content area reacts to
its container (`@container content`), so a rail and a full sidebar both get
the right grid.

| Viewport | Sidebar | Top bar | Content |
| --- | --- | --- | --- |
| < 768 | off-canvas drawer behind a menu button | brand, live dot, one scope menu (tenant, range, density, theme, session) | one column; state chips scroll sideways; tables scroll inside their card with the first column pinned; header actions drop below the title; the document scrolls |
| 768–1279 | icon rail (`--sidebar-rail-w`, 56px) | full controls | stat tiles 3 across, charts 2 across |
| 1280–1919 | full, collapsible to the rail (`localStorage["lux.sidebar"]`) | full controls | overview feed becomes a side column at 1100px of content; charts 2–3 across; stat tiles 5 across from 1000px of content |
| ≥ 1920 / ≥ 2560 | full | full controls | charts grow and go 3–4 across; the feed widens (`--feed-w`); lists cap at `--page-list-w` (1760px), detail pages at `--page-detail-w` (1920px) and centre; the overview is unbounded |

Page widths (`src/layout.css`): `.page` (detail), `.page-list` (tables), `.page-wide`
(dashboards). Grids: `.grid-stats` (2 / 3 / 5 by container width: under
720px, from 720px, from 1000px; a row of exactly four goes 2 / 4, one of
exactly three stays 3, one of exactly six goes 3 / 6 from 1400px, so no row
leaves a gap or an orphan; a `KpiStrip` of exactly five goes 2 / 3 + 2 on
six columns from 760px of its container / 5 across from 960px, never 4 + 1),
`.grid-charts` (1 / 2 / 3 / 4 by container width), `.grid-2`, `.grid-3`,
`.grid-2-1` (a main card two thirds wide and one beside it: a chart and its
summary) (collapse to one column under 900px of content).

Tables (`Table`): every column that has a `sortValue` (client sort) or
`sortable: true` (server sort) sorts. Its header is focusable (Enter or
Space sorts, as a click does), sets `aria-sort` and shows ↕, or ↑ / ↓ on
the sorted column; clicking it again reverses. The first click sorts text
A→Z and numbers, times and durations largest (newest) first
(`firstSortDir`; `sortFirst` overrides). Missing values sort last in both
directions (`sortRows`). A small, unpaged table sorts its loaded rows
(`defaultSort`, or controlled `sort`); a paged one passes
`sortMode="server"`, `sort` and `onSortChange` and fetches the page in that
order, so a sorted page is the whole list sorted, never the visible rows
re-ordered. `footer` holds its `Pagination`.

`Pagination` has two modes. Count (`mode="count"`): "1–25 of 1,284 hosts",
numbered pages (first, last, the current and its neighbours: `pageList`)
and a page size, where the server counts the whole result; an empty result
reads "0–0 of 0". Cursor (`mode="cursor"`):
"Page 3 · runs 101–150", First / Previous / Next in the current sort
order and a page size, where an exact total is costly or keeps moving
(runs, events). First is on whenever the page is past 1, even when the
server gave that page no previous one. `busy` keeps the buttons in place
but inert. The page owns the cursors; a refresh re-reads the page it
is on and never moves the reader to another. The sort in words beside the
range comes from the columns: `sortInWords(columns, sort)` reads the
sorted column's `label` (or its header, when that is text) and its
`sortKind` (`time`: newest/oldest first, `number`: largest/smallest first,
`text`: A→Z / Z→A; right-aligned columns default to number):
"Created, newest first". `eventSortInWords(sort)` is the same for an
EventTable's columns.

Table helpers: `isSortable(column)`, `firstSortDir(column, rows)`,
`nextSort(sort, column, rows)` (the sort after a header click: the other
direction on the sorted column, else its first direction), `sortRows`,
`dropsOptional`. Row actions: icon buttons (`IconButton size="sm"`) in a
`span.row-actions` in a narrow, header-less last column (the Pools list:
Make default `IconStar`, Rename `IconPencil`).

`table-layout: fixed`; columns with a `width` keep it and
the rest share the remainder. `lead` marks the name column, `optional`
columns drop out when the table's container is under 1100px, or when with
them the table would not fit: a column without a width would get under
140px, or, when every column has one, their sum is wider than the container
(`dropsOptional`). Below the
table's minimum width (the column widths, or `minWidth`) it scrolls sideways
with the first column pinned.

## Tokens

All in `src/tokens.css`.

| Group | Tokens |
| --- | --- |
| Surfaces | `--bg-page` `--bg-surface` `--bg-raised` `--bg-subtle` `--bg-muted` `--bg-inset` `--overlay` |
| Borders | `--border` `--border-soft` (cards, rows) `--border-strong` (controls) |
| Text | `--fg` `--fg-secondary` `--fg-muted` `--fg-faint` |
| Accent | `--accent` `--accent-hover` `--accent-active` `--accent-fg` `--accent-subtle` `--accent-text` `--focus-ring` |
| Semantic | `--{success,warn,danger,info}-{fg,bg,dot}` |
| State hues | `--st-{neutral,blue,teal,green,amber,red,violet}-{fg,bg,dot}` |
| Chart | `--chart-1` … `--chart-8` (fixed order; cost families and stored blob kinds map onto them, compute is `--chart-1`, breakdown bands never are: `bandColor`), `--chart-grid` `--chart-axis` `--chart-label` `--chart-cursor`, `--chart-h`; unallocated cost uses `--st-neutral-dot` |
| Agents | `--agent-1` … `--agent-5`: aliases of `--chart-3, 7, 2, 5, 4` (no new colour, the validated palette, never compute's blue); a subagent type takes the next slot in first-seen order for the whole Run (`agentSlot`, `agentColor`), always beside its name |
| Logs | `--log-stderr-bg` `--log-stderr-fg` `--log-line-hover` |
| Terminal | `--term-bg` `--term-scrollbar` (the frame around the screen, following the console theme; a Terminal with a `scheme` sets its own on `.term[data-term-scheme]`; the screen's palette is `terminalThemes.ts`) |
| Type | `--font-sans` `--font-mono`, `--text-{xs,sm,md,lg,xl,2xl,3xl}` (density-dependent), `--leading-{tight,normal}`, `--weight-{normal,medium,semibold}` |
| Spacing | `--sp-1` … `--sp-9` (2, 4, 6, 8, 12, 16, 24, 32, 48px); density-dependent `--gap` `--gap-lg` `--pad-page` `--pad-card` `--pad-cell` |
| Radius | `--radius-{sm,md,lg,pill}` (3, 5, 8px, pill) |
| Elevation | `--shadow-{sm,md,lg}` (popovers, dialogs, toasts; cards have none) |
| Pill controls | `--seg-track` (the sunken track), `--seg-on` and `--seg-on-shadow` (the chosen pill: white with a hairline in light, a lighter step in dark) |
| Motion | `--dur-{fast,normal,slow}` (100/180/300ms, 0 under reduced motion), `--ease` |
| Layout | `--sidebar-w` `--sidebar-rail-w` `--topbar-h` `--row-h` `--row-h-dense` `--control-h` `--control-h-sm` `--control-h-lg` `--page-list-w` `--page-detail-w` `--feed-w` |
| Session | `--session-rail-w` (the rail beside a transcript), `--measure` (longest line of agent prose), `--turn-gap`, `--tool-row-h` (a folded tool call), `--subagent-indent` (18px, a level of the subagent tree), `--session-pane-offset` (600px in both densities: the viewport height `.session-pane-h` leaves for the page above a session pane) |

State mapping (`src/states.ts`):

| Hue | Run states | Host states | Server states |
| --- | --- | --- | --- |
| neutral | submitted, pending, provisioning, stopped, terminated | provisioning, terminated | stopped |
| blue | scheduled, starting, resuming | registered | starting |
| teal | running, busy | | |
| violet | idle | | |
| green | succeeded | ready | ready |
| amber | stopping | draining, terminating | unreachable |
| red | lost, failed | lost, launch failed (outline) | exited |

`launch_failed` is not a state luxd sets: a host's operational state stays
`terminated` (cleanup, tokens and costs treat it as gone), and its launch
outcome (`launch.outcome`, from GET /v1/hosts) says the provider refused
the launch. `hostDisplayState(host)` maps the two to the pill the console
shows: a red outline, "Launch failed", apart from a Terminated host that
ran and from a Lost one.

Cost status (`costStatusStyle`, `CostStatusBadge`): the `status` of
`GET /v1/runs/{id}/cost`, as a Badge whose Tooltip says what it means (and,
when incomplete, which sources it waits on).

| Status | Badge | Meaning |
| --- | --- | --- |
| `pending` | neutral "Pending" | no source has reported yet |
| `complete` | info "Estimate" | every source answered; some lines may still change |
| `incomplete` | warn "Incomplete" | a source has not answered or failed |
| `final` | success "Final" | every source settled |

Cost family colours (`familySlot`, `familyColor`): a
family maps to a categorical `--chart-N` slot, never a raw colour.
`compute` is always `--chart-1` and `block-storage` (lux's own Block
storage family) always `--chart-3`, whatever their hint. A plugin's describe `color` hint picks the
slot: a name (`violet` → 7, `amber` → 4, `orange` → 2, `teal` → 3, `pink`
→ 5, `green` → 6, `red` → 8; blue names go to 7, as slot 1 is compute's)
or `#rrggbb` by nearest hue. Without a hint the family's name picks one of
slots 2–8 (a hash of the name). A family's slot depends on its key and
hint alone, never on the families shown beside it, so it has one colour in
every view.
Two families that land on one slot share it: a collision is accepted, a
colour that changes between views is not.

Breakdown band colours (`bandColor(i)`, `OTHER_BAND_COLOR`,
`NONE_BAND_COLOR`): when cost is stacked by something
other than family (a label value, an API key, a pool, a tenant, a Run),
the bands take the `--chart-N` slots in rank order, skipping compute's
slot (`--chart-1`), so compute's blue only ever means compute. That is
slots 2–8 for bands 1–7, repeating from slot 2 past seven. "Other"
(every value folded past the top ones) is `--st-neutral-fg` and "(none)"
(cost with no value) `--st-neutral-dot`. The Family view uses
`familyColor` and keeps compute on `--chart-1`.

Stored bytes by blob kind (`storageKindStyle`, `STORAGE_KIND_LIST`, the
`StorageKind` type): each kind luxd keeps in S3 has a fixed label and
`--chart-N` slot, stacked in this order (gallery: charts, "Stored").
Slot 1 stays compute's. An unknown kind keeps its key as the label and
`--st-neutral-dot`, so a new kind is shown, never dropped.

| Kind | Label | Slot |
| --- | --- | --- |
| `volume` | Snapshots | 3 |
| `output` | Output | 7 |
| `artifact` | Artifacts | 4 |

`context` (`storedContext`) has no writer yet, so its always-zero series
is omitted rather than drawn along the stack's top. To chart it, add it
to `STORAGE_KINDS` and the Overview's `STORED`.

## Components

Logo (the star, 16–32px; the detailed mark is `docs/brand/lux.svg`),
Button, IconButton, LinkButton (an anchor styled as a Button), Badge,
StatePill (run, host and server states; ServerStateMark is the shorthand
for a server's process, ServedStateMark for a server as the tenant's list
shows it: ready, waking, asleep, stopped, unreachable, exited, no answer),
IdleCountdown ("Idle in 7:42 · 10m after the last request"; `formatCountdown`),
ConnectionBadge,
StatTile (compact: a 12px muted label, the value at `--text-2xl`, its
`unit` or note on its own line under it; `swatch` puts a `ColorKey` before
the label for a figure that is one family or series; `lead` tints the one
figure a page is about, at most one per row; `tone` warn / danger /
success colours the value), Sparkline, Card, Table, Pagination, Tabs
(`items`, `value`, `onChange`, `size`; role tablist; a count is a muted
number in the pill; `size="sm"` inside a card or a `SectionHeader`),
SegmentedControl (one of a few values, a radio group, drawn exactly as
Tabs), RelativeTime ("3h ago",
the exact date, time and zone in a Tooltip; every table's times),
DurationCell (a duration with how it was measured in a Tooltip, a live one
in the foreground, a slow one in the warn tone),
Tooltip, Select (an option's `disabled` is the reason it cannot be picked: shown greyed with the reason on its right; clicks and arrow keys pass it by; `hint` is a quiet note on the right of a pickable one; `footer` sits under the options), TenantPicker, TimeRangePicker, StepPicker (the page's step beside the range, "Every": Auto shows what it resolved to, `min/hour`; each choice its points or why it is off; a footer says what each kind of chart does with it), TimeSeriesChart (uPlot, with
optional vertical `marks`; height from `--chart-h` unless given), Timeline
(placement waterfall: a stage is a bar from `start` to `end`, striped while
it has no `end`; a `point: true` stage is an instant, a dot at `start` with
its clock time and no duration, and it never extends the axis past itself;
`note` follows the label, the whole label is in its title), EventTable (a lifecycle event log: Run, pool, host;
every column sorts, and with `onSortChange`, `sort` and a `footer`
Pagination it is a server-paged, server-sorted table, where Details does
not sort: the server sorts by #, time and type only), LogView, Terminal (xterm.js in the LogView's frame,
Solarized inside via `terminalThemes`; `scheme` light or dark, else the
console theme; a transport-agnostic handle: `write`, `onData`, `onResize`) with
TerminalOverlay (the card over a dimmed screen), ServerList / ServerRow (a
run's servers: state, URL, wake and lifetime tags, start/stop/restart/remove
or, for a server kept after its run, detach; `hrefFor` links a name to its
own page; an expandable log the caller renders),
KeyValue (`columns` 1–3; `className` is on the `<dl>`, or with `children` on
the wrapper around grid and rows; `children` are full-width rows under the
grid, the whole one card surface unless `surface={false}`, for a KeyValue
already inside a `Card`: then the rows draw on the Card's padding with no
second frame: `FactRow({label, count?, aside?, detail?})`, a
labelled one-line fact with an optional control at its right and content
under it; `ImageRef({value})`, an image reference on one line, the registry
path muted, the name in the foreground, a digest shortened to the algorithm +
first 12 + `…` + last 8 hex (`shortDigest`, `splitImageRef`; tags whole),
the full reference in its title and copied by its copy affordance, wrapping
instead of truncating on a phone; `LabelsFact({labels, label?, first?, max?, defaultOpen?})`,
"Labels n" (or `label`, for Versions or a Template), a `LabelChips` preview
and a Show all / Hide toggle (the same text whatever the `label`), a ghost `Button` in the link colour
(`--accent-text`, also on hover; ServerList's Logs toggle stays neutral
because it sits among action buttons) (`aria-expanded`; its
accessible name is its visible text, so tests find it by `aria-expanded` and that text) that opens every pair, key over a
complete, selectable mono value with a copy affordance; collapsed by
default (`defaultOpen` for the gallery), "Labels –" and no toggle when there are none), IdChip, Code, PageHeader (with optional breadcrumbs; CrumbSep),
SectionHeader (a sentence-case `--text-lg` semibold title between groups
of cards, its `note` beside it, `actions` such as a `Tabs size="sm"` on
the right), ConfirmDialog, Dialog (a form modal), Toast (`useToast`),
EmptyState, Spinner, Skeleton, PartBar, Meter, Callout (below), and the
Session components (SessionTranscript, SessionComposer, InputTurn, MessageTurn, ThoughtTurn,
PlanTurn, ToolCall, SessionMarker, Note, SubagentStub, SubagentTree,
SubagentPanel, AgentMark, SessionRail, RailCard, RailFacts, ToolUsage;
below). Hooks: `useTheme`, `useDensity`, `useTerminalScheme`, `useNow` (the shared
clock relative times tick on), `useCopy`. All exported
from `src/index.ts` with typed props; icons from `@lux/design-system/icons`.

Formatting and utility classes used beside the components:

| Export / class | What |
| --- | --- |
| `formatTimestamp(t)`, `formatTimestampZone(t)` | "2026-09-30 22:38:08", and the same with the browser's zone name: the hover text of an exact time (`RelativeTime`, the launch-failed host's times) |
| `formatDuration(s)` | "1h 12m", "3.2s", "450ms", "0s" for zero; a negative figure keeps its sign, so callers clamp what cannot be negative |
| `rangeText(range)` | a `TIME_RANGES` value in running text: "last 24 hours" |
| `.pill-outline` | the outlined pill variant (`StatePill` for launch failed): the hue on the border, no fill |
| `.text-danger` | a figure in the danger colour beside its label (a non-zero failure count in a table) |
| `.spark-row` | an inline `Sparkline` with its figure, in a table cell |
| `.row-actions` | a table row's icon buttons, right-aligned (above) |
| `.field`, `.field-label` | a labelled value or control, the label above it (a form field; the launch-failed host's provider error) |

Cost additions (`src/Cost.tsx`, `format.ts`, `states.ts`; gallery section
"costs"):

| Export | What |
| --- | --- |
| `formatMoney(amount, currency, {decimals?})` | exact decimal string → `$1.2843`, `$0.15`, `€12,345.50`, `3.20 XTS`: rounded half to even to 4 decimals (`MONEY_DECIMALS`), trailing zeros trimmed down to cents; a non-zero amount that rounds to zero reads `<$0.0001` (`>-$0.0001` below zero); missing or unparseable → `–`. The same rule as the CLI (`internal/cli/money.go`), which writes the code after the number (`1.2843 USD`) and trims to the integer |
| `formatMoneyExact(amount, currency)`, `moneyIsRounded(amount, decimals?)` | every digit of an amount (`$0.000074`), and whether `formatMoney` rounded it |
| `Money({amount, currency, decimals?})` | one amount as `formatMoney` shows it; when rounded, the exact value in a Tooltip ("Exactly $0.000074"). `decimals={CENTS}` (2) rounds a headline figure (a KPI, a ranked list) to the cent: `$73.32`, its Tooltip "Exactly $73.3186" |
| `CostFigure({status, totals})` | a cost in a table cell, the same as `lux ls`'s COST: the total for one currency, `multi` for several, `–` while pending; `~` before a total that may still change (an estimate part, or `incomplete`); the Tooltip says the status in words and the exact amounts; a table cell holding one shows its Tooltip unclipped |
| `sumMoney(amounts)`, `compareMoney(a, b)` | exact sum and order of decimal strings of one currency |
| `formatUnit(v, "money", currency)` | the chart/tile unit for money (axes and tooltips) |
| `CostStatusBadge({status, waitingOn?})` | the status as a Badge with its meaning in a Tooltip |
| `costStatusStyle`, `COST_STATUS_LIST`, `CostStatus` | the mapping above |
| `ListPriceNote` | the page's one "list price" label, explained in a Tooltip |
| `ColorKey({color})`, `FamilyKey({family, displayName, color})` | a square swatch before its label (colour never without one) |
| `MoneyList({amounts, large?, decimals?})` | one figure per currency, side by side; `–` when empty |
| `familySlot`, `familyColor` | cost family → `--chart-N` (above) |
| `familyDisplay(families)` | each family's `{label, color}` from its describe `displayName` and `color` hint (the key when unnamed; `Compute` for compute): every view of cost families (Run card, Overview chart) resolves both here, so a family reads the same everywhere |
| `TimeSeriesChart` `stacked` | series stacked bottom-first as filled bands (28% fill, 2px edges); the tooltip adds a Total; hiding a series from the legend restacks the rest; a missing value adds nothing and shows `–` |
| `TimeSeriesChart` `bars` | each x is a bar for the bucket starting there, as wide as the smallest step of x (an amount per bucket, such as cost per hour: a line between two buckets would read as a ramp); with `stacked` the bars stack bottom-first, solid. A missing value draws no bar and a bucket with no figure stays empty, never a zero-height bar; positive values stack up from zero and negative ones (a refund) down from it, so no bar overdraws another. The x scale leaves half a bucket at either end; the hovered bucket is shaded and the tooltip names it ("19:00–20:00") (`barSegments`, `barRange`, `bucketText` in `src/chartData.ts`; gallery: "Cost per hour, as bars") |
| `TimeSeriesChart` `legendValues`, `legendNote` | a figure after each legend entry (its total over the range, `.tschart-legend-value`; the label is `.tschart-legend-label`, so a test finds it by its exact text), and a note at the legend's right end ("each bar is one hour") |
| `TimeSeriesChart` `currency` | the currency of `unit="money"` |

Cost panel (`src/CostPanel.tsx`; gallery section "costpanel"), what the
Overview's Cost panel is composed of:

| Export | What |
| --- | --- |
| `KpiStrip`, `Kpi({label, value, sub?, loading?, muted?})`, `KpiSub` | figures across the top of a card, the first wider; two across in a narrow container, four from 760px, and a strip of five (Total, Compute, Block storage, External, peak) 3 + 2 from 760px and five across from 960px; a muted Kpi is a figure the view hides but still states |
| `SplitBar({parts, whole, currency, label?, scale?})` | parts of one amount as one thin bar, each part its share of `whole`; a `faint` part is hidden by the view, drawn, never dropped; amounts in the title; `scale` sets its length against the largest row |
| `BreakdownTable({rows, lead, onRowClick?, blockStorage?})` | cost by one dimension: swatch and name (mono for ids and label values, `quiet` for the value-less row, a `pill` such as "revoked"), Runs, Compute, External, Total, Share per currency; `blockStorage` adds a Block storage column between Compute and External (lux's disk family, apart from both); a missing part is `–`, not $0; a non-zero share under 0.5% reads `<1%` |
| `LabelChips({labels, first?, max?} \| {entries, max?})` | a Run's labels as `key=value` chips, the `first` keys first, `max` shown and the rest counted (`+2`, their text in the title); `sortLabels(labels, first)` is that order as pairs, and `entries` takes such pairs already sorted (one of `labels` or `entries`, not both). Chips cap at 20ch; in a `LabelsFact` preview they may run to 40ch |
| `InfoStrip({tone, children})` | a one-line notice inside a card (why some figures read as they do) |
| `FilterBar({add, note?})`, `FilterChip({name, op, value, onRemove})` | the active filters as removable chips, the add control, and what the filters reach |
| `LabelFilterPopover({keys, values, notSet?, onApply, onKeyChange?})` | "＋ Label filter": a label key (with its Runs), then values as checkboxes with their cost, biggest first, a search (`matchValues`), and "(not set)"; Apply reports `{key, values, notSet}`; Cancel, Escape or a click outside leave things as they were |

Parts of a whole, ratios and page notes (gallery section "parts"):

| Export | What |
| --- | --- |
| `PartBar({parts, label, height?})` | one total split into parts: a single bar of segments `{label, value, color, text?}` with 1px gaps, each its share of the drawn parts' sum, each with a Tooltip (its `ColorKey` label, the caller's formatted `text`, its share). `value` is a number for geometry only; zero, negative or missing draws nothing; with nothing to draw the bar is an empty track. Pair it with a list of the parts |
| `partShares(values)` | each value's share and whole percent: largest remainder, so drawn parts add up to exactly 100; a drawn part under 1% reads `<1%`, never `0%` |
| `Meter({value, color?, decimals?})` | a ratio in a table cell: a 46px track with its fill (default `--chart-1`) and the percentage to its right in mono. The fill clamps to 0..1, the text keeps the true figure (`112%`); null or not finite is an en dash; a non-zero ratio that rounds to zero reads `<1%` (`meterFill`, `meterText`) |
| `Callout({children, icon?})` | the page's one explanatory sentence ("Pool cost is the machines only…"): a soft info-tinted box, `--text-sm`, between cards. Not a toast, an error strip or an `InfoStrip` (which qualifies figures inside one card) |
| `TimeSeriesChart` series `faded` | a lighter, opaque shade of the series' colour (`fadedColor`: mixed toward `--bg-surface`, keeping `FADE_KEEP`, 42%), the same in the plot, the legend and the tooltip: one family split into "charged to runs" and "not" (gallery: costs, "Host cost per day") |
| `fadedCss(color)` | the same faded shade for the DOM: `color-mix()` of a token toward `--bg-surface`, keeping `FADE_KEEP`, so a `PartBar` segment or swatch under a chart matches its faded series (gallery: parts, "Charged to runs vs unallocated") |
| `TimeSeriesChart` day bars | bars a day or wider put one x tick under each bar (`barTicks`), every k-th when they do not fit. They are UTC-day buckets (cost): the tick names the UTC date (`timeTickText`) and the tooltip the UTC day ("2026-10-09 UTC", `bucketText`), whatever the browser's zone |

Session: an agent Run as a conversation (gallery section "session"; the
console's Run page Session tab). Layout: `.session-layout` puts the
transcript beside a `SessionRail` of `--session-rail-w`; under 1000px of
content the rail is not drawn and a subagent opens in a modal `<dialog>`
with the `Dialog` frame (`.dialog`), its `SubagentPanel` `bare`.
`.session-pane-h` gives transcript and panel
`clamp(420px, 100vh − var(--session-pane-offset), 860px)`: room above for a
Run page's facts and tabs, and inside for the composer. The offset is a
token (600px), so a page with less above it can set its own.

| Export | What |
| --- | --- |
| `SessionTranscript({title?, subtitle?, actions?, itemCount, contentKey?, note?, status?, empty?, composer?})` | a card: head (title, adapter · model, an Everything / Messages `SegmentedControl` in `actions`), a scroller whose turns keep to `--measure`, a bar (read-only note, figures, Follow), the "N new · Jump to latest" pill, and `composer` under the bar. Follow as `LogView`: scrolling up turns it off, back to the bottom turns it on; `itemCount` growth while off is what the pill counts; `contentKey` keeps a streaming reply's tail in view. `empty` replaces the turns (an `EmptyState` before the first record). With a `composer` the bar drops its lock and read-only `note` |
| `SessionComposer({label, onSend, onInterrupt?, interruptTitle?, disabled?, placeholder?, defaultValue?, hint?, errorText?})` | the input under a transcript: a form named by `label` ("Message the agent", "Send to stdin"), a labelled 2-row textarea that grows to 160px, a `hint` line (where input lands, or why it cannot be sent), Interrupt (only with `onInterrupt`) and Send (primary). Send is off while the trimmed draft is empty; while `onSend` is pending Send is loading and the draft read-only; resolving clears the draft, rejecting keeps it and shows `errorText(err)` in a `role="alert"` line in the danger colour until the next edit. Ctrl/⌘+Enter in the textarea sends (`aria-keyshortcuts`; not while an IME is composing), Enter is a newline. The textarea is described by the hint and, while there is one, the error (`aria-invalid`). A rejecting `onInterrupt` is caught: its feedback is the caller's (the console toasts). `disabled` turns off the textarea and both buttons. Only as a `SessionTranscript`'s `composer` (it draws the card's hairline and padding) |
| `InputTurn({text, sender?, steer?, phase, error?, truncated?, attachments?})` | what lux delivered: Prompt or Steer, from whom, its phase as a Badge in words (`sent`, `accepted`, `consumed`, `failed`, each explained in its title) |
| `MessageTurn({text, streaming?, cutNote?})` | the agent's prose through `Markdown` (paragraphs, fenced code, lists, `code`, **bold**; text only, never HTML); a drawn caret while `streaming` (not blinking: only live dots animate) |
| `ThoughtTurn({text, seconds?})`, `PlanTurn({steps})` | a thought folded to "Thought for 6s"; a plan as a checklist, "n of m done", the step in progress in the foreground weight, done ones struck through and checked (never colour alone) |
| `ToolCall({name, summary?, summaryMono?, status, start?, end?, now?, input?, diff?, output?, error?, cutNote?, record?, children?})` | one folded row (tool icon by name: `toolIcon`, the name in mono, the summary, a live duration, a status icon with its word for screen readers) that opens to Input, Change (`DiffLines`: `+`, `-`, `@@` keep their sign beside their colour), Output and the record link (`record {label, href?, onClick?}`). An error's first line shows folded. `children`: its `SubagentStub` |
| `SessionMarker({type, children, summary?, summaryNote?})`, `Note({tone, children, detail?})` | a hairline with text for lux's own events (epoch, move, resume, turn end, compaction with an expandable summary, Run ended); one iconed line for a permission lux answered by policy (`Shield`) or a warning |
| `SubagentStub`, `SubagentTree`, `SubagentPanel` (`SubagentSummary`: `{id, title, agent?, model?, color, status, startedAt?, endedAt?, tools, subagents?}`) | the row under a task call (colour rail, agent name, `SubagentStatusBadge`, last activity, counts, Open; Enter or Space open it); the rail's tree (a guide per depth, `--subagent-indent`); one subagent in the rail's place: header ruled in its colour, back link up its parent chain, facts, id, "Briefed with" (its cut named), its turns, the adapter's footnote. `bare` drops the card frame for a `Dialog` |
| `AgentMark({color, live?})`, `agentSlot(slots, agent)`, `agentColor(slot)` | the agent colour dot, pulsing only while the subagent runs (none under reduced motion); a type's slot in first-seen order, and its `--agent-N` |
| `SessionRail`, `RailCard({title, count?, aside?, flush?})`, `RailFacts({items})` | the column beside a transcript, its cards and label/value lines |
| `ToolUsage({counts})` | calls per tool, most used first, one `--chart-1` bar each with the count beside it |

Icons for it (`@lux/design-system/icons`): `IconAgent`, `IconFile`,
`IconThought`, `IconCompress`, `IconShield`, `IconListCheck`, `IconMove`,
`IconLock`.

A cost family is always named with its `ColorKey` square: rows
(`FamilyKey`, `BreakdownTable`), KPIs and StatTiles (`swatch`), bar and
stacked chart legends and tooltips. Line charts keep a line key.

Behaviour shared by every chart and tooltip:

- **Y axis**: zero-based without a fixed `yMax`, the scale's top is the first
  multiple of a 1/2/2.5/5 × 10ⁿ step at or above the largest value (the
  stacked total when `stacked`), and gridlines sit on those steps. No point
  is ever above the top gridline, sparse data included (gallery: "Host cost,
  one hour costed"). When a value (a refund, a negative stacked sum) is below
  zero the scale's bottom is the last step at or below the smallest value,
  with gridlines down to it (gallery: "Cost by family, a refund hour");
  non-negative data keeps zero as the bottom (`niceScale` in `src/scale.ts`).
  A series prop that is a fresh but equal array does not rebuild the plot;
  new `x`/`ys` alone are applied with uPlot's `setData`.
- **Chart tooltip**: sized to its content (`width: max-content`); series
  labels never wrap.
- **Tooltip placement**: `side` is a preference. When the tooltip opens it is
  measured before paint: it flips to the opposite side when its side leaves
  the viewport, and shifts along that side (`--tip-shift`) to stay 8px inside
  it. A "list price" note at a card's right edge stays readable (gallery:
  Feedback, the right-aligned row). `className` and `style` go on its anchor
  (a `PartBar` segment is one).

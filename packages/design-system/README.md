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
bun run test           # bun test: formatMoney's rounding rule (src/format.test.ts)
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
  states.ts         run/host state -> hue family + label; cost status; cost family -> chart slot
  theme.ts          theme and density: toggles, persistence, cssVar()
  icons.tsx         the icon set
  *.tsx             components
gallery/            the gallery app (index.html, Gallery.tsx, fake data)
```

## Principles

- Clean, calm, comfortable. One elevated surface (the card, a soft hairline,
  no shadow), quiet headers, whitespace instead of rules. Every page opens
  with a `PageHeader` (title, one line of context, primary actions on the
  right). Nothing animates except live state dots, spinners and toasts.
- Names lead, ids support. Ids are plain mono in the muted colour with a
  copy affordance on hover or focus (`IdChip`), never boxed. Monospace is
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

## Density

Two settings, switched from the console's top bar (the rows icon) and persisted in
`localStorage["lux.density"]`, applied before first paint as
`<html data-density="compact">`. Only tokens change; every component reads
them.

| Token | Comfortable (default) | Compact |
| --- | --- | --- |
| `--text-md` (body) | 14px | 13px |
| `--text-{xs,sm,lg,xl,2xl,3xl}` | 12, 13, 15, 17, 22, 30px | 11, 12, 14, 16, 20, 28px |
| `--row-h` / `--row-h-dense` | 40 / 34px | 32 / 28px |
| `--control-h` / `-sm` / `-lg` | 32 / 26 / 38px | 28 / 24 / 34px |
| `--pad-card`, `--pad-cell` | 20, 14px | 16, 12px |
| `--gap`, `--gap-lg`, `--pad-page` | 20, 28, 32px | 16, 20, 24px |
| `--sidebar-w`, `--topbar-h` | 232, 52px | 208, 44px |
| `--chart-h` | 200px (240 at 1920, 280 at 2560) | 180px (210, 240) |

## Breakpoints

The shell reacts to the viewport; layout inside the content area reacts to
its container (`@container content`), so a rail and a full sidebar both get
the right grid.

| Viewport | Sidebar | Top bar | Content |
| --- | --- | --- | --- |
| < 768 | off-canvas drawer behind a menu button | brand, live dot, one scope menu (tenant, range, density, theme, session) | one column; state chips scroll sideways; tables scroll inside their card with the first column pinned; header actions drop below the title; the document scrolls |
| 768–1279 | icon rail (`--sidebar-rail-w`, 56px) | full controls | stat tiles 3 across, charts 2 across |
| 1280–1919 | full, collapsible to the rail (`localStorage["lux.sidebar"]`) | full controls | overview feed becomes a side column at 1100px of content; charts 2–3 across; stat tiles 6 across from 1400px |
| ≥ 1920 / ≥ 2560 | full | full controls | charts grow and go 3–4 across; the feed widens (`--feed-w`); lists cap at `--page-list-w` (1760px), detail pages at `--page-detail-w` (1920px) and centre; the overview is unbounded |

Page widths (`src/layout.css`): `.page` (detail), `.page-list` (tables), `.page-wide`
(dashboards). Grids: `.grid-stats` (2 / 3 / 6), `.grid-charts` (1 / 2 / 3 /
4 by container width), `.grid-2`, `.grid-3` (collapse to one column under
900px of content).

Tables (`Table`): `table-layout: fixed`; columns with a `width` keep it and
the rest share the remainder. `lead` marks the name column, `optional`
columns drop out when the table's container is under 1100px, and below the
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
| Chart | `--chart-1` … `--chart-8` (fixed order; cost families map onto them, compute is `--chart-1`), `--chart-grid` `--chart-axis` `--chart-label` `--chart-cursor`, `--chart-h`; unallocated cost uses `--st-neutral-dot` |
| Logs | `--log-stderr-bg` `--log-stderr-fg` `--log-line-hover` |
| Type | `--font-sans` `--font-mono`, `--text-{xs,sm,md,lg,xl,2xl,3xl}` (density-dependent), `--leading-{tight,normal}`, `--weight-{normal,medium,semibold}` |
| Spacing | `--sp-1` … `--sp-9` (2, 4, 6, 8, 12, 16, 24, 32, 48px); density-dependent `--gap` `--gap-lg` `--pad-page` `--pad-card` `--pad-cell` |
| Radius | `--radius-{sm,md,lg,pill}` (3, 5, 8px, pill) |
| Elevation | `--shadow-{sm,md,lg}` (popovers, dialogs, toasts; cards have none) |
| Motion | `--dur-{fast,normal,slow}` (100/180/300ms, 0 under reduced motion), `--ease` |
| Layout | `--sidebar-w` `--sidebar-rail-w` `--topbar-h` `--row-h` `--row-h-dense` `--control-h` `--control-h-sm` `--control-h-lg` `--page-list-w` `--page-detail-w` `--feed-w` |

State mapping (`src/states.ts`):

| Hue | Run states | Host states |
| --- | --- | --- |
| neutral | submitted, pending, provisioning, stopped, cancelled | provisioning, terminated |
| blue | scheduled, starting, resuming | registered |
| teal | running, busy | |
| violet | idle | |
| green | succeeded | ready |
| amber | stopping | draining, terminating |
| red | lost, failed | lost |

Cost status (`costStatusStyle`, `CostStatusBadge`): the `status` of
`GET /v1/runs/{id}/cost`, as a Badge whose Tooltip says what it means (and,
when incomplete, which sources it waits on).

| Status | Badge | Meaning |
| --- | --- | --- |
| `pending` | neutral "Pending" | no source has reported yet |
| `complete` | info "Estimate" | every source answered; some lines may still change |
| `incomplete` | warn "Incomplete" | a source has not answered or failed |
| `final` | success "Final" | every source settled |

Cost family colours (`familySlot`, `familyColor`, `familyColors`): a
family maps to a categorical `--chart-N` slot, never a raw colour.
`compute` is always `--chart-1`. A plugin's describe `color` hint picks the
slot: a name (`violet` → 7, `amber` → 4, `orange` → 2, `teal` → 3, `pink`
→ 5, `green` → 6, `red` → 8; blue names go to 7, as slot 1 is compute's)
or `#rrggbb` by nearest hue. Without a hint the family's name picks one of
slots 2–8, so it keeps its colour everywhere. When several families are
shown together, `familyColors` keeps compute and hinted families in place
and moves an unhinted family off a slot already taken.

## Components

Logo (the star, 16–32px; the detailed mark is `docs/brand/lux.svg`),
Button, IconButton, Badge, StatePill, StatTile, Sparkline, Card, Table, Tabs,
Tooltip, Select, TenantPicker, TimeRangePicker, TimeSeriesChart (uPlot, with
optional vertical `marks`; height from `--chart-h` unless given), Timeline
(placement waterfall), LogView, KeyValue, IdChip, Code, PageHeader,
SectionHeader, ConfirmDialog, Dialog (a form modal), Toast (`useToast`),
EmptyState, Spinner, Skeleton. Hooks: `useTheme`, `useDensity`. All exported
from `src/index.ts` with typed props; icons from `@lux/design-system/icons`.

Cost additions (`src/Cost.tsx`, `format.ts`, `states.ts`; gallery section
"costs"):

| Export | What |
| --- | --- |
| `formatMoney(amount, currency, {decimals?})` | exact decimal string → `$1.2843`, `$0.15`, `€12,345.50`, `3.20 XTS`: rounded half to even to 4 decimals (`MONEY_DECIMALS`), trailing zeros trimmed down to cents; a non-zero amount that rounds to zero reads `<$0.0001` (`>-$0.0001` below zero); missing or unparseable → `–`. The same rule as the CLI (`internal/cli/money.go`), which writes the code after the number (`1.2843 USD`) and trims to the integer |
| `formatMoneyExact(amount, currency)`, `moneyIsRounded(amount)` | every digit of an amount (`$0.000074`), and whether `formatMoney` rounded it |
| `Money({amount, currency})` | one amount as `formatMoney` shows it; when rounded, the exact value in a Tooltip ("Exactly $0.000074") |
| `CostFigure({status, totals})` | a cost in a table cell, the same as `lux ls`'s COST: the total for one currency, `multi` for several, `–` while pending; `~` before a total that may still change (an estimate part, or `incomplete`); the Tooltip says the status in words, the exact amounts and "list price" |
| `sumMoney(amounts)`, `compareMoney(a, b)` | exact sum and order of decimal strings of one currency |
| `formatUnit(v, "money", currency)` | the chart/tile unit for money (axes and tooltips) |
| `CostStatusBadge({status, waitingOn?})` | the status as a Badge with its meaning in a Tooltip |
| `costStatusStyle`, `COST_STATUS_LIST`, `CostStatus` | the mapping above |
| `ListPriceNote` | the page's one "list price" label, explained in a Tooltip |
| `ColorKey({color})`, `FamilyKey({family, displayName, color, swatch?})` | a square swatch before its label (colour never without one) |
| `MoneyList({amounts, large?})` | one figure per currency, side by side; `–` when empty |
| `familySlot`, `familyColor`, `familyColors` | cost family → `--chart-N` (above) |
| `familyDisplay(families)` | each family's `{label, color}` from its describe `displayName` and `color` hint (the key when unnamed; `Compute` for compute): every view of cost families (Run card, Overview chart) resolves both here, so a family reads the same everywhere |
| `TimeSeriesChart` `stacked` | series stacked bottom-first as filled bands (28% fill, 2px edges); the tooltip adds a Total; hiding a series from the legend restacks the rest; a missing value adds nothing and shows `–` |
| `TimeSeriesChart` `currency` | the currency of `unit="money"` |

Behaviour shared by every chart and tooltip:

- **Y axis**: zero-based without a fixed `yMax`, the scale's top is the first
  multiple of a 1/2/2.5/5 × 10ⁿ step at or above the largest value (the
  stacked total when `stacked`), and gridlines sit on those steps. No point
  is ever above the top gridline, sparse data included (gallery: "Host cost,
  one hour costed").
- **Chart tooltip**: sized to its content (`width: max-content`); series
  labels never wrap.
- **Tooltip placement**: `side` is a preference. When the tooltip opens it is
  measured before paint: it flips to the opposite side when its side leaves
  the viewport, and shifts along that side (`--tip-shift`) to stay 8px inside
  it. A "list price" note at a card's right edge stays readable (gallery:
  Feedback, the right-aligned row).

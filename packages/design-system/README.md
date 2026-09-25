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
  format.ts         bytes, durations, relative time, cores, percentages
  states.ts         run/host state -> hue family + label
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
- Missing values render as an en dash, never as `0`.

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
| Chart | `--chart-1` … `--chart-8` (fixed order), `--chart-grid` `--chart-axis` `--chart-label` `--chart-cursor`, `--chart-h` |
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

## Components

Button, IconButton, Badge, StatePill, StatTile, Sparkline, Card, Table, Tabs,
Tooltip, Select, TenantPicker, TimeRangePicker, TimeSeriesChart (uPlot, with
optional vertical `marks`; height from `--chart-h` unless given), Timeline
(placement waterfall), LogView, KeyValue, IdChip, Code, PageHeader,
SectionHeader, ConfirmDialog, Dialog (a form modal), Toast (`useToast`),
EmptyState, Spinner, Skeleton. Hooks: `useTheme`, `useDensity`. All exported
from `src/index.ts` with typed props; icons from `@lux/design-system/icons`.

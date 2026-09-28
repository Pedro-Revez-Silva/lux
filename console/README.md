# lux console

Operator UI for lux, served by `luxd` at `/`. React + TypeScript, built
with Bun only (no Node, npm or Vite).

## Develop

```bash
bun install          # once, at the repository root (a Bun workspace: console + packages/*)
cd console
bun run dev          # http://localhost:5173/  (Bun HTML-import server, HMR)
bun run typecheck    # tsc --noEmit
bun run build        # static files in dist/, assets under /
bun run preview      # serve dist/ at / with SPA fallback (what luxd does)
```

`dist/` is what `luxd` embeds (`go:embed`) and serves at `/`. Every path
outside `/v1/` and `/runner/` that is not a built file returns
`dist/index.html` (a missing `.js`/`.css`/… asset is a 404 instead); the app
routes client-side with the History API (`src/app/router.tsx`, at the root).
`/v1/...` and `/runner/...` keep their own JSON errors and 404s, and luxd
redirects the old `/console/...` paths to the same page at `/`.
The tenant and time range live in the query string so links carry their scope.

### Against a real luxd

The console is same-origin with the API. In development `dev.ts` proxies
`/v1/*` (and `/health`) to a luxd: `LUX_URL` if set, else the last
`run_tests.py --serve` environment (`/tmp/lux-dev-env.json` → `env.json`'s
`luxd_url`). SSE bodies stream through untouched.

```bash
cd tests && uv run python run_tests.py --serve --detach   # writes env.json (luxd_url, admin_key, ...)
cd ../console && bun run dev                              # proxies /v1 to that luxd
# an operator key (admin_key is a tenant admin key):
LUX_DATABASE_URL=<owner dsn from env.json> ../bin/luxd admin create-operator-key
cd ../tests && uv run python run_tests.py --down          # when done
```

The key is entered in the UI (sign-in screen), never configured in the server.

### Without a luxd

`bun run mock` (`mock.ts`) serves the app with a luxd stand-in: one run
with three servers, the servers API, a fake shell behind the exec
WebSocket, an event stream that stays open. Any key signs in.
`MOCK_STATE=stopped` makes the run stopped, `MOCK_AUTH=cloudflare-access`
signs in a person instead of a key, `MOCK_PREVIEW=` (empty) turns previews
off, `MOCK_OPEN_DELAY=<ms>` holds the shell's opening. For layout, states
and screenshots only: nothing it answers is real.

## Auth

Every API call sends `Authorization: Bearer <key>`. The key lives in
`sessionStorage["lux.key"]` (`src/api/auth.ts`): a reload keeps it, closing the
tab drops it, and "Sign out" at the foot of the sidebar clears it. A 401 from any call signs
out and shows the sign-in screen again.

Operator keys see every tenant; tenant keys their own. The role is learned from
`GET /v1/tenants`: a 403 means a tenant key, and the tenant picker, the Tenants
page and operator-only actions (Migrate, target-host choice on Resume) are
hidden. Other 403s render as "Your key cannot do this: …".

A deep link (`/runs/{id}/terminal`, `/preview-auth?to=…`) opened without a
session shows the sign-in on the page itself, saying where it continues;
the URL is kept, so signing in lands there. `/?next=<path>` continues to
`path` once signed in.

## API module (`src/api/`)

| File | What |
| --- | --- |
| `types.ts` | Mirrors of the Go JSON types (`internal/server/{api,status,history,feed,output,blobs}.go`) |
| `client.ts` | `request()`/`apiFetch()`: bearer auth, `?tenant=` from the URL scope, `ApiError {status, code, message, details}`, `download()` via blob |
| `endpoints.ts` | One typed function per endpoint (`api.runs`, `api.stopRun`, …) |
| `query.ts` | `useQuery(key, fn, {interval})`: polling with abort on unmount/key change, paused while the tab is hidden; `useNow()` |
| `sse.ts` | `streamSSE()`: fetch-streamed `text/event-stream` parsing (EventSource cannot send headers), reconnect with `Last-Event-ID` and backoff |
| `exec.ts` | `openExec()`: the exec WebSocket (`internal/server/stream.go`'s protocol: a `StreamOpen`, then base64 `StreamData` both ways, `{rows, cols}` to resize, `{exitCode}` or `{error}` at the end). A browser cannot put a header on a WebSocket: behind Cloudflare Access the cookie rides along; with a key, a single-use ticket (`POST /tickets`) goes in `?ticket=` |

Run-scoped calls (`/runs/{id}/…`, `/hosts/{id}/…`, `/artifacts/…`) do not send
`?tenant=`: the object names its tenant. Lists and `/status`, `/history`,
`/events` do, when a tenant is selected.

## Pages

| Route | Page | Data |
| --- | --- | --- |
| `/` | Overview: stat tiles, charts over the selected range, live activity feed | `/status` (5s), `/history?since=` (30s), `/events` SSE |
| `/runs` | Runs table with state presets and chips, resumable/host/label filters, "Load more" (`before=`) | `/runs` (5s) |
| `/runs/:id` | Header + actions (Terminal, Stop, Cancel, Resume, Migrate), tabs (`?tab=`): Output (SSE), Servers (list, Add server, start/stop/restart/remove, a log per server), Timeline (per-epoch waterfall), Resources (charts, epoch marks), Events, Snapshots & artifacts, Spec | `/runs/{id}` (3s while active), `/runs/{id}/output`, `/runs/{id}/servers` (+ `/servers/{name}/log`), `/history`, `/events`, `/snapshots`, `/artifacts` |
| `/runs/:id/terminal` | A shell in the run's container: xterm.js over the exec WebSocket, font size (persisted), Reconnect, Open in new tab; exited / lost overlays; an empty state while the run is not running | `/runs/{id}` (5s), `GET /runs/{id}/exec` (WebSocket; `POST /runs/{id}/tickets` first with a key) |
| `/hosts` | Hosts table (pool/state filters, include terminated) with allocation bars | `/hosts` (5s), `/pools` |
| `/hosts/:id` | Details, Drain, lifecycle timeline, live placements, usage charts, recent runs | `/hosts/{id}` (5s), `/hosts/{id}/history`, `/runs?host=` |
| `/pools` | Pools with host counts | `/pools`, `/hosts` |
| `/tenants` | Tenants (operators); a row sets the tenant scope and opens Overview | `/tenants` |
| `/preview-auth?to=` | No shell: the preview listener sends a browser here without a cookie. Mints a `preview` ticket for the run in `to`'s host (`<server>-<runsuffix>.<domain>`) and redirects to `https://<host>/.lux/auth?ticket=…&to=<path>` | `POST /runs/{id}/tickets` |

Pages live in `src/app/pages/`; `common.tsx` holds the shared bits (error
blocks, usage bar, series builders, scoped links).

## Layout

```
console/
  index.html            entry; loads the design system's stylesheets, shell.css and src/main.tsx
  dev.ts / build.ts / preview.ts
  src/main.tsx
  src/api/              typed client, polling hook, SSE, Go type mirrors
  src/app/              shell (sidebar, top bar), router, global scope, sign-in, pages
```

## Design system

Tokens, styles and components come from `@lux/design-system`
([packages/design-system](../packages/design-system/README.md)), a Bun
workspace package with its own gallery (`bun run gallery` there): the
principles, density, breakpoints and tokens are documented there.

The shell (`src/app/Shell.tsx`, `shell.css`) owns the sidebar (full, rail,
drawer; the collapse arrow at its top, who is signed in and "Sign out" at
its foot), the top bar (section name, live indicator, tenant and range
pickers, density and theme toggles; folded into one menu on phones) and the
scrolling content area.

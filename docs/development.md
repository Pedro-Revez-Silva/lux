# Development

## Layout

```
cmd/            one package per binary: luxd, lux-runner, lux-shim, lux, lux-fake
internal/
  server/       luxd: API, runner hub, scheduler, reapers, output relay
  store/        Postgres access, RLS scopes, migrations (store/migrations/*.sql)
  blob/         S3
  runner/       lux-runner: placements, snapshots, uploads, re-adoption
  podman/       Podman through its CLI
  shim/         lux-shim: PID 1, output file, redaction
  adapter/      generic, acp, claude-code, codex
  proto/        luxd ↔ runner and runner ↔ shim messages
  spec/         the RunSpec: parsing, validation, defaults
  cli/, client/ the CLI and its API client
console/        the operator console: React + TypeScript, built with Bun; console.go embeds dist/ in luxd
packages/design-system/  the console's tokens and components, and their gallery
tests/          the end-to-end harness (Python, uv, pytest)
docs/
```

## Building

```bash
make build        # the console (console/dist, needs Bun), then static binaries in bin/
make lint         # go vet, gofmt
make unit         # go test ./... (store tests need Postgres; see below)
make tf-validate  # deploy/terraform: fmt -check, init -backend=false, validate, mocked terraform test
```

`make tf-validate` (`scripts/tf-validate.sh`, needs Terraform >= 1.10) is
also what `.github/workflows/terraform.yml` runs on pushes to main and pull
requests touching `deploy/terraform/`. It needs no cloud credentials: the
`terraform test` suites (`deploy/terraform/aws/tests/`) use mocked
providers, and check, among other things, that the control host's
user_data stays under EC2's 16 KiB limit. After changing a module's
providers, refresh its lock file for every platform it carries:
`terraform providers lock -platform=linux_amd64 -platform=linux_arm64 -platform=darwin_arm64 -platform=darwin_amd64`.

`go build` alone works too: luxd then embeds whatever `console/dist` holds,
and without a console build serves a page saying how to make one. To work
on the console, see [console/README.md](../console/README.md); on its design
system and gallery, [packages/design-system](../packages/design-system/README.md).
Both are one Bun workspace: `bun install` at the repository root.
`make console` removes `console/node_modules` and `packages/*/node_modules`
before installing: a checkout from before the workspace has a stale
`console/node_modules` with its own React, and a console built with it
renders blank ("Cannot read properties of null (reading 'useState')"). Run
by hand, do the same: `rm -rf console/node_modules packages/*/node_modules
&& bun install`.

The toolchain and every dependency are kept at their latest release.

## Releases

`make dist VERSION=vX.Y.Z` (`scripts/dist.sh`) builds the contract's
tarballs into `dist/`: `lux_<version>_linux_{arm64,amd64}.tar.gz` (`FEATURES`,
`bin/luxd`, `bin/lux`, and `lib/lux/runner/linux-{arm64,amd64}/{lux-runner,lux-shim}` —
both runner arches in every linux tarball, so any luxd serves both),
`lux_<version>_darwin_{arm64,amd64}.tar.gz` (CLI only), and `SHA256SUMS`
over them. Every binary is static (`CGO_ENABLED=0`), `-trimpath`, with its
version baked in (`lux --version`, `luxd version`). Unpacking a Linux
tarball into `/usr/local` gives the default `runner_bin_dir` layout.

`FEATURES` (the `FEATURES` array in `scripts/dist.sh`) names, one per line,
what a deployer may rely on in this release ([Operations](operations.md#upgrading));
`dist.sh` fails if a tarball lacks it. A feature a deployer must detect
before using it adds a line there. Configuration changes follow
[the compatibility rules](operations.md#configuration-compatibility).

`.github/workflows/release.yml` runs `make dist` and publishes the result
as a GitHub Release on every `v*` tag.

## The API spec

The tenant API (`/v1/...`, in `internal/server`) is declared with
[huma](https://huma.rocks): each operation is registered with typed input
and output structs, and the OpenAPI spec is generated from them. The
committed copy is `docs/openapi.yaml`; after changing an operation or a
type it carries, regenerate it:

```bash
go run ./cmd/luxd openapi > docs/openapi.yaml
```

`TestOpenAPIIsCurrent` fails while the committed copy is stale. A running
luxd serves the same spec at `/openapi.yaml` and `/openapi.json`. The
runner's routes (`/runner/v1/...`) are not part of it.

## The end-to-end harness

lux's real bugs live at the seams between luxd, the runner, Podman, the
container and the shim, where unit tests cannot see. So every feature is
tested end to end through `tests/run_tests.py`. Tests drive **the `lux`
CLI** wherever possible, which tests the CLI and the behaviour at once. They
go around it only for admin bootstrap (`luxd admin`), for things no client
can see (the database, a host's disk, nftables), and for fault injection.

**Console suites share one browser page per file.** A suite that drives the
web console signs in on one page for the whole file (`test_console.py`'s
`page` fixture resets it between tests: signed out, default size, local
settings cleared). So keep a file's tests alike, and start a new file for
a different part of the console rather than growing one without end.

```bash
cd tests
uv run python run_tests.py -j 4              # all suites, across 4 environments (~4 min; make e2e)
uv run python run_tests.py                   # all suites in one environment, serially (~13 min)
uv run python run_tests.py --infra-only      # only check the environment itself
uv run python run_tests.py suites/test_x.py  # one suite; -x, -k work as in pytest
uv run python run_tests.py --keep            # keep containers and database to debug
uv run python run_tests.py --hosts 3         # more simulated hosts
uv run python run_tests.py --real-ec2        # EC2 suites against real AWS (nightly)
uv run pytest test_harness_config.py         # the harness's own config; no Docker (make harness-unit)
uv run pytest test_fake_ec2.py               # the fake EC2's answers; no Docker (make harness-unit)
```

Your own lux settings never reach the harness's `lux` and `luxd`: every
`LUX_*` variable is dropped except the harness's `LUX_TEST_*` and
`LUX_DEBUG`, and so are your CLI config (`XDG_CONFIG_HOME`) and any
`/etc/lux/luxd.toml`. A `LUX_TENANT` pointing at your deployment, say,
changes nothing in a test.

Each invocation:

1. Builds every `cmd/<name>` into `bin/` (static) and the fake agent's
   image (`tests/images/fake`).
2. Starts (or reuses) a shared **Postgres** (`lux-e2e-postgres-18`, port
   55432, `LUX_TEST_PG_PORT`) and an S3-compatible store, **versitygw**
   (`lux-e2e-s3`, port 59000, `LUX_TEST_S3_PORT`; the older
   `LUX_TEST_MINIO_PORT` is still read), and creates a database and a
   bucket for this run. `LUX_TEST_S3_IMAGE` replaces the image
   (default `versity/versitygw:v1.7.0`), e.g. with another versitygw
   release: it is started with versitygw's arguments and polled at
   `/health`. A `lux-e2e-minio` container from older checkouts is no
   longer used and is left as it is; while it runs it holds port 59000,
   so stop it (`docker rm -f lux-e2e-minio`) or set `LUX_TEST_S3_PORT`
   to a free port. A `LUX_TEST_MINIO_PORT` still set in your shell
   carries the old MinIO port over, so it collides the same way.
   A running `lux-e2e-s3` is reused only if its image and host port
   match `LUX_TEST_S3_IMAGE` and `LUX_TEST_S3_PORT`; otherwise the run
   stops and says which differs, and never replaces it.
3. Creates a Docker network and N **simulated hosts**. Each host is a
   privileged `quay.io/podman/stable` container with its own Podman
   storage, `bin/` mounted at `/opt/lux`, a `containers` range in
   `/etc/subuid`, and the test images preloaded.
4. Runs `luxd migrate` and then `luxd serve` on the network's gateway
   address, which the hosts, the tests and presigned S3 URLs can all
   reach.
5. Runs pytest. Fixtures start runners inside hosts
   (`runners.start(hosts[0])`), so a test can kill a host, restart a runner
   or stop one mid-Run.
6. Tears everything down. Logs stay in the directory it prints:
   `luxd.log`, `host-a/runner.log`, `host-a/podman-ps.txt`, …

The store unit tests create throwaway databases in the harness's Postgres:

```bash
LUX_TEST_PG='postgres://lux:lux@127.0.0.1:55432/postgres?sslmode=disable' go test ./internal/store
```

`LUX_TEST_ASSIGN_HOLD` is a fault-injection hook read by `lux-runner`
only, from its own environment: setting it for `run_tests.py` does not
reach the runner. It names a file inside the host. While that file exists,
the runner holds each assignment it receives before taking it up and
acking it, checking every 100 ms, so luxd has a placement `assigned` that
its runner does not know yet; removing the file releases it. Unset, it does
nothing. A test passes it when it starts the runner, and creates and
removes the file with `Host.exec`:

```python
runners.start(hosts[0], environ={"LUX_TEST_ASSIGN_HOLD": "/tmp/lux-hold"})
hosts[0].exec("touch", "/tmp/lux-hold")
# ... the next assignment is held ...
hosts[0].exec("rm", "-f", "/tmp/lux-hold")
```

With the hook set, the runner also appends a line `<runID> <epoch> <subID>`
to `<hold>.unknown` (here `/tmp/lux-hold.unknown`) each time it ends an
output subscription, with no error, for an epoch it does not hold. A test
waits for that line to know the runner has answered "not mine" for a held
assignment before it removes the hold:

```python
hosts[0].exec("cat", "/tmp/lux-hold.unknown", check=False)  # "run_… 2 sub_…"
```

### A lux to develop against

`--serve` brings the same environment up and leaves it running, with a
runner on every host and a tenant ready to use:

```sh
cd tests
uv run python run_tests.py --serve                       # up until Ctrl-C
uv run python run_tests.py --serve --detach --hosts 3    # up in the background
uv run python run_tests.py --serve --image ghcr.io/acme/agent:dev   # preload from local Docker
uv run python run_tests.py --serve --preview-local 8090  # previews at http://<host>.lux.localhost:8090
uv run python run_tests.py --serve --nested              # hosts offer nested containers
uv run python run_tests.py --down                        # take the detached one down
```

It prints and writes `env.json` in its log directory. `luxd_url` and
`api_key` (scopes `run` and `read`) are what a client needs. `admin_key`
and `tenant_id` are there too. `--image` (repeatable) copies an image from
the local Docker into every host, so Runs using it never pull. The
`lux-fake` test agent is always preloaded as `localhost/lux-fake:test`.
`--nested` starts every runner with `lux-runner --nested`, so Runs with
`sandbox.nestedContainers` ([runspec](runspec.md#nested-containers)) are
placed instead of waiting, and preloads `localhost/lux-nested:test`
(rootless Podman inside, from `tests/images/nested`) to try them with.
Without it no host offers nested containers. `--nested` without `--serve`
is refused: the suites start their own nested runners.

### Try branch previews locally

`scripts/demo-wake.sh` shows a branch preview end to end on one Linux
machine: a server that sleeps while nobody looks at it, wakes on the
branch's latest commit when someone does, and keeps its state across
wakes. It needs what the harness needs (Docker, Go, uv; no Podman, no
Bun: the hosts are Podman containers, and the console is not needed),
and a browser on the same machine (Chrome, Chromium or Firefox: they
resolve `*.localhost` to this machine without DNS).

```sh
scripts/demo-wake.sh up                 # lux (--serve --preview-local 8090), a git server, the orchestrator
scripts/demo-wake.sh open               # prints a sign-in link: open it within 60s
scripts/demo-wake.sh push "hello from commit B"   # a new commit on main
scripts/demo-wake.sh status             # the server and its Run
scripts/demo-wake.sh logs               # what the orchestrator did
scripts/demo-wake.sh down
```

What it brings up:

- `run_tests.py --serve --detach --preview-local 8090`: the dev
  environment, with previews at `http://<host>.lux.localhost:8090`
  ([previews on this machine](operators.md#previews); the listener on
  `127.0.0.1:8090`).
- The e2e suite's git server with a repository `app` (`message.txt`:
  "hello from commit A").
- `examples/preview-orchestrator`, a reference owner: it creates the
  server `web.pr1.lux.localhost` (wake on request, `lux-fake app`: the
  page shows the checkout's commit and a visit counter kept on the state
  volume), follows `GET /v1/events`, and on `server.wake_requested`
  resumes the server's Run with `sync: [{repo: app, ref: main}]` (the
  first time, submits a servers-only Run and attaches the server); on
  `server.idle`, stops the Run.

Then, in the browser: open the link from `open` (it signs the browser in
to that server and lands on `/`). The waking page shows "Asked the
orchestrator to start it", then the Run being placed, restored, updated
and started, and drops into the app: commit A, visit 1. Leave it for
`DEMO_IDLE` (default `1m`): the orchestrator stops the Run. Run
`push "hello from commit B"`, reload the page: it wakes again, on commit
B, and the visit counter carries on (2). Pushed while the preview is up,
`push` also syncs its Run (`lux sync`, what an owner does on a forge
webhook), and a reload shows the new commit without a wake. A host name nothing serves
(`http://gone.lux.localhost:8090/`) says "This preview is gone".

`DEMO_PREVIEW_PORT` changes the port (8090), `DEMO_IDLE` the idle time.
Signing in through the console's `/preview-auth` page (what a browser
without a cookie is sent to) needs the console built (`make console`,
Bun); `open` goes around it with a ticket. A headless walk-through with
screenshots: `cd tests && uv run python ../scripts/demo-wake-verify.py
/tmp/shots` (Playwright's Chromium: `uv run playwright install chromium`).

### Agent harnesses: one set of tests for every agent

Every coding agent lux drives is described once, in `tests/harnesses.py`:
its adapter, how to build a spec for it, which credentials it needs, and
what its protocol can do (`Caps`). `tests/suites/test_agents.py` takes a
`harness` fixture, so each test runs once per agent, in two variants:

| Variant | What runs | When |
| --- | --- | --- |
| `<agent>-fake` | `lux-fake` speaking that agent's protocol | always |
| `<agent>-real` | the real CLI, with real model calls | with `-m agents` and its credentials |

Tests assert from capabilities, never from an agent's name. For example,
input sent mid-turn joins the running turn only where
`caps.steer_joins_turn` (Codex, OpenCode, Claude Code when a tool call
follows), and a `lux.input.consumed` receipt is expected only where
`caps.steer_receipt`. **Adding an agent is one `Harness` entry**
(plus a `lux-fake` protocol mode if it speaks a new protocol). Every
existing test then covers it. Use `@harnesses(pred)` to limit a test to
the agents it concerns.

`lux-fake` speaks all three protocols: ACP by default, Claude Code's
stream-json (`-p --input-format stream-json …`), and Codex's app-server
(`app-server`), with the steering behaviour each real agent has: a prompt
sent during a turn is read after the script line running when it came
(the next step), with Claude Code's `command_lifecycle` frames, Codex's
`userMessage` items, and OpenCode's event bus and `prompt_async` (`lux-fake
acp --port <p>`). It follows a script from the prompt (`write f text`,
`sleep 5`, `history`, …; see `cmd/lux-fake/main.go`), keeps a transcript on
a state volume, and resumes from it. Its ACP replies stream in chunks
without line breaks, as real agents' do. `sh <command>` runs a command as
the agent's shell tool, reported as each protocol reports a real one
(Claude Code's `Bash` tool_use and tool_result, Codex's
`commandExecution` item, OpenCode's `execute` tool_call), so the steering
tests assert the same tool events on fake and real variants.

### EC2 pools: fake by default, real nightly

`suites/test_ec2_*.py` (scaling, failures, spot) run luxd's real EC2 provider against a **fake EC2**
(`tests/fake_ec2.py`). The fake is an HTTP server that speaks the three
EC2 API calls lux makes (RunInstances, TerminateInstances,
DescribeInstances). Each "instance" is a new simulated host that boots
`lux-runner` from the instance's user data, as a real AMI would. Tests
can make launches fail, or make instances never register. A
`RunInstances` with `DryRun` (the check when a pool is set) answers
`DryRunOperation`, or the error the real call would get: a launch
template not in `ec2.launch_templates` is
`InvalidLaunchTemplateName.NotFound`, and an injected `launch_failures`
code fails it too, except the capacity shortages a dry run does not test
(`InsufficientInstanceCapacity`, `InsufficientCapacity`).
`UnauthorizedOperation` is a 403, as EC2's. Dry runs are recorded in
`ec2.dry_runs`, not in `ec2.calls`; `tests/test_fake_ec2.py` checks these
answers without Docker.

`--real-ec2` runs the same suite against AWS. The tests that need the
fake's failure injection are skipped. It needs:

- AWS credentials in the environment, for both luxd and the test
  (boto3);
- `LUX_TEST_EC2_TEMPLATE`: a pool template JSON
  (`{"region", "launchTemplate", "instanceType", "subnets"}`). Its launch
  template's AMI must have Podman and lux-runner (see
  [operations](operations.md#ec2-pools)), and its instances must reach
  luxd's `LUX_PUBLIC_URL`;
- a luxd reachable from those instances, which in practice means running
  the suite from inside the VPC.

Run it nightly. It launches real instances and terminates every one it
started.

### Real agents (opt-in)

The `-real` variants make model calls, so each runs only when its
credentials are set and the suite is selected with `-m agents` (which also
makes the harness build the agents' images):

| Agent | Variables |
| --- | --- |
| Claude Code | `LUX_TEST_ANTHROPIC_API_KEY`, optional `LUX_TEST_ANTHROPIC_BASE_URL`, `LUX_TEST_CLAUDE_MODEL` (default `haiku`) |
| Codex | `LUX_TEST_OPENAI_API_KEY`, optional `LUX_TEST_OPENAI_BASE_URL`, `LUX_TEST_CODEX_MODEL` |
| OpenCode | `LUX_TEST_OPENCODE_AUTH` (an `auth.json`), `LUX_TEST_OPENCODE_CONFIG` (an `opencode.json`), `LUX_TEST_OPENCODE_MODEL` (`provider/model`) |

```bash
LUX_TEST_ANTHROPIC_API_KEY=sk-… uv run python run_tests.py suites/test_agents.py -m agents
```

The harness copies each CLI installed on the developer machine (the native
binary, never a Node launcher) into a test image built from
`tests/images/agent`. Credentials go in as secrets, never as mounted
config.

`suites/test_input_images.py::test_real_agent_reads_an_image` checks that
each real CLI reads a steer's image. For OpenCode, the model's entry in
`LUX_TEST_OPENCODE_CONFIG` must say it takes images (`"attachment": true`,
`"modalities": {"input": ["text", "image"], "output": ["text"]}`) when it
is a custom provider's: without it, the model answered that it does not
support image input.

### Guards are mutation-checked

When you add a guard (fencing, RLS, redaction), break it on purpose and
watch its test fail before you trust the test.

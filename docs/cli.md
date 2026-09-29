# CLI

`lux` talks to luxd. It reads its configuration from flags, then the
environment, then `~/.config/lux/config.toml`:

```toml
url = "https://luxd.example.com"
api_key = "lux_…"
```

| Flag | Env | |
| --- | --- | --- |
| `--url` | `LUX_URL` | luxd's address |
| `--api-key` | `LUX_API_KEY` | an API key (scopes: `read`, `run`, `admin`; or an operator key) |
| `--tenant` | `LUX_TENANT` | with an operator key: one tenant only (id or name) |
| `-o json` | | machine-readable output, for every command that prints data |

**Exit codes:** `0` success; the Run's own exit code for `run --follow`,
`run --wait`, `resume --follow` and `wait`; `3` not found; `4` conflict or
invalid spec (for example, steering a stopped Run); `5` a tenant quota
was reached; `1` anything else.

## Runs

```bash
lux run -f spec.yaml [--follow | --wait] [--name N] [-l k=v] [--idempotency-key K] [--secrets-from .env]
lux run --image alpine -- echo hello           # a quick generic Run
lux ls [--state running,stopped] [-l team=x] [--resumable] [--host H] [--limit N]   # with a COST column
lux get <run>                                  # state, placements, usage
lux logs <run> [-f] [--since <cursor>] [--events] [--stderr=false]
lux events <run>                               # lifecycle events
lux wait <run> [--state s1,s2] [--timeout 5m]  # default: until it ends; exits with its code
```

Specs are YAML or JSON (`-f -` reads stdin). A secret's value can come from
the environment (`value: ${GITHUB_TOKEN}`), from a `.env` file
(`--secrets-from`), or, when the spec omits the value, from an environment
variable of the same name.

`lux logs -o json` prints one JSON record per line:
`{"cursor","epoch","seq","t","ch","data"|"event"}`. Pass a record's
`cursor` to `--since` to continue from it, across placements and hosts.

## Steering

```bash
lux steer <run> "also add a test" [--interrupt] [--request-id ID]
lux interrupt <run>
```

Agents get the text as a message. Generic workloads get it on stdin. See
[adapters](adapters.md) for when a message is delivered.

## Stop, resume, cancel

```bash
lux stop <run> [--wait]         # graceful; snapshot; resumable
lux resume <run> [--wait | --follow] [--input "..."] [--secret NAME=VALUE] [--secrets-from .env] [--from-snapshot ID] [--disk SIZE] [--to HOST]
           [--add-repo name=url[@ref][,ref=REF][,credential=SECRET][,path=/abs][,push=false]]... [--request-id ID]
lux cancel <run> [--wait]       # final (a snapshot is still taken)
lux snapshots <run>             # where each snapshot lives
```

A resume needs the Run's secrets again. lux looks for each one in
`--secret`, then `--secrets-from`, then an environment variable of the same
name.

`--add-repo` (repeatable) adds a repository to a stopped, lost or failed
Run. The runner clones it into the restored workspace before the Run
starts again (see [Git](runspec.md#git)):

```bash
lux resume run_x --add-repo two=https://github.com/o/two.git@main,credential=GIT_TOKEN
lux resume run_x --add-repo docs=git@github.com:o/docs.git,ref=v2,push=false --request-id r-1
```

- An `@` names the ref only after the URL's last `/` and `:`, so
  `git@github.com:o/r.git` has none. `ref=` names it explicitly (don't use both).
- A new `credential` is a secret the runner alone uses, and its value is
  found like any other secret's.
- The name must be new (422 otherwise). A Run already resuming refuses an
  added repository (409).
- The request id, from `--request-id` or generated, is printed on stderr
  as `request <id>`. The repository's `git.clone` event carries it.

## Git

```bash
lux push <run> [--wait]         # push each repository to git.push.branch (leased)
```

## Diff

```bash
lux diff <run> [--repo NAME] [--base clone|head] [--stat] [--color never|always|auto] [-o json]
```

What a Run changed in its repositories, while it runs and after it stops
(stopped, lost, failed, succeeded): per repository, from its base to its
working tree, committed changes, staged, unstaged and untracked (not
ignored) files alike.

- `--base clone` (the default) diffs from the commit the repository was
  cloned at; a resumed Run keeps its original base, and a repository added
  on resume diffs from its own clone. `--base head` diffs from the
  checkout's `HEAD`: uncommitted work only.
- While the Run's container runs, the diff is computed there, now
  (`live`). Otherwise it is the one saved with the latest snapshot
  (`snapshot`), which has both bases. A Run that has never stopped since it
  started (or whose snapshots predate this) has none: exit code 3.
- Each repository's section starts with a comment line, which `git apply`
  skips, so the output applies as it is:

  ```
  # repo app: 1a2b3c4d5e6f..9f8e7d6c5b4a (snapshot, snapshot snap_… at 2026-09-29T10:00:00Z)
  diff --git a/src/x.go b/src/x.go
  …
  ```

  `, TRUNCATED` ends it when the patch was cut at its limit (10 MiB per
  repository, binary files' content included): it then holds only the
  whole files' diffs that fit, possibly none, so it still applies. `--stat`
  prints `git diff --stat`'s lines instead, for the whole diff even when
  its patch was cut.
- Colour follows `git diff`: on when stdout is a terminal (`--color auto`,
  and `NO_COLOR` unset). Nothing is printed, and the exit code is 0, when
  nothing changed. A repository whose diff failed is reported on stderr and
  the exit code is 1 (`base_unreachable`: its base is no longer in its
  history; `--base head` still works). Files with clean/smudge filters,
  which are never run, are compared raw, and named on stderr.
- `-o json` prints the API's response: per repository `repo`, `push`,
  `base`, `head`, `source`, `snapshotId`, `at`, `truncated`, `files`,
  `insertions`, `deletions`, `fileStats` and `patch`. The API also answers
  `Accept: text/x-diff` with the patches alone
  (`GET /v1/runs/{id}/diff`).

## Files

```bash
lux artifacts <run> [--download DIR]
```

## Hosts and pools

```bash
lux hosts ls [--all] [--pool P] [--state S]
lux hosts get <host>            # lifecycle, capacity, allocation, live Runs
lux hosts drain <host> [--force-evict]   # admin: no new Runs; without --force-evict its live Runs finish where they are
lux hosts price <host> --hourly-price 0.40 --currency USD   # admin: a static host's flat price, from now on
lux hosts price <host> --clear           # admin: no price, so its Runs get no compute cost from now on
lux pools ls
lux pools set <name> --provider static|ec2 [--min N] [--max N] [--warm N] [--template JSON]
lux pools set <name> --provider static --hourly-price 0.40 --currency USD   # default price of hosts registering into it
lux pools rm <name> [--force-evict]      # admin: cordons its hosts, terminated once idle; --force-evict stops their live Runs too
```

A static pool's `--hourly-price` is copied to each host when it first
registers. Changing it later does not reprice the pool's existing hosts:
use `lux hosts price` for those. `lux pools set` replaces the whole pool,
the default price included, like every other field: a `pools set` without
`--hourly-price` leaves the pool with no default price. Each price or
capacity change starts a new rate period, and earlier periods are never
changed ([Run costs](costs.md), section 2). `ec2` pools take no price: the
provider prices their hosts.

## Status and history

```bash
lux status                      # Runs by state, queue, time to start, hosts, capacity now
lux history [--since 24h]       # the same over time
lux history <run>               # a Run's resource use, across placements
lux history --host <host>
lux events --all                # every Run's events as they happen
```

## Costs

```bash
lux cost <run>                  # totals per currency (final/estimate), families, lines, sources
lux costs [--since 7d] [--by family] [--by label:team] [--family ai] [--interval day]
lux costs --from 2026-09-01T00:00:00Z --to 2026-09-08T00:00:00Z --by tenant   # operators
```

Amounts are list prices ([Run costs](costs.md)), per currency: amounts in
different currencies are never added. They are shown rounded half-even to
4 decimals with trailing zeros trimmed, and a non-zero amount below that
as `<0.0001`; `-o json` prints luxd's exact response. The console rounds
the same way. A value lux does not have is `—`, never `0`.

- `lux cost` prints the status: `pending` (nothing reported yet),
  `incomplete` (naming the sources that have not answered), `complete`, or
  `final`.
- `lux costs --by` takes `tenant` (operators), `pool`, `host`, `family`,
  `run` or `label:KEY`, up to twice. `--interval hour|day` adds a series.
  Ranges are whole UTC hours, at most 90 days. With an operator key and no
  `--tenant`, the hosts' unallocated cost is shown too, and `--by host`
  adds each host's ALLOCATED and UNALLOCATED. A host's allocated plus
  unallocated is its billed cost for those hours and need not equal the sum
  of its Runs' lines: host hours refresh on their own schedule and include
  idle time.
- `lux ls` has a COST column: the Run's total when it has one currency,
  `multi` when it has several, `—` while nothing has been reported. A
  leading `~` (`~0.0421 USD`) marks a total that may still change.

## Operators

With an operator key, every command covers every tenant (`--tenant`
narrows it), and there is more: `lux tenants ls`, `lux migrate`,
`lux resume --to`. See [Operators and the console](operators.md).

## Interactive

All three go through luxd, which relays to the Run's host. The client
never talks to the host. They need the Run `running` on a host with a live
connection.

```bash
lux exec <run> [-t | -T] -- command...
lux attach <run>
lux port-forward <run> <port-name> <local-port> [--address 127.0.0.1]
```

- **exec** runs a command in the container as the workload user, with the
  workload's environment and working directory. Its stdin, stdout, stderr
  and exit code are yours. A terminal is allocated when your stdin is one
  (`-t` forces it, `-T` turns it off). If the client goes away, the
  command is killed. Exec output is not part of the Run's output.
- **attach** joins the terminal of a `generic` workload started with
  `workload.tty: true`: its output from now on, and your typing. Ctrl-]
  detaches; the workload keeps running. What the workload prints on its
  terminal is also the Run's output, as always.
- **port-forward** listens locally and tunnels each connection to a port
  the Run declares in `network.ports`, by name. Undeclared ports cannot be
  reached.

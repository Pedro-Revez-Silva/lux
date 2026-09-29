# CLI

`lux` talks to luxd. It reads its configuration from flags, then the
environment, then `~/.config/lux/config.toml`:

```toml
url = "https://luxd.example.com"
api_key = "lux_…"
tenant = "acme"   # optional: the default --tenant
```

| Flag | Env | |
| --- | --- | --- |
| `--url` | `LUX_URL` | luxd's address |
| `--api-key` | `LUX_API_KEY` | an API key (scopes: `read`, `run`, `admin`; or an operator key) |
| `--tenant` | `LUX_TENANT` | with an operator key: one tenant only (id or name) |
| `-o json` | | machine-readable output, for every command that prints data |

For the tenant, a flag or variable set to the empty string still counts:
`--tenant ""` or `LUX_TENANT=` means all tenants, even with `tenant` in the
config file. A tenant key ignores it, wherever it comes from.

**Exit codes:** `0` success; the Run's own exit code for `run --follow`,
`run --wait`, `resume --follow` and `wait`; `3` not found; `4` conflict or
invalid spec (for example, steering a stopped Run); `5` a tenant quota
was reached; `1` anything else.

## Runs

```bash
lux run -f spec.yaml [--follow | --wait] [--name N] [-l k=v] [--pool P] [--idempotency-key K] [--secrets-from .env]
lux run --image alpine -- echo hello           # a quick generic Run
lux run --image alpine --pool arm64 -- uname -m
lux ls [--state running,stopped] [-l team=x] [--resumable] [--host H] [--limit N]   # with RUNTIME and COST columns
lux get <run>                                  # state, placements, usage
lux logs <run> [-f] [--since <cursor>] [--events] [--stderr=false] [--server NAME | --servers]
lux events <run>                               # lifecycle events
lux wait <run> [--state s1,s2] [--timeout 5m]  # default: until it ends; exits with its code
```

Specs are YAML or JSON (`-f -` reads stdin). A secret's value can come from
the environment (`value: ${GITHUB_TOKEN}`), from a `.env` file
(`--secrets-from`), or, when the spec omits the value, from an environment
variable of the same name.

Flags win over the spec file: `--pool` replaces `placement.pool`, as
`--image` and `--name` replace theirs. Without `--pool` or
`placement.pool`, the server picks the pool.

`lux logs -o json` prints one JSON record per line:
`{"cursor","epoch","seq","t","ch","data"|"event"}`. Pass a record's
`cursor` to `--since` to continue from it, across placements and hosts.
The Run's servers' output is left out: `--server web` prints one
server's, `--servers` adds all of theirs, each line prefixed with its
server's name (records with `ch: "server"`, `server` and `stream`).

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
lux pools ls                             # DEFAULT: * on the pool Runs naming no pool go to; operators without --tenant: every tenant's, OWNER platform or the tenant
lux pools set <name> --provider static|ec2 [--min N] [--max N] [--warm N] [--template JSON] [--default]
lux pools set <name> --default           # admin: make it the tenant's default pool (moves the mark); changes nothing else
lux pools set <name> --default=false     # admin: clear it
lux pools set <name> --provider static --hourly-price 0.40 --currency USD   # default price of hosts registering into it
lux pools rm <name> [--force-evict]      # admin: cordons its provisioned hosts, terminated once idle; --force-evict stops their live Runs too
lux pools events <name> [--platform] [--limit N] [--before ID] [--all]   # what happened to it, newest first
lux hosts events <host> [--limit N] [--before ID] [--all]
```

A pool name is 1-32 characters: lowercase letters, digits and `-`,
starting and ending with a letter or digit (`arm64`, `gpu-a100`). The name
reaches AWS in each instance's `lux:pool` tag and in its host name
(`<pool>-xxxxxxxx`, which must fit a hostname). A pool created before this
rule keeps its name and can still be updated; a new one cannot take it.

`lux pools events` and `lux hosts events` print one line per event: its
time, type and what it says (`-o json`: the events as the API has them).
A pool's are `pool.scale_up` (how many hosts and why: `waiting runs`,
`warm` or `minimum`, with the counts), `pool.launch_requested`,
`pool.host_launched` (`recovered` when luxd found the instance by its tag
after losing the provider's reply), `pool.launch_failed` (the provider's error),
`pool.host_registered`, `pool.placement` (a Run placed on one of its
hosts), `pool.host_released` (why: `idle` for how long, `pool removed`,
`outdated`, `manual`, `evicted`, or why it was terminated),
`pool.spot_interrupted`, `pool.config_changed` (each field, old→new;
the default mark is `isDefault`: marking a pool records it on that pool,
and on the pool that was the default before, which loses it),
`pool.retired` (removed: `lux pools rm`) and `pool.restored` (set again
after it was removed; a pool keeps its events across both), and
`pool.provider_error`. A host's are `host.registered`, `host.ready`,
`host.placement_assigned`, `host.placement_ended` (with the Run's
outcome), `host.drain_requested` (its cause), `host.lost`,
`host.terminate_requested`, `host.terminated` and `host.provider_error`.
A failure that repeats on every provisioner pass is one event, shown with
`(×N, last <time>)`. A tenant sees its own pools' and hosts' events; a
platform pool's or host's are the operators' (not shown with `--tenant`,
which shows what that tenant would see). A tenant's pool and the
platform's may share a name: `lux pools events <name>` is the tenant's
(for an operator without `--tenant`, an error when two pools share the
name), `--platform` the platform's.

A static pool's `--hourly-price` is copied to each host when it first
registers. Changing it later does not reprice the pool's existing hosts:
use `lux hosts price` for those. `lux pools set` replaces the whole pool,
the default price included, like every other field: a `pools set` without
`--hourly-price` leaves the pool with no default price. Each price or
capacity change starts a new rate period, and earlier periods are never
changed ([Run costs](costs.md), section 2). `ec2` pools take no price: the
provider prices their hosts.

A Run whose spec names no pool goes to the tenant's default pool, else the
platform's, else the pool named `default`
([operations](operations.md#pools-and-the-default-pool)). `--default` is
the one flag `pools set` does not replace when omitted: a `pools set`
without it keeps the pool's mark. Given alone, it changes only the mark;
with any other flag, `--provider` is needed too (the pool is replaced).
An operator without `--tenant` marks a platform pool as the platform's
default.

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
- `lux ls` has a RUNTIME column: the time the Run's placements have spent
  running, summed (`runtimeSeconds`, see [Concepts](concepts.md#placement-and-epoch));
  `-` for a Run that never ran.
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
lux shell <run>
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
- **shell** opens a login shell on a terminal: `bash -l` where the image
  has bash, `sh -l` otherwise (`lux exec -t <run> -- /bin/sh -c 'exec bash
  -l 2>/dev/null || exec sh -l'`).
- **port-forward** listens locally and tunnels each connection to a port
  the Run declares in `network.ports`, or to one of its servers, by name.
  No other port can be reached.

## Servers

A Run's named ports, optionally with commands lux runs in its container
(see [concepts](concepts.md#servers)).

```bash
lux server add <run> <name> <port> [--workdir DIR] [--env K=V]... [--no-start] [-- command...]
lux server ls <run> [name]                     # state, since, preview URL, command
lux server start|stop|restart <run> <name>
lux server rm <run> <name>                     # stops it first
lux server logs <run> <name> [--tail N] [-f]   # its recent lines, across placements; -f follows
lux server wait <run> <name> [--state ready] [--timeout 2m]
```

`add` starts a server with a command at once (the Run must be running)
unless `--no-start`; one without a command is only its port. `lux get`
lists the Run's servers too. Every command takes `-o json` (the API's
Server object, or a list of them).

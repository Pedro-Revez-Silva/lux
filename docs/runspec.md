# The RunSpec

A RunSpec describes what a Run executes. You submit it as YAML or JSON
(`lux run -f spec.yaml`). luxd validates it and fills in defaults,
reporting every problem at once. It stores the result without secret
values.

```yaml
name: fix-flaky-test            # optional, for humans
labels: { team: payments }      # free-form; filter with `lux ls -l team=payments`

image:
  ref: ghcr.io/acme/agent@sha256:…        # exactly one of ref or build

workload:
  adapter: claude-code          # generic | acp | claude-code | codex | opencode
  command: [claude, --model, sonnet]      # default: the adapter's
  prompt: "Fix the flaky test in tests/api"
  workdir: /workspace/repos/api
  user: agent                   # default: the image's USER
  grace: 30s                    # graceful stop before SIGKILL
  resume: { command: [...] }    # generic only: what to run on resume
  mcpServers:                   # remote MCP servers the agent is given
    - name: tracker
      url: https://mcp.acme.dev/mcp
      headers:
        - { name: Authorization, secret: TRACKER_TOKEN }  # the value is the secret's
  services:                     # HTTP services called through a local socket
    - name: tracker-api         # /.lux/services/tracker-api.sock, $LUX_SERVICE_TRACKER_API
      url: https://api.acme.dev/v2
      headers:
        - { name: Authorization, secret: TRACKER_TOKEN }  # added by lux; the workload never has it

init:
  script: npm ci                # runs before the workload, on every start

env: { NODE_ENV: test }

secrets:                        # values supplied by the caller; never stored
  - { name: ANTHROPIC_API_KEY, value: sk-…, as: env }
  - { name: npmrc, value: "…", as: file, path: /home/agent/.npmrc }
  - { name: GITHUB_TOKEN, value: ghp_… }   # used as a git credential below
  - { name: TRACKER_TOKEN, value: "Bearer …" }  # an MCP header above

git:
  repositories:
    - name: api
      url: https://github.com/acme/api.git
      ref: main                 # branch, tag or sha; default: the remote's HEAD
      credential: GITHUB_TOKEN  # a secret name; the runner uses it, the container never sees it
      path: /workspace/repos/api  # default: /workspace/repos/<name>
    - name: shared-lib
      url: https://github.com/acme/shared-lib.git
      push: false               # for context: `lux push` leaves it alone
  push:
    branch: lux/fix-flaky-test  # where `lux push` pushes

volumes:
  - { name: workspace, path: /workspace, kind: state }     # snapshotted on every exit
  - { name: home, path: /home/agent, kind: state }
  - { name: cache, path: /cache, kind: ephemeral }         # empty on every start

resources: { cpus: 4, memory: 8Gi, disk: 50Gi, pids: 2048 }
timeout: 4h                     # running time, over all placements; unset: no limit

placement:
  pool: default
  requires: { arch: amd64 }     # host labels that must match
  prefers: { region: eu-west-1 }

network:
  egress:                       # default deny: only these are reachable
    - host: api.anthropic.com
    - host: mcp.acme.dev        # the MCP server needs its own rule
    - cidr: 140.82.112.0/20
  # unrestricted: true          # no egress filtering (the hard blocks still apply)
  ports:
    - { port: 3000, name: web }

sandbox:
  nestedContainers: false
  readOnlyRoot: false

artifacts:
  paths: ["/workspace/out/**"]
```

## Rules

- `image`: exactly one of `ref` or `build`.
- `workload.adapter` decides how the workload is started, steered, stopped
  and resumed (see [adapters](adapters.md)). Agent adapters need their
  session paths on a state volume (for example `/home/agent` for Claude
  Code's `~/.claude`). A spec that leaves them off is rejected, because it
  would lose its conversation on the first resume.
- `env` names must be valid variable names and must not start with `LUX_`.
- Secrets are `env` (the default) or `file` (placed on a tmpfs and linked
  at `path`). A file secret is never in a snapshot. Every resume must
  supply every secret again.
- Volume paths must be absolute. `/.lux` is reserved.
- **Resources** are what a Run gets and what the scheduler reserves on its
  host: `cpus` (a CPU quota; `0.5` is half a CPU), `memory` (the limit,
  with no swap beyond it), `disk` (what it may write: its container's
  writable layer plus its state volumes), `pids` (processes). They default
  to **2 CPUs, 8 GiB of memory, 20 GiB of disk and 1024 processes**, and an
  operator can change the defaults (`LUX_DEFAULT_CPUS`,
  `LUX_DEFAULT_MEMORY`, `LUX_DEFAULT_DISK`, `LUX_DEFAULT_PIDS` on luxd).
  Sizes accept `512Mi`, `8Gi`, `1G`, or bytes. A Run waits until a host in
  its pool has that much free; an image build is held to the same CPU,
  memory and process limits.
- **Disk is measured, not capped by the filesystem**, so lux needs no
  special storage on hosts. The runner samples each Run's use (every 15s
  by default, `lux-runner --usage-every`); a Run over its limit gets a
  `disk.exceeded` event, is stopped and snapshotted as usual, and ends
  `failed` with reason `disk limit exceeded`. It can write about one
  sampling interval's worth past the limit before that. Its state is
  kept: resume it with a larger limit (`lux resume <run> --disk 40Gi`, or
  `resources.disk` in the resume request), or it is stopped again. tmpfs
  mounts (with `readOnlyRoot`) count against memory, not disk.
- **Disk is reserved only where the host says how much it has**
  (`lux-runner --disk`); otherwise each Run's limit still applies, but
  the scheduler does not add them up.

## Images

`image.ref` names an image. Pin it by digest (`name@sha256:…`) for a Run
that is the same everywhere. It is pulled once per host and cached.

`image.build` builds a Containerfile on the host that runs the Run. It
needs no registry (see [the build cache](#a-shared-build-cache) for
sharing builds between hosts):

```yaml
image:
  build:
    containerfile: |
      FROM docker.io/library/node:24
      RUN npm install -g pnpm
    args: { NODE_ENV: test }      # --build-arg
```

- **The first build pins every `FROM` to a digest.** The pinned
  Containerfile and the image id are recorded on the Run (`lux get` shows
  them). Every later placement, on any host, builds from the pinned form,
  so a tag that moves in between does not change the base.
- A host builds each distinct pinned Containerfile, args, tenant and
  network rules once, and reuses the image after that. A build's result
  depends on what it could reach, so no image, and no layer cache, is
  shared between tenants or between different egress rules. Built images a
  host has not used for its host TTL are removed.
- `COPY --from` and `RUN --mount=from=` may name earlier stages only. An
  image named there would not be pinned, so it is refused. Add
  `FROM image AS name` and use the name instead.
- A build has the Run's limits: CPUs, memory (no swap), and processes.
- A stop or cancel during a build ends the build.
- If a rebuild on another host produces a different image, the Run
  continues and records an `image.rebuild-differs` event. That happens when
  a `RUN` step is not reproducible, for example one that downloads the
  latest of something. State volumes do not depend on the image.
- **Builds are contained like the workload:** their own user namespace, the
  same dropped capabilities, the Run's resource limits, and the Run's
  network, so a `RUN` step has the Run's egress (and needs its registries
  and package mirrors allowed).
- A `FROM` naming its image through a build argument can't be pinned and
  is refused. `scratch` and earlier stages are left alone.
- Not yet: a build context. `COPY` and `ADD` have nothing to copy from, so
  a spec with `image.build.context` is refused.

### Private registries

`image.registryAuth` logs the runner in to registries, for every pull and
push of the placement: the `image.ref` pull, `FROM` bases, and the
[build cache](#a-shared-build-cache).

```yaml
image:
  ref: ghcr.io/acme/agent@sha256:…
  registryAuth:
    - { registry: ghcr.io, secret: GHCR }
secrets:
  - { name: GHCR, value: "acme-bot:ghp_…" }
```

- `registry` is the registry's host, in lowercase, with an optional port:
  `ghcr.io`, `123.dkr.ecr.eu-west-1.amazonaws.com`, `10.0.0.5:5000`. No
  scheme or path. Each registry once. Registries not listed get no
  credentials. Not `localhost`, a loopback or a link-local address: the
  runner pulls from the host itself, outside the Run's egress rules.
- **A private image is its puller's.** Images are kept per host, not per
  tenant, so a Run reuses a local copy only if its tenant has pulled that
  image itself. Otherwise it pulls it again, with its own credentials,
  which is cheap when the layers are already there, and fails without
  access. Images the host had before lux are anyone's.
- The secret's value is `user:password`, or a bare token. A bare token is
  sent as the password of the user `lux`. Many registries take a token
  with any user name. For those that want a particular user (Docker Hub,
  for example), use `user:token`.
- **The credential is the runner's.** Like a git credential, it is
  runner-only and `as: none` by default. It never enters the container,
  even with an explicit `as`, and no MCP or service header may use it. The
  runner writes it to a temporary `auth.json` (mode 0600, in its data
  directory), passes it to podman with `--authfile`, and deletes it once
  the image is ready. It is redacted from errors, and no event or log line
  carries it.
- **ECR:** the secret can be `AWS:<token>` from
  `aws ecr get-login-password`, which expires after 12 hours. Resuming a
  Run later needs a fresh token, supplied with the resume like any secret.
  Using the host's instance role (a credential helper) is future work.
- Registries should be HTTPS. A plain-HTTP registry must be listed as
  insecure on every host ([operations](operations.md#hosts)).

### A shared build cache

`image.build.cache` names a registry repository, with no tag, where hosts
share their builds:

```yaml
image:
  build:
    containerfile: |
      FROM docker.io/library/node:24
      RUN npm install -g pnpm
    cache: registry.example.com/team/lux-cache
  registryAuth:
    - { registry: registry.example.com, secret: CACHE_CREDS }
```

- A build is cached as `<cache>:<key>`. The key is the same one the host
  tags its local build with: the tenant, the egress rules, the pinned
  Containerfile, and the args. Different tenants or rules never share an
  image, as on a single host.
- A host without the image locally first pulls `<cache>:<key>`. On a hit
  it runs that image, with no build (an `image.cache` event,
  `{hit: true, ref}`). When the cache does not have it, the host builds.
  Any other pull error is recorded as `image.cache` with a `warning`, and
  the host builds too.
- After a build, the host pushes it to the cache in the background. The
  Run doesn't wait for the push. Then comes an `image.cache` event with
  `{pushed: true, ref}`, or with a `warning` if the push failed.
- Pinning is unchanged: the first placement pins every `FROM` and records
  the result, and later placements use it. A hit is therefore the same
  pinned Containerfile. The `image.built` event records the cached image's
  id (with `fromCache: true`), and `image.rebuild-differs` compares it as
  it would a build.
- Each Run pins an unpinned `FROM` on the host of its first placement, to
  the digest that host has for it. Two hosts can have different digests
  for the same image (for example, one that loaded it and one that pulled
  it), and different digests give different keys. So for cache hits
  across Runs, pin `FROM` by digest yourself.
- The cache's registry usually has a `registryAuth` entry. Without one,
  the cache is used anonymously.
- Two hosts that build the same key at once both push it. The last push
  wins, and either image is the build.
- lux never deletes from the cache. Old keys go through the registry's own
  retention (an ECR lifecycle rule, for example).
- Keys keep tenants apart, but a credential that can push to the
  repository can push any tag. Share a cache repository's push
  credentials only among tenants you trust with each other's images.

### Images on hosts

Hosts remove the images lux put there once they go unused:

- lux records the images it pulled (`image.ref`, `FROM` bases, cache
  pulls), built, or tagged, with their last use on the host. After the
  host TTL (`lux-runner --host-ttl`, 24h by default) without use, it
  removes them.
- Images the host already had when a Run needed them are the operator's.
  lux never removes them, and never prunes unnamed images either.
- An image a starting Run is about to use is never removed, however long
  its volumes take to restore.
- When the disk holding Podman's storage is over `--image-disk-high` (80%
  by default), each pass removes lux's images that no container uses,
  least recently used first, until it is under the mark. Images used in
  the last ten minutes are spared, and so is an image that also has a name
  lux didn't give it.
- An image a container still uses is never removed. A later pass retries.

## Nested containers

`sandbox.nestedContainers: true` lets the workload run containers itself,
with rootless Podman inside its container. The Run is placed only on hosts
started with `lux-runner --nested`. Its image must have Podman (and
`fuse-overlayfs`, and `newuidmap`/`newgidmap`), and the workload user
needs `/etc/subuid` and `/etc/subgid` entries. `tests/images/nested` is a
minimal example.

A nested Run is never `--privileged`. Beyond what every Run gets, it gets:

- `CAP_SYS_CHROOT` in its bounding set;
- no `no-new-privileges`, so that `newuidmap`/`newgidmap` can take
  `CAP_SETUID`/`CAP_SETGID` from their file capabilities. The workload
  itself starts with no capabilities. But without `no-new-privileges`, any
  setuid-root program in the image (`sudo`, `su`) can make it root in its
  container, exactly as a Run whose workload runs as root. That is within
  the Run: container root is an unprivileged uid range on the host, with
  the same bounding set as every Run. Leave setuid programs out of nested
  images if the workload should stay unprivileged in its own container;
- `/dev/fuse` and `/dev/net/tun`;
- `unmask=ALL` and `label=disable`;
- the host's seccomp profile, plus `sethostname`, `setdomainname` and
  `setns` (normally allowed only with `CAP_SYS_ADMIN`, which it does not
  get).

A host started without `--nested` refuses nested Runs.

It is still in its own user namespace (an unprivileged uid range on the
host). The containers it starts use the Run's network, so they have its
egress rules and hard blocks, and nothing more.

## Artifacts

Artifacts are files a Run produces, kept after it ends and downloadable
with `lux artifacts <run> --download DIR`. They are collected **on every
exit** (a stop, a failure, a cancel, not only success), per placement:

- files matching `artifacts.paths`: absolute globs on the Run's volumes,
  where `*` matches within a directory and `**` any depth
  (`/workspace/out/**`, `/workspace/**/*.xml`);
- anything the workload writes into `$LUX_ARTIFACTS` while it runs,
  listed as `/.lux/artifacts/<name>`. That directory is emptied once
  collected, so each placement publishes its own.

Each artifact is stored like a snapshot blob (uploaded through luxd to S3)
with its size, sha256 and content type, and listed by placement epoch.
Downloads stream through luxd as the file the Run wrote.

Limits: 1000 artifacts per placement, 1 GiB per file. Symlinks are never
collected: a workload's link could point anywhere on the host.

## Git

lux clones repositories for you, on the host, before the container
starts:

1. Each repository is fetched into a **mirror** on the host. The mirror is
   shared by the tenant's Runs there (never another tenant's: one may hold
   what another can't read) and updated on use, so a second Run on the
   same host fetches only what is new. The scheduler prefers hosts that
   already hold the mirrors a spec needs.
2. It is cloned from the mirror into the Run's `path`, which must be on a
   **state volume**, at `ref`: a branch checked out as a local branch, or a
   tag or sha detached. The clone is self-contained, so it moves with the
   Run to hosts that have no mirror.
3. The checkout's `origin` is the plain `url`. The credential is used by
   the runner for the fetch, and later the push. It is **never in the
   container**: not in the environment, not in `.git/config`, and not on
   the host's disk. A `url` that carries credentials (`https://user:tok@…`)
   is refused; use `credential:`.
4. On a **resume**, the checkout is already on the restored volume and is
   left exactly as the workload left it.

Every clone is a `git.clone` event: `{repo, status: "cloned", commit}`, or
`{repo, status: "failed", error}` (git's output, with the token redacted).
`git.checkout` still follows a successful clone, with its ref and branch.

Clones are the runner's, on the host, never the Run's, so a repository
needs no `network.egress` rule and none is checked.

### Adding repositories on resume

A resume can add repositories to a stopped, lost or failed Run. Pass them in
the request's `git.repositories` (`lux resume --add-repo`), in the same shape
as the spec's:

```json
POST /v1/runs/{id}/resume
{"requestId": "r-1",
 "git": {"repositories": [{"name": "two", "url": "https://github.com/o/two.git", "credential": "GIT_TOKEN"}]},
 "secrets": [{"name": "GIT_TOKEN", "value": "…"}]}
```

- They join the Run's spec, each with `addedBy` set to the request id
  (`requestId`, or one luxd generates; the response's `Lux-Request-Id`
  header). Only luxd sets `addedBy`: a submitted spec that sets it is
  refused with 422 `invalid_spec`.
- The merged spec is validated like a submitted one (422 `invalid_spec`).
  Names must be new, and paths must be on a state volume.
- A credential the spec doesn't declare becomes a secret the runner alone
  uses (`as: none`). Its value must be in the resume's `secrets`, like the
  Run's others (422 `secrets_required`). An operator's resume without
  secrets has only the values luxd holds, so it cannot add a new
  credential. A secret the workload already sees can't be a credential.
- The runner clones them into the restored workspace before the container
  starts, next to the existing checkouts. Their `git.clone` events carry
  the `requestId`.
- **A failed clone does not fail the Run.** The agent keeps its
  conversation, so the Run starts without that repository. Its `git.clone`
  event says `failed`, and luxd removes it from the spec, so later resumes
  don't retry it and `lux push` doesn't list it. A repository in the
  submitted spec that can't be cloned still fails the placement (stage
  `git`).
- `push: false` works as for any repository. Pushes include the added
  repositories.
- Every resume records a `resume.requested` event:
  `{requestId, by, addedRepositories}`.
- Adding needs a Run that is stopped, lost or failed. While it is
  resuming, the request gets 409.

`lux push <run> [--wait]` pushes each repository's current commit to
`git.push.branch`, again with the runner's credential:

- The push is **leased**: it replaces the branch only if the branch still
  points where this Run last pushed it (or, the first time, only if the
  branch does not exist). If someone else pushed in between, the push is
  `rejected` and their commit stays. luxd keeps the lease, not the
  checkout.
- **The runner never runs git in the checkout.** The workload controls its
  `.git`, including hooks and config such as URL rewrites. So the push works
  from a bundle: the workload's user writes a bundle of `HEAD` inside the
  container, and the runner pushes that bundle from a repository it owns.
  The checkout's hooks and config never run as the runner and never see the
  token.
- Pushing with nothing new reports `up-to-date`.
- A repository with `push: false` is never pushed: it is reported
  `skipped`, and `expect` may not name it. Use it for repositories cloned
  for context; their credential may be read-only. The workload can still
  commit in such a checkout; lux just never pushes it.
- Each repository's outcome is a `git.push` event. `--wait` prints the
  outcomes and exits non-zero unless every repository was pushed or already
  up to date.

The workload commits as it likes. The checkout is a normal git repository
owned by the workload user.

## MCP servers

`workload.mcpServers` gives an agent remote MCP servers (streamable HTTP),
through its own protocol, so the same spec works for every agent adapter:

```yaml
workload:
  adapter: claude-code
  mcpServers:
    - name: tracker                   # unique; lowercase, digits, - and _
      url: https://mcp.acme.dev/mcp   # http or https, no user:password@
      headers:
        - { name: Authorization, secret: TRACKER_TOKEN }
secrets:
  - { name: TRACKER_TOKEN, value: "Bearer …" }
network:
  egress:
    - host: mcp.acme.dev
```

- **To keep the credential from the agent, back the server with a
  service.** With `headers`, the agent's MCP client holds the value
  (that's how it sends it). Instead, name a [service](#services) that has
  `loopback: true`. The agent is given its loopback address and no
  headers, and the service adds them on the way out:

  ```yaml
  workload:
    services:
      - name: tracker
        url: https://mcp.acme.dev
        headers: [{ name: Authorization, secret: TRACKER_TOKEN }]
        loopback: true
    mcpServers:
      - { name: tracker, service: tracker, path: /mcp }   # → http://127.0.0.1:41000/mcp
  ```

  `service` replaces `url` and `headers`. `path`, if given, is appended to
  the service's URL (without it, the agent is sent to the URL itself, so
  `url: https://mcp.acme.dev/mcp` with no `path` works as well). The
  service's URL, egress and headers are checked as the service's.
- **Header values come only from secrets.** A header names a secret in
  `secrets`, and its value is the secret's whole value (`Bearer …` for a
  bearer token). The spec stores the secret's name, never its value, and
  every resume must supply it again, like any secret. It is redacted from
  output like any secret. A git credential can't be a header's secret: it
  never enters the container. A secret used only as headers (or as a git
  credential) is not put in any process's environment or files unless its
  `as:` says so (its default is then `none`).
- **Egress must allow the server,** or the spec is refused: its host must
  equal a `host:` rule, or, for an IP address, fall in a `cidr:` rule. An
  unrestricted Run needs no rule. (This matches on the name only; the
  runner's firewall enforces the rest, as for any egress.)
- **Never the control plane.** Every Run is blocked from luxd's address, so
  a spec whose MCP server is luxd's host (or a name resolving to one of its
  addresses) is refused at submit with a 422: run the MCP server
  elsewhere.
- **How each adapter delivers them.** None of them puts a header value in
  the command line, which the `lux.workload` event records.

  | Adapter | Delivery |
  | --- | --- |
  | `acp`, `opencode` | `mcpServers` on `session/new` and `session/load`, as ACP `http` servers with their headers. An agent that does not advertise `mcpCapabilities.http` gets none, and the Run gets a `lux.warning` saying so. |
  | `claude-code` | `--mcp-config` with a file on the secrets tmpfs (mode 0400, the workload user's; never on a volume or in a snapshot). The user's own MCP config still applies: no `--strict-mcp-config`. |
  | `codex` | `-c mcp_servers.<name>.url="…"` and `-c mcp_servers.<name>.env_http_headers={"Header"="LUX_MCP_<n>_<m>"}`; the values are in those environment variables of the agent's process (not init's, nor `lux exec`'s). Codex passes its environment on to the shell commands it runs, so those see them too. |
  | `generic` | nothing: the servers are not passed on. |
- **A custom `workload.resume.command` is run as written:** an adapter adds
  nothing to it, so the MCP flags (claude-code and codex) are not added on
  resume. Put them in it yourself if you need them there.

## Services

`workload.services` lets the workload call an HTTP service as its Run
without ever holding the credential. lux-shim serves each service on a
unix socket in the container and adds the service's headers to every
request:

```yaml
workload:
  services:
    - name: tracker-api
      url: https://api.acme.dev/v2
      headers:
        - { name: Authorization, secret: TRACKER_TOKEN }
network:
  egress:
    - { host: api.acme.dev }
secrets:
  - { name: TRACKER_TOKEN, value: "Bearer …" }
```

```bash
# inside the container
curl --unix-socket /.lux/services/tracker-api.sock http://tracker-api/items?open=1
echo $LUX_SERVICE_TRACKER_API   # unix:/.lux/services/tracker-api.sock
```

- **The socket:** `/.lux/services/<name>.sock`, mode 0600 and owned by the
  workload user, on a tmpfs, so it is never in the image or a snapshot.
  `LUX_SERVICE_<NAME>=unix:/.lux/services/<name>.sock` (the name
  upper-cased, `-` as `_`) is set for the agent, init and `lux exec`.
- **Requests:** any method, forwarded to `url` with the request's path
  appended to `url`'s path (`/items` above goes to `/v2/items`) and its
  query kept. The service's headers are set over any the workload sent
  with the same names. Request and response bodies stream both ways (SSE
  and chunked responses arrive as they are sent). Nothing is cached. If the
  service can't be reached, the workload gets `502` with a short text body.
- **The credential stays out of the workload's reach.** Header values come
  only from secrets, and a secret used only for headers is placed nowhere
  else (its `as:` defaults to `none`). They live only in lux-shim's memory:
  never in a file, an environment or argv. The shim is not dumpable, so its
  `/proc` entries and memory are closed to the workload even when it runs
  as container root: Runs have no `CAP_SYS_PTRACE`. They are redacted from
  output like any secret.
- **The same rules as MCP servers:** unique names (lowercase, digits, `-`,
  `_`); an http or https URL with no credentials; `network.egress` must
  allow its host (`host:`) or address (`cidr:`), unless unrestricted;
  never the control plane's address; no duplicate header names; a git
  credential can't be a header's secret.
- **`loopback: true`** also serves the service on
  `http://127.0.0.1:<port>` inside the Run, for clients that take a URL
  rather than a socket (an agent's MCP client). The port is
  41000 plus the service's position in `workload.services`, the same on
  every placement, and `LUX_SERVICE_<NAME>_URL` holds the full address.
  A `network.ports` entry can't use that port.
  Anything in the container can call it while the Run lives, as with the
  socket; the credential still never leaves the shim.
- **Each placement** serves them again: a resume supplies the header
  secrets with the rest, as always.
- Requests leave from the Run's network, under its egress rules.
- **Point a service only at an API that never echoes request headers
  back.** The workload chooses the path. A debug, echo or verbose-error
  endpoint anywhere on that host would hand it the credential in a
  response body. Prefer a narrow base URL, and a token scoped to what the
  Run needs.
- A path with a `..` segment is refused (400), so the workload can't climb
  out of `url`'s path. Paths are otherwise forwarded as sent: trailing
  slashes and escaped characters (`%2F`) are kept.

## Network egress

**Default deny.** A Run can reach only the CIDRs and hostnames in
`network.egress`. A spec with no rules reaches nothing.

- **CIDRs** are allowed as written.
- **Hostnames** are resolved by the runner (not the container) when the Run
  starts, and every minute after that. New addresses are added and none are
  removed while the Run lives, so a connection is not cut when a CDN
  rotates. Wildcards can't be resolved, so list the concrete hostnames.
- **DNS:** the Run's resolver is a stub on its network's gateway. It
  answers only allowed names, with exactly the addresses the firewall
  allows. It refuses every other name and records each distinct lookup
  (name, allowed or not) as a `dns` event, so "what did this Run try to
  reach" has an answer. Queries sent to
  any other DNS server are redirected to the stub, so DNS can't be used to
  get data out.
- **IPv4 only.** Allow rules are IPv4; a restricted Run's IPv6 traffic is
  dropped, and the stub answers no AAAA queries.
- **Always blocked, whatever the spec says:** link-local and the cloud
  metadata services (`169.254.0.0/16`, `fe80::/10`, `fd00:ec2::254`),
  loopback, the control plane, and other Runs on the same host. An allowed hostname that resolves into a blocked range does
  not open it.
- `unrestricted: true` switches the filtering off, and the Run resolves
  names through the host's resolvers. The hard blocks above still apply.
- **A known limitation, by choice:** rules match IP addresses, so allowing
  a name hosted on a CDN allows everything else on those shared addresses.
  Filtering on hostnames (SNI) through a proxy is the upgrade path.

How it works: each Run gets its own bridge network, with Podman's own DNS
turned off. The runner owns an nftables table, `inet lux`, which netavark
never touches:

- Traffic from each Run's bridge is sent to that Run's chain, which allows
  replies and the Run's allow set, drops the hard-blocked ranges, and drops
  everything else.
- Traffic from a lux bridge with no rules loaded, for example while the
  runner restarts, is dropped, so a Run fails closed, never open. The
  runner replaces the table in one transaction, so there is no moment
  without it. A restarted runner re-applies the rules for the Runs it
  re-adopts before anything else.
- The rules and the DNS stub are in place before the container starts,
  so its first packet and its first lookup are covered. They are removed
  when the container exits.
- The control plane is blocked by every address its URL's host resolves
  to when the runner starts.

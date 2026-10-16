# Podman: what lux relies on, and what we found

lux runs every Run in rootful Podman, driven through its CLI
(`internal/podman`). Everything below was checked on Podman 5.8. The e2e
suite runs it: its hosts are `quay.io/podman/stable` containers, with the
host changes described here applied by `tests/env.py`.

## Host setup

- **Rootful, with a `containers` subordinate range.** `--userns=auto`
  gives each container its own slice of that range, so container root is a
  different unprivileged uid on the host in every Run. Without an entry in
  `/etc/subuid` and `/etc/subgid` (for example
  `containers:2147483647:2147483648`), every container creation fails.
- **Native overlay storage, not fuse-overlayfs.** With fuse-overlayfs, an
  image build in its own user namespace sees the base image's `/` owned by
  `nobody`, so `RUN` steps cannot create files at the root. Native overlay
  shifts ownership correctly. Images inside Docker
  (`quay.io/podman/stable`) default to fuse-overlayfs; the test hosts write
  a `storage.conf` that selects `overlay`. `/var/lib/containers` must be on
  a real filesystem (a volume, not the container's own overlay).
- **cgroup v2.** The runner reads usage from each container's cgroup and
  makes its own under `/sys/fs/cgroup/lux.slice` for image builds.

## Containers

Every Run's container gets (`hardening()` and `containment()`):

- `--userns=auto:size=65536`, `--cap-drop=ALL`, then only `CHOWN`,
  `DAC_OVERRIDE`, `FOWNER`, `SETUID`, `SETGID` and `KILL`, and
  `--security-opt=no-new-privileges`.
- Private cgroup, IPC, UTS and PID namespaces, set explicitly
  (`--cgroupns=private --ipc=private --uts=private --pid=private`). A
  host's `containers.conf` may default any of them to the host's.
- `--init=false`: the shim is PID 1 and reaps.
- `--log-driver=none`: the shim writes the output file itself; Podman's
  log would be a second, unredacted copy.
- `--restart=no`: the runner decides what happens after an exit.

### Volumes and idmapped mounts

- State volumes and the runtime volume (`lux-<run>--rt`) are mounted with
  `:idmap`. Under `--userns=auto` a volume would otherwise show its files
  as `nobody` inside the container. With `:idmap`, container uid 1000 is
  host uid 1000 on the volume, so the runner (root on the host) can read
  and chown what the workload wrote, and the same volume works whatever
  range the next container gets.
- The runtime volume is a named volume, not a bind mount: an idmapped
  mount needs a filesystem that supports it, and the shim's socket in it
  must be reachable from both sides.
- File secrets live on `--tmpfs /.lux/secrets` (mode 0700, nosuid, nodev),
  never in a volume, so never in a snapshot.

### Snapshots

`podman volume export` and `podman volume import` move a volume as a tar,
and the runner compresses it with zstd. Exporting a volume while its
container runs works, but only an exit gives a consistent copy: lux
snapshots on exit.

### Networks

- Each Run gets its own network: `podman network create --disable-dns
  --interface-name lux<hash>`. With Podman's DNS off, the Run's resolver is
  lux's DNS stub on the gateway, and a fixed interface name lets lux's
  nftables rules exist before the bridge does.
- netavark manages its own nftables table and never touches lux's
  `inet lux` table. lux's forward chain runs before netavark's (priority
  -10) and drops what the Run may not reach.

## Image builds

`podman build` (buildah) runs tenant code, so it is contained like the
workload:

- `--isolation=oci`. The default for images inside Docker is `chroot`
  (`BUILDAH_ISOLATION`), which cannot use a network other than the host's.
- `--userns=auto:size=65536`, the same capabilities and
  `no-new-privileges` as a workload, `--memory` and `--memory-swap` equal,
  and `--cpu-quota`.
- `--network <the Run's network>`, so `RUN` steps have the Run's egress.
- **No process limit flag.** `podman build` has no `--pids-limit`. lux
  creates `lux.slice/build-<run>` with `pids.max`, and enables the pids
  controller in `/sys/fs/cgroup` and `lux.slice` for it, then passes
  `--cgroup-parent`. (`--ulimit nproc` also works, but a cgroup is what
  lets lux end everything the build started.)
- **`--cgroup-manager=cgroupfs`.** That cgroup is a plain cgroupfs path,
  so the build runs under the cgroupfs manager, whatever the host's
  `containers.conf` says. Under systemd (Fedora CoreOS's default) crun
  turns `/lux.slice/build-<run>` into a unit name and every `RUN` step
  fails: first `bpf pin to /sys/fs/bpf/crun/lux_slice/build-<run>_scope:
  No such file or directory`, and systemd rejects the name past that.
- **Cancelling a build.** SIGKILL to `podman build` can leave its current
  `RUN` step running. lux sends SIGTERM, and afterwards writes `1` to the
  build cgroup's `cgroup.kill` before removing it, so nothing outlives the
  build.
- **Reproducible image ids.**
  - `--timestamp=0` removes build dates.
  - The environment variable `_CONTAINERS_OVERLAY_DISABLE_IDMAP=yes` turns
    off idmapped overlay mounts for the build. With them, files a `RUN`
    step creates are owned by host ids from whatever range the build got,
    so root-owned files are not root's in the image and the id changes on
    every build. This is containers/storage's only switch for it, and it is
    undocumented: `tests/suites/test_images.py::test_rebuilds_match_whatever_uid_range_they_get`
    fails if a Podman upgrade drops it.
  - `FROM` names are recorded in the image. lux pins each `FROM` to a
    digest, then tags the base `localhost/lux-base:<digest>` and builds
    from that name, so every host builds from the same text.
  - Labels change the image id (`--label` is part of it); lux sets only
    `lux.managed=true`, the same everywhere.
- `--layers=false --no-cache`: a cached layer would share a `RUN` step's
  result between tenants or between different egress rules.
- `--iidfile`: with `--quiet`, `RUN` output still goes to stdout, so
  stdout is not the image id.

## Nested containers

A Run with `sandbox.nestedContainers` runs rootless Podman inside its
container. Under `--userns=auto` that needed more than the commonly cited
`unmask=ALL`, `/dev/fuse` and `label=disable`:

- **`CAP_SYS_CHROOT`**: Podman's storage setup chroots.
- **`/dev/net/tun`**: rootless networking (pasta) needs it.
- **`sethostname`, `setdomainname`, `setns` in seccomp**: the default
  profile allows them only with `CAP_SYS_ADMIN`, which crun needs for the
  inner container's UTS namespace. lux derives a profile from the host's
  default with just those three added.
- **No `no-new-privileges`**: `newuidmap` and `newgidmap` need
  `CAP_SETUID` and `CAP_SETGID`, from their file capabilities, to map the
  inner containers' ids. Under `no-new-privileges` they cannot gain them.
  Ambient capabilities on the workload would work too, but would let the
  workload become root in its own container; dropping
  `no-new-privileges` keeps the workload at no capabilities, though a
  setuid program in the image can still make it root in its container.
- The image needs `newuidmap` and `newgidmap` with those file
  capabilities (`setcap cap_setuid=ep`), and the workload user needs
  `/etc/subuid` and `/etc/subgid` entries. `tests/images/nested` is a
  minimal example.
- A private IPC namespace matters here: rootless Podman keeps its locks
  in `/dev/shm`.
- **AppArmor (Ubuntu 24.04 and later).** With
  `kernel.apparmor_restrict_unprivileged_userns=1`, a user namespace made
  under an AppArmor profile that does not grant `userns` loses its
  capabilities. The profile Podman gives containers by default
  (`containers-default`) does not grant it, so rootless Docker inside a Run
  fails with "failed to create a detached netns … Permission denied"
  (measured on an Ubuntu EC2 host: the same Run starts with
  `apparmor=unconfined`). A `--nested` runner on such a host runs nested
  Runs `apparmor=unconfined`, which lifts that profile's other rules too
  (mount and ptrace ones among them). Seccomp, the capability set and the
  Run's own user namespace still bound them. A narrower profile (the
  default plus `userns`) is a possible follow-up. Fedora hosts have no
  AppArmor, so the e2e suite does not see this there; CI's Ubuntu runner
  does.
- **AppArmor attaches by executable path, and Ubuntu has two pasta
  profiles.** Ubuntu's passt package loads `passt` for
  `/usr/bin/passt{,.avx2}` and `pasta` for `/usr/bin/pasta{,.avx2}`; they
  apply inside a Run too, against the path the Run sees. Alpine ships
  `pasta` as a symlink to `passt`, so the kernel execs `passt.avx2` and
  attaches the `passt` profile. That profile has no read rule for the
  network namespace Podman bind-mounts under its runtime directory
  (`$XDG_RUNTIME_DIR/netns`, else `/tmp/storage-run-<uid>/netns`): it may
  only write `owner /tmp/**` and `owner @{HOME}/**`. Every inner container
  with a network then fails with "pasta failed with exit code 1: Couldn't
  open network namespace … Permission denied" (kernel audit:
  `profile="passt" comm="passt.avx2"`). `tests/images/nested` hard-links
  pasta instead: the exec path stays `pasta`, so the `pasta` profile
  applies, and it allows the namespace under `/tmp` or `/run/user/<n>`.
  Link `pasta.avx2` too where it exists: pasta re-execs
  `<its own path>.avx2`, and through the symlink that is `passt.avx2`,
  which the `pasta` profile runs unconfined.
  An `apk upgrade` of passt in a later layer restores the symlinks.
- **Rootless Docker** (`dockerd-rootless`, RootlessKit) works with the
  same grants, including BuildKit and compose; `tests/images/docker` is an
  example. `docker run --privileged` inside is refused (it cannot mount
  `/sys`), and what it starts has the Run's egress.
- **Each engine's store is on its own volume.** Kernel overlay cannot
  stack on the container's own overlay root, so an engine there falls back
  to `fuse-overlayfs` (Docker, Podman) or `vfs`. The runner mounts an
  ephemeral volume at `$XDG_DATA_HOME/docker` and
  `$XDG_DATA_HOME/containers` (`~/.local/share` by default; `/var/lib/...`
  for a root workload), where both get kernel overlay: a file-heavy build
  measured 1.6 to 2.2 times faster than on `fuse-overlayfs`.
  `tests/suites/test_nested_docker.py::test_disk_speed_is_native` fails if
  that regresses.
  - The runner keeps these volumes apart from the spec's (`EngineVolumes`
    in the run state): they are mounted and count toward the Run's
    `resources.disk` like any volume, hardlinked layer files once, but are
    never snapshotted, never artifact sources, and never a repository's
    home.
  - The shim hands the directories the mount made (`~/.local`,
    `~/.local/share`) to the workload user, and only those: the runner
    lists what neither the image nor a volume over it has. Below the home
    that is every such directory; for a store outside it, only the data
    home itself. The shim walks to each from `/` through directory file
    descriptors with `O_NOFOLLOW`, so a link the workload planted (or swaps
    in meanwhile) is never followed.
  - Every placement starts with empty engine stores: the runner removes an
    existing ephemeral volume and creates it again, and podman copies in
    what the image has at its path, as on a new host. It stops a
    still-running earlier container first; one that cannot be stopped fails
    the placement.
  - A resume always gets a new container, on the same host too: the
    runner removes the earlier placement's and creates one from the image
    on the Run's volumes (on the same host, the ones it already has). A
    stopped container is never started again: its writable layer (`/tmp`,
    `/run`) would hold an engine's runtime state (rootless Podman's
    `pause.pid` and `storage-run-<uid>`, dockerd's pid file) for a store
    that is now empty, which a resume on another host never has. The
    runner removes a placement's container once its stop is reported; its
    volumes stay.
  - One epoch of a Run at a time on a host. Every epoch's container has
    the Run's name, and they share its volumes and run state. When luxd
    assigns a newer epoch while an older one is still on the host, the
    newer epoch touches nothing of the Run's until the older one is done:
    - an older epoch whose workload may still run is fenced off (it
      reports nothing more and writes no run state) and killed;
    - an older epoch exporting a snapshot luxd has not acked abandons the
      export: luxd assigns over it only once it is lost, and would refuse
      that snapshot, or it is a re-export after a restart, whose original
      luxd already holds;
    - an older epoch whose snapshot luxd acked finishes as usual (its
      container is removed, its snapshot uploaded).

    That wait is bounded by a host lease, plus the minute a finishing
    epoch has to remove its container, plus the 30 s podman gets to exit
    once that minute is up; past it, the newer epoch fails to start and
    says why. A placement kills, stops and waits on only the container it
    created, by its ID, never by the Run's name.

### Limits of nested containers

A Run's container has a private cgroup namespace, so its
`/sys/fs/cgroup` is the Run's own cgroup, with the Run's `memory.max`,
`cpu.max` and `pids.max`. Those files belong to host root, outside the
Run's user namespace, so nothing in the Run can raise them. The cgroup's
delegation files (`cgroup.procs`, `cgroup.subtree_control`,
`cgroup.threads`) and the directory belong to the Run's root: crun and
runc both chown them.

An engine inside cannot use that cgroup as it is: cgroup v2 enables
controllers for children only in a cgroup with no processes of its own,
and the shim (PID 1) and the workload are in it. Without more, rootless
Podman inside ignores `-m`, `--cpus` and `--pids-limit` and its containers
have the Run's limits, with no error. For a Run with both
`sandbox.nestedContainers: true` and `sandbox.cgroupDelegation: true`,
the shim splits the cgroup before anything
of the tenant's runs:

```
/                 the Run's limits; no processes; +cpu +memory +pids +io below
├── lux.shim/     the shim, and its own work (a resume's sync)
└── workload/     no processes; +cpu +memory +pids +io below; cgroup.max.descendants 1024
    └── main/     the workload, init, beforeStop, servers, lux exec
```

- The shim makes the three cgroups, moves itself into `lux.shim`, then
  enables the controllers in the root and in `workload`.
- It gives both directories and their `cgroup.procs` and `cgroup.threads`
  to the workload user, plus `workload/cgroup.subtree_control` only.
  `main/cgroup.subtree_control` stays Run-root owned: `main` remains a
  leaf, and engines must create siblings under `workload`, not evacuate
  into children of `main`. Resource files stay Run-root owned.
- The shim copies the root's `cpu.max`, `memory.max` and `memory.swap.max`
  to `main`, so runtimes reading only their leaf see the Run's limits.
- `cgroup.max.descendants` on `workload` is 1024, including `main`: an
  engine makes one or two cgroups per container. This bounds non-root
  workload users; a root workload is the Run's root and can remove the
  descendant bound or create siblings of `workload`, but remains bounded
  by the Run's host-owned resource limits.
- Every tenant process is started directly in `workload/main` (clone3
  with `CLONE_INTO_CGROUP`), so none is ever, even briefly, in a cgroup
  that must stay empty. The engine makes its containers' cgroups beside
  `main`, under `workload`. If the workload moves all processes out and
  removes `main` itself, subsequent tenant starts fail; the shim does not
  recreate a workload-deleted leaf.
- `podman exec` from the runner (`lux diff`, a push's bundle) lands in
  PID 1's cgroup, `lux.shim`: crun and runc put an exec'd process there
  when the container's root has controllers enabled. It is still inside
  the Run's limits.
- The runner's accounting reads the container's cgroup from the host,
  whose counters (`memory.peak`, `cpu.stat`, `pids.current`, pressure)
  include every cgroup below it.

With it, rootless Podman in a Run enforces `-m`, `--cpus` and
`--pids-limit`, and OOM-kills a container over its `-m`, all within the
Run's own limits: a container given `-m 4g` in a 1 GiB Run is still held
to 1 GiB. `tests/suites/test_nested.py` checks each.

When the Run's cgroup cannot be split (it is not writable, it lacks the
`cpu`, `memory` or `pids` controller, or the kernel or seccomp profile
refuses `CLONE_INTO_CGROUP`), the shim writes one `lux.warning` saying
why. On partial failure it best-effort disables the enabled controllers,
returns the shim's processes to the root, and removes the new directories.
Tenant starts fall back to the shim's cgroup even if rollback is incomplete:
nested containers retain the Run's limits, but their own may not be enforced.
A Run without explicit `sandbox.cgroupDelegation: true` is never split,
including when nested containers are enabled: a non-root workload cannot
create cgroups (the Run's cgroup belongs to the Run's root), so its engines'
containers stay in the Run's cgroup and see its limits at their cgroup root.
A root workload in a nested Run can create cgroups; lux does not split or
delegate its cgroup when the flag is off. Delegation requires nested
containers and adds no host permissions.

**Docker inside a Run does not enforce per-container limits.** Rootless
`dockerd` uses the cgroup driver `none` whenever it runs rootless without
systemd, whatever `--exec-opt native.cgroupdriver` or `daemon.json` say,
so `docker run -m`, `--cpus` and `--pids-limit` have no effect: its
containers remain bounded by the Run's limits. Without delegation they stay
in the non-root Run's cgroup and read its limits (measured with ClickHouse
version 26.8: 920 MB / 2 threads; JVM 21 and 17: heap 256 MB / 2 CPUs in a
2 CPU / 1 GiB Run). With delegation, rootless runc can make each
container its own cgroup with no limits (`/workload/<id>`); in Docker's
private cgroup namespace that is the root, so ClickHouse and the JVM size
themselves to the physical host's memory, still enforced by the Run.

`tests/images/docker` ships the agent's `~/.config/docker/daemon.json` with
`{"default-cgroupns-mode":"host"}`. "host" is the enclosing **Run**'s cgroup
namespace, not the physical host's: inner containers see the Run's cgroup
tree (read-only for them) and no more. The delegated-Docker test pins that
rootlesskit does not unshare that namespace: the inner process's cgroup
path starts with `/workload/`, and walking up from it finds the Run's
memory and CPU limits. With delegation, ClickHouse and JVM 21 then size
to the Run (measured); JVM 17, which reads only its own cgroup, still does
not. It does not make Docker enforce `-m` or `--cpus`.

### Out of memory

A placement's `oom` attribution uses the cumulative `oom` count over the
placement, summed from the root's and (when delegated) `workload/main`'s
`memory.events.local`. Both enforce the Run's memory limit. The shim reads
these at exit; the runner reports `oom` when the count is above zero,
the shim's reason is `exited`, and the workload died by SIGKILL (or exited
137, as a shell does whose command was killed). Stops and spot reclaims
keep their reasons. The count is not tied to that kill: earlier recovered
pressure followed by an unrelated SIGKILL or a passed-through nested 137
can be attributed to `oom`; a supervisor masking the kill with exit 1 is
not. Podman's
`.State.OOMKilled` is true when the OOM killer killed any process in the
container's cgroup tree, a nested container's included, even if the
workload carried on and exited 0, so it is not used when the shim has
reported the count. It still decides when there is no count: the shim was
killed before it wrote its exit file, or predates the count.

## Exec, attach, ports

These go through the shim, not `podman exec`, so they run as the
workload user with the workload's environment, and a TTY is the shim's.
Port forwarding dials the container's address on its network from the
host (`podman container inspect` for the address, looked up once).

## Re-adoption

Podman is daemonless: each container has its own `conmon`, so a runner
restart does not touch running containers. A restarted runner finds its
containers by the `lux.managed` and `lux.run` labels and re-adopts them.

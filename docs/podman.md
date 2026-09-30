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
  AppArmor, so the e2e suite does not see this.
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
  - The runner empties every existing ephemeral volume in place rather than
    removing it (`podman volume rm -f` would also remove the stopped
    container a same-host resume reuses), gives its root back to root, and
    copies in what the image has at its path with `cp -a` (hardlinks,
    setuid bits and file capabilities kept), as podman does into a new
    volume. It stops a still-running earlier container first; one that
    cannot be stopped fails the placement. The image is mounted once for
    all of this.
  - A stopped container is reused only if it was made with the same create
    arguments (their hash is its `lux.spec` label), so one made without
    these mounts or under another AppArmor mode is not.

## Exec, attach, ports

These go through the shim, not `podman exec`, so they run as the
workload user with the workload's environment, and a TTY is the shim's.
Port forwarding dials the container's address on its network from the
host (`podman container inspect` for the address, looked up once).

## Re-adoption

Podman is daemonless: each container has its own `conmon`, so a runner
restart does not touch running containers. A restarted runner finds its
containers by the `lux.managed` and `lux.run` labels and re-adopts them.

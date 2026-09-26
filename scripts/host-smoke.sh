#!/usr/bin/env bash
# Container smoke test of the control host's reconciler (deploy/terraform/
# examples/aws/host): a debian:trixie container with systemd and Postgres
# 18, a local bare repo as the config repo, a fake `aws` serving SSM values,
# and a release built by `make dist` served over local HTTP. Runs the
# reconciler twice (then once more through its systemd unit) and fails if
# a later run changes anything. Then replaces the host: a second, fresh
# container (postgres under another uid) attaches the first one's Postgres
# volume, and the reconciler must adopt its cluster and data and reconnect
# luxd. Needs a privileged container (loop device for the Postgres volume,
# systemd as PID 1). DOCKER names the client.
# HOST_DIR=path smoke-tests another copy of host/ (e.g. a downstream repo's).
set -euo pipefail

docker=${DOCKER:-docker}
version=${VERSION:-v0.0.0-smoke}
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
host_dir=${HOST_DIR:-deploy/terraform/examples/aws/host}
[[ $host_dir == /* ]] || host_dir=$root/$host_dir
[ -f "$host_dir/reconcile.py" ] || { echo "host-smoke: FAIL: $host_dir/reconcile.py not found" >&2; exit 1; }
host_dir=$(cd "$host_dir" && pwd)
arch=$("$docker" version --format '{{.Server.Arch}}')

if [ ! -f "$root/dist/lux_${version}_linux_${arch}.tar.gz" ]; then
  # Only the tarball the container runs; `make dist` alone builds all four.
  ARCHES="linux_$arch" make -C "$root" dist VERSION="$version"
fi

image=lux-host-smoke
# The Postgres volume's image file lives in this Docker volume, so it
# outlives the first container.
vol=lux-host-smoke-$$-pgvol
name=
echo "host-smoke: host/ under test: $host_dir"
"$docker" build -q -t "$image" "$root/scripts/host-smoke" >/dev/null

# Detaches the current container's loop device: losetup -d on a mounted
# one sets autoclear, so it goes when the container's mounts do.
detach_loop() {
  "$docker" exec "$name" sh -c 'dev=$(cat /run/smoke-loop 2>/dev/null) && losetup -d "$dev"' >/dev/null 2>&1 || true
}
cleanup() {
  if [ -n "$name" ]; then
    detach_loop
    "$docker" rm -f "$name" >/dev/null 2>&1 || true
  fi
  "$docker" volume rm -f "$vol" >/dev/null 2>&1 || true
}
trap cleanup EXIT
"$docker" volume create "$vol" >/dev/null

# The container's boot log and failed units, for a failure after start.
diagnose() {
  echo "host-smoke: docker logs $name:" >&2
  "$docker" logs "$name" 2>&1 | tail -n 200 >&2 || true
  echo "host-smoke: systemctl --failed:" >&2
  "$docker" exec "$name" systemctl --failed --no-pager >&2 || true
}

# Starts a fresh container as $name and waits for systemd to boot.
# A private cgroup namespace: systemd gets its own writable cgroup2 root
# and never touches the host's hierarchy. -t gives systemd a console, so
# its boot log reaches `docker logs`; container= is how it detects Docker.
start_host() {
  name=lux-host-smoke-$$-$1
  "$docker" run -d -t --name "$name" --privileged --cgroupns=private -e container=docker \
    --tmpfs /run --tmpfs /run/lock --tmpfs /tmp \
    -v "$host_dir:/src/host:ro" \
    -v "$root/dist:/src/dist:ro" \
    -v "$root/scripts/host-smoke/inside.sh:/src/inside.sh:ro" \
    -v "$vol:/smoke-vol" \
    "$image" >/dev/null

  # `systemctl is-system-running --wait` fails at once while systemd's bus
  # socket does not exist yet, so poll until the boot finishes (degraded:
  # some unit failed, e.g. modules or getty in a container) or time out.
  local boot_timeout=${BOOT_TIMEOUT:-120} state= i
  for ((i = 0; i < boot_timeout; i++)); do
    state=$("$docker" exec "$name" systemctl is-system-running 2>&1 || true)
    case $state in running | degraded) break ;; esac
    sleep 1
  done
  case $state in
    running | degraded) ;;
    *)
      echo "host-smoke: FAIL: systemd did not boot within ${boot_timeout}s (state: $state)" >&2
      diagnose
      exit 1
      ;;
  esac
}

start_host first
"$docker" exec "$name" bash /src/inside.sh first "$version" || { diagnose; exit 1; }

# Shut the first host down as an instance stop would: Postgres stops
# cleanly (checked in its control file), the volume is unmounted and its
# loop device detached before the container goes.
echo "== replacing the host: stopping $name"
"$docker" exec "$name" bash -euo pipefail -c '
  systemctl stop lux-reconcile.timer lux-pg-backup.timer luxd
  systemctl stop postgresql@18-main
  state=$(/usr/lib/postgresql/18/bin/pg_controldata /var/lib/postgresql/18/main | sed -n "s/^Database cluster state: *//p")
  echo "cluster state: $state"
  [ "$state" = "shut down" ] || { echo "host-smoke: FAIL: cluster state $state after stop" >&2; exit 1; }
  umount /var/lib/postgresql/18
  dev=$(cat /run/smoke-loop)
  losetup -d "$dev"
  rm /run/smoke-loop
  echo "unmounted, $dev detached"
' || { diagnose; exit 1; }
"$docker" stop -t 60 "$name" >/dev/null
"$docker" rm "$name" >/dev/null
name=

start_host replaced
"$docker" exec "$name" bash /src/inside.sh replaced "$version" || { diagnose; exit 1; }
echo "host-smoke: ok"

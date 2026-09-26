#!/usr/bin/env bash
# Container smoke test of the control host's reconciler (deploy/terraform/
# examples/aws/host): a debian:trixie container with systemd and Postgres
# 18, a local bare repo as the config repo, a fake `aws` serving SSM values,
# and a release built by `make dist` served over local HTTP. Runs the
# reconciler twice (then once more through its systemd unit) and fails if
# a later run changes anything. Needs a privileged container (loop device
# for the Postgres volume, systemd as PID 1). DOCKER names the client.
set -euo pipefail

docker=${DOCKER:-docker}
version=${VERSION:-v0.0.0-smoke}
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
arch=$("$docker" version --format '{{.Server.Arch}}')

if [ ! -f "$root/dist/lux_${version}_linux_${arch}.tar.gz" ]; then
  # Only the tarball the container runs; `make dist` alone builds all four.
  ARCHES="linux_$arch" make -C "$root" dist VERSION="$version"
fi

image=lux-host-smoke
name=lux-host-smoke-$$
"$docker" build -q -t "$image" "$root/scripts/host-smoke" >/dev/null
trap '"$docker" rm -f "$name" >/dev/null 2>&1 || true' EXIT

# The container's boot log and failed units, for a failure after start.
diagnose() {
  echo "host-smoke: docker logs $name:" >&2
  "$docker" logs "$name" 2>&1 | tail -n 200 >&2 || true
  echo "host-smoke: systemctl --failed:" >&2
  "$docker" exec "$name" systemctl --failed --no-pager >&2 || true
}

# A private cgroup namespace: systemd gets its own writable cgroup2 root
# and never touches the host's hierarchy. -t gives systemd a console, so
# its boot log reaches `docker logs`; container= is how it detects Docker.
"$docker" run -d -t --name "$name" --privileged --cgroupns=private -e container=docker \
  --tmpfs /run --tmpfs /run/lock --tmpfs /tmp \
  -v "$root/deploy/terraform/examples/aws:/src/config:ro" \
  -v "$root/dist:/src/dist:ro" \
  -v "$root/scripts/host-smoke/inside.sh:/src/inside.sh:ro" \
  "$image" >/dev/null

# `systemctl is-system-running --wait` fails at once while systemd's bus
# socket does not exist yet, so poll until the boot finishes (degraded:
# some unit failed, e.g. modules or getty in a container) or time out.
boot_timeout=${BOOT_TIMEOUT:-120}
state=
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

"$docker" exec "$name" bash /src/inside.sh "$version" || { diagnose; exit 1; }

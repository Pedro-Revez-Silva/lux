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
"$docker" run -d --name "$name" --privileged --cgroupns=host \
  -v /sys/fs/cgroup:/sys/fs/cgroup:rw --tmpfs /run --tmpfs /run/lock --tmpfs /tmp \
  -v "$root/deploy/terraform/examples/aws:/src/config:ro" \
  -v "$root/dist:/src/dist:ro" \
  -v "$root/scripts/host-smoke/inside.sh:/src/inside.sh:ro" \
  "$image" >/dev/null
"$docker" exec "$name" bash /src/inside.sh "$version"

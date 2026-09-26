#!/usr/bin/env bash
# dist.sh builds the release tarballs the contract names, plus SHA256SUMS,
# into dist/. Called by `make dist VERSION=vX.Y.Z`.
#
#   lux_<version>_linux_{arm64,amd64}.tar.gz   bin/{luxd,lux}, lib/lux/runner/linux-{arm64,amd64}/{lux-runner,lux-shim}
#   lux_<version>_darwin_{arm64,amd64}.tar.gz  bin/lux (CLI only)
#
# Both runner arches go in every linux tarball (arm64 and amd64), so any
# luxd unpacked from either serves both to runner hosts without a second
# download. Unpacking a tarball into /usr/local gives the default layout
# (runner_bin_dir defaults to /usr/local/lib/lux/runner).
#
# ARCHES limits which tarballs are built, as <os>_<arch> words (default:
# all four). The runner binaries are still built for both linux arches.
set -euo pipefail

VERSION="${VERSION:?set VERSION=vX.Y.Z}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DIST="$ROOT/dist"
LDFLAGS="-s -w -X github.com/marcioapm/lux/internal/version.Version=$VERSION"
# Reproducible tarballs: root-owned regardless of the CI runner's uid,
# and a fixed mtime (the tagged commit's own date, so a rebuild of the
# same tag is byte-identical) rather than each build's wall clock.
MTIME="$(git -C "$ROOT" log -1 --format=%cI 2>/dev/null || date -u +%Y-%m-%dT%H:%M:%SZ)"
TAR_REPRO_FLAGS=(--owner=0 --group=0 --numeric-owner --sort=name --mtime="$MTIME")
ARCHES="${ARCHES:-linux_arm64 linux_amd64 darwin_arm64 darwin_amd64}"
for target in $ARCHES; do
  case "$target" in
    linux_arm64 | linux_amd64 | darwin_arm64 | darwin_amd64) ;;
    *) echo "dist.sh: ARCHES: unknown target $target" >&2; exit 1 ;;
  esac
done
wanted() { [[ " $ARCHES " == *" $1 "* ]]; }

rm -rf "$DIST"
mkdir -p "$DIST"

build() {
  local goos=$1 goarch=$2 pkg=$3 out=$4
  echo "building $pkg for $goos/$goarch"
  CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" go build -trimpath -ldflags "$LDFLAGS" -o "$out" "./cmd/$pkg"
}

# Every linux tarball carries both runner arches: build them once into
# lib/, shared by both work trees.
for rarch in arm64 amd64; do
  build linux "$rarch" lux-runner "$DIST/lib/lux/runner/linux-$rarch/lux-runner"
  build linux "$rarch" lux-shim "$DIST/lib/lux/runner/linux-$rarch/lux-shim"
done
for arch in arm64 amd64; do
  wanted "linux_$arch" || continue
  work="$DIST/work-linux-$arch"
  mkdir -p "$work/bin"
  cp -r "$DIST/lib" "$work/lib"
  build linux "$arch" luxd "$work/bin/luxd"
  build linux "$arch" lux "$work/bin/lux"
  chmod +x "$work/bin/"* "$work/lib/lux/runner"/*/*
  tar "${TAR_REPRO_FLAGS[@]}" -C "$work" -czf "$DIST/lux_${VERSION}_linux_${arch}.tar.gz" bin lib
  rm -rf "$work"
done
rm -rf "${DIST:?}/lib"

# darwin: the CLI only.
for arch in arm64 amd64; do
  wanted "darwin_$arch" || continue
  work="$DIST/work-darwin-$arch"
  mkdir -p "$work/bin"
  build darwin "$arch" lux "$work/bin/lux"
  chmod +x "$work/bin/lux"
  tar "${TAR_REPRO_FLAGS[@]}" -C "$work" -czf "$DIST/lux_${VERSION}_darwin_${arch}.tar.gz" bin
  rm -rf "$work"
done

(cd "$DIST" && sha256sum lux_*.tar.gz > SHA256SUMS)

echo "dist/:"
ls -la "$DIST"

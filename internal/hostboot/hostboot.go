// Package hostboot renders what a runner host needs to boot: the
// environment lux-runner starts with, the systemd unit that runs it, and
// the script that downloads its binaries and verifies them against luxd
// (internal/server's /runner/v1/bin/... contract). One source of truth for
// the unit and the fetch script, rendered for EC2 pools (internal/ec2's
// template.userData: ignition, script, env) and for GET
// /runner/v1/bootstrap.sh (static hosts).
package hostboot

import (
	"fmt"
	"strings"
)

// Env is what a runner needs to reach luxd and register. URL and
// HostToken are the only ones every launch needs; HostName defaults to
// the hostname when empty (a static host, or a launch that left it out);
// EC2IMDS is empty outside EC2.
type Env struct {
	URL       string
	HostToken string
	HostName  string
	EC2IMDS   string
}

// pairs are Env's fields as KEY, value, only the ones set: the order
// every rendering (and a human reading one) can rely on.
func (e Env) pairs() [][2]string {
	var out [][2]string
	add := func(k, v string) {
		if v != "" {
			out = append(out, [2]string{k, v})
		}
	}
	add("LUX_URL", e.URL)
	add("LUX_HOST_TOKEN", e.HostToken)
	add("LUX_HOST_NAME", e.HostName)
	add("LUX_EC2_IMDS", e.EC2IMDS)
	return out
}

// Lines renders KEY=value lines: the "env" userData format (for a custom
// AMI whose own boot script reads them) and /etc/lux/runner.env.
func (e Env) Lines() string {
	var b strings.Builder
	for _, kv := range e.pairs() {
		fmt.Fprintf(&b, "%s=%s\n", kv[0], kv[1])
	}
	return b.String()
}

// InstallDir is where a runner host installs the binaries and the fetch
// script. /usr/local/bin because Fedora CoreOS's SELinux policy labels it
// bin_t; systemd cannot exec from /usr/local/lib (203/EXEC). Unrelated to
// luxd's own runner_bin_dir.
const InstallDir = "/usr/local/bin"

// UnitName is the systemd unit every format installs.
const UnitName = "lux-runner.service"

// Unit is lux-runner.service. ExecStartPre (FetchBinariesScript) updates
// the binaries on every start, including the restart after an exit luxd
// asked for, and appends LUX_PROVIDER_ID to the env file on EC2.
// StartLimitIntervalSec=0 and RestartSec=10s: a luxd outage must not
// exhaust systemd's default start limit and leave the unit dead.
const Unit = `[Unit]
Description=lux runner
After=network-online.target
Wants=network-online.target
StartLimitIntervalSec=0

[Service]
EnvironmentFile=/etc/lux/runner.env
ExecStartPre=` + FetchBinariesPath + `
ExecStart=` + InstallDir + `/lux-runner --data-dir /var/lib/lux --shim ` + InstallDir + `/lux-shim
Restart=always
RestartSec=10s

[Install]
WantedBy=multi-user.target
`

// FetchBinariesPath is where every format installs FetchBinariesScript.
const FetchBinariesPath = InstallDir + "/lux-fetch-binaries.sh"

// FetchBinariesScript is lux-runner.service's ExecStartPre. It appends
// the subuid/subgid range if absent (the one place both formats do it:
// Ignition has no "append if absent"), then downloads only the binaries
// whose sha256 differs from luxd's manifest, verifies them against it
// and X-Lux-Sha256, and installs the pair together. On any failure it
// keeps previously verified binaries (exit 0) rather than leave the unit
// dead.
const FetchBinariesScript = `#!/bin/bash
[ -n "${BASH_VERSION:-}" ] || { echo "lux: fetch-binaries.sh needs bash, not sh" >&2; exit 1; }
set -uo pipefail

install_dir="` + InstallDir + `"

grep -q '^containers:' /etc/subuid 2>/dev/null || echo '` + SubuidRange + `' >> /etc/subuid
grep -q '^containers:' /etc/subgid 2>/dev/null || echo '` + SubuidRange + `' >> /etc/subgid

warn_and_keep_running() {
  echo "lux: $1" >&2
  if [ -x "$install_dir/lux-runner" ] && [ -x "$install_dir/lux-shim" ] \
     && [ -f "$install_dir/.lux-installed-sha256" ]; then
    echo "lux: keeping the already-installed, previously verified binaries" >&2
    exit 0
  fi
  echo "lux: no verified binaries installed; cannot start" >&2
  exit 1
}

arch=$(uname -m)
case "$arch" in
  aarch64|arm64) larch=arm64 ;;
  x86_64|amd64)  larch=amd64 ;;
  *) echo "lux: unsupported architecture $arch" >&2; exit 1 ;;
esac

mkdir -p "$install_dir"
curl_retry() { curl -fsS --retry 5 --retry-connrefused --retry-delay 2 "$@"; }

manifest=$(curl_retry -H "Authorization: Bearer $LUX_HOST_TOKEN" "$LUX_URL/runner/v1/bin/manifest") \
  || warn_and_keep_running "fetching the manifest failed"

sha_for() {
  # {"linux-arm64": {"lux-runner": "<sha>", ...}, ...} without a JSON tool:
  # find the "linux-$larch" object, then the "$1" key inside it.
  printf '%s' "$manifest" | tr -d '\n' | grep -o "\"linux-$larch\"[^}]*}" \
    | grep -o "\"$1\"[[:space:]]*:[[:space:]]*\"[a-f0-9]*\"" | grep -o '[a-f0-9]\{64\}'
}

staged=()
cleanup() { [ ${#staged[@]} -eq 0 ] || rm -f "${staged[@]}"; }
trap cleanup EXIT

for bin in lux-runner lux-shim; do
  want=$(sha_for "$bin")
  if [ -z "$want" ]; then
    warn_and_keep_running "luxd's manifest has no $bin for linux-$larch"
  fi
  have=""
  [ -x "$install_dir/$bin" ] && have=$(sha256sum "$install_dir/$bin" | awk '{print $1}')
  if [ "$have" = "$want" ]; then
    continue  # already current: nothing to download
  fi
  tmp=$(mktemp "$install_dir/.$bin.XXXXXX")
  staged+=("$tmp")
  url="$LUX_URL/runner/v1/bin/linux-$larch/$bin"
  headers=$(mktemp)
  curl_retry -H "Authorization: Bearer $LUX_HOST_TOKEN" -D "$headers" -o "$tmp" "$url" \
    || warn_and_keep_running "downloading $bin failed"
  got_header=$(tr -d '\r' < "$headers" | awk -F': ' 'BEGIN{IGNORECASE=1} tolower($1)=="x-lux-sha256"{print tolower($2)}')
  rm -f "$headers"
  got=$(sha256sum "$tmp" | awk '{print $1}')
  if [ "$got" != "$want" ] || [ "$got_header" != "$want" ]; then
    warn_and_keep_running "sha256 mismatch downloading $bin (got $got, want $want)"
  fi
  chmod 0755 "$tmp"
done

# Atomic install: both staged files (if any were downloaded) rename into
# place together, so a process between here and ExecStart never sees one
# new binary next to one old one.
for tmp in "${staged[@]:-}"; do
  [ -z "$tmp" ] && continue
  bin=$(basename "$tmp" | sed 's/^\.//; s/\.[^.]*$//')
  mv -f "$tmp" "$install_dir/$bin"
done
staged=()
sha256sum "$install_dir/lux-runner" "$install_dir/lux-shim" > "$install_dir/.lux-installed-sha256"

if [ -n "${LUX_EC2_IMDS:-}" ] && ! grep -q '^LUX_PROVIDER_ID=' /etc/lux/runner.env 2>/dev/null; then
  token=$(curl_retry -X PUT -H 'X-aws-ec2-metadata-token-ttl-seconds: 21600' "$LUX_EC2_IMDS/latest/api/token")
  id=$(curl_retry -H "X-aws-ec2-metadata-token: $token" "$LUX_EC2_IMDS/latest/meta-data/instance-id")
  printf 'LUX_PROVIDER_ID=%s\n' "$id" >> /etc/lux/runner.env
fi
`

// SubuidRange is the range the runner's --userns=auto allocates from (see
// docs/operations.md "Host requirements"); appended to /etc/subuid and
// /etc/subgid, never replacing what a distro already put there.
const SubuidRange = "containers:2147483647:2147483648"

// Render builds a host's user data in format ("" defaults to "ignition").
func Render(format string, env Env) ([]byte, error) {
	switch format {
	case "", "ignition":
		return Ignition(env)
	case "script":
		return []byte(Script(env)), nil
	case "env":
		return []byte(env.Lines()), nil
	default:
		return nil, fmt.Errorf("hostboot: unknown userData format %q", format)
	}
}

// ValidUserData reports whether Render accepts v: checked when a pool is
// set, so a typo is refused there rather than surfacing as a boot failure
// no one is watching for.
func ValidUserData(v string) bool {
	_, err := Render(v, Env{})
	return err == nil
}

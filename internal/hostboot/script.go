package hostboot

import (
	"fmt"
	"strings"
)

// scriptBody backs both userData "script" (Script) and GET
// /runner/v1/bootstrap.sh (Bootstrap). It checks the host requirements from
// docs/operations.md and installs only what is missing (dnf, else
// apt-get). Idempotent: a rerun never disturbs a running lux-runner.
const scriptBody = `#!/bin/bash
[ -n "${BASH_VERSION:-}" ] || { echo "lux: run this script with bash, not sh (curl ... | sudo env ... bash)" >&2; exit 1; }
set -euo pipefail

: "${LUX_URL:?LUX_URL is required}"
: "${LUX_HOST_TOKEN:?LUX_HOST_TOKEN is required}"
LUX_HOST_NAME="${LUX_HOST_NAME:-$(hostname)}"
LUX_EC2_IMDS="${LUX_EC2_IMDS:-}"

echo "lux: bootstrapping $LUX_HOST_NAME against $LUX_URL"

missing=()
command -v podman >/dev/null 2>&1 || missing+=(podman)
command -v nft >/dev/null 2>&1 || missing+=(nftables)
command -v git >/dev/null 2>&1 || missing+=(git)
[ -f /sys/fs/cgroup/cgroup.controllers ] || { echo "lux: cgroup v2 is not mounted; this kernel/boot config cannot run lux-runner" >&2; exit 1; }
if command -v podman >/dev/null 2>&1; then
  podman_major=$(podman version --format '{{.Client.Version}}' 2>/dev/null | cut -d. -f1)
  if [ -z "$podman_major" ] || [ "$podman_major" -lt 5 ]; then
    missing+=(podman)
  fi
fi

if [ "${#missing[@]}" -gt 0 ]; then
  echo "lux: installing missing packages: ${missing[*]}"
  if command -v dnf >/dev/null 2>&1; then
    dnf -y install podman netavark aardvark-dns nftables git shadow-utils
  elif command -v apt-get >/dev/null 2>&1; then
    export DEBIAN_FRONTEND=noninteractive
    apt-get update
    apt-get install -y podman netavark aardvark-dns nftables git uidmap
  else
    echo "lux: missing ${missing[*]} and neither dnf nor apt-get is available; install them and rerun" >&2
    exit 1
  fi
fi

mkdir -p /etc/lux
umask 077
{
  printf 'LUX_URL=%s\n' "$LUX_URL"
  printf 'LUX_HOST_TOKEN=%s\n' "$LUX_HOST_TOKEN"
  printf 'LUX_HOST_NAME=%s\n' "$LUX_HOST_NAME"
  [ -n "$LUX_EC2_IMDS" ] && printf 'LUX_EC2_IMDS=%s\n' "$LUX_EC2_IMDS"
} > /etc/lux/runner.env
chmod 0600 /etc/lux/runner.env

cat > ` + FetchBinariesPath + ` <<'LUX_FETCH_SCRIPT'
` + FetchBinariesScript + `LUX_FETCH_SCRIPT
chmod 0755 ` + FetchBinariesPath + `

cat > /etc/systemd/system/` + UnitName + ` <<'LUX_UNIT'
` + Unit + `LUX_UNIT

systemctl daemon-reload
systemctl enable ` + UnitName + `
systemctl is-active --quiet ` + UnitName + ` || systemctl start ` + UnitName + `
echo "lux: bootstrap complete"
`

// Script renders the userData "script" format: scriptBody with the env
// exported ahead of it.
func Script(env Env) string {
	var b strings.Builder
	b.WriteString("#!/bin/bash\n")
	for _, kv := range env.pairs() {
		fmt.Fprintf(&b, "export %s=%s\n", kv[0], shellQuote(kv[1]))
	}
	b.WriteString(strings.TrimPrefix(scriptBody, "#!/bin/bash\n"))
	return b.String()
}

// Bootstrap renders GET /runner/v1/bootstrap.sh: the same script, taking its
// env from the environment it runs in (curl ... | sudo env LUX_URL=...
// LUX_HOST_TOKEN=... bash). Its guard line refuses a bare `sh` (dash on
// Debian/Ubuntu), which ignores the shebang when piped.
func Bootstrap() string { return scriptBody }

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

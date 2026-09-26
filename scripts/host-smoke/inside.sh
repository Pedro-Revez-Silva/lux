#!/usr/bin/env bash
# Runs inside the smoke container (see scripts/host-smoke.sh): sets up what
# cloud-init and AWS would provide, then runs the reconciler twice and
# checks the second run changes nothing.
set -euo pipefail

version=$1
prefix=/lux
volume=vol-0123456789abcdef0
backup_bucket=lux-pg-backups-123456789012-eu-north-1
checkout=/var/lib/lux/config

# The release, served like GitHub's <base>/<tag>/<file>.
arch=$(dpkg --print-architecture)
mkdir -p "/srv/releases/$version"
cp "/src/dist/lux_${version}_linux_${arch}.tar.gz" /src/dist/SHA256SUMS "/srv/releases/$version/"
systemd-run -q --unit smoke-releases python3 -m http.server 8000 --bind 127.0.0.1 --directory /srv/releases
systemd-run -q --unit smoke-s3 python3 /usr/local/lib/smoke/fake-s3.py 9000
install -d -m 1777 /srv/s3

# luxd's blob store: the fake S3 endpoint (env overrides luxd.toml).
mkdir -p /etc/systemd/system/luxd.service.d
cat >/etc/systemd/system/luxd.service.d/smoke.conf <<'EOF'
[Service]
Environment=LUX_S3_ENDPOINT=http://127.0.0.1:9000 LUX_S3_ACCESS_KEY=smoke LUX_S3_SECRET_KEY=smoke
EOF

# The Postgres data volume: a loop device under its EBS by-id name.
truncate -s 256M /var/lib/smoke-pg.img
dev=$(losetup -f --show /var/lib/smoke-pg.img)
mkdir -p /dev/disk/by-id
ln -sf "$dev" "/dev/disk/by-id/nvme-Amazon_Elastic_Block_Store_${volume//-/}"

# The config repo: this repo's example host/ with a desired state naming
# the local release.
git config --global user.email smoke@example.com
git config --global user.name smoke
git config --global init.defaultBranch main
git init -q --bare /srv/config.git
work=$(mktemp -d)
tar -C /src/config --exclude=__pycache__ --exclude=.pytest_cache --exclude=.ruff_cache -cf - host | tar -C "$work" -xf -
cat >"$work/host/lux-host.toml" <<EOF
lux_version = "$version"
release_base_url = "http://127.0.0.1:8000"

[luxd]
scale_down_after = "15m"
EOF
git -C "$work" init -q
git -C "$work" add -A
git -C "$work" commit -q -m "desired state"
git -C "$work" push -q /srv/config.git HEAD:main

mkdir -p /etc/smoke
cat >/etc/smoke/ssm.json <<EOF
{
  "$prefix/public_url": "https://lux.example.com",
  "$prefix/blob_bucket": "lux-blobs-123456789012-eu-north-1",
  "$prefix/backup_bucket": "$backup_bucket",
  "$prefix/db_name": "lux",
  "$prefix/luxd_port": "7070",
  "$prefix/pg_data_volume_id": "$volume",
  "$prefix/tunnel_token_parameter": "$prefix/cloudflare-tunnel-token",
  "$prefix/cloudflare-tunnel-token": "smoke-token",
  "$prefix/config_repo_url": "/srv/config.git",
  "$prefix/config_repo_ref": "main"
}
EOF

# What cloud-init does: host.json and the clone.
mkdir -p /etc/lux
cat >/etc/lux/host.json <<EOF
{"region": "eu-north-1", "ssm_prefix": "$prefix", "checkout": "$checkout", "config_repo_path": ""}
EOF
git clone -q -b main /srv/config.git "$checkout"

fail() { echo "host-smoke: FAIL: $*" >&2; exit 1; }

echo "== run 1"
out1=$(PYTHONDONTWRITEBYTECODE=1 python3 "$checkout/host/reconcile.py") || { echo "$out1"; fail "first run exited non-zero"; }
echo "$out1"
summary1=$(grep '^lux-reconcile: status=' <<<"$out1")
[[ $summary1 == "lux-reconcile: status=ok version=$version changed=["* ]] || fail "first run: $summary1"
[[ $summary1 == *"version:$version"* ]] || fail "first run did not install $version"
curl -fsS http://127.0.0.1:7070/health >/dev/null || fail "luxd /health"
[ "$(/usr/local/bin/luxd version)" = "$version" ] || fail "luxd version"
mountpoint -q /var/lib/postgresql/18 || fail "data volume not mounted"
for u in luxd cloudflared lux-reconcile.timer lux-pg-backup.timer; do
  systemctl is-active -q "$u" || fail "$u not active"
done
[ "$(stat -c %a /etc/lux/luxd.toml)" = 600 ] || fail "luxd.toml mode"

echo "== run 2"
out2=$(PYTHONDONTWRITEBYTECODE=1 python3 "$checkout/host/reconcile.py") || { echo "$out2"; fail "second run exited non-zero"; }
echo "$out2"
summary2=$(grep '^lux-reconcile: status=' <<<"$out2")
[ "$summary2" = "lux-reconcile: status=ok version=$version changed=[]" ] || fail "second run changed something: $summary2"

echo "== run 3, through the systemd unit"
systemctl start lux-reconcile.service || fail "lux-reconcile.service"
journalctl -u lux-reconcile.service -o cat | grep '^lux-reconcile: status=' | tail -1 \
  | grep -qx "lux-reconcile: status=ok version=$version changed=\[\]" || fail "unit run changed something"

echo "== backup"
systemctl start lux-pg-backup.service || fail "lux-pg-backup.service"
dump=$(ls /srv/s3/$backup_bucket/lux-*.dump)
[ "$(head -c5 "$dump")" = PGDMP ] || fail "$dump is not a pg_dump -Fc archive"

echo "host-smoke: ok"

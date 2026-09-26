#!/usr/bin/env bash
# Runs inside a smoke container (see scripts/host-smoke.sh). Both phases
# first set up what cloud-init and AWS would provide.
#   first VERSION     a new host on a blank volume: runs the reconciler
#                     twice, then through its unit, and a backup; then
#                     writes sentinel data and records it in the volume dir.
#   replaced VERSION  a fresh host on the first host's volume (and, with a
#                     different postgres uid): checks the reconciler adopts
#                     the cluster, keeps the data and reconnects luxd.
set -euo pipefail

phase=$1
version=$2
prefix=/lux
volume=vol-0123456789abcdef0
backup_bucket=lux-pg-backups-123456789012-eu-north-1
checkout=/var/lib/lux/config
# The harness's volume: the Postgres volume image and what the first
# phase recorded for the second.
voldir=/smoke-vol
expected=$voldir/expected.env
calls=/run/smoke-calls

fail() { echo "host-smoke: FAIL: $*" >&2; exit 1; }
reconcile() { PYTHONDONTWRITEBYTECODE=1 python3 "$checkout/host/reconcile.py"; }
lux_sql() { runuser -u postgres -- psql -X -q -tA -v ON_ERROR_STOP=1 -d lux -c "$1"; }

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

# Commands that create or destroy a volume or cluster log their argv to
# $calls, then run the real one. /usr/local/sbin is first on PATH for
# docker exec and systemd units alike. The pg_* commands come with the
# Postgres package the reconciler installs, so the real one is looked up
# at call time. postgresql-18's postinst creates its default cluster on
# the root disk without passing through here: apt runs maintainer scripts
# with DPkg::Path (/usr/sbin:/usr/bin:/sbin:/bin), so $calls holds only
# what the reconciler ran.
for cmd in mkfs.ext4 pg_createcluster pg_dropcluster; do
  cat >"/usr/local/sbin/$cmd" <<EOF
#!/bin/sh
echo "$cmd \$*" >>$calls
real=\$(PATH=/usr/sbin:/usr/bin:/sbin:/bin command -v $cmd) || { echo "$cmd: not installed" >&2; exit 127; }
exec "\$real" "\$@"
EOF
  chmod 755 "/usr/local/sbin/$cmd"
done
: >"$calls"

# The Postgres data volume: a loop device under its EBS by-id name, backed
# by an image in the harness's Docker volume so that a second container
# can attach it. Loop devices are the Docker host's: the container's /dev
# holds only the nodes that existed when it started, so the free one may
# need its node. host-smoke.sh detaches it through /run/smoke-loop. The
# reconciler mounts LABEL=pgdata, so a device left attached by an earlier
# run would be mounted instead of this one.
leftover=$(blkid -t LABEL=pgdata -o device 2>/dev/null || true)
[ -z "$leftover" ] || fail "LABEL=pgdata already on $leftover (detach with losetup -d on the Docker host)"
case $phase in
  first)
    [ ! -e "$voldir/pg.img" ] || fail "$voldir/pg.img exists before the first host"
    truncate -s 256M "$voldir/pg.img"
    ;;
  replaced) [ -f "$voldir/pg.img" ] && [ -f "$expected" ] || fail "the first host left no volume or expected.env" ;;
  *) fail "unknown phase $phase" ;;
esac
dev=
for _ in 1 2 3 4 5; do
  dev=$(losetup -f)
  [ -b "$dev" ] || mknod -m 0660 "$dev" b 7 "${dev#/dev/loop}"
  losetup "$dev" "$voldir/pg.img" && break
  dev=
done
[ -n "$dev" ] || fail "no loop device"
echo "$dev" >/run/smoke-loop
mkdir -p /dev/disk/by-id
ln -sf "$dev" "/dev/disk/by-id/nvme-Amazon_Elastic_Block_Store_${volume//-/}"

# The config repo: the host/ under test (HOST_DIR in host-smoke.sh) with a
# desired state naming the local release.
git config --global user.email smoke@example.com
git config --global user.name smoke
git config --global init.defaultBranch main
git init -q --bare /srv/config.git
work=$(mktemp -d)
mkdir "$work/host"
tar -C /src/host --exclude=__pycache__ --exclude=.pytest_cache --exclude=.ruff_cache -cf - . | tar -C "$work/host" -xf -
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

check_host_up() {
  curl -fsS http://127.0.0.1:7070/health >/dev/null || fail "luxd /health"
  [ "$(/usr/local/bin/luxd version)" = "$version" ] || fail "luxd version"
  mountpoint -q /var/lib/postgresql/18 || fail "data volume not mounted"
  for u in luxd cloudflared lux-reconcile.timer lux-pg-backup.timer; do
    systemctl is-active -q "$u" || fail "$u not active"
  done
  [ "$(stat -c %a /etc/lux/luxd.toml)" = 600 ] || fail "luxd.toml mode"
}

check_second_run() {
  local out summary
  out=$(reconcile) || { echo "$out"; fail "$1 exited non-zero"; }
  echo "$out"
  summary=$(grep '^lux-reconcile: status=' <<<"$out")
  [ "$summary" = "lux-reconcile: status=ok version=$version changed=[]" ] || fail "$1 changed something: $summary"
}

first() {
  echo "== run 1"
  getent passwd postgres >/dev/null && fail "postgres user exists before the first run"
  t0=$(date +%s)
  out1=$(reconcile) || { echo "$out1"; fail "first run exited non-zero"; }
  echo "$out1"
  echo "host-smoke: first run took $(($(date +%s) - t0))s (package installs included)"
  summary1=$(grep '^lux-reconcile: status=' <<<"$out1")
  [[ $summary1 == "lux-reconcile: status=ok version=$version changed=["* ]] || fail "first run: $summary1"
  [[ $summary1 == *"install:postgresql-18"* ]] || fail "first run did not install postgresql-18: $summary1"
  [[ $summary1 == *"version:$version"* ]] || fail "first run did not install $version"
  check_host_up
  # The wrappers see a new host's mkfs and cluster: their absence on the
  # replaced host means something.
  grep -q '^mkfs.ext4 ' "$calls" || fail "first run: mkfs.ext4 not seen by the wrapper"
  grep -q '^pg_createcluster ' "$calls" || fail "first run: pg_createcluster not seen by the wrapper"

  echo "== run 2"
  check_second_run "second run"

  echo "== run 3, through the systemd unit"
  systemctl start lux-reconcile.service || fail "lux-reconcile.service"
  journalctl -u lux-reconcile.service -o cat | grep '^lux-reconcile: status=' | tail -1 \
    | grep -qx "lux-reconcile: status=ok version=$version changed=\[\]" || fail "unit run changed something"

  echo "== backup"
  systemctl start lux-pg-backup.service || fail "lux-pg-backup.service"
  dump=$(ls /srv/s3/$backup_bucket/lux-*.dump)
  [ "$(head -c5 "$dump")" = PGDMP ] || fail "$dump is not a pg_dump -Fc archive"

  echo "== sentinel for the replaced host"
  sentinel=$(head -c 12 /dev/urandom | od -An -tx1 | tr -d ' \n')
  lux_sql "CREATE TABLE smoke_sentinel (v text NOT NULL); INSERT INTO smoke_sentinel VALUES ('$sentinel');"
  # A tenant written by luxd as lux_app, through luxd's own config.
  /usr/local/bin/luxd admin create-tenant --name smoke-sentinel >/dev/null || fail "luxd admin create-tenant"
  cat >"$expected" <<EOF
sentinel=$sentinel
migrations=$(lux_sql "SELECT count(*) FROM schema_migrations")
tenants=$(lux_sql "SELECT count(*) FROM tenants WHERE name = 'smoke-sentinel'")
system_identifier=$(lux_sql "SELECT system_identifier FROM pg_control_system()")
fs_uuid=$(blkid -s UUID -o value "$dev")
pg_uid=$(id -u postgres)
EOF
  cat "$expected"
  echo "host-smoke: first host ok"
}

replaced() {
  # shellcheck disable=SC1090
  . "$expected"
  for f in /root/.lux-pg-owner-password /root/.lux-app-password /root/.lux-migrate-dsn; do
    [ ! -e "$f" ] || fail "$f exists before the replaced host's first run"
  done
  [ ! -e /usr/local/lux ] || fail "lux is installed before the replaced host's first run"

  # A newer image may give postgres another uid, so the adopted files are
  # foreign. The reconciler installs the package, whose postinst keeps an
  # existing postgres user only if it is a system user (adduser --system
  # exits 13 otherwise): pick another uid in the system range.
  getent passwd postgres >/dev/null && fail "postgres user exists before the replaced host's first run"
  new_id=990
  [ "$new_id" != "$pg_uid" ] || new_id=991
  groupadd -r -g "$new_id" postgres
  useradd -r -u "$new_id" -g postgres -d /var/lib/postgresql -M -s /bin/bash postgres
  echo "== postgres uid $pg_uid on the first host; $new_id here"

  echo "== replaced host: run 1"
  t0=$(date +%s)
  out1=$(reconcile) || { echo "$out1"; fail "replaced host's first run exited non-zero"; }
  echo "$out1"
  echo "host-smoke: replaced host's first run took $(($(date +%s) - t0))s (package installs included)"
  [ "$(id -u postgres)" = "$new_id" ] || fail "the package install changed the postgres uid to $(id -u postgres)"
  summary1=$(grep '^lux-reconcile: status=' <<<"$out1")
  [[ $summary1 == "lux-reconcile: status=ok version=$version changed=["* ]] || fail "replaced host's first run: $summary1"
  [[ $summary1 == *"version:$version"* ]] || fail "replaced host did not install $version"
  [[ $summary1 == *"install:postgresql-18"* ]] || fail "replaced host did not install postgresql-18: $summary1"
  # pg-volume is the mount (adopted or new alike); database is createdb.
  changed=${summary1#*changed=[}
  changed=,${changed%]},
  [[ $changed == *,database,* ]] && fail "replaced host created the database: $summary1"
  [[ $changed == *,pg-passwords,* ]] || fail "replaced host did not regenerate the passwords: $summary1"
  check_host_up

  echo "== replaced host: volume and cluster adopted"
  if [ -s "$calls" ]; then
    cat "$calls" >&2
    fail "replaced host ran mkfs/pg_createcluster/pg_dropcluster"
  fi
  echo "no mkfs.ext4, pg_createcluster or pg_dropcluster"
  [ "$(blkid -s UUID -o value "$dev")" = "$fs_uuid" ] || fail "volume filesystem UUID changed: reformatted"
  sysid=$(lux_sql "SELECT system_identifier FROM pg_control_system()")
  [ "$sysid" = "$system_identifier" ] || fail "cluster system identifier $sysid, was $system_identifier: a new cluster"
  echo "filesystem UUID $fs_uuid, cluster system identifier $sysid: unchanged"
  if grep -E 'not properly shut down|was interrupted' /var/log/postgresql/postgresql-18-main.log >&2; then
    fail "the adopted cluster was not shut down cleanly"
  fi
  foreign=$(find /var/lib/postgresql/18/main ! -uid "$new_id" | head -3)
  [ -z "$foreign" ] || fail "not owned by uid $new_id after adoption: $foreign"
  echo "data directory owned by postgres uid $new_id (uid $pg_uid on the first host)"

  echo "== replaced host: sentinel data and migrations"
  got=$(lux_sql "SELECT v FROM smoke_sentinel")
  [ "$got" = "$sentinel" ] || fail "sentinel row: '$got', want '$sentinel'"
  echo "sentinel row $got: kept"
  got=$(lux_sql "SELECT count(*) FROM schema_migrations")
  [ "$got" = "$migrations" ] || fail "schema_migrations count $got, was $migrations"
  echo "schema_migrations count $got: unchanged"
  got=$(lux_sql "SELECT count(*) FROM tenants WHERE name = 'smoke-sentinel'")
  [ "$got" = "$tenants" ] || fail "sentinel tenants $got, was $tenants"

  echo "== replaced host: password files"
  for f in /root/.lux-pg-owner-password /root/.lux-app-password /root/.lux-migrate-dsn; do
    [ -s "$f" ] || fail "$f missing"
    [ "$(stat -c %a "$f")" = 600 ] || fail "$f mode $(stat -c %a "$f")"
  done
  echo "/root/.lux-{pg-owner-password,app-password,migrate-dsn}: present, 0600"

  echo "== replaced host: luxd authenticates as lux_app"
  url=$(python3 -c 'import tomllib; print(tomllib.load(open("/etc/lux/luxd.toml", "rb"))["database"]["url"])')
  [[ $url == postgres://lux_app:* ]] || fail "luxd.toml database.url is not lux_app's"
  [[ $url == *":$(cat /root/.lux-app-password)@"* ]] || fail "luxd.toml does not carry /root/.lux-app-password"
  who=$(psql -X -tA "$url" -c "SELECT current_user") || fail "lux_app cannot log in with luxd.toml's URL"
  [ "$who" = lux_app ] || fail "logged in as $who"
  echo "psql with luxd.toml's database.url: current_user=$who"
  # Every luxd tick queries the database, so the running luxd holds a
  # lux_app connection once it has authenticated.
  n=0
  for _ in $(seq 20); do
    n=$(lux_sql "SELECT count(*) FROM pg_stat_activity WHERE usename = 'lux_app' AND datname = 'lux'")
    [ "$n" -gt 0 ] && break
    sleep 1
  done
  [ "$n" -gt 0 ] || fail "the running luxd holds no lux_app connection"
  echo "running luxd: $n lux_app connection(s) in pg_stat_activity"
  out=$(/usr/local/bin/luxd admin create-tenant --name smoke-replaced) || fail "luxd admin create-tenant as lux_app"
  [[ $out == *'"tenantId"'* ]] || fail "luxd admin create-tenant: $out"
  echo "luxd admin create-tenant (luxd.toml, lux_app): ok"

  echo "== replaced host: run 2"
  check_second_run "replaced host's second run"

  echo "host-smoke: replaced host ok"
}

"$phase"

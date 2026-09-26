"""The packages the control host runs beyond cloud-init's bootstrap:
Postgres 18 from PGDG (trixie ships 17; docs/operations.md says tested on
18) and cloudflared from its GitHub release. Each is installed only when
missing, so a steady-state run runs no apt command and downloads nothing;
a failed install fails the run and the next run tries again.
"""
import os
import tempfile

from . import release
from .host import Host, HostError, write_if_changed

PG_PACKAGE = "postgresql-18"
PGDG_KEY_URL = "https://www.postgresql.org/media/keys/ACCC4CF8.asc"
CLOUDFLARED_URL = "https://github.com/cloudflare/cloudflared/releases/latest/download/cloudflared-linux-{arch}.deb"
# apt-get waits this long for the dpkg lock (e.g. cloud-init's or
# apt-daily's apt run) instead of failing at once.
LOCK_TIMEOUT_S = 300
LOCK_POLL_S = 5
APT_ENV = {"DEBIAN_FRONTEND": "noninteractive"}


def _apt_get(*args: str) -> list:
    return ["apt-get", "-o", f"DPkg::Lock::Timeout={LOCK_TIMEOUT_S}", *args]


def is_installed(host: Host, package: str) -> bool:
    # dpkg -s also succeeds for a removed package whose config files remain.
    out = host.run(["dpkg", "-s", package], check=False)
    return out.returncode == 0 and "Status: install ok installed" in (out.stdout or "")


def apt_update(host: Host) -> None:
    # apt-get update locks /var/lib/apt/lists, which DPkg::Lock::Timeout
    # does not cover: retry it for the same bound.
    deadline = host.now() + LOCK_TIMEOUT_S
    while True:
        out = host.run(_apt_get("update"), check=False)
        if out.returncode == 0:
            return
        if "Could not get lock" not in (out.stderr or "") or host.now() >= deadline:
            raise HostError(f"apt-get update: exit {out.returncode}: {(out.stderr or '').strip()}")
        host.sleep(LOCK_POLL_S)


def ensure_postgres(host: Host) -> bool:
    if is_installed(host, PG_PACKAGE):
        return False
    host.log(f"installing {PG_PACKAGE} from PGDG")
    key = os.path.join(host.paths.pgdg_key_dir, "apt.postgresql.org.asc")
    os.makedirs(host.paths.pgdg_key_dir, mode=0o755, exist_ok=True)
    try:
        with host.urlopen(PGDG_KEY_URL, timeout=60) as resp:
            key_text = resp.read().decode()
    except Exception as e:
        raise HostError(f"PGDG signing key download failed: {e}") from None
    write_if_changed(key, key_text, 0o644)
    source = (
        f"deb [signed-by=/usr/share/postgresql-common/pgdg/apt.postgresql.org.asc] "
        f"https://apt.postgresql.org/pub/repos/apt trixie-pgdg main\n"
    )
    write_if_changed(os.path.join(host.paths.apt_sources_dir, "pgdg.list"), source, 0o644)
    apt_update(host)
    host.run(_apt_get("install", "-y", PG_PACKAGE), env=APT_ENV)
    host.run(["systemctl", "enable", "postgresql"])
    return True


def cloudflared_present(host: Host) -> bool:
    # The unit's ExecStart path. The .deb ships /usr/bin/cloudflared; its
    # postinst links it here, so this also means the install completed.
    return os.access(os.path.join(host.paths.bin_dir, "cloudflared"), os.X_OK)


def ensure_cloudflared(host: Host) -> bool:
    if cloudflared_present(host):
        return False
    arch = host.run(["dpkg", "--print-architecture"]).stdout.strip()
    url = CLOUDFLARED_URL.format(arch=arch)
    host.log(f"installing cloudflared from {url}")
    with tempfile.TemporaryDirectory(prefix="lux-cloudflared-") as tmp:
        deb = os.path.join(tmp, "cloudflared.deb")
        try:
            release.download(url, deb, host.urlopen)
        except Exception as e:
            raise HostError(f"cloudflared download failed: {e}") from None
        # apt-get on a local .deb: dpkg -i plus its dependencies, waiting
        # for the dpkg lock where dpkg -i would fail at once.
        host.run(_apt_get("install", "-y", deb), env=APT_ENV)
    if not cloudflared_present(host):
        raise HostError(f"cloudflared installed but {host.paths.bin_dir}/cloudflared is missing")
    return True

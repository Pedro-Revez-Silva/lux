"""One reconcile run: bring the control host to what the config repo and
SSM describe. See host/README.md for the steps and how to read the result."""
import fcntl
import os
import sys

from . import desired as desired_mod
from . import gitsync, luxdconf, packages, postgres, release, units
from .host import Host, HostError, read_file, write_if_changed
from .infra import Bootstrap, get_secure_parameter, load_bootstrap, load_infra

BOOTSTRAP_PATH = "/etc/lux/host.json"
REEXEC_ENV = "LUX_RECONCILE_REEXECED"
# What cloud-init installed before the reconciler existed: the old deploy
# timer would otherwise keep rewriting luxd.toml alongside this one.
LEGACY_UNITS = ["lux-deploy.timer", "lux-deploy.service"]
LEGACY_FILES = [
    "lib/lux/deploy-lux.py",
    "lib/lux/pg-backup.sh",
    "sbin/lux-init-postgres.sh",
    "sbin/lux-render-config.sh",
    "sbin/lux-fetch-cf-token.sh",
]


class Run:
    def __init__(self, host: Host, boot: Bootstrap, reexec, environ: dict):
        self.host = host
        self.boot = boot
        self.reexec = reexec
        self.environ = environ
        # Carried over from the run that re-executed this one.
        self.changed: list = [c for c in environ.get(REEXEC_ENV, "").split(",") if c not in ("", "none")]
        self.step = "start"
        self.deferred_error: HostError | None = None

    def sync_checkout(self, infra: dict) -> None:
        self.step = "git"
        key_param = infra["config_repo_deploy_key_parameter"]
        if gitsync.refresh_deploy_key(self.host, key_param, self.boot.region):
            self.changed.append("deploy-key")
        env = gitsync.git_env(self.host, key_param)
        host_rel = os.path.relpath(self.boot.host_dir, self.boot.checkout)
        moved, host_changed = gitsync.sync(
            self.host, self.boot.checkout, infra["config_repo_url"], infra["config_repo_ref"], host_rel, env,
        )
        if moved:
            self.changed.append("checkout")
        if host_changed and not self.environ.get(REEXEC_ENV):
            # The rest of this run is the new code's job; it re-reads
            # everything, so nothing done so far is lost.
            self.host.log("host/ changed; re-executing the new reconciler")
            script = os.path.join(self.boot.host_dir, "reconcile.py")
            carried = ",".join(self.changed) or "none"
            self.reexec(sys.executable, [sys.executable, script], {**self.environ, REEXEC_ENV: carried})

    def load_desired(self) -> desired_mod.Desired:
        self.step = "desired-state"
        path = os.path.join(self.boot.host_dir, "lux-host.toml")
        text = read_file(path)
        if text is None:
            raise HostError(f"{path} is missing")
        return desired_mod.parse(text, path)

    def remove_legacy(self) -> None:
        self.step = "legacy"
        sd = self.host.paths.systemd_dir
        present = [u for u in LEGACY_UNITS if os.path.exists(os.path.join(sd, u))]
        if not present:
            return
        self.host.run(["systemctl", "disable", "--now", *present], check=False)
        for u in present:
            os.remove(os.path.join(sd, u))
        for rel in LEGACY_FILES:
            f = os.path.join(self.host.paths.usr_local, rel)
            if os.path.exists(f):
                os.remove(f)
        self.host.run(["systemctl", "daemon-reload"])
        self.changed.append("legacy-removed")

    def packages(self) -> None:
        self.step = "packages"
        # Recorded one at a time: a failed cloudflared install must not
        # hide the Postgres install that succeeded before it.
        if packages.ensure_postgres(self.host):
            self.changed.append(f"install:{packages.PG_PACKAGE}")
        if packages.ensure_cloudflared(self.host):
            self.changed.append("install:cloudflared")

    def postgres(self, infra: dict) -> dict:
        self.step = "postgres"
        if postgres.ensure_volume(self.host, infra["pg_data_volume_id"]):
            self.changed.append("pg-volume")
        creds, changed = postgres.ensure_database(self.host, infra["db_name"])
        self.changed += changed
        return creds

    def write_units(self, infra: dict) -> dict:
        """Writes the units; returns which services need a restart for it."""
        self.step = "units"
        changed_units = units.apply(self.host, units.render(self.host, self.boot.host_dir, infra, self.boot.region))
        self.changed += [f"unit:{u}" for u in changed_units]
        return {"luxd": "luxd.service" in changed_units, "cloudflared": "cloudflared.service" in changed_units}

    def luxd(self, infra: dict, want: desired_mod.Desired, creds: dict, restart: dict) -> str:
        """luxd.toml, the version switch and the luxd restart. Returns the
        installed version ("" if none). A failed switch is kept in
        self.deferred_error so the remaining steps still run."""
        host = self.host
        self.step = "luxd.toml"
        ip = self.environ.get("LUX_RUNNER_IP") or luxdconf.primary_ip()
        toml = luxdconf.render(infra, self.boot.region, want, creds, ip, host.paths.runner_bin_dir)
        installed = release.installed_version(host.paths.install_root)
        deploying = want.lux_version is not None and want.lux_version != installed
        if write_if_changed(os.path.join(host.paths.etc_lux, "luxd.toml"), toml, 0o600):
            self.changed.append("luxd.toml")
            restart["luxd"] = True

        if deploying:
            self.step = "version"
            try:
                release.deploy(
                    host, want.lux_version, want.release_base_url, creds["migrate_dsn"],
                    f"http://127.0.0.1:{infra['luxd_port']}/health",
                )
                self.changed.append(f"version:{want.lux_version}")
                installed = want.lux_version
            except HostError as e:
                # The previous version is still in place: keep the tunnel
                # and timers reconciled, and fail the run at the end.
                self.deferred_error = HostError(f"version: {e}")
        # A successful deploy has restarted luxd on the new config already.
        if not deploying or self.deferred_error:
            if restart["luxd"] and installed and release.is_luxd_active(host):
                host.run(["systemctl", "restart", "luxd"])
                self.changed.append("luxd-restarted")
        return installed

    def enable(self, installed: str) -> None:
        self.step = "enable"
        if installed and units.enable_now(self.host, "luxd.service"):
            self.changed.append("enable:luxd")
        for timer in units.TIMERS:
            if units.enable_now(self.host, timer):
                self.changed.append(f"enable:{timer}")

    def cloudflared(self, infra: dict, restart: dict) -> None:
        host = self.host
        self.step = "cloudflared"
        token = get_secure_parameter(host, infra["tunnel_token_parameter"], self.boot.region)
        if not token.strip():
            raise HostError(f"{infra['tunnel_token_parameter']} is empty")
        token_file = os.path.join(host.paths.cloudflared_dir, "token")
        if write_if_changed(token_file, token.strip() + "\n", 0o600):
            self.changed.append("cloudflared-token")
            restart["cloudflared"] = True
        if units.enable_now(host, "cloudflared.service"):
            self.changed.append("enable:cloudflared")
        elif restart["cloudflared"]:
            host.run(["systemctl", "restart", "cloudflared"])
            self.changed.append("cloudflared-restarted")

    def run(self) -> str:
        self.step = "ssm"
        infra = load_infra(self.host, self.boot)
        self.sync_checkout(infra)
        want = self.load_desired()
        self.remove_legacy()
        self.packages()
        creds = self.postgres(infra)
        restart = self.write_units(infra)
        installed = self.luxd(infra, want, creds, restart)
        self.enable(installed)
        self.cloudflared(infra, restart)
        if self.deferred_error:
            self.step = "version"
            raise self.deferred_error
        return installed or "none"


def summary(status: str, version: str, changed: list, error: str = "") -> str:
    line = f"lux-reconcile: status={status} version={version} changed=[{','.join(changed)}]"
    return f"{line} error={error!r}" if error else line


def main(host: Host | None = None, bootstrap_path: str | None = None,
         reexec=os.execve, environ=None) -> int:
    host = host or Host()
    environ = dict(os.environ if environ is None else environ)
    bootstrap_path = bootstrap_path or environ.get("LUX_HOST_BOOTSTRAP", BOOTSTRAP_PATH)
    os.makedirs(host.paths.etc_lux, exist_ok=True)
    with open(os.path.join(host.paths.etc_lux, ".reconcile.lock"), "w") as lock:
        # Blocks while a previous run (timer or by hand) is still going.
        fcntl.flock(lock, fcntl.LOCK_EX)

        def reexec_unlocked(*args):
            # The new process takes the lock itself.
            fcntl.flock(lock, fcntl.LOCK_UN)
            reexec(*args)

        run = None
        try:
            boot = load_bootstrap(bootstrap_path)
            run = Run(host, boot, reexec_unlocked, environ)
            version = run.run()
        except Exception as e:  # noqa: BLE001 - every failure ends in the summary line
            installed = release.installed_version(host.paths.install_root) or "none"
            step = run.step if run else "bootstrap"
            detail = str(e) if isinstance(e, HostError) else repr(e)
            print(summary("error", installed, run.changed if run else [], f"{step}: {detail}"), flush=True)
            return 1
    print(summary("ok", version, run.changed), flush=True)
    return 0

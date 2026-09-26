"""Fakes for everything the reconciler shells out to, and a host rooted in
a temporary directory.

`aws`, `systemctl`, `dpkg`, `apt-get`, the Postgres tools, mount/blkid/mkfs
and `luxd migrate` are answered by FakeSh from in-memory state. `git` runs for real, against
bare repositories in the temporary directory: the reconciler's checkout
logic (fetch, hard reset, detecting a host/ change) is what is under test,
and a local remote needs no network.
"""
import dataclasses
import hashlib
import io
import json
import os
import subprocess
import sys
import tarfile
import urllib.error

import pytest

HOST_DIR = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
sys.path.insert(0, HOST_DIR)

from luxhost import release  # noqa: E402
from luxhost.host import Host, Paths  # noqa: E402

PREFIX = "/lux"
REGION = "eu-north-1"
VOLUME = "vol-0123456789abcdef0"
INSTALLED = "install ok installed"


def completed(argv, rc=0, stdout="", stderr=""):
    return subprocess.CompletedProcess(argv, rc, stdout=stdout, stderr=stderr)


@dataclasses.dataclass
class FakeSh:
    root: str
    ssm: dict = dataclasses.field(default_factory=dict)
    active: set = dataclasses.field(default_factory=set)
    enabled: set = dataclasses.field(default_factory=set)
    databases: set = dataclasses.field(default_factory=lambda: {"postgres"})
    mounted: bool = False
    has_filesystem: bool = False
    migrate_rc: int = 0
    # Whether luxd comes up after a restart, by the version `current` points at.
    healthy_versions: set = dataclasses.field(default_factory=set)
    calls: list = dataclasses.field(default_factory=list)
    # What `dpkg -s` reports as Status: per package; a fresh host has none
    # of the reconciler's packages.
    dpkg_status: dict = dataclasses.field(default_factory=dict)
    # Packages (or "update") whose apt-get run exits 100.
    apt_fail: set = dataclasses.field(default_factory=set)
    # Packages whose apt-get install exits 100 after unpacking, leaving
    # them half-configured (a failed postinst).
    apt_half_configure: set = dataclasses.field(default_factory=set)
    # How many `apt-get update` calls find the lists lock held first.
    apt_lists_locked: int = 0
    arch: str = "arm64"

    def __call__(self, argv, input=None, env=None, **kwargs):
        argv = list(argv)
        if argv[0] == "git":
            return subprocess.run(argv, input=input, env=env, capture_output=True, text=True, check=False)
        self.calls.append(argv)
        cmd = os.path.basename(argv[0])
        handler = getattr(self, "_" + cmd.replace("-", "_").replace(".", "_"), None)
        if cmd == "luxd":
            return completed(argv, self.migrate_rc, stderr="" if self.migrate_rc == 0 else "migration 7 failed")
        if handler is None:
            raise AssertionError(f"unexpected command: {argv}")
        if cmd == "apt-get":
            return handler(argv, input, env)
        return handler(argv, input)

    def commands(self, name):
        return [c for c in self.calls if os.path.basename(c[0]) == name]

    def _aws(self, argv, _input):
        region = argv[argv.index("--region") + 1]
        assert region == REGION
        if argv[1:3] == ["ssm", "get-parameters"]:
            names = argv[argv.index("--names") + 1:argv.index("--region")]
            found = [{"Name": n, "Value": self.ssm[n]} for n in names if n in self.ssm]
            return completed(argv, stdout=json.dumps({"Parameters": found}))
        if argv[1:3] == ["ssm", "get-parameter"]:
            name = argv[argv.index("--name") + 1]
            if name not in self.ssm:
                return completed(argv, 254, stderr="ParameterNotFound")
            return completed(argv, stdout=self.ssm[name] + "\n")
        raise AssertionError(f"unexpected aws call: {argv}")

    def _systemctl(self, argv, _input):
        args = [a for a in argv[1:] if not a.startswith("--")]
        verb, units = args[0], [u.removesuffix(".service") for u in args[1:]]
        if verb == "is-active":
            ok = units[0] in self.active
            return completed(argv, 0 if ok else 3, stdout="active\n" if ok else "inactive\n")
        if verb == "is-enabled":
            return completed(argv, 0 if units[0] in self.enabled else 1)
        if verb == "enable":
            self.enabled.update(units)
            if "--now" in argv:
                for u in units:
                    self._start(u)
            return completed(argv)
        if verb == "disable":
            self.enabled.difference_update(units)
            self.active.difference_update(units)
            return completed(argv)
        if verb in ("restart", "start"):
            self._start(units[0])
            return completed(argv)
        if verb == "stop":
            self.active.difference_update(units)
            return completed(argv)
        if verb == "daemon-reload":
            return completed(argv)
        raise AssertionError(f"unexpected systemctl call: {argv}")

    def _start(self, unit):
        if unit != "luxd":
            self.active.add(unit)
            return
        current = os.path.join(self.root, "usr/local/lux/current")
        version = os.path.basename(os.readlink(current)) if os.path.islink(current) else None
        if version in self.healthy_versions:
            self.active.add("luxd")
        else:
            self.active.discard("luxd")

    def _dpkg(self, argv, _input):
        if argv[1:] == ["--print-architecture"]:
            return completed(argv, stdout=self.arch + "\n")
        if argv[1] == "-s":
            if argv[2] in self.dpkg_status:
                return completed(argv, stdout=f"Package: {argv[2]}\nStatus: {self.dpkg_status[argv[2]]}\n")
            return completed(argv, 1, stderr=f"dpkg-query: package '{argv[2]}' is not installed")
        raise AssertionError(f"unexpected dpkg call: {argv}")

    def _apt_get(self, argv, _input, env=None):
        assert argv[1:3] == ["-o", "DPkg::Lock::Timeout=300"], argv
        args = argv[3:]
        if args == ["update"]:
            if self.apt_lists_locked:
                self.apt_lists_locked -= 1
                return completed(argv, 100, stderr="E: Could not get lock /var/lib/apt/lists/lock")
            pgdg = os.path.join(self.root, "etc/apt/sources.list.d/pgdg.list")
            assert os.path.exists(pgdg), "apt-get update before the PGDG source is written"
            return completed(argv, 100 if "update" in self.apt_fail else 0)
        assert args[:2] == ["install", "-y"] and len(args) == 3, argv
        assert (env or {}).get("DEBIAN_FRONTEND") == "noninteractive", "apt-get install may prompt"
        target = args[2]
        if target.endswith(".deb"):
            with open(target, "rb") as f:
                assert f.read() == FakeWeb.CLOUDFLARED_DEB
            package = "cloudflared"
        else:
            package = target
        if package in self.apt_fail:
            return completed(argv, 100, stderr=f"E: Unable to install {package}")
        if package in self.apt_half_configure:
            self.dpkg_status[package] = "install ok half-configured"
            return completed(argv, 100, stderr="E: Sub-process /usr/bin/dpkg returned an error code (1)")
        self.dpkg_status[package] = INSTALLED
        if package == "cloudflared":
            # The package's postinst links /usr/bin/cloudflared here.
            path = os.path.join(self.root, "usr/local/bin/cloudflared")
            os.makedirs(os.path.dirname(path), exist_ok=True)
            with open(path, "w") as f:
                f.write("#!/bin/sh\n")
            os.chmod(path, 0o755)
        return completed(argv)

    def _mountpoint(self, argv, _input):
        return completed(argv, 0 if self.mounted else 1)

    def _blkid(self, argv, _input):
        return completed(argv, 0 if self.has_filesystem else 2)

    def _mkfs_ext4(self, argv, _input):
        assert not self.has_filesystem, "formatted a volume that already has a filesystem"
        self.has_filesystem = True
        return completed(argv)

    def _mount(self, argv, _input):
        self.mounted = True
        return completed(argv)

    def _succeed(self, argv, _input):
        return completed(argv)

    _pg_dropcluster = _chown = _succeed

    def _pg_createcluster(self, argv, _input):
        # Like the real one: refuses a data directory that already holds a
        # cluster without its postgresql.conf (PGDG keeps that in /etc).
        datadir = argv[argv.index("-d") + 1]
        marker = os.path.join(datadir, "PG_VERSION")
        if os.path.exists(marker) and not os.path.exists(os.path.join(datadir, "postgresql.conf")):
            return completed(argv, 1, stderr="Error: move_conffile: required configuration file does not exist")
        os.makedirs(datadir, exist_ok=True)
        with open(marker, "w") as f:
            f.write("18\n")
        if "--start" in argv:
            self.active.add(f"postgresql@{argv[1]}-{argv[2]}")
        return completed(argv)

    def _runuser(self, argv, input):
        inner = argv[argv.index("--") + 1:]
        if inner[0] == "createdb":
            self.databases.add(inner[1])
            return completed(argv)
        if inner[0] == "psql":
            if input.startswith("SELECT 1 FROM pg_database"):
                name = input.split("'")[1]
                return completed(argv, stdout="1\n" if name in self.databases else "")
            if input.startswith("ALTER USER postgres"):
                return completed(argv)
        raise AssertionError(f"unexpected runuser call: {argv} {input!r}")


class FakeResponse(io.BytesIO):
    def __init__(self, body: bytes, status: int = 200):
        super().__init__(body)
        self.status = status


def make_release(version: str, luxd_body: str | None = None) -> dict:
    """A release as urlopen would serve it: {filename: bytes}."""
    buf = io.BytesIO()
    with tarfile.open(fileobj=buf, mode="w:gz") as tf:
        for name, body in [("bin/luxd", luxd_body or f"luxd {version}"), ("bin/lux", f"lux {version}")]:
            data = body.encode()
            info = tarfile.TarInfo(name)
            info.size, info.mode = len(data), 0o755
            tf.addfile(info, io.BytesIO(data))
        for arch in release.RUNNER_ARCHES:
            info = tarfile.TarInfo(f"lib/lux/runner/{arch}")
            info.type, info.mode = tarfile.DIRTYPE, 0o755
            tf.addfile(info)
    tarball = f"lux_{version}_linux_{release.arch()}.tar.gz"
    body = buf.getvalue()
    return {tarball: body, "SHA256SUMS": f"{hashlib.sha256(body).hexdigest()}  {tarball}\n".encode()}


@dataclasses.dataclass
class FakeWeb:
    """urlopen: releases under BASE_URL/<version>/, luxd's /health, the
    PGDG signing key and the cloudflared .deb."""
    sh: FakeSh
    releases: dict = dataclasses.field(default_factory=dict)
    fetched: list = dataclasses.field(default_factory=list)

    BASE_URL = "https://releases.example.com/lux"
    PGDG_KEY = b"-----BEGIN PGP PUBLIC KEY BLOCK-----\npgdg\n-----END PGP PUBLIC KEY BLOCK-----\n"
    CLOUDFLARED_DEB = b"!<arch>\ncloudflared"
    # The PGDG key and the cloudflared .deb, apart from the releases.
    package_fetched: list = dataclasses.field(default_factory=list)
    # URLs answering 503 instead.
    failing: set = dataclasses.field(default_factory=set)

    def __call__(self, url, timeout=None):
        if url in self.failing:
            raise urllib.error.HTTPError(url, 503, "Service Unavailable", {}, None)
        if url == "https://www.postgresql.org/media/keys/ACCC4CF8.asc":
            self.package_fetched.append(url)
            return FakeResponse(self.PGDG_KEY)
        if url == f"https://github.com/cloudflare/cloudflared/releases/latest/download/cloudflared-linux-{self.sh.arch}.deb":
            self.package_fetched.append(url)
            return FakeResponse(self.CLOUDFLARED_DEB)
        if url.endswith("/health"):
            if "luxd" in self.sh.active:
                return FakeResponse(b'{"status":"ok"}')
            raise urllib.error.URLError("connection refused")
        self.fetched.append(url)
        version, name = url.removeprefix(self.BASE_URL + "/").split("/", 1)
        try:
            return FakeResponse(self.releases[version][name])
        except KeyError:
            raise urllib.error.HTTPError(url, 404, "Not Found", {}, None) from None


def git(*args, cwd=None):
    out = subprocess.run(["git", *args], cwd=cwd, capture_output=True, text=True, check=False)
    assert out.returncode == 0, out.stderr
    return out.stdout.strip()


class ConfigRepo:
    """A bare 'origin' plus a working clone to commit to it from."""

    def __init__(self, base: str, host_src: str, config_repo_path: str = ""):
        # Where host/ sits in the repo: config_repo_path/host.
        self.host_rel = os.path.join(config_repo_path, "host")
        self.bare = os.path.join(base, "origin.git")
        self.work = os.path.join(base, "work")
        git("init", "-q", "--bare", "-b", "main", self.bare)
        git("clone", "-q", self.bare, self.work)
        git("config", "user.email", "ops@example.com", cwd=self.work)
        git("config", "user.name", "ops", cwd=self.work)
        # The real host code, so a re-exec runs a working reconciler.
        host_dir = os.path.join(self.work, self.host_rel)
        os.makedirs(os.path.dirname(host_dir), exist_ok=True)
        subprocess.run(["cp", "-R", host_src, host_dir], check=True)
        subprocess.run(["rm", "-rf", os.path.join(host_dir, "tests")], check=True)
        for dirpath, dirnames, _ in os.walk(host_dir):
            for d in list(dirnames):
                if d in ("__pycache__", ".pytest_cache"):
                    subprocess.run(["rm", "-rf", os.path.join(dirpath, d)], check=True)
        self.commit("initial")

    def write(self, rel: str, content: str):
        path = os.path.join(self.work, rel)
        os.makedirs(os.path.dirname(path), exist_ok=True)
        with open(path, "w") as f:
            f.write(content)

    def commit(self, msg: str):
        git("add", "-A", cwd=self.work)
        git("commit", "-q", "--allow-empty", "-m", msg, cwd=self.work)
        git("push", "-q", "origin", "HEAD:main", cwd=self.work)

    def set_desired(self, text: str):
        self.write(f"{self.host_rel}/lux-host.toml", text)
        self.commit("desired state")


@dataclasses.dataclass
class Env:
    root: str
    sh: FakeSh
    web: FakeWeb
    host: Host
    repo: ConfigRepo
    checkout: str
    bootstrap: str
    reexecs: list

    def path(self, rel: str) -> str:
        return os.path.join(self.root, rel)

    def run(self):
        from luxhost.reconcile import main

        def reexec(exe, argv, env):
            self.reexecs.append(argv)
            # Run the new code in-process, as execve would have.
            raise SystemExit(main(host=self.host, bootstrap_path=self.bootstrap, reexec=reexec, environ=env))

        try:
            return main(host=self.host, bootstrap_path=self.bootstrap, reexec=reexec,
                        environ={"LUX_RUNNER_IP": "10.60.0.10"})
        except SystemExit as e:
            return e.code


def desired(version: str = "none", extra: str = "") -> str:
    return (
        f'lux_version = "{version}"\n'
        f'release_base_url = "{FakeWeb.BASE_URL}"\n'
        f"{extra}"
    )


@pytest.fixture
def env(tmp_path, request):
    # Indirect parametrisation sets the bootstrap's config_repo_path.
    config_repo_path = getattr(request, "param", "")
    root = str(tmp_path / "root")
    os.makedirs(root)
    paths = Paths.under(root)
    for d in ("etc", paths.dev_by_id, paths.root_home):
        os.makedirs(os.path.join(root, d) if not os.path.isabs(d) else d, exist_ok=True)
    open(os.path.join(paths.dev_by_id, "nvme-Amazon_Elastic_Block_Store_" + VOLUME.replace("-", "")), "w").close()

    repo = ConfigRepo(str(tmp_path), HOST_DIR, config_repo_path)
    repo.set_desired(desired())
    checkout = os.path.join(root, "var/lib/lux/config")
    git("clone", "-q", "-b", "main", repo.bare, checkout)

    sh = FakeSh(root=root)
    sh.ssm.update({
        f"{PREFIX}/public_url": "https://lux.example.com",
        f"{PREFIX}/blob_bucket": "lux-blobs-123456789012-eu-north-1",
        f"{PREFIX}/backup_bucket": "lux-pg-backups-123456789012-eu-north-1",
        f"{PREFIX}/db_name": "lux",
        f"{PREFIX}/luxd_port": "7070",
        f"{PREFIX}/pg_data_volume_id": VOLUME,
        f"{PREFIX}/tunnel_token_parameter": f"{PREFIX}/cloudflare-tunnel-token",
        f"{PREFIX}/cloudflare-tunnel-token": "tunnel-token",
        f"{PREFIX}/config_repo_url": repo.bare,
        f"{PREFIX}/config_repo_ref": "main",
        f"{PREFIX}/cf_access_team": "acme",
        f"{PREFIX}/cf_access_aud": "aud-tag",
    })
    web = FakeWeb(sh)
    clock = [0.0]
    host = Host(
        sh=sh, paths=paths, urlopen=web, log=lambda m: None,
        sleep=lambda s: clock.__setitem__(0, clock[0] + s), now=lambda: clock[0],
    )
    bootstrap = os.path.join(paths.etc_lux, "host.json")
    os.makedirs(paths.etc_lux, exist_ok=True)
    with open(bootstrap, "w") as f:
        json.dump({"region": REGION, "ssm_prefix": PREFIX, "checkout": checkout, "config_repo_path": config_repo_path}, f)
    return Env(root=root, sh=sh, web=web, host=host, repo=repo, checkout=checkout, bootstrap=bootstrap, reexecs=[])

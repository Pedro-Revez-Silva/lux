"""TestEnvironment — one isolated lux environment per invocation.

Shared, long-lived: a Postgres and a MinIO container. Per run: a database, a
bucket, a Docker network, N simulated hosts (privileged Podman containers,
each with its own storage), and `luxd` on the developer machine listening on
the network's gateway address, which the hosts, the tests and presigned MinIO
URLs can all reach.

Everything a test needs is written to `<log dir>/env.json`; the pytest
subprocess reads it back with `TestEnvironment.load()`.
"""

from __future__ import annotations

import json
import os
import socket
import subprocess
import time
import uuid
from concurrent.futures import ThreadPoolExecutor
from dataclasses import asdict, dataclass, field
from pathlib import Path

import psycopg
import requests

REPO_ROOT = Path(__file__).resolve().parent.parent
BIN_DIR = REPO_ROOT / "bin"

PODMAN_IMAGE = "quay.io/podman/stable:v5.8.7"
POSTGRES_IMAGE = "postgres:18.6-alpine"
# MinIO stopped publishing community images after this release.
MINIO_IMAGE = "quay.io/minio/minio:RELEASE.2025-09-07T16-13-09Z"

# Versioned, so upgrading the image does not reuse a data directory from an
# older major version.
PG_CONTAINER = "lux-e2e-postgres-18"
PG_PORT = int(os.environ.get("LUX_TEST_PG_PORT", "55432"))
PG_OWNER = "lux"
PG_OWNER_PASSWORD = "lux"
# Created by `luxd migrate`; it has neither SUPERUSER nor BYPASSRLS, so
# row-level security is a real boundary in every test.
PG_APP_USER = "lux_app"
PG_APP_PASSWORD = "lux_app"

MINIO_CONTAINER = "lux-e2e-minio"
MINIO_PORT = int(os.environ.get("LUX_TEST_MINIO_PORT", "59000"))
MINIO_USER = "luxminio"
MINIO_PASSWORD = "luxminio-secret"

# Images every host has preloaded, so no test waits on a registry.
ALPINE_IMAGE = "docker.io/library/alpine:3.24.2"
PRELOAD_IMAGES = [ALPINE_IMAGE]

LABEL = "lux-e2e"


def sh(*args: str, check: bool = True, input: bytes | None = None, capture: bool = True) -> str:
    result = subprocess.run(args, input=input, capture_output=capture)
    if check and result.returncode != 0:
        err = result.stderr.decode(errors="replace") if capture else ""
        raise RuntimeError(f"{' '.join(args)} failed ({result.returncode}): {err}")
    return result.stdout.decode(errors="replace") if capture else ""


def free_port() -> int:
    with socket.socket() as s:
        s.bind(("", 0))
        return int(s.getsockname()[1])


def container_running(name: str) -> bool:
    out = sh("docker", "inspect", "-f", "{{.State.Running}}", name, check=False)
    return out.strip() == "true"


def wait_until(fn, timeout: float = 30, interval: float = 0.3, message: str = "condition not met"):
    """Poll fn until it returns something truthy; return that, or fail."""
    deadline = time.time() + timeout
    last = None
    while time.time() < deadline:
        last = fn()
        if last:
            return last
        time.sleep(interval)
    raise AssertionError(f"{message} (after {timeout}s; last: {last!r})")


def docker_hub_ref(ref: str) -> str:
    """The full reference of an image as Docker names it: alpine:3 is
    docker.io/library/alpine:3, acme/tool is docker.io/acme/tool."""
    first = ref.split("/", 1)[0]
    if "/" in ref and ("." in first or ":" in first or first == "localhost"):
        return ref
    return ("docker.io/" if "/" in ref else "docker.io/library/") + ref


def build_test_image(name: str) -> str:
    """Builds tests/images/<name> as localhost/lux-<name>:test."""
    ctx = Path(__file__).parent / "images" / name
    tag = f"localhost/lux-{name}:test"
    sh("docker", "build", "-q", "-t", tag, "-f", str(ctx / "Containerfile"), str(ctx))
    return tag


def start_service(env, name: str, image: str, **envs: str) -> str:
    """Runs a test service on the run network; returns its address. It is
    labelled with the run, so teardown removes it with everything else."""
    args = ["docker", "run", "-d", "--name", name, "--label", f"lux-e2e-run={env.run_id}", "--network", env.network]
    for k, v in envs.items():
        args += ["-e", f"{k}={v}"]
    sh(*args, image)
    return json.loads(sh("docker", "inspect", name))[0]["NetworkSettings"]["Networks"][env.network]["IPAddress"]


def host_names(n: int) -> list[str]:
    return [f"host-{chr(ord('a') + i)}" for i in range(n)]


@dataclass
class Host:
    """A simulated machine: a privileged Podman container on the run network."""

    name: str
    container: str
    ip: str
    log_dir: str

    def exec(self, *cmd: str, check: bool = True, input: bytes | None = None) -> str:
        return sh("docker", "exec", "-i", self.container, *cmd, check=check, input=input)

    def podman(self, *args: str, check: bool = True) -> str:
        return self.exec("podman", *args, check=check)

    def start_runner(self, env: "TestEnvironment", token: str, *extra: str, name: str | None = None,
                     url: str | None = None) -> subprocess.Popen:
        """Start lux-runner inside this host, logging to <log dir>/<host>/runner.log."""
        binary = BIN_DIR / "lux-runner"
        if not binary.exists():
            raise RuntimeError("lux-runner is not built")
        Path(self.log_dir).mkdir(parents=True, exist_ok=True)
        log = open(Path(self.log_dir) / "runner.log", "ab")
        return subprocess.Popen(
            [
                "docker", "exec", "-i",
                "-e", f"LUX_URL={url or env.luxd_url}",
                "-e", f"LUX_HOST_TOKEN={token}",
                self.container,
                # The host image has no pkill: record the pid to stop it by.
                "sh", "-c", 'echo $$ > /run/lux-runner.pid; exec "$@"', "lux-runner",
                "/opt/lux/lux-runner",
                "--name", name or self.name,
                "--data-dir", "/var/lib/lux",
                "--shim", "/opt/lux/lux-shim",
                *extra,
            ],
            stdout=log,
            stderr=subprocess.STDOUT,
            start_new_session=True,
        )

    def insecure_registry(self, address: str) -> None:
        """Let Podman here use a plain-HTTP registry (the tests' own).
        Production registries are HTTPS and need none of this."""
        name = address.replace(":", "_")
        self.exec("sh", "-c", f"mkdir -p /etc/containers/registries.conf.d && printf '[[registry]]\\nlocation = \"{address}\"\\ninsecure = true\\n' "
                  f"> /etc/containers/registries.conf.d/50-lux-test-{name}.conf")

    def running(self, *argv: str) -> bool:
        """Whether a process with exactly this command line runs here."""
        want = "\x00".join(argv) + "\x00"
        return want in self.exec("sh", "-c", "cat /proc/[0-9]*/cmdline 2>/dev/null; true")

    def runner_pid(self) -> str:
        pid = self.exec("cat", "/run/lux-runner.pid", check=False).strip()
        if pid and self.exec("sh", "-c", f"kill -0 {pid} 2>/dev/null && echo alive", check=False).strip() == "alive":
            return pid
        return ""

    def stop_runner(self, signal: str = "TERM") -> None:
        pid = self.runner_pid()
        if not pid:
            return
        self.exec("kill", f"-{signal}", pid, check=False)
        deadline = time.time() + 15
        while time.time() < deadline:
            if not self.runner_pid():
                return
            time.sleep(0.2)
        raise RuntimeError(f"lux-runner on {self.name} did not stop")

    def kill(self) -> None:
        """Simulate losing the machine: it stops, with everything on it."""
        sh("docker", "kill", self.container, check=False)

    def pause(self) -> None:
        sh("docker", "pause", self.container, check=False)

    def unpause(self) -> None:
        sh("docker", "unpause", self.container, check=False)


# luxd processes this harness started, by pid: stop_luxd waits on them.
_luxd_procs: dict[int, subprocess.Popen] = {}


@dataclass
class TestEnvironment:
    __test__ = False  # not a pytest test class

    run_id: str = field(default_factory=lambda: uuid.uuid4().hex[:8])
    n_hosts: int = 2
    log_dir: str = ""
    network: str = ""
    gateway: str = ""
    subnet: str = ""
    luxd_port: int = 0
    luxd_url: str = ""
    db_name: str = ""
    bucket: str = ""
    hosts: list[Host] = field(default_factory=list)
    binaries: dict[str, str | None] = field(default_factory=dict)
    fake_image: str | None = None
    extra: dict = field(default_factory=dict)
    # Set by the dev environment (serve.py): a tenant and its keys.
    tenant_id: str = ""
    api_key: str = ""
    admin_key: str = ""

    def __post_init__(self) -> None:
        if not self.log_dir:
            self.log_dir = str(Path(os.environ.get("LUX_TEST_LOG_ROOT", "/tmp")) / f"lux-e2e-{self.run_id}")
        self.network = self.network or f"lux-e2e-{self.run_id}"
        self.db_name = self.db_name or f"lux_test_{self.run_id}"
        self.bucket = self.bucket or f"lux-test-{self.run_id}"

    # -- DSNs and URLs ------------------------------------------------------

    def _dsn(self, user: str, password: str, db: str) -> str:
        return f"postgres://{user}:{password}@127.0.0.1:{PG_PORT}/{db}?sslmode=disable"

    @property
    def owner_dsn(self) -> str:
        return self._dsn(PG_OWNER, PG_OWNER_PASSWORD, self.db_name)

    @property
    def app_dsn(self) -> str:
        return self._dsn(PG_APP_USER, PG_APP_PASSWORD, self.db_name)

    @property
    def s3_endpoint(self) -> str:
        # The gateway address, so presigned URLs signed for it work from the
        # hosts as well as from here.
        return f"http://{self.gateway}:{MINIO_PORT}"

    @property
    def data_dir(self) -> str:
        return str(Path(self.log_dir) / "luxd-data")

    def luxd_env(self) -> dict[str, str]:
        return {
            "LUX_DATABASE_URL": self.app_dsn,
            "LUX_LISTEN": f"{self.gateway}:{self.luxd_port}",
            "LUX_PUBLIC_URL": self.luxd_url,
            "LUX_DATA_DIR": self.data_dir,
            "LUX_S3_ENDPOINT": self.s3_endpoint,
            "LUX_S3_BUCKET": self.bucket,
            "LUX_S3_ACCESS_KEY": MINIO_USER,
            "LUX_S3_SECRET_KEY": MINIO_PASSWORD,
            "LUX_S3_REGION": "us-east-1",
            # Never the developer's AWS account: the EC2 provider gets only
            # what a test passes it (the fake's endpoint, or --real-ec2).
            **({} if os.environ.get("LUX_TEST_REAL_EC2") else {
                "AWS_CONFIG_FILE": "/dev/null", "AWS_SHARED_CREDENTIALS_FILE": "/dev/null",
                "AWS_EC2_METADATA_DISABLED": "true", "LUX_EC2_ENDPOINT": "http://127.0.0.1:9",
                "AWS_ACCESS_KEY_ID": "", "AWS_SECRET_ACCESS_KEY": "", "AWS_SESSION_TOKEN": "", "AWS_PROFILE": ""}),
            # History fills in seconds, not minutes.
            "LUX_SAMPLE_EVERY": "2s",
            # A 10s lease: heartbeats every ~3s (lease/3), and a host or a
            # Run's secrets are given up in seconds, not a production 30s.
            "LUX_LEASE": "10s",
        }

    # -- setup --------------------------------------------------------------

    def setup(self, fake_image: str | None, luxd: bool = True) -> None:
        Path(self.log_dir).mkdir(parents=True, exist_ok=True)
        self.fake_image = fake_image
        self._shared_services()
        self._network()
        self._database()
        self._bucket()
        self._hosts()
        self.luxd_port = free_port()
        self.luxd_url = f"http://{self.gateway}:{self.luxd_port}"
        if luxd and self.binaries.get("luxd"):
            self._migrate()
            self.start_luxd()
        self.save()

    def hand_over_luxd(self) -> None:
        """Stops the luxd setup started, so the process that runs the tests
        starts its own (conftest's env fixture) and owns it: waiting on its
        own child is instant, polling another's zombie takes 10s."""
        self.stop_luxd()

    def _shared_services(self) -> None:
        if not container_running(PG_CONTAINER):
            sh("docker", "rm", "-f", PG_CONTAINER, check=False)
            sh(
                "docker", "run", "-d", "--name", PG_CONTAINER, "--label", LABEL,
                "-p", f"0.0.0.0:{PG_PORT}:5432",
                "-e", f"POSTGRES_USER={PG_OWNER}", "-e", f"POSTGRES_PASSWORD={PG_OWNER_PASSWORD}",
                POSTGRES_IMAGE, "-c", "max_connections=500",
            )
        if not container_running(MINIO_CONTAINER):
            sh("docker", "rm", "-f", MINIO_CONTAINER, check=False)
            sh(
                "docker", "run", "-d", "--name", MINIO_CONTAINER, "--label", LABEL,
                "-p", f"0.0.0.0:{MINIO_PORT}:9000",
                "-e", f"MINIO_ROOT_USER={MINIO_USER}", "-e", f"MINIO_ROOT_PASSWORD={MINIO_PASSWORD}",
                MINIO_IMAGE, "server", "/data",
            )
        deadline = time.time() + 60
        while time.time() < deadline:
            try:
                with psycopg.connect(self._dsn(PG_OWNER, PG_OWNER_PASSWORD, "postgres"), connect_timeout=2):
                    pass
                if requests.get(f"http://127.0.0.1:{MINIO_PORT}/minio/health/live", timeout=2).ok:
                    return
            except Exception:  # noqa: BLE001 - still starting
                pass
            time.sleep(0.5)
        raise RuntimeError("shared Postgres/MinIO did not become ready")

    def _network(self) -> None:
        sh("docker", "network", "create", "--label", LABEL, self.network)
        info = json.loads(sh("docker", "network", "inspect", self.network))[0]
        cfg = info["IPAM"]["Config"][0]
        self.subnet = cfg["Subnet"]
        self.gateway = cfg["Gateway"]

    def _database(self) -> None:
        with psycopg.connect(self._dsn(PG_OWNER, PG_OWNER_PASSWORD, "postgres"), autocommit=True) as conn:
            conn.execute(f'CREATE DATABASE "{self.db_name}" OWNER {PG_OWNER}')

    def s3(self):
        import boto3

        return boto3.client(
            "s3",
            endpoint_url=f"http://127.0.0.1:{MINIO_PORT}",
            aws_access_key_id=MINIO_USER,
            aws_secret_access_key=MINIO_PASSWORD,
            region_name="us-east-1",
        )

    def _bucket(self) -> None:
        self.s3().create_bucket(Bucket=self.bucket)

    def _image_tar(self) -> Path:
        tar = Path(self.log_dir) / "images.tar"
        images = list(PRELOAD_IMAGES)
        if self.fake_image:
            images.append(self.fake_image)
        images += [i for i in self.extra.get("images", {}).values() if i]
        sh("docker", "save", "-o", str(tar), *images)
        return tar

    def _hosts(self) -> None:
        tar = self._image_tar()
        names = host_names(self.n_hosts)
        with ThreadPoolExecutor(max_workers=len(names)) as pool:
            self.hosts = list(pool.map(lambda n: self.add_host(n, tar), names))

    def add_host(self, name: str, image_tar: Path | None = None) -> Host:
        """Start one simulated host. Also used by the fake EC2 provider."""
        container = f"lux-e2e-{self.run_id}-{name}"
        BIN_DIR.mkdir(exist_ok=True)
        sh(
            "docker", "run", "-d", "--privileged", "--name", container, "--hostname", name,
            "--label", LABEL, "--label", f"lux-e2e-run={self.run_id}",
            "--network", self.network,
            "-v", f"{BIN_DIR}:/opt/lux:ro",
            PODMAN_IMAGE, "sleep", "infinity",
        )
        host_ip = json.loads(sh("docker", "inspect", container))[0]["NetworkSettings"]["Networks"][self.network]["IPAddress"]
        host = Host(name=name, container=container, ip=host_ip, log_dir=str(Path(self.log_dir) / name))
        # --userns=auto allocates from the `containers` range; without it
        # every container creation fails.
        host.exec("sh", "-c",
                  "echo containers:2147483647:2147483648 >> /etc/subuid && "
                  "echo containers:2147483647:2147483648 >> /etc/subgid")
        # Native overlay, as on a real host. The image's default,
        # fuse-overlayfs, cannot give a build's RUN steps (in their own user
        # namespace) write access to the image's root directory.
        host.exec("sh", "-c",
                  "printf '[storage]\\ndriver = \"overlay\"\\nrunroot = \"/run/containers/storage\"\\n"
                  "graphroot = \"/var/lib/containers/storage\"\\n' > /etc/containers/storage.conf")
        tar = image_tar or Path(self.log_dir) / "images.tar"
        with open(tar, "rb") as f:
            subprocess.run(["docker", "exec", "-i", container, "podman", "load", "-q"],
                           stdin=f, check=True, capture_output=True)
        # docker save records Docker Hub images by short name (alpine:3.24.2),
        # which podman loads as localhost/alpine; tag them with the full
        # references specs use, so nothing ever reaches a registry.
        for ref in [*PRELOAD_IMAGES, *self.extra.get("images", {}).values()]:
            if not ref:
                continue
            full = docker_hub_ref(ref)
            short = full.removeprefix("docker.io/library/").removeprefix("docker.io/")
            if short != full:
                host.exec("sh", "-c", f"podman image exists {full} || podman tag localhost/{short} {full}")
        return host

    def _migrate(self) -> None:
        result = subprocess.run(
            [str(BIN_DIR / "luxd"), "migrate"],
            env={**os.environ, "LUX_DATABASE_URL": self.owner_dsn, "LUX_APP_PASSWORD": PG_APP_PASSWORD},
            capture_output=True, text=True,
        )
        if result.returncode != 0:
            raise RuntimeError(f"luxd migrate failed:\n{result.stdout}\n{result.stderr}")

    def start_luxd(self, **overrides: str) -> subprocess.Popen:
        Path(self.data_dir).mkdir(parents=True, exist_ok=True)
        log = open(Path(self.log_dir) / "luxd.log", "ab")
        # Its own session: a terminal's Ctrl-C goes to the harness, which
        # stops luxd in its teardown, and a detached dev environment's luxd
        # outlives the harness.
        proc = subprocess.Popen(
            [str(BIN_DIR / "luxd"), "serve"],
            env={**os.environ, **self.luxd_env(), **self.extra.get("luxd_env", {}), **overrides},
            stdout=log, stderr=subprocess.STDOUT, start_new_session=True,
        )
        (Path(self.log_dir) / "luxd.pid").write_text(str(proc.pid))
        _luxd_procs[proc.pid] = proc
        if not self.wait_healthy(proc):
            raise RuntimeError(f"luxd did not become healthy; see {self.log_dir}/luxd.log")
        return proc

    def stop_luxd(self) -> None:
        pidfile = Path(self.log_dir) / "luxd.pid"
        if not pidfile.exists():
            return
        pid = int(pidfile.read_text())
        proc = _luxd_procs.pop(pid, None)
        try:
            os.kill(pid, 15)
            if proc is not None:
                # Ours: wait on it, which also reaps it. Polling kill(pid, 0)
                # would see the unreaped zombie as alive for the full 10s.
                try:
                    proc.wait(timeout=10)
                except subprocess.TimeoutExpired:
                    proc.kill()
                    proc.wait()
            else:
                # Started by another process (a detached dev environment,
                # re-parented and reaped by init, so this ends).
                for _ in range(100):
                    os.kill(pid, 0)
                    time.sleep(0.1)
                os.kill(pid, 9)
        except ProcessLookupError:
            pass
        pidfile.unlink(missing_ok=True)

    def wait_healthy(self, proc: subprocess.Popen | None = None, timeout: float = 30) -> bool:
        deadline = time.time() + timeout
        while time.time() < deadline:
            if proc is not None and proc.poll() is not None:
                return False
            try:
                r = requests.get(f"{self.luxd_url}/health", timeout=1)
                if r.ok and r.json().get("status") == "ok":
                    return True
            except requests.RequestException:
                pass
            time.sleep(0.1)
        return False

    # -- admin helpers ------------------------------------------------------

    def luxd_admin(self, *args: str) -> dict:
        result = subprocess.run(
            [str(BIN_DIR / "luxd"), "admin", *args],
            env={**os.environ, "LUX_DATABASE_URL": self.owner_dsn},
            capture_output=True, text=True,
        )
        if result.returncode != 0:
            raise RuntimeError(f"luxd admin {' '.join(args)} failed:\n{result.stderr}")
        return json.loads(result.stdout)

    # -- persistence --------------------------------------------------------

    @property
    def env_file(self) -> Path:
        return Path(self.log_dir) / "env.json"

    def save(self) -> None:
        self.env_file.write_text(json.dumps(asdict(self), indent=2))

    @classmethod
    def load(cls, path: str | None = None) -> "TestEnvironment":
        data = json.loads(Path(path or os.environ["LUX_TEST_ENV"]).read_text())
        data["hosts"] = [Host(**h) for h in data["hosts"]]
        return cls(**data)

    # -- teardown -----------------------------------------------------------

    def teardown(self, keep: bool = False) -> None:
        self.stop_luxd()
        # Collect each host's runner and Podman state for debugging before
        # the containers go.
        for c in self._run_containers():
            d = Path(self.log_dir) / c.removeprefix(f"lux-e2e-{self.run_id}-")
            d.mkdir(parents=True, exist_ok=True)
            out = sh("docker", "exec", c, "podman", "ps", "-a", check=False)
            (d / "podman-ps.txt").write_text(out)
        if keep:
            return
        # Unconditional: a leaked privileged container holds CPU and disk.
        containers = self._run_containers()
        if containers:
            sh("docker", "rm", "-f", "-v", *containers, check=False)
        sh("docker", "network", "rm", self.network, check=False)
        try:
            with psycopg.connect(self._dsn(PG_OWNER, PG_OWNER_PASSWORD, "postgres"), autocommit=True) as conn:
                conn.execute(
                    "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = %s AND pid <> pg_backend_pid()",
                    (self.db_name,),
                )
                conn.execute(f'DROP DATABASE IF EXISTS "{self.db_name}"')
        except Exception as exc:  # noqa: BLE001 - cleanup must not mask failures
            print(f"warning: dropping {self.db_name}: {exc}")
        try:
            s3 = self.s3()
            for page in s3.get_paginator("list_objects_v2").paginate(Bucket=self.bucket):
                for obj in page.get("Contents", []):
                    s3.delete_object(Bucket=self.bucket, Key=obj["Key"])
            s3.delete_bucket(Bucket=self.bucket)
        except Exception as exc:  # noqa: BLE001
            print(f"warning: deleting bucket {self.bucket}: {exc}")
        (Path(self.log_dir) / "images.tar").unlink(missing_ok=True)

    def _run_containers(self) -> list[str]:
        out = sh("docker", "ps", "-aq", "--filter", f"label=lux-e2e-run={self.run_id}", check=False)
        ids = out.split()
        if not ids:
            return []
        return [n.lstrip("/") for n in sh("docker", "inspect", "-f", "{{.Name}}", *ids).split()]

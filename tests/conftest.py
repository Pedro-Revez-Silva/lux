"""pytest fixtures for the lux end-to-end suite.

Tests drive lux through its CLI wherever possible, so every test exercises
the CLI and the behaviour together. They go around it only for admin
bootstrap (`luxd admin`), for things no client can see (the database, a
host's disk, nftables), and for fault injection.
"""

from __future__ import annotations

import json
import os
import shutil
import subprocess
import sys
import uuid
from pathlib import Path

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

import pytest

from env import BIN_DIR, Host, TestEnvironment, wait_until


def _require(env: TestEnvironment, *binaries: str) -> None:
    missing = [b for b in binaries if not env.binaries.get(b)]
    if missing:
        pytest.skip(f"not built yet: {', '.join(missing)}")


@pytest.fixture(scope="session")
def env() -> TestEnvironment:
    """The environment run_tests.py set up. Its luxd is started here, as
    this process's child: stop_luxd can then wait on it (reaping it) rather
    than poll one that would linger as another process's zombie."""
    e = TestEnvironment.load()
    if e.binaries.get("luxd") and not (Path(e.log_dir) / "luxd.pid").exists():
        e.start_luxd()
    return e


@pytest.fixture(scope="session")
def hosts(env: TestEnvironment):
    return env.hosts


@pytest.fixture(scope="session")
def require(env: TestEnvironment):
    """`require("luxd", "lux-runner")` skips the test until those exist."""
    return lambda *b: _require(env, *b)


class CLIError(Exception):
    def __init__(self, args, code, stdout, stderr):
        super().__init__(f"lux {' '.join(args)} exited {code}\nstdout: {stdout}\nstderr: {stderr}")
        self.code, self.stdout, self.stderr = code, stdout, stderr


class Lux:
    """The lux CLI, as a tenant."""

    def __init__(self, env: TestEnvironment, api_key: str, tenant_id: str):
        self.env, self.api_key, self.tenant_id = env, api_key, tenant_id

    def _env(self) -> dict:
        return {**os.environ, "LUX_URL": self.env.luxd_url, "LUX_API_KEY": self.api_key, "HOME": "/nonexistent"}

    def run(self, *args: str, check: bool = True, input: str | None = None, timeout: float = 120) -> subprocess.CompletedProcess:
        p = subprocess.run([str(BIN_DIR / "lux"), *args], env=self._env(), input=input,
                           capture_output=True, text=True, timeout=timeout)
        if check and p.returncode != 0:
            raise CLIError(args, p.returncode, p.stdout, p.stderr)
        return p

    def json(self, *args: str, **kw):
        out = self.run(*args, "-o", "json", **kw).stdout
        return json.loads(out)

    def popen(self, *args: str, stdin=None) -> subprocess.Popen:
        return subprocess.Popen([str(BIN_DIR / "lux"), *args], env=self._env(), stdin=stdin,
                                stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)

    # -- conveniences ---------------------------------------------------

    def submit(self, spec: dict, *args: str) -> str:
        """lux run -f - with a spec; returns the run id."""
        return self.run("run", "-f", "-", *args, input=json.dumps(spec)).stdout.strip()

    def events(self, run_id: str, typ: str) -> list[dict]:
        """The Run's events of one type, whole (type, data, time...)."""
        return [e for e in self.json("events", run_id) if e["type"] == typ]

    def get(self, run_id: str) -> dict:
        return self.json("get", run_id)

    def wait_state(self, run_id: str, *states: str, timeout: float = 90) -> dict:
        return self.json("wait", run_id, "--state", ",".join(states), "--timeout", f"{int(timeout)}s", timeout=timeout + 10)

    def logs(self, run_id: str, *args: str) -> str:
        return self.run("logs", run_id, *args).stdout

    def records(self, run_id: str, *args: str) -> list[dict]:
        out = self.run("logs", run_id, "-o", "json", *args).stdout
        return [json.loads(l) for l in out.splitlines() if l.strip()]

    def wait_output(self, run_id: str, text: str, timeout: float = 60) -> str:
        return wait_until(lambda: (lambda out: out if text in out else None)(self.logs(run_id)),
                          timeout, 0.5, f"{text!r} not in output of {run_id}")

    def wait_activity(self, run_id: str, activity: str, timeout: float = 60) -> dict:
        return wait_until(lambda: (lambda r: r if r.get("activity") == activity else None)(self.get(run_id)),
                          timeout, 0.3, f"run {run_id} never became {activity}")

    def wait_placement_uploaded(self, run_id: str, timeout: float = 30) -> dict:
        """Wait until the Run's first placement has all its blobs in S3."""
        return wait_until(lambda: self.get(run_id)["placements"][0].get("uploadedAt"),
                          timeout, 0.5, f"placement of {run_id} never uploaded")

    def api(self, path: str, token: str | None = None, **kw):
        """A raw GET to luxd, for what the CLI does not expose."""
        import requests
        return requests.get(f"{self.env.luxd_url}{path}", timeout=10,
                            headers={"Authorization": f"Bearer {token or self.api_key}"}, **kw)

    def wait_uploaded(self, run_id: str, timeout: float = 30) -> dict:
        """Wait until the Run's latest snapshot is in S3."""
        return wait_until(lambda: (lambda sn: sn[-1] if sn and sn[-1]["uploaded"] else None)(self.json("snapshots", run_id)),
                          timeout, 0.5, f"snapshot of {run_id} never uploaded")


@pytest.fixture(scope="session")
def tenant_factory(env: TestEnvironment):
    def make(**kw) -> Lux:
        _require(env, "luxd", "lux")
        name = f"t{uuid.uuid4().hex[:8]}"
        args = ["create-tenant", "--name", name]
        for k, v in kw.items():
            args += [f"--{k.replace('_', '-')}", str(v)]
        t = env.luxd_admin(*args)
        return Lux(env, t["apiKey"], t["tenantId"])
    return make


@pytest.fixture
def lux(tenant_factory) -> Lux:
    """A fresh tenant, driven through the CLI. Per test, so tests cannot
    leak state into each other."""
    return tenant_factory()


@pytest.fixture(scope="session")
def operator(env: TestEnvironment) -> Lux:
    """The CLI with an operator key: every tenant. The database is shared by
    the whole session, so tests look for their own Runs and hosts in what
    it sees, never at totals."""
    _require(env, "luxd", "lux")
    return Lux(env, env.luxd_admin("create-operator-key")["apiKey"], "")


class Runners:
    """Runners on the simulated hosts, for one tenant."""

    def __init__(self, env: TestEnvironment, lux: Lux):
        self.env, self.lux = env, lux
        self.procs: dict[str, subprocess.Popen] = {}
        self.tokens: dict[str, str] = {}

    def token(self, *args: str) -> str:
        return self.env.luxd_admin("create-host-token", "--tenant", self.lux.tenant_id, *args)["token"]

    def start(self, host: Host, *extra: str, token: str | None = None, wait: bool = True, name: str | None = None) -> Host:
        tok = token or self.tokens.get(host.name) or self.token()
        self.tokens[host.name] = tok
        self.procs[host.name] = host.start_runner(self.env, tok, *extra, name=name)
        if wait:
            self.wait_ready(name or host.name)
        return host

    def wait_ready(self, name: str, timeout: float = 30):
        def ready():
            for proc in self.procs.values():
                if proc.poll() is not None:
                    raise AssertionError(f"a runner exited {proc.returncode}; see its runner.log")
            return any(h["name"] == name and h["state"] == "ready" for h in self.lux.json("hosts", "ls"))
        wait_until(ready, timeout, 0.2, f"host {name} did not become ready")

    def stop(self, host: Host, signal: str = "TERM"):
        host.stop_runner(signal)
        p = self.procs.pop(host.name, None)
        if p:
            try:
                p.wait(timeout=10)
            except subprocess.TimeoutExpired:
                p.kill()

    def stop_all(self):
        for name in list(self.procs):
            host = next(h for h in self.env.hosts if h.name == name)
            self.stop(host)


def cleanup_host(host: Host):
    """Remove everything lux left on a host, so tests do not see each
    other's containers, volumes or snapshots."""
    host.exec("sh", "-c",
              "podman rm -f -t 0 $(podman ps -aq --filter label=lux.managed=true) >/dev/null 2>&1; "
              "podman volume rm -f $(podman volume ls -q --filter label=lux.managed=true) >/dev/null 2>&1; "
              "podman network rm -f $(podman network ls -q --filter label=lux.managed=true) >/dev/null 2>&1; "
              "rm -rf /var/lib/lux; true", check=False)


@pytest.fixture
def runners(env: TestEnvironment, lux: Lux, require):
    require("luxd", "lux-runner", "lux-shim", "lux")
    r = Runners(env, lux)
    yield r
    r.stop_all()
    for h in env.hosts:
        cleanup_host(h)


@pytest.fixture
def fake_image(env: TestEnvironment, require) -> str:
    require("lux-fake")
    return env.fake_image


def generic(image: str, *cmd: str, **extra) -> dict:
    spec = {"image": {"ref": image}, "workload": {"adapter": "generic", "command": list(cmd)}}
    spec.update(extra)
    return spec


AGENT_VOLUMES = [
    {"name": "workspace", "path": "/workspace", "kind": "state"},
    {"name": "home", "path": "/home/agent", "kind": "state"},
]


def fake_agent(image: str, prompt: str = "", adapter: str = "acp", **extra) -> dict:
    """A spec running lux-fake behind an adapter (acp, claude-code, codex:
    lux-fake speaks each protocol), with its transcript on a state volume."""
    spec = {
        "image": {"ref": image},
        "workload": {"adapter": adapter, "command": ["lux-fake"], "prompt": prompt, "workdir": "/workspace"},
        "volumes": [dict(v) for v in AGENT_VOLUMES],
    }
    for k, v in extra.items():
        if isinstance(v, dict) and isinstance(spec.get(k), dict):
            spec[k] = {**spec[k], **v}
        else:
            spec[k] = v
    return spec


# ---- agent harnesses --------------------------------------------------------

def pytest_generate_tests(metafunc):
    """Parameterize every test that takes `harness` over all harnesses:
    fake variants always, real ones as `agents`-marked cases."""
    if "harness" not in metafunc.fixturenames:
        return
    from harnesses import HARNESSES
    only = getattr(metafunc.function, "harness_filter", None)
    params = []
    for h in HARNESSES:
        if only and not only(h):
            continue
        params.append(pytest.param((h, False), id=f"{h.name}-fake"))
        if h.credentials:
            params.append(pytest.param((h, True), id=f"{h.name}-real", marks=pytest.mark.agents))
    metafunc.parametrize("harness", params, indirect=True)


@pytest.fixture
def harness(request, env: TestEnvironment):
    """An agent harness, fake or real (see tests/harnesses.py)."""
    from harnesses import Variant
    h, real = request.param
    if real:
        img = env.extra.get("images", {}).get(h.name)
        if not img:
            pytest.skip(f"no real {h.name}: set {', '.join(h.credentials)} and select -m agents")
        return Variant(h, True, img)
    _require(env, "lux-fake")
    return Variant(h, False, env.fake_image)


def harnesses(pred):
    """Decorator: run a harness-parameterized test only for harnesses
    matching pred (a capability check, e.g. lambda h: h.caps.steer_joins_turn)."""
    def mark(fn):
        fn.harness_filter = pred
        return fn
    return mark


# ---- a git forge -------------------------------------------------------------

class GitServer:
    """A git server on the run network, reachable from hosts, with smart
    HTTP behind basic auth (the password is `token`)."""

    def __init__(self, env: TestEnvironment, container: str, ip: str, token: str):
        self.env, self.container, self.ip, self.token = env, container, ip, token

    def url(self, repo: str) -> str:
        return f"http://{self.ip}:8080/{repo}.git"

    def sh(self, script: str) -> str:
        from env import sh
        return sh("docker", "exec", self.container, "sh", "-c", script)

    def create(self, repo: str, files: dict[str, str], branch: str = "main") -> str:
        """A repository with one commit of files on branch; returns its sha."""
        writes = " && ".join(f"printf %s {_q(content)} > {_q(path)}" for path, content in files.items())
        return self.sh(
            f"set -e; rm -rf /tmp/w && mkdir -p /tmp/w && cd /tmp/w && git init -q -b {branch} && "
            f"git config user.email t@t && git config user.name t && {writes} && git add -A && "
            f"git commit -qm init && rm -rf /repos/{repo}.git && "
            f"git clone -q --bare /tmp/w /repos/{repo}.git && git -C /repos/{repo}.git config http.receivepack true && "
            f"git -C /repos/{repo}.git rev-parse {branch}").strip()

    def rev(self, repo: str, ref: str) -> str:
        return self.sh(f"git -C /repos/{repo}.git rev-parse --verify -q {ref} || true").strip()

    def show(self, repo: str, ref: str, path: str) -> str:
        return self.sh(f"git -C /repos/{repo}.git show {ref}:{path}")

    def commit_on(self, repo: str, branch: str, path: str, content: str) -> str:
        """Someone else pushes to branch (for lease tests)."""
        return self.sh(
            f"set -e; rm -rf /tmp/o && git clone -q /repos/{repo}.git /tmp/o && cd /tmp/o && "
            f"git config user.email o@o && git config user.name o && git checkout -q -B {branch} origin/{branch} 2>/dev/null || git checkout -q -b {branch}; "
            f"printf %s {_q(content)} > {_q(path)} && git add -A && git commit -qm other && "
            f"git push -q origin {branch} && git rev-parse HEAD").strip().splitlines()[-1]

    def requests(self) -> list[str]:
        return self.sh("cat /repos/requests.log 2>/dev/null; true").splitlines()


def _q(s: str) -> str:
    return "'" + s.replace("'", "'\\''") + "'"


@pytest.fixture(scope="session")
def git_server(env: TestEnvironment):
    from env import build_test_image, sh, start_service
    name = f"lux-e2e-{env.run_id}-git"
    token = "ghp_" + uuid.uuid4().hex
    ip = start_service(env, name, build_test_image("gitserver"), GIT_TOKEN=token)
    yield GitServer(env, name, ip, token)
    sh("docker", "rm", "-f", name, check=False)


# ---- a private registry ----------------------------------------------------------

class Registry:
    """A registry on the run network behind basic auth, over plain HTTP
    (every host lists it as insecure)."""

    def __init__(self, container: str, ip: str, user: str, password: str):
        self.container, self.user, self.password = container, user, password
        self.address = f"{ip}:5000"

    @property
    def creds(self) -> str:
        return f"{self.user}:{self.password}"

    def push(self, host: Host, image: str, repo: str) -> str:
        """Pushes an image of its own, on a host's image, to
        <registry>/<repo>; returns the reference. Its own (a unique label):
        pulling the host's image back under another name would give that
        image the registry's digest, and builds pin FROM to it. Made in a
        storage of its own, so the host has none of it."""
        ref = f"{self.address}/{repo}"
        store = "--root /var/tmp/lux-test-push --runroot /run/lux-test-push"
        host.exec("sh", "-c", f"set -e; podman save -q {image} | podman {store} load -q >/dev/null; "
                  f"printf 'FROM {image}\\nLABEL lux.test={uuid.uuid4().hex}\\n' | podman {store} build -q -t {ref} -f - >/dev/null; "
                  f"podman {store} push -q --creds {self.creds} {ref}; podman {store} rmi -a -f >/dev/null")
        return ref

    def tags(self, repo: str) -> list[str]:
        """The registry's tags of a repository."""
        import requests
        r = requests.get(f"http://{self.address}/v2/{repo}/tags/list", auth=(self.user, self.password), timeout=10)
        return (r.json().get("tags") or []) if r.ok else []


@pytest.fixture(scope="session")
def registry(env: TestEnvironment):
    from env import build_test_image, sh, start_service
    name = f"lux-e2e-{env.run_id}-registry"
    password = "regpw-" + uuid.uuid4().hex
    ip = start_service(env, name, build_test_image("registry"), REG_USER="luxtest", REG_PASSWORD=password)
    reg = Registry(name, ip, "luxtest", password)
    for h in env.hosts:
        h.insecure_registry(reg.address)
    wait_until(lambda: "401" in sh("docker", "exec", name, "sh", "-c",
                                   "wget -S -q -O /dev/null http://127.0.0.1:5000/v2/ 2>&1; true"),
               30, 0.5, "the registry did not start")
    yield reg
    sh("docker", "rm", "-f", name, check=False)


# ---- an MCP server ---------------------------------------------------------------

class MCPServer:
    """An MCP server (streamable HTTP) on the run network, reachable from
    hosts. Every request needs `Authorization: Bearer <token>`; it has one
    tool, echo, answering "echo: <text>"."""

    def __init__(self, container: str, ip: str, token: str):
        self.container, self.ip, self.token = container, ip, token
        self.url = f"http://{ip}:8080/mcp"

    def calls(self) -> list[dict]:
        """What reached it: method, tool, whether the auth was right."""
        from env import sh
        return json.loads(sh("docker", "exec", self.container, "wget", "-qO-", "http://127.0.0.1:8080/calls"))


@pytest.fixture(scope="session")
def mcp_server(env: TestEnvironment):
    from env import build_test_image, sh, start_service
    name = f"lux-e2e-{env.run_id}-mcp"
    token = "mcp-" + uuid.uuid4().hex
    ip = start_service(env, name, build_test_image("mcp"), TOKEN=token)
    yield MCPServer(name, ip, token)
    sh("docker", "rm", "-f", name, check=False)


# ---- egress targets -----------------------------------------------------------

class NetTargets:
    """Two web servers on the run network and a DNS server that names them.
    The hosts resolve through it, so the tests need no internet."""

    def __init__(self, allowed_ip: str, denied_ip: str, dns_ip: str):
        self.allowed_ip, self.denied_ip, self.dns_ip = allowed_ip, denied_ip, dns_ip
        self.allowed_name, self.denied_name = "allowed.lux.test", "denied.lux.test"


@pytest.fixture(scope="session")
def net_targets(env: TestEnvironment):
    from env import build_test_image, sh, start_service
    image = build_test_image("netsvc")
    names = []

    def start(role: str, **envs) -> str:
        names.append(f"lux-e2e-{env.run_id}-{role}-{len(names)}")
        return start_service(env, names[-1], image, ROLE=role, **envs)

    allowed = start("web", NAME="allowed")
    denied = start("web", NAME="denied")
    dns = start("dns", RECORDS=f"allowed.lux.test={allowed},denied.lux.test={denied},meta.lux.test=169.254.169.254")
    yield NetTargets(allowed, denied, dns)
    for n in names:
        sh("docker", "rm", "-f", n, check=False)


@pytest.fixture
def egress_hosts(hosts, net_targets):
    """Hosts whose runners resolve through the test DNS."""
    for h in hosts:
        h.exec("sh", "-c", f"cp /etc/resolv.conf /etc/resolv.conf.lux-orig 2>/dev/null; echo 'nameserver {net_targets.dns_ip}' > /etc/resolv.conf")
    yield hosts
    for h in hosts:
        h.exec("sh", "-c", "[ -f /etc/resolv.conf.lux-orig ] && mv /etc/resolv.conf.lux-orig /etc/resolv.conf; true", check=False)


# ---- EC2 -----------------------------------------------------------------------

EC2_TEMPLATE = {"region": "us-east-1", "launchTemplate": "lt-0lux000000000test", "instanceType": "m7i.large",
                "subnets": ["subnet-a", "subnet-b"]}


class RealEC2:
    """--real-ec2: what the tests ask of the fake, answered by AWS. Only
    what real EC2 can do: no injected failures."""

    real = True

    def __init__(self, template: dict):
        import boto3
        self.template = template
        self.client = boto3.client("ec2", region_name=template.get("region"))

    def running(self) -> list[dict]:
        out = self.client.describe_instances(Filters=[
            {"Name": "tag:lux:managed", "Values": ["true"]},
            {"Name": "instance-state-name", "Values": ["pending", "running"]}])
        return [{"id": i["InstanceId"], "tags": {t["Key"]: t["Value"] for t in i.get("Tags", [])},
                 "launchTemplate": self.template.get("launchTemplate"), "instanceType": i["InstanceType"]}
                for r in out["Reservations"] for i in r["Instances"]]

    def kill(self, instance_id: str):
        self.client.terminate_instances(InstanceIds=[instance_id])

    def close(self):
        pass


# luxd's provisioning timers against the fake EC2: seconds, not minutes, so
# tests see a timer fire without waiting out a production default.
FAKE_EC2_TIMERS = {"LUX_SCALE_DOWN_AFTER": "3s", "LUX_LAUNCH_TIMEOUT": "8s",
                   "LUX_PROVIDER_CHECK_EVERY": "2s", "LUX_LOST_GRACE": "4s", "LUX_LISTING_LAG": "4s"}


@pytest.fixture
def ec2(env: TestEnvironment, require):
    """luxd with its EC2 provider pointed at a fake EC2, or, with
    --real-ec2, at AWS (LUX_TEST_EC2_TEMPLATE: the pool template JSON for an
    AMI with lux-runner; AWS credentials from the environment). Fast
    scale-down for the tests. `ec2.template` is the pool template to use."""
    require("luxd", "lux-runner", "lux")
    env.stop_luxd()
    if os.environ.get("LUX_TEST_REAL_EC2"):
        raw = os.environ.get("LUX_TEST_EC2_TEMPLATE")
        if not raw:
            env.start_luxd()
            pytest.skip("--real-ec2 needs LUX_TEST_EC2_TEMPLATE (see docs/development.md)")
        cloud = RealEC2(json.loads(raw))
        env.start_luxd(LUX_LAUNCH_TIMEOUT="600s", LUX_SCALE_DOWN_AFTER="30s")
    else:
        from fake_ec2 import FakeEC2
        cloud = FakeEC2(env, EC2_TEMPLATE)
        env.start_luxd(LUX_EC2_ENDPOINT=cloud.url, AWS_ACCESS_KEY_ID="fake", AWS_SECRET_ACCESS_KEY="fake",
                       AWS_REGION="us-east-1", **FAKE_EC2_TIMERS)
    yield cloud
    cloud.close()
    env.stop_luxd()
    env.start_luxd()


def fake_only(ec2):
    if ec2.real:
        pytest.skip("needs the fake EC2 (it injects failures or inspects API calls)")


@pytest.fixture(scope="session")
def browser():
    """A headless Chromium for the console suites: the system's Chrome, or
    Playwright's own if installed; skipped when neither is there."""
    sync_api = pytest.importorskip("playwright.sync_api")
    chrome = os.environ.get("LUX_TEST_CHROME") or shutil.which("google-chrome") or shutil.which("chromium")
    with sync_api.sync_playwright() as p:
        try:
            b = p.chromium.launch(executable_path=chrome) if chrome else p.chromium.launch()
        except Exception as e:  # no browser at all
            pytest.skip(f"no headless browser: {e}")
        yield b
        b.close()

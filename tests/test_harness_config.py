"""The harness's own configuration: needs neither Docker nor an environment.

    cd tests && uv run pytest test_harness_config.py    # or: make harness-unit
"""

from __future__ import annotations

import os
import subprocess
import sys
from pathlib import Path

import pytest

from env import S3_CONTAINER, ContainerState, SharedServiceError, ensure_s3

TESTS_DIR = Path(__file__).resolve().parent
DEFAULT = "versity/versitygw:v1.7.0"
OTHER = "versity/versitygw:v1.6.0"


class FakeDocker:
    """Containers by name; state identity distinguishes reuse from replacement."""

    def __init__(self):
        self.containers: dict[str, ContainerState] = {}

    def add(self, name: str, state: ContainerState) -> None:
        self.containers[name] = state

    def inspect(self, name: str) -> ContainerState | None:
        return self.containers.get(name)

    def remove(self, name: str) -> None:
        self.containers.pop(name, None)

    def run(self, *args: str) -> None:
        opts, rest, it = {}, [], iter(args)
        for a in it:
            if a == "-d":
                continue
            if a.startswith("-") and not rest:
                opts.setdefault(a, []).append(next(it))
            else:
                rest.append(a)
        name = opts["--name"][0]
        if name in self.containers:
            raise RuntimeError(f"docker run failed (125): Conflict. The container name \"/{name}\" is already in use")
        host_port = int(opts["-p"][0].rsplit(":", 2)[1])
        self.add(name, ContainerState(True, rest[0], host_port))

def running(image: str, port: int) -> ContainerState:
    return ContainerState(running=True, image=image, host_port=port)


def test_absent_container_starts_with_selected_image_and_port():
    docker = FakeDocker()
    ensure_s3(docker, OTHER, 59201)
    assert docker.inspect(S3_CONTAINER) == running(OTHER, 59201)


def test_stopped_container_is_replaced_with_selected_image():
    docker = FakeDocker()
    docker.add(S3_CONTAINER, ContainerState(running=False, image=DEFAULT, host_port=59000))
    ensure_s3(docker, OTHER, 59000)
    assert docker.inspect(S3_CONTAINER) == running(OTHER, 59000)


def test_matching_running_container_is_reused():
    docker = FakeDocker()
    docker.add(S3_CONTAINER, running(DEFAULT, 59000))
    before = docker.inspect(S3_CONTAINER)
    ensure_s3(docker, DEFAULT, 59000)
    assert docker.inspect(S3_CONTAINER) is before


def test_running_container_with_other_image_is_an_error_and_kept():
    docker = FakeDocker()
    docker.add(S3_CONTAINER, running(DEFAULT, 59000))
    before = docker.inspect(S3_CONTAINER)
    with pytest.raises(SharedServiceError) as e:
        ensure_s3(docker, OTHER, 59000)
    msg = str(e.value)
    assert DEFAULT in msg and OTHER in msg and "LUX_TEST_S3_IMAGE" in msg
    assert docker.inspect(S3_CONTAINER) is before


def test_running_container_on_other_port_is_an_error_and_kept():
    docker = FakeDocker()
    docker.add(S3_CONTAINER, running(DEFAULT, 59000))
    before = docker.inspect(S3_CONTAINER)
    with pytest.raises(SharedServiceError) as e:
        ensure_s3(docker, DEFAULT, 59201)
    msg = str(e.value)
    assert "59000" in msg and "59201" in msg and "LUX_TEST_S3_PORT" in msg
    assert docker.inspect(S3_CONTAINER) is before


def test_host_port_taken_on_start_is_explained():
    class PortTaken(FakeDocker):
        def run(self, *args: str) -> None:
            raise RuntimeError(
                "docker run failed (125): Bind for 0.0.0.0:59000 failed: port is already allocated")

    with pytest.raises(SharedServiceError) as e:
        ensure_s3(PortTaken(), DEFAULT, 59000)
    assert "59000" in str(e.value) and "LUX_TEST_S3_PORT" in str(e.value)


@pytest.mark.parametrize("env, port", [
    ({"LUX_TEST_S3_PORT": "59201", "LUX_TEST_MINIO_PORT": "59001"}, 59201),
    ({"LUX_TEST_MINIO_PORT": "59001"}, 59001),
    ({}, 59000),
])
def test_s3_port_precedence(env, port):
    clean = {k: v for k, v in os.environ.items() if k not in ("LUX_TEST_S3_PORT", "LUX_TEST_MINIO_PORT")}
    out = subprocess.run(
        [sys.executable, "-c", "import env; print(env.S3_PORT)"],
        cwd=TESTS_DIR, env={**clean, **env}, capture_output=True, text=True, check=True,
    ).stdout
    assert int(out) == port


def test_clean_environ_drops_the_developers_lux_settings(monkeypatch):
    from env import clean_environ

    for k, v in {"LUX_TENANT": "their-tenant", "LUX_URL": "https://their.lux", "LUX_API_KEY": "theirs",
                 "LUX_CONFIG": "/etc/their/luxd.toml", "XDG_CONFIG_HOME": "/home/them/.config",
                 "LUX_TEST_S3_PORT": "59100", "LUX_DEBUG": "1", "PATH_FOR_TEST": "kept"}.items():
        monkeypatch.setenv(k, v)
    env = clean_environ()
    for gone in ("LUX_TENANT", "LUX_URL", "LUX_API_KEY", "LUX_CONFIG", "XDG_CONFIG_HOME"):
        assert gone not in env, gone
    assert env["LUX_TEST_S3_PORT"] == "59100" and env["LUX_DEBUG"] == "1" and env["PATH_FOR_TEST"] == "kept"


def test_nested_without_serve_is_refused_before_building():
    p = subprocess.run([sys.executable, "run_tests.py", "--nested"], cwd=TESTS_DIR, capture_output=True, text=True)
    assert p.returncode == 2 and "--nested needs --serve" in p.stderr, (p.stdout, p.stderr)
    assert "building" not in p.stdout, p.stdout


# Two suites: the nested image must follow --nested whatever the selection.
@pytest.mark.parametrize("suite", ["suites/test_serve.py", "suites/test_scheduling.py"])
@pytest.mark.parametrize("flags, nested", [(["--serve", "--nested"], True), (["--serve"], False)])
def test_serve_passes_nested_to_the_image_build_and_the_runners(monkeypatch, flags, nested, suite):
    import run_tests
    import serve

    calls = {}
    monkeypatch.setattr(run_tests, "build_binaries", lambda: {})
    monkeypatch.setattr(run_tests, "build_fake_image", lambda _bin: None)
    monkeypatch.setattr(run_tests, "build_agent_images", lambda _wanted: {})

    def build_nested_images(podman, docker):
        calls["podman"] = podman
        return ("localhost/lux-nested:test" if podman else None), None
    monkeypatch.setattr(run_tests, "build_nested_images", build_nested_images)

    def fake_serve(env, fake_image, detach, nested=False):
        calls["nested"], calls["images"] = nested, env.extra["images"]
    monkeypatch.setattr(serve, "serve", fake_serve)
    # suites/test_serve.py names a suite, so the nested image is selected by
    # --nested alone, not by a full run.
    monkeypatch.setattr(sys, "argv", ["run_tests.py", *flags, "--detach", suite])
    run_tests.main()
    assert calls["nested"] is nested, calls
    assert calls["podman"] is nested, calls
    if nested:
        assert calls["images"]["nested"] == "localhost/lux-nested:test", calls


# The real-app images are pulled from registries: a bare --serve runs no suite
# and must not pull them; a named suite brings them only if it needs them.
@pytest.mark.parametrize("args, sizing", [
    (["--serve"], False),
    (["--serve", "--nested"], False),
    (["--serve", "suites/test_serve.py"], False),
    (["--serve", "suites/test_run_size.py"], True),
    (["suites/test_nested.py"], False),
    (["suites/test_run_size.py::test_plain_run"], True),
    ([], True),
    (["suites/"], True),
])
def test_dry_run_preloads_real_app_images_only_for_suites_that_need_them(args, sizing):
    p = subprocess.run([sys.executable, "run_tests.py", "--dry-run", *args], cwd=TESTS_DIR,
                       capture_output=True, text=True)
    assert p.returncode == 0, (p.stdout, p.stderr)
    assert "building" not in p.stdout, p.stdout
    assert f"sizing={'yes' if sizing else 'no'}" in p.stdout, p.stdout


# Fake EC2 instances join through add_host mid-test and must not load the
# nested and real-app images (~1.7 GB): loading them delays the instance's
# registration past the EC2 suites' waits. Only the environment's own hosts do.
def test_only_the_environments_own_hosts_load_the_nested_and_real_app_images(monkeypatch, tmp_path):
    import build
    import env as envmod

    saves, loads = {}, []
    monkeypatch.setattr(build, "ensure_local", lambda *a: None)

    def sh(*args, **_kw):
        if args[:2] == ("docker", "save"):
            saves[Path(args[3]).name] = list(args[4:])
            Path(args[3]).touch()
        return ""
    monkeypatch.setattr(envmod, "sh", sh)
    monkeypatch.setattr(envmod, "_load", lambda host, tar, refs: loads.append((host, tar.name, refs)))
    monkeypatch.setattr(envmod.TestEnvironment, "add_host",
                        lambda self, name: envmod._load(name, Path("images.tar"), self._host_images()[0]) or name)

    e = envmod.TestEnvironment(n_hosts=2)
    e.log_dir, e.fake_image = str(tmp_path), "localhost/lux-fake:test"
    e.extra["images"] = {"nested": "localhost/lux-nested:test", "docker": "localhost/lux-docker:test",
                         "sizing": None, "claude": "localhost/lux-claude:test"}
    e._hosts()

    every = [envmod.ALPINE_IMAGE, "localhost/lux-fake:test", "localhost/lux-claude:test"]
    own = ["localhost/lux-nested:test", "localhost/lux-docker:test"]
    assert saves == {"images.tar": every, "own-hosts.tar": own}, saves
    assert sorted(loads) == sorted([(h, "images.tar", every) for h in e.hosts]
                                   + [(h, "own-hosts.tar", own) for h in e.hosts]), loads
    assert not (tmp_path / "own-hosts.tar").exists()

    loads.clear()
    e.add_host("ec2-1")  # a fake EC2 instance
    assert loads == [("ec2-1", "images.tar", every)], loads


def test_without_nested_or_real_app_images_there_is_one_tar(monkeypatch, tmp_path):
    import build
    import env as envmod

    saves = []
    monkeypatch.setattr(build, "ensure_local", lambda *a: None)
    monkeypatch.setattr(envmod, "sh", lambda *a, **_k: saves.append(Path(a[3]).name) if a[:2] == ("docker", "save") else "")
    monkeypatch.setattr(envmod, "_load", lambda *a: None)
    monkeypatch.setattr(envmod.TestEnvironment, "add_host", lambda self, name: name)

    e = envmod.TestEnvironment(n_hosts=1)
    e.log_dir = str(tmp_path)
    e.extra["images"] = {"nested": None, "claude": "localhost/lux-claude:test"}
    e._hosts()
    assert saves == ["images.tar"], saves

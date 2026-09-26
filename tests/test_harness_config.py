"""The harness's own configuration: needs neither Docker nor an environment.

    cd tests && uv run pytest test_harness_config.py    # or: make harness-unit
"""

from __future__ import annotations

import itertools
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
    """Containers by name; each start gets a new id, so a reused container
    is told apart from a replaced one by what is running afterwards."""

    _ids = itertools.count(1)

    def __init__(self):
        self.containers: dict[str, tuple[ContainerState, int]] = {}

    def add(self, name: str, state: ContainerState) -> None:
        self.containers[name] = (state, next(self._ids))

    def inspect(self, name: str) -> ContainerState | None:
        entry = self.containers.get(name)
        return entry[0] if entry else None

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

    def id_of(self, name: str) -> int | None:
        entry = self.containers.get(name)
        return entry[1] if entry else None


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
    before = docker.id_of(S3_CONTAINER)
    ensure_s3(docker, DEFAULT, 59000)
    assert docker.id_of(S3_CONTAINER) == before
    assert docker.inspect(S3_CONTAINER) == running(DEFAULT, 59000)


def test_running_container_with_other_image_is_an_error_and_kept():
    docker = FakeDocker()
    docker.add(S3_CONTAINER, running(DEFAULT, 59000))
    before = docker.id_of(S3_CONTAINER)
    with pytest.raises(SharedServiceError) as e:
        ensure_s3(docker, OTHER, 59000)
    msg = str(e.value)
    assert DEFAULT in msg and OTHER in msg and "LUX_TEST_S3_IMAGE" in msg
    assert docker.id_of(S3_CONTAINER) == before
    assert docker.inspect(S3_CONTAINER) == running(DEFAULT, 59000)


def test_running_container_on_other_port_is_an_error_and_kept():
    docker = FakeDocker()
    docker.add(S3_CONTAINER, running(DEFAULT, 59000))
    before = docker.id_of(S3_CONTAINER)
    with pytest.raises(SharedServiceError) as e:
        ensure_s3(docker, DEFAULT, 59201)
    msg = str(e.value)
    assert "59000" in msg and "59201" in msg and "LUX_TEST_S3_PORT" in msg
    assert docker.id_of(S3_CONTAINER) == before
    assert docker.inspect(S3_CONTAINER) == running(DEFAULT, 59000)


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

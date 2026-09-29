"""Build the artifacts the suite drives.

Every `cmd/<name>` that exists is built into `bin/`, statically: the binaries
run inside Fedora hosts, and the shim runs inside arbitrary images. A binary
whose package does not exist yet is reported, not fatal — suites that need it
skip.
"""

from __future__ import annotations

import os
import shutil
import subprocess
import sys
import tempfile
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parent.parent
BIN_DIR = REPO_ROOT / "bin"
TESTS_DIR = REPO_ROOT / "tests"

BINARIES = ["luxd", "lux-runner", "lux-shim", "lux", "lux-fake"]

FAKE_IMAGE = "localhost/lux-fake:test"


def build_binaries() -> dict[str, Path | None]:
    """Build each binary that has a package. Returns name → path or None."""
    BIN_DIR.mkdir(exist_ok=True)
    built: dict[str, Path | None] = {}
    if not (REPO_ROOT / "go.mod").exists():
        for name in BINARIES:
            print(f"  {name:12} missing (no go.mod yet)")
            built[name] = None
        return built

    for name in BINARIES:
        pkg = REPO_ROOT / "cmd" / name
        if not pkg.is_dir():
            print(f"  {name:12} missing (no cmd/{name})")
            built[name] = None
            continue
        out = BIN_DIR / name
        result = subprocess.run(
            ["go", "build", "-trimpath", "-o", str(out), f"./cmd/{name}"],
            cwd=REPO_ROOT,
            env={**os.environ, "CGO_ENABLED": "0", "GOOS": "linux"},
        )
        if result.returncode != 0:
            print(f"go build failed for {name}", file=sys.stderr)
            sys.exit(1)
        print(f"  {name:12} built")
        built[name] = out
    return built


def _build_image(tag: str, files: dict[str, Path], containerfile: Path, args: dict[str, str] | None = None) -> None:
    """docker build from a context holding only the given files (not the
    repository: a context that large makes every build slow)."""
    with tempfile.TemporaryDirectory() as ctx:
        for name, src in files.items():
            dst = Path(ctx) / name
            try:
                os.link(src, dst)  # hard link: agent binaries are ~200MB
            except OSError:
                shutil.copy(src, dst)
        shutil.copy(containerfile, Path(ctx) / "Containerfile")
        cmd = ["docker", "build", "-q", "-t", tag, "-f", "Containerfile"]
        for k, v in (args or {}).items():
            cmd += ["--build-arg", f"{k}={v}"]
        if subprocess.run([*cmd, "."], cwd=ctx, stdout=subprocess.DEVNULL).returncode != 0:
            print(f"{tag} build failed", file=sys.stderr)
            sys.exit(1)
    print(f"  {tag} built")


def build_fake_image(fake_binary: Path | None) -> str | None:
    if fake_binary is None:
        return None
    _build_image(FAKE_IMAGE, {"lux-fake": fake_binary}, TESTS_DIR / "images" / "fake" / "Containerfile")
    return FAKE_IMAGE


NESTED_IMAGE = "localhost/lux-nested:test"
DOCKER_IMAGE = "localhost/lux-docker:test"


def build_nested_images(podman: bool, docker: bool) -> tuple[str | None, str | None]:
    """Workload images with Podman and with Docker in them, each only if
    asked, and an alpine archive to run inside (see tests/images/nested and
    tests/images/docker)."""
    from env import ALPINE_IMAGE
    if not (podman or docker):
        return None, None
    with tempfile.TemporaryDirectory() as d:
        tar = Path(d) / "alpine.tar"
        subprocess.run(["docker", "save", "-o", str(tar), ALPINE_IMAGE], check=True)
        if podman:
            _build_image(NESTED_IMAGE, {"alpine.tar": tar}, TESTS_DIR / "images" / "nested" / "Containerfile")
        if docker:
            _build_image(DOCKER_IMAGE, {"alpine.tar": tar}, TESTS_DIR / "images" / "docker" / "Containerfile")
    return (NESTED_IMAGE if podman else None), (DOCKER_IMAGE if docker else None)


# The opt-in real-agent suites: the credentials each needs, and where its
# self-contained executable is.
AGENTS = {
    "claude": (["LUX_TEST_ANTHROPIC_API_KEY"], lambda: shutil.which("claude")),
    "codex": (["LUX_TEST_OPENAI_API_KEY"], lambda: _codex_native()),
    "opencode": (["LUX_TEST_OPENCODE_AUTH", "LUX_TEST_OPENCODE_CONFIG", "LUX_TEST_OPENCODE_MODEL"],
                 lambda: shutil.which("opencode")),
}


def _codex_native() -> str | None:
    """The codex npm package's launcher is a Node script; the native binary
    it runs, for this machine's architecture, is what goes in the image."""
    launcher = shutil.which("codex")
    if not launcher:
        return None
    arch = {"x86_64": "x64", "aarch64": "arm64", "arm64": "arm64"}.get(os.uname().machine, os.uname().machine)
    pkg = Path(os.path.realpath(launcher)).parent.parent
    found = sorted(pkg.glob(f"node_modules/@openai/codex-linux-{arch}/vendor/*/bin/codex"))
    return str(found[0]) if found else None


def _is_elf(path: str) -> bool:
    with open(path, "rb") as f:
        return f.read(4) == b"\x7fELF"


def build_agent_images(selected: bool) -> dict[str, str | None]:
    """Images for the real-agent suites: only when those suites are
    selected, and only for agents whose credentials are all set and whose
    CLI is installed here as a native binary (a Node launcher script cannot
    run in the image)."""
    images: dict[str, str | None] = {name: None for name in AGENTS}
    if not selected:
        return images
    for name, (env_vars, find) in AGENTS.items():
        if not all(os.environ.get(v) for v in env_vars):
            continue
        exe = find()
        if not exe or not _is_elf(os.path.realpath(exe)):
            print(f"  {name}: no native {name} binary installed; its real-agent suite will skip")
            continue
        tag = f"localhost/lux-{name}:test"
        _build_image(tag, {name: Path(os.path.realpath(exe))}, TESTS_DIR / "images" / "agent" / "Containerfile",
                     {"BIN": name})
        images[name] = tag
    return images

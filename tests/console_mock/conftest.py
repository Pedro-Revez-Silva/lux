from __future__ import annotations

import os
import shutil
import socket
import subprocess
import time
from pathlib import Path

import pytest
import requests

CONSOLE = Path(__file__).resolve().parents[2] / "console"


def _free_port() -> int:
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


@pytest.fixture(scope="session")
def mock_console():
    """console/mock.ts on a free port: the console with a fake luxd behind
    it (a running Run whose exec WebSocket is a fake shell)."""
    bun = shutil.which("bun")
    if not bun:
        pytest.skip("bun not installed")
    port = _free_port()
    proc = subprocess.Popen([bun, "mock.ts"], cwd=CONSOLE, env={**os.environ, "PORT": str(port)},
                            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    url = f"http://127.0.0.1:{port}"
    try:
        deadline = time.time() + 60
        while True:
            try:
                if requests.get(url + "/health", timeout=2).ok:
                    break
            except requests.RequestException:
                pass
            if time.time() > deadline or proc.poll() is not None:
                pytest.fail("console mock did not start")
            time.sleep(0.3)
        yield url
    finally:
        proc.terminate()
        proc.wait(timeout=10)

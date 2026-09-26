"""A lux to develop against: the test environment, left running.

    uv run python run_tests.py --serve [--hosts N] [--image REF ...]   # up until Ctrl-C
    uv run python run_tests.py --serve --detach                        # up in the background
    uv run python run_tests.py --down [ENV_JSON]                       # take a detached one down

It brings up what the suite uses (Postgres, S3, simulated Podman hosts,
luxd), starts a runner on every host, creates a tenant, and writes
env.json: luxd_url, api_key (scopes run, read), admin_key, tenant_id,
and the rest of the environment. `--image` preloads an image from the
local Docker into every host, so Runs never pull.
"""

from __future__ import annotations

import json
import os
import signal
import time
from pathlib import Path

from env import TestEnvironment

# Where the last detached environment is recorded, for --down without a path.
LAST = Path(os.environ.get("LUX_TEST_LOG_ROOT", "/tmp")) / "lux-dev-env.json"


def serve(env: TestEnvironment, fake_image: str | None, detach: bool) -> None:
    env.setup(fake_image=fake_image)
    t = env.luxd_admin("create-tenant", "--name", "dev")
    env.tenant_id, env.admin_key = t["tenantId"], t["apiKey"]
    env.api_key = env.luxd_admin("create-key", "--tenant", env.tenant_id, "--name", "dev", "--scopes", "run,read")["apiKey"]
    # The suite's own runner handling: start on every host, wait until ready.
    from conftest import Lux, Runners
    runners = Runners(env, Lux(env, env.api_key, env.tenant_id))
    token = runners.token()
    for h in env.hosts:
        runners.start(h, token=token, wait=False)
    for h in env.hosts:
        runners.wait_ready(h.name, timeout=60)
    env.save()
    LAST.write_text(str(env.env_file))

    print(json.dumps({"luxd_url": env.luxd_url, "api_key": env.api_key, "env": str(env.env_file)}, indent=2))
    print(f"\nlux CLI:  LUX_URL={env.luxd_url} LUX_API_KEY={env.api_key} bin/lux ls")
    print(f"logs:     {env.log_dir}")
    if detach:
        print("detached; take it down with: uv run python run_tests.py --down")
        return
    print("up; Ctrl-C to take it down")
    stop = False

    def on_signal(*_):
        nonlocal stop
        stop = True
    signal.signal(signal.SIGINT, on_signal)
    signal.signal(signal.SIGTERM, on_signal)
    while not stop:
        time.sleep(0.5)
    print("\ntaking it down")
    down(str(env.env_file))


def down(path: str | None) -> None:
    path = path or (LAST.read_text().strip() if LAST.exists() else None)
    if not path or not Path(path).exists():
        raise SystemExit("no dev environment to take down")
    env = TestEnvironment.load(path)
    env.teardown()
    if LAST.exists() and LAST.read_text().strip() == path:
        LAST.unlink()
    print(f"down; logs kept in {env.log_dir}")

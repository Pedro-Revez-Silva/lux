"""workload.beforeStop: a command the Run leaves behind with, on every stop.

The shim runs it in the container before the workload is signalled — for a
stop asked for, a cancel, a timeout — as the workload's user with its
environment. What it writes into $LUX_ARTIFACTS is collected like any
artifact, and a hook that overruns is cut off rather than holding the stop.
"""

from __future__ import annotations

import json
import time

from conftest import generic
from env import ALPINE_IMAGE, wait_until

WORKSPACE = [{"name": "workspace", "path": "/workspace", "kind": "state"}]

# The workload keeps a counter going; the hook records where it got to, so
# the test can see the hook ran while the workload was still whole.
WORK = "mkdir -p /workspace && i=0; while true; do i=$((i+1)); echo $i > /workspace/count; sleep 0.2; done"
HOOK = ["sh", "-c", "echo hook-ran; cp /workspace/count $LUX_ARTIFACTS/final-count; echo \"$HOME\" > $LUX_ARTIFACTS/hook-home"]


def hooked(**extra) -> dict:
    spec = generic(ALPINE_IMAGE, "sh", "-c", WORK, volumes=WORKSPACE)
    spec["workload"]["beforeStop"] = {"command": HOOK, "timeout": "10s"}
    spec.update(extra)
    return spec


def started(lux, run_id: str) -> None:
    lux.wait_state(run_id, "running")
    wait_until(lambda: lux.run("exec", run_id, "-T", "--", "test", "-s", "/workspace/count", input="", check=False).returncode == 0,
               30, 0.5, "the workload never started counting")


def artifact_paths(lux, run_id: str) -> list[str]:
    arts = wait_until(lambda: (a := lux.json("artifacts", run_id)) and all(x["available"] for x in a) and a,
                      60, 0.5, "no artifacts")
    return sorted(a["path"] for a in arts)


def test_runs_before_a_stop_and_its_files_are_collected(lux, runners, hosts, tmp_path):
    runners.start(hosts[0])
    run_id = lux.submit(hooked())
    started(lux, run_id)
    lux.run("stop", run_id, "--wait")
    assert artifact_paths(lux, run_id) == ["/.lux/artifacts/final-count", "/.lux/artifacts/hook-home"]
    assert "hook-ran" in lux.logs(run_id)
    lux.run("artifacts", run_id, "--download", str(tmp_path))
    assert int((tmp_path / "1/.lux/artifacts/final-count").read_text()) > 0
    # As the workload's user, with its environment.
    assert (tmp_path / "1/.lux/artifacts/hook-home").read_text().strip() != ""


def test_runs_before_a_cancel(lux, runners, hosts):
    runners.start(hosts[0])
    run_id = lux.submit(hooked())
    started(lux, run_id)
    lux.run("cancel", run_id)
    lux.wait_state(run_id, "cancelled", timeout=60)
    assert "/.lux/artifacts/final-count" in artifact_paths(lux, run_id)


def test_runs_before_a_timeout_stop(lux, runners, hosts):
    """A stop lux decides on (the Run's time is up) runs it too: no caller
    has to remember to ask for the Run's last state first."""
    runners.start(hosts[0])
    run_id = lux.submit(hooked(timeout="8s"))
    started(lux, run_id)
    lux.wait_state(run_id, "failed", "stopped", "succeeded", "cancelled", timeout=90)
    assert "/.lux/artifacts/final-count" in artifact_paths(lux, run_id)


def test_an_overrunning_hook_is_cut_off_and_the_stop_goes_on(lux, runners, hosts):
    runners.start(hosts[0])
    spec = generic(ALPINE_IMAGE, "sh", "-c", WORK, volumes=WORKSPACE)
    spec["workload"]["beforeStop"] = {"command": ["sh", "-c", "echo slow > $LUX_ARTIFACTS/began; sleep 600"], "timeout": "3s"}
    run_id = lux.submit(spec)
    started(lux, run_id)
    t0 = time.monotonic()
    lux.run("stop", run_id, "--wait")
    assert time.monotonic() - t0 < 30, "the stop waited on the hook past its timeout"
    assert "/.lux/artifacts/began" in artifact_paths(lux, run_id)


def test_a_timeout_past_the_grace_is_refused(lux):
    spec = hooked()
    spec["workload"]["grace"] = "5s"
    spec["workload"]["beforeStop"]["timeout"] = "20s"
    res = lux.run("run", "-f", "-", input=json.dumps(spec), check=False)
    assert res.returncode != 0 and "beforeStop.timeout" in (res.stderr + res.stdout), res

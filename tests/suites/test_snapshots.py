"""Step 3: snapshots, stop and resume, on the same host and on another.

The migration test is the most important test in the suite: start on host
A, write state, steer, stop, resume on host B, and check that the files and
the conversation continue, and that host A's late reports are rejected by
epoch."""

from __future__ import annotations

import time

import pytest

from conftest import CLIError, fake_agent, generic
from env import ALPINE_IMAGE, wait_until


def placement_hosts(run: dict) -> list[str]:
    return [p["hostName"] for p in run["placements"]]


def test_migration(env, lux, runners, hosts, fake_image):
    a, b = hosts[0], hosts[1]
    runners.start(a)
    run_id = lux.submit(fake_agent(fake_image, "write notes.txt first\necho started"))
    lux.wait_output(run_id, "started")
    lux.wait_activity(run_id, "idle")

    lux.run("steer", run_id, "append notes.txt second\necho steered")
    lux.wait_output(run_id, "steered")
    lux.wait_activity(run_id, "idle")
    session = lux.get(run_id)["sessionId"]
    assert session

    lux.run("stop", run_id, "--wait")
    run = lux.get(run_id)
    assert run["state"] == "stopped", run
    assert run["snapshotId"]

    # Host A goes away; the Run must resume on B from the uploaded snapshot.
    lux.wait_uploaded(run_id)
    runners.stop(a)
    runners.start(b)

    lux.run("resume", run_id, "--wait", "--input", "read notes.txt\nhistory")
    lux.wait_output(run_id, "history:")
    run = lux.get(run_id)
    assert placement_hosts(run) == [a.name, b.name], run["placements"]
    assert run["sessionId"] == session, "the conversation continued, not restarted"
    out = lux.logs(run_id)
    # The file written on A, appended to by steering, read on B.
    assert "first\nsecond" in out, out
    # The transcript came along: history includes the prompts from host A.
    history = out[out.index("history:"):]
    assert "write notes.txt first" in history and "append notes.txt second" in history, history

    # Host A comes back and reports about its old epoch: rejected.
    ep1 = run["placements"][0]["epoch"]
    assert run["epoch"] == 2 and ep1 == 1
    stale = stale_report(env, runners.tokens[a.name], a, run_id, ep1)
    assert stale["type"] == "nack" and stale["data"]["stale"], stale


def stale_report(env, token: str, host, run_id: str, epoch: int) -> dict:
    """Send a status report for an old epoch, as host A would after coming
    back, through the runner poll endpoint (fault injection)."""
    import requests
    body = {"acks": [], "reports": [{"type": "status", "id": 1, "runId": run_id, "epoch": epoch,
                                     "data": {"state": "running"}}]}
    r = requests.post(f"{env.luxd_url}/runner/v1/poll?name={host.name}", json=body,
                      headers={"Authorization": f"Bearer {token}"}, timeout=10)
    r.raise_for_status()
    return r.json()["replies"][0]


def test_same_host_resume_moves_nothing(lux, runners, hosts, fake_image):
    runners.start(hosts[0])
    runners.start(hosts[1])
    run_id = lux.submit(fake_agent(fake_image, "write a.txt kept\necho ok"))
    lux.wait_output(run_id, "ok")
    lux.wait_activity(run_id, "idle")
    lux.run("stop", run_id, "--wait")
    first = lux.get(run_id)["placements"][0]["hostName"]
    lux.run("resume", run_id, "--wait", "--input", "read a.txt")
    lux.wait_output(run_id, "kept")
    run = lux.get(run_id)
    assert placement_hosts(run) == [first, first], "affinity: resumed where the snapshot is"
    events = lux.json("events", run_id)
    types = [e["type"] for e in events]
    assert "volumes.local" in types, types
    assert "volumes.restored" not in types, "nothing was downloaded"


def test_generic_state_volume_survives_stop(lux, runners, hosts):
    runners.start(hosts[0])
    spec = generic(ALPINE_IMAGE, "sh", "-c",
                   "echo run >> /data/count; wc -l < /data/count; sleep 300",
                   volumes=[{"name": "data", "path": "/data", "kind": "state"}])
    run_id = lux.submit(spec)
    lux.wait_output(run_id, "1")
    lux.run("stop", run_id, "--wait")
    lux.run("resume", run_id, "--wait")
    out = wait_until(lambda: (lambda o: o if o[-1:] == ["2"] else None)(lux.logs(run_id).split()),
                     30, 0.5, "second placement never counted 2")
    assert out == ["1", "2"], out
    lux.run("cancel", run_id, "--wait")
    assert lux.get(run_id)["state"] == "cancelled"


def test_ephemeral_volume_does_not(lux, runners, hosts):
    runners.start(hosts[0])
    spec = generic(ALPINE_IMAGE, "sh", "-c", "ls /scratch; touch /scratch/x; sleep 300",
                   volumes=[{"name": "scratch", "path": "/scratch", "kind": "ephemeral"}])
    run_id = lux.submit(spec)
    lux.wait_state(run_id, "running")
    time.sleep(1)
    lux.run("stop", run_id, "--wait")
    lux.run("resume", run_id, "--wait")
    time.sleep(2)
    assert "x" not in lux.logs(run_id).split()
    lux.run("cancel", run_id, "--wait")


def test_input_to_a_stopped_run_points_at_resume(lux, runners, hosts, fake_image):
    runners.start(hosts[0])
    run_id = lux.submit(fake_agent(fake_image, "echo hi"))
    lux.wait_output(run_id, "hi")
    lux.run("stop", run_id, "--wait")
    with pytest.raises(CLIError) as e:
        lux.run("steer", run_id, "hello")
    assert "resume" in e.value.stderr


def test_lost_host_and_resume_from_the_previous_snapshot(env, lux, runners, hosts, fake_image):
    """A host dies mid-Run: the Run is lost, and resumable from the last
    snapshot taken before that placement."""
    a, b = hosts[0], hosts[1]
    runners.start(a)
    run_id = lux.submit(fake_agent(fake_image, "write f.txt v1\necho one"))
    lux.wait_output(run_id, "one")
    lux.wait_activity(run_id, "idle")
    lux.run("stop", run_id, "--wait")
    lux.wait_uploaded(run_id)
    lux.run("resume", run_id, "--wait", "--input", "write f.txt v2\necho two\nsleep 600")
    lux.wait_output(run_id, "two")
    # Pull the plug on the runner and freeze the host.
    runners.stop(a, "KILL")
    a.pause()
    try:
        run = lux.wait_state(run_id, "lost", timeout=90)
        assert "heartbeat" in run["stateReason"], run
        runners.start(b)
        lux.run("resume", run_id, "--wait", "--input", "read f.txt")
        lux.wait_output(run_id, "v1")
        out = lux.logs(run_id)
        assert "v2" not in out.split("read f.txt")[-1] if "read f.txt" in out else True
        run = lux.get(run_id)
        assert run["placements"][1]["state"] == "lost"
        assert run["placements"][-1]["hostName"] == b.name
    finally:
        a.unpause()




def test_resume_elsewhere_right_after_stop(lux, runners, hosts, fake_image):
    """Resuming on another host immediately after a stop, possibly before
    the upload finished: the scheduler waits for the upload rather than
    starting from nothing."""
    a, b = hosts[0], hosts[1]
    runners.start(a, "--max-runs", "1")
    run_id = lux.submit(fake_agent(fake_image, "write big.txt data\necho ready"))
    lux.wait_output(run_id, "ready")
    # Occupy A so the resume must go to B.
    blocker = lux.submit(generic(ALPINE_IMAGE, "sleep", "600"))
    lux.run("stop", run_id, "--wait")
    lux.wait_state(blocker, "running")
    runners.start(b)
    lux.run("resume", run_id, "--wait", "--input", "read big.txt")
    lux.wait_output(run_id, "data")
    assert lux.get(run_id)["placements"][-1]["hostName"] == b.name
    lux.run("cancel", blocker)

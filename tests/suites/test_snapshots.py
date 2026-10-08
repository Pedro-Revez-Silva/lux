"""Step 3: snapshots, stop and resume, on the same host and on another.

The migration test is the most important test in the suite: start on host
A, write state, steer, stop, resume on host B, and check that the files and
the conversation continue, and that host A's late reports are rejected by
epoch."""

from __future__ import annotations

import json
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


def test_resume_resizes_a_stopped_run(lux, runners, hosts):
    """A resume with less memory and more CPUs: the container's own cgroup
    limits are the new ones, the state volume's data survived, and the
    Run's spec and resume.requested event say so. A disk smaller than its
    saved state plus headroom is kept, and the Run still resumes; one that
    fits is applied."""
    runners.start(hosts[0], "--usage-every", "1s")
    show = ("echo run >> /data/count; "
            "echo \"count=$(wc -l < /data/count) cpu=$(tr ' ' / < /sys/fs/cgroup/cpu.max) mem=$(cat /sys/fs/cgroup/memory.max)\"; "
            "sleep 300")
    run_id = lux.submit(generic(ALPINE_IMAGE, "sh", "-c", show, resources={"cpus": 1, "memory": "1Gi"},
                                volumes=[{"name": "data", "path": "/data", "kind": "state"}]))
    lux.wait_output(run_id, "count=1")
    lux.run("stop", run_id, "--wait")

    p = lux.run("resume", run_id, "--cpus", "2", "--memory", "512Mi", "--disk", "100Mi", "-o", "json")
    answer = json.loads(p.stdout)
    assert "disk kept:" in p.stderr, p.stderr
    rz = answer["resize"]
    assert rz["applied"]["cpus"] == 2 and rz["applied"]["memory"] == 512 << 20 and rz["applied"]["disk"] == 20 << 30, rz
    assert rz["disk"]["requested"] == 100 << 20 and rz["disk"]["kept"] == 20 << 30 and rz["disk"]["reason"], rz
    out = wait_until(lambda: (lambda o: o if "count=2" in o else None)(lux.logs(run_id)), 60, 0.5,
                     "the resumed Run never counted 2")
    line = next(l for l in out.splitlines() if "count=2" in l)
    assert f"cpu=200000/100000 mem={hosts[0].memory_limit(512 << 20)}" in line, line
    run = lux.get(run_id)
    assert run["spec"]["resources"]["cpus"] == 2 and run["spec"]["resources"]["memory"] == 512 << 20, run["spec"]
    assert run["placements"][-1]["memoryLimit"] == hosts[0].memory_limit(512 << 20)
    ev = lux.events(run_id, "resume.requested")[-1]["data"]["resources"]
    assert ev["requested"] == {"cpus": 2, "memory": 512 << 20, "disk": 100 << 20}, ev
    assert ev["disk"]["kept"] == 20 << 30, ev

    lux.run("stop", run_id, "--wait")
    rz = lux.json("resume", run_id, "--disk", "2Gi")["resize"]
    assert "disk" not in rz and rz["applied"]["disk"] == 2 << 30, rz
    wait_until(lambda: "count=3" in lux.logs(run_id), 60, 0.5, "the third placement never counted 3")
    assert lux.get(run_id)["spec"]["resources"]["disk"] == 2 << 30
    lux.run("cancel", run_id, "--wait")


def test_ephemeral_volume_does_not(lux, runners, hosts):
    runners.start(hosts[0])
    spec = generic(ALPINE_IMAGE, "sh", "-c", "ls /scratch; touch /scratch/x; echo ready; sleep 300",
                   volumes=[{"name": "scratch", "path": "/scratch", "kind": "ephemeral"}])
    run_id = lux.submit(spec)
    lux.wait_output(run_id, "ready")
    lux.run("stop", run_id, "--wait")
    lux.run("resume", run_id, "--wait")
    wait_until(lambda: lux.logs(run_id).split().count("ready") == 2, 30, 0.5, "the resumed Run never listed /scratch")
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


# A podman in front of the host's own (first on PATH, so the runner runs
# it) whose `volume export` first touches /tmp/lux-idlefix-exporting, then
# waits `secs` seconds or until /tmp/lux-idlefix-release exists. Longer
# than the harness's 10s lease, it makes a snapshot outlast the lease.
SLOW_EXPORT = """#!/bin/sh
if [ "$1 $2" = "volume export" ]; then
  touch /tmp/lux-idlefix-exporting
  n=0
  while [ ! -e /tmp/lux-idlefix-release ] && [ $n -lt {ticks} ]; do sleep 0.2; n=$((n+1)); done
fi
exec /usr/bin/podman "$@"
"""


def slow_volume_export(host, secs: int):
    host.exec("sh", "-c", "rm -f /tmp/lux-idlefix-exporting /tmp/lux-idlefix-release; "
              "cat > /usr/local/bin/podman && chmod 755 /usr/local/bin/podman",
              input=SLOW_EXPORT.replace("{ticks}", str(secs * 5)).encode())


def normal_volume_export(host):
    host.exec("sh", "-c", "touch /tmp/lux-idlefix-release; rm -f /usr/local/bin/podman", check=False)


def exporting(host) -> bool:
    return host.exec("sh", "-c", "test -e /tmp/lux-idlefix-exporting && echo y", check=False).strip() == "y"


def counting_spec() -> dict:
    return generic(ALPINE_IMAGE, "sh", "-c", "echo run >> /data/count; wc -l < /data/count; sleep 300",
                   volumes=[{"name": "data", "path": "/data", "kind": "state"}])


def test_snapshot_longer_than_the_lease_is_not_lost(lux, runners, hosts):
    """A stop whose snapshot takes twice the lease, on a host that keeps
    heartbeating: the Run ends stopped, with that snapshot recorded."""
    a = hosts[0]
    runners.start(a)
    run_id = lux.submit(counting_spec())
    lux.wait_output(run_id, "1")
    slow_volume_export(a, 20)
    try:
        started = time.monotonic()
        lux.run("stop", run_id, "--wait", check=False)
        run = lux.get(run_id)
        assert run["state"] == "stopped", run
        assert exporting(a) and time.monotonic() - started >= 20, "the snapshot was not slowed down"
        snaps = lux.json("snapshots", run_id)
        assert [s["epoch"] for s in snaps] == [1], snaps
        assert run["snapshotId"] == snaps[0]["id"], run
    finally:
        normal_volume_export(a)
    lux.run("resume", run_id, "--wait")
    wait_until(lambda: lux.logs(run_id).split()[-1:] == ["2"], 30, 0.5, "second placement never counted 2")
    lux.run("cancel", run_id, "--wait")


def test_host_dead_mid_snapshot_is_still_lost(lux, runners, hosts):
    """A host whose runner dies while it snapshots a stopping Run stops
    renewing its lease: the Run is lost, with no snapshot recorded."""
    a = hosts[0]
    runners.start(a)
    run_id = lux.submit(counting_spec())
    lux.wait_output(run_id, "1")
    slow_volume_export(a, 600)
    try:
        lux.run("stop", run_id)
        wait_until(lambda: exporting(a), 60, 0.5, "the snapshot never started")
        runners.stop(a, "KILL")
        run = lux.wait_state(run_id, "lost", timeout=60)
        assert "heartbeat" in run["stateReason"], run
        assert lux.json("snapshots", run_id) == [], "a snapshot was recorded"
    finally:
        normal_volume_export(a)


def test_resume_after_lost_on_the_same_host(lux, runners, hosts):
    """A Run lost on a host that keeps its stopped container, resumed there:
    a new container is made, and the Run starts from the snapshot before the
    lost placement."""
    a = hosts[0]
    runners.start(a)
    run_id = lux.submit(counting_spec())
    lux.wait_output(run_id, "1")
    lux.run("stop", run_id, "--wait")
    lux.wait_uploaded(run_id)
    lux.run("resume", run_id, "--wait")
    wait_until(lambda: lux.logs(run_id).split()[-1:] == ["2"], 30, 0.5, "second placement never counted 2")

    # The runner dies; its container runs on until the runner, back after
    # luxd gave up on the placement, fences it off and kills it.
    runners.stop(a, "KILL")
    run = lux.wait_state(run_id, "lost", timeout=60)
    assert "heartbeat" in run["stateReason"], run
    runners.start(a)
    wait_until(lambda: a.exec("podman", "ps", "-a", "--filter", f"name=lux-{run_id}",
                              "--format", "{{.State}}").strip() == "exited",
               60, 0.5, "the lost placement's container was not stopped")

    lux.run("resume", run_id, "--wait")
    run = lux.get(run_id)
    assert run["state"] == "running" and run["epoch"] == 3, run
    # From the snapshot of epoch 1: the lost placement's count is gone.
    wait_until(lambda: lux.logs(run_id).split()[-1:] == ["2"], 30, 0.5, "third placement never counted 2")
    ev3 = [e["type"] for e in lux.json("events", run_id) if e.get("epoch") == 3]
    assert "volumes.restored" in ev3, ev3
    lux.run("cancel", run_id, "--wait")




def test_same_host_resume_has_a_new_container(lux, runners, hosts):
    """A resume on the host the Run stopped on keeps its state volume there
    (nothing restored) and starts in a new container, as on another host:
    nothing written outside the volumes, /tmp included, is still there."""
    runners.start(hosts[0])
    script = "[ -e /tmp/mark ] && echo LEAK; date > /tmp/mark; echo run >> /data/count; echo count=$(wc -l < /data/count); sleep 300"
    run_id = lux.submit(generic(ALPINE_IMAGE, "sh", "-c", script,
                                volumes=[{"name": "data", "path": "/data", "kind": "state"}]))
    lux.wait_output(run_id, "count=1")
    lux.run("stop", run_id, "--wait")
    lux.run("resume", run_id, "--wait")
    out = lux.wait_output(run_id, "count=2")
    lux.run("cancel", run_id, "--wait")
    assert "LEAK" not in out, out
    assert lux.events(run_id, "volumes.local") and not lux.events(run_id, "volumes.restored")
    # Documents the event a reused stopped container used to emit; the
    # runner no longer has it, so LEAK above is the guard.
    assert not lux.events(run_id, "container.reused")


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

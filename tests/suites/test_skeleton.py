"""Step 1: the skeleton. A generic command runs in a hardened container on
a host and its exit status comes back, through the CLI."""

from __future__ import annotations

import time

import pytest

from conftest import CLIError, generic
from env import ALPINE_IMAGE


def test_echo_hello(lux, runners, hosts):
    runners.start(hosts[0])
    run_id = lux.submit(generic(ALPINE_IMAGE, "echo", "hello"))
    run = lux.wait_state(run_id, "succeeded", "failed")
    assert run["state"] == "succeeded", run
    assert run["exitCode"] == 0
    assert "hello" in lux.logs(run_id)


def test_run_follow_exits_with_the_runs_code(lux, runners, hosts):
    runners.start(hosts[0])
    p = lux.run("run", "--image", ALPINE_IMAGE, "--follow", "--", "sh", "-c", "echo out; echo err >&2; exit 7", check=False)
    assert p.returncode == 7, p.stderr
    assert "out" in p.stdout
    assert "err" in p.stderr


def test_invalid_spec_lists_every_problem(lux):
    with pytest.raises(CLIError) as e:
        lux.submit({"workload": {"adapter": "nope"}, "env": {"LUX_X": "1"}})
    assert e.value.code == 4
    assert "unknown adapter" in e.value.stderr
    assert "exactly one of ref or build" in e.value.stderr
    assert "LUX_ prefix is reserved" in e.value.stderr


def test_idempotent_submit(lux):
    spec = generic(ALPINE_IMAGE, "true")
    a = lux.submit(spec, "--idempotency-key", "k1")
    b = lux.submit(spec, "--idempotency-key", "k1")
    assert a == b
    assert lux.submit(spec, "--idempotency-key", "k2") != a


def test_container_is_hardened(lux, runners, hosts):
    """Checked from inside: no capabilities for a non-root workload, no
    privilege escalation, its own user namespace, and the limits applied."""
    runners.start(hosts[0])
    script = (
        "grep -E '^(CapEff|NoNewPrivs)' /proc/self/status; "
        "cat /proc/self/uid_map; "
        "cat /sys/fs/cgroup/memory.max /sys/fs/cgroup/pids.max"
    )
    spec = generic(ALPINE_IMAGE, "sh", "-c", script,
                   resources={"memory": "256Mi", "pids": 100, "cpus": 1})
    spec["workload"]["user"] = "nobody"
    run_id = lux.submit(spec)
    assert lux.wait_state(run_id, "succeeded", "failed")["state"] == "succeeded"
    out = lux.logs(run_id)
    assert "CapEff:\t0000000000000000" in out, out
    assert "NoNewPrivs:\t1" in out, out
    uid_map = next(l for l in out.splitlines() if l.strip().startswith("0 "))
    assert int(uid_map.split()[1]) > 0, "container root must not be host root"
    assert str(256 * 1024 * 1024) in out
    assert "100" in out.split()


def test_two_containers_get_different_uid_ranges(lux, runners, hosts):
    runners.start(hosts[0])
    ids = [lux.submit(generic(ALPINE_IMAGE, "sh", "-c", "cat /proc/self/uid_map; sleep 3")) for _ in range(2)]
    ranges = set()
    for i in ids:
        assert lux.wait_state(i, "succeeded", "failed")["state"] == "succeeded"
        ranges.add(lux.logs(i).split()[1])
    assert len(ranges) == 2, ranges


def test_failure_is_reported(lux, runners, hosts):
    runners.start(hosts[0])
    run_id = lux.submit(generic(ALPINE_IMAGE, "sh", "-c", "exit 3"))
    run = lux.wait_state(run_id, "succeeded", "failed")
    assert run["state"] == "failed"
    assert run["exitCode"] == 3
    assert "exit code 3" in run["stateReason"]


def test_unknown_command_fails_cleanly(lux, runners, hosts):
    runners.start(hosts[0])
    run_id = lux.submit(generic(ALPINE_IMAGE, "no-such-binary"))
    run = lux.wait_state(run_id, "succeeded", "failed")
    assert run["state"] == "failed"
    assert "no-such-binary" in lux.logs(run_id, "--events") or "no-such-binary" in run["stateReason"] or run["exitCode"] == 125


def test_telemetry_is_recorded(lux, runners, hosts):
    runners.start(hosts[0])
    run_id = lux.submit(generic(ALPINE_IMAGE, "sh", "-c", "head -c 30000000 /dev/zero > /tmp/x; sleep 12"))
    run = lux.wait_state(run_id, "succeeded", "failed", timeout=60)
    assert run["state"] == "succeeded"
    p = run["placements"][0]
    for field in ("assignedAt", "acceptedAt", "imageReadyAt", "volumesRestoredAt", "containerStartedAt",
                  "workloadStartedAt", "exitedAt", "snapshotDoneAt"):
        assert p.get(field), f"{field} missing: {p}"
    assert run["firstStartedAt"] and run["finishedAt"]
    assert p["peakMemoryBytes"] > 0
    assert run["usage"]["peakMemoryBytes"] > 0
    assert run["usage"]["queueSeconds"] is not None
    hosts_ = lux.json("hosts", "ls")
    h = next(h for h in hosts_ if h["name"] == hosts[0].name)
    assert h["times"]["registered"] and h["times"]["firstPlacement"] and h["lastHeartbeat"]


def test_polling_fallback(lux, runners, hosts):
    """A runner that cannot hold a WebSocket polls, and still runs work."""
    runners.start(hosts[0], "--poll")
    run_id = lux.submit(generic(ALPINE_IMAGE, "echo", "polled"))
    run = lux.wait_state(run_id, "succeeded", "failed")
    assert run["state"] == "succeeded"
    assert "polled" in lux.logs(run_id)


def test_tenants_cannot_see_each_other(tenant_factory, runners, hosts):
    a, b = tenant_factory(), tenant_factory()
    run_id = a.submit(generic(ALPINE_IMAGE, "true"))
    assert run_id not in [r["id"] for r in b.json("ls")]
    with pytest.raises(CLIError) as e:
        b.get(run_id)
    assert e.value.code == 3
    with pytest.raises(CLIError):
        b.run("stop", run_id)


def test_scopes_are_enforced(env, lux):
    key = env.luxd_admin("create-key", "--tenant", lux.tenant_id, "--scopes", "read")["apiKey"]
    from conftest import Lux
    reader = Lux(env, key, lux.tenant_id)
    reader.json("ls")
    with pytest.raises(CLIError) as e:
        reader.submit(generic(ALPINE_IMAGE, "true"))
    assert "scope" in e.value.stderr


def test_resource_defaults_and_requests_reach_the_container(lux, runners, hosts):
    """A Run without resources gets the defaults (2 CPUs, 8 GiB, 1024
    processes); one that asks gets what it asked for — as the container's
    own cgroup limits."""
    runners.start(hosts[0])
    show = "cat /sys/fs/cgroup/cpu.max /sys/fs/cgroup/memory.max /sys/fs/cgroup/pids.max"
    default = lux.submit(generic(ALPINE_IMAGE, "sh", "-c", show))
    lux.wait_state(default, "succeeded")
    out = lux.logs(default).split()  # cpu.max is "<quota> <period>"
    assert out[:2] == ["200000", "100000"] and int(out[2]) == 8 << 30 and int(out[3]) == 1024, out
    spec = lux.get(default)["spec"]["resources"]
    assert spec["cpus"] == 2 and spec["memory"] == 8 << 30 and spec["pids"] == 1024, spec

    asked = lux.submit(generic(ALPINE_IMAGE, "sh", "-c", show, resources={"cpus": 0.5, "memory": "512Mi", "pids": 64}))
    lux.wait_state(asked, "succeeded")
    out = lux.logs(asked).split()
    assert out[:2] == ["50000", "100000"] and int(out[2]) == 512 << 20 and int(out[3]) == 64, out


def test_a_run_over_its_disk_limit_is_stopped_and_fails(lux, runners, hosts):
    """resources.disk bounds what a Run writes (its writable layer and state
    volumes): past it, the Run is stopped and fails, its state kept."""
    runners.start(hosts[0], "--usage-every", "1s")
    script = "echo start; dd if=/dev/zero of=/w/big bs=1M count=64 2>/dev/null; echo wrote; sleep 600"
    run_id = lux.submit(generic(ALPINE_IMAGE, "sh", "-c", script, resources={"disk": "32Mi"},
                                volumes=[{"name": "w", "path": "/w", "kind": "state"}]))
    run = lux.wait_state(run_id, "failed", timeout=60)
    assert run["stateReason"] == "disk limit exceeded", run
    assert run["placements"][-1]["stopReason"] == "disk", run["placements"]
    exceeded = lux.events(run_id, "disk.exceeded")
    assert exceeded and exceeded[0]["data"]["limitBytes"] == 32 << 20, exceeded
    assert lux.json("snapshots", run_id), "its state was not kept"
    # Resumed with room to spare: it runs on, its 64 MiB still there.
    lux.run("resume", run_id, "--disk", "256Mi")
    lux.wait_output(run_id, "start\nwrote\nstart", timeout=60)
    run = lux.get(run_id)
    assert run["state"] == "running" and run["spec"]["resources"]["disk"] == 256 << 20, run
    lux.run("cancel", run_id, "--wait")

    # Under the limit: untouched. The default is 20 GiB.
    ok = lux.submit(generic(ALPINE_IMAGE, "sh", "-c", "dd if=/dev/zero of=/tmp/f bs=1M count=8 2>/dev/null; sleep 3"))
    run = lux.wait_state(ok, "succeeded", timeout=60)
    assert run["spec"]["resources"]["disk"] == 20 << 30, run["spec"]["resources"]


def test_disk_is_reserved_on_the_host(lux, runners, hosts):
    """The scheduler reserves resources.disk against what the host offers."""
    runners.start(hosts[0], "--disk", str(1 << 30))
    big = lux.submit(generic(ALPINE_IMAGE, "sleep", "600", resources={"disk": "768Mi"}))
    lux.wait_state(big, "running", timeout=60)
    second = lux.submit(generic(ALPINE_IMAGE, "true", resources={"disk": "512Mi"}))
    from env import wait_until
    wait_until(lambda: lux.get(second).get("stateReason") == "waiting for capacity", 20, 0.5, "not held for disk")
    lux.run("cancel", big, "--wait")
    lux.wait_state(second, "succeeded", timeout=60)


def test_a_timeout_counts_running_time_only(lux, runners, hosts):
    """A Run's timeout is time spent running, over its placements: parked
    (stopped) time does not count, so a Run resumed later keeps what it had
    left. With no timeout there is no limit at all."""
    runners.start(hosts[0])
    spec = generic(ALPINE_IMAGE, "sh", "-c", "echo up; trap 'exit 0' TERM; while :; do sleep 1; done",
                   volumes=[{"name": "w", "path": "/w", "kind": "state"}])
    spec["timeout"] = "12s"
    run_id = lux.submit(spec)
    lux.wait_output(run_id, "up", timeout=60)
    time.sleep(4)
    lux.run("stop", run_id, "--wait")
    time.sleep(14)  # parked past the whole timeout: it must not count
    lux.run("resume", run_id, "--wait")
    time.sleep(3)  # about 8s of running so far
    assert lux.get(run_id)["state"] == "running", "stopped on resume for time it spent parked"
    run = lux.wait_state(run_id, "failed", timeout=30)
    assert run["stateReason"] == "timeout", run

    # No timeout: never stopped for time; nothing else stops it here.
    spec.pop("timeout")
    free = lux.submit(spec)
    lux.wait_output(free, "up", timeout=60)
    assert lux.get(free)["spec"].get("timeout") in (None, "0s"), lux.get(free)["spec"]
    time.sleep(5)
    assert lux.get(free)["state"] == "running"
    lux.run("cancel", free, "--wait")

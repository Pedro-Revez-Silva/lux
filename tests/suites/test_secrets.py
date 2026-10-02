"""Step 8: secrets. Values come with the submit and resume requests and are
never stored: not in the database, not in output, events or logs, not in
snapshots. File secrets live on a tmpfs. A resume must supply them again,
and may rotate them."""

from __future__ import annotations

import base64
import json
from pathlib import Path

import psycopg
import pytest
from compression import zstd

from conftest import CLIError, fake_agent, generic
from env import ALPINE_IMAGE, wait_until

TOKEN = "lux-test-secret-7f3a9c2e1b"


def everything_stored(env, hosts) -> str:
    """Every place lux keeps anything: the database, every object in the
    bucket (decompressed), and the luxd and runner logs."""
    parts = []
    with psycopg.connect(env.owner_dsn) as conn:
        for table in ("runs", "run_events", "placements", "snapshots", "blobs", "artifacts", "host_messages", "outbox"):
            for row in conn.execute(f"SELECT to_jsonb(t)::text FROM {table} t"):
                parts.append(row[0])
    s3 = env.s3()
    for obj in s3.list_objects_v2(Bucket=env.bucket).get("Contents", []):
        body = s3.get_object(Bucket=env.bucket, Key=obj["Key"])["Body"].read()
        if body[:4] == b"\x28\xb5\x2f\xfd":
            body = zstd.decompress(body)
        parts.append(body.decode("latin-1"))
    parts.append((Path(env.log_dir) / "luxd.log").read_text(errors="replace"))
    for h in hosts:
        log = Path(h.log_dir) / "runner.log"
        if log.exists():
            parts.append(log.read_text(errors="replace"))
    return "\n".join(parts)


def test_values_are_redacted_and_never_stored(env, lux, runners, hosts):
    """The workload prints its secret raw and base64-encoded; both come out
    redacted. After a stop (a snapshot) the value is nowhere lux keeps
    anything. The env secret is really set: the redaction is not an empty
    variable."""
    runners.start(hosts[0])
    b64 = base64.b64encode(TOKEN.encode()).decode()
    script = ('echo "raw=$TOKEN"; printf %s "$TOKEN" | base64; echo "len=${#TOKEN}"; '
              'echo "file=$(cat /home/agent/.token)"; mkdir -p /workspace; echo kept > /workspace/f; sleep 300')
    run_id = lux.submit(generic(ALPINE_IMAGE, "sh", "-c", script,
                                volumes=[{"name": "workspace", "path": "/workspace", "kind": "state"}],
                                secrets=[{"name": "TOKEN", "value": TOKEN},
                                         {"name": "TOKFILE", "value": TOKEN + "-file", "as": "file", "path": "/home/agent/.token"}]))
    out = lux.wait_output(run_id, "file=")
    assert f"len={len(TOKEN)}" in out, out
    assert "raw=[REDACTED:TOKEN]" in out, out
    assert "file=[REDACTED:TOKFILE]" in out, out
    assert TOKEN not in out and b64 not in out, out
    lux.run("stop", run_id, "--wait")
    lux.wait_uploaded(run_id)
    for view in (lux.run("get", run_id, "-o", "json").stdout, lux.run("events", run_id, "-o", "json").stdout, lux.logs(run_id)):
        assert TOKEN not in view
    stored = everything_stored(env, hosts)
    assert "kept" in stored, "the snapshot must be in the bucket for this check to mean anything"
    assert TOKEN not in stored and b64 not in stored


def test_file_secrets_are_on_a_tmpfs(lux, runners, hosts):
    """A file secret is a link into the secrets tmpfs, readable by the
    workload's user only."""
    runners.start(hosts[0])
    script = "readlink /home/agent/.token; stat -c '%a %U' $(readlink /home/agent/.token); grep ' /.lux/secrets ' /proc/mounts"
    run_id = lux.submit(generic(ALPINE_IMAGE, "sh", "-c", script,
                                secrets=[{"name": "TOKFILE", "value": TOKEN, "as": "file", "path": "/home/agent/.token"}]))
    lux.wait_state(run_id, "succeeded", "failed")
    out = lux.logs(run_id)
    assert "/.lux/secrets/TOKFILE" in out, out
    assert "tmpfs" in out, out


def test_resume_requires_the_secrets_and_can_rotate_them(lux, runners, hosts):
    """A resume without the Run's secrets is refused before scheduling. With
    a new value, the resumed workload gets it, and output redacts it (not
    only the old one)."""
    runners.start(hosts[0])
    first, second = "first-value-1", "second-value-22"
    run_id = lux.submit(generic(ALPINE_IMAGE, "sh", "-c", 'echo "len=${#TOKEN} raw=$TOKEN"; sleep 300',
                                secrets=[{"name": "TOKEN", "value": first}]))
    lux.wait_output(run_id, f"len={len(first)} ")
    lux.run("stop", run_id, "--wait")

    with pytest.raises(CLIError) as e:
        lux.run("resume", run_id)
    assert "TOKEN" in e.value.stderr, e.value.stderr
    assert lux.get(run_id)["state"] == "stopped"

    lux.run("resume", run_id, "--secret", f"TOKEN={second}")
    out = lux.wait_output(run_id, f"len={len(second)} ")
    assert f"len={len(second)} raw=[REDACTED:TOKEN]" in out, out
    assert first not in out and second not in out, out
    lux.run("cancel", run_id)


def test_resume_declares_and_removes_secrets(lux, runners, hosts):
    """A resume's value for a name the Run lacks declares it (env): the
    workload has it from then on, output redacts it, and later resumes need
    it. --remove-secret takes it out again: no longer needed, no longer set."""
    runners.start(hosts[0])
    extra = "extra-value-333"
    script = 'echo "len=${#EXTRA} extra=${EXTRA-unset}"; sleep 300'
    run_id = lux.submit(generic(ALPINE_IMAGE, "sh", "-c", script, secrets=[{"name": "TOKEN", "value": TOKEN}]))
    lux.wait_output(run_id, "len=0 extra=unset")
    lux.run("stop", run_id, "--wait")

    lux.run("resume", run_id, "--secret", f"TOKEN={TOKEN}", "--secret", f"EXTRA={extra}")
    out = lux.wait_output(run_id, f"len={len(extra)} ")
    assert f"len={len(extra)} extra=[REDACTED:EXTRA]" in out, out
    assert extra not in out, out
    assert sorted(s["name"] for s in lux.get(run_id)["secrets"]) == ["EXTRA", "TOKEN"]
    lux.run("stop", run_id, "--wait")

    with pytest.raises(CLIError) as e:
        lux.run("resume", run_id, "--secret", f"TOKEN={TOKEN}")
    assert "EXTRA" in e.value.stderr, e.value.stderr
    assert lux.get(run_id)["state"] == "stopped"

    lux.run("resume", run_id, "--secret", f"TOKEN={TOKEN}", "--remove-secret", "EXTRA")
    wait_until(lambda: lux.logs(run_id).count("len=0 extra=unset") == 2, 60, 0.5, "the workload still has EXTRA")
    run = lux.get(run_id)
    assert [s["name"] for s in run["secrets"]] == ["TOKEN"], run["secrets"]
    assert [s["name"] for s in run["spec"]["secrets"]] == ["TOKEN"], run["spec"]["secrets"]
    lux.run("cancel", run_id)


def test_resume_cannot_remove_a_git_credential(lux, runners, hosts, git_server):
    """A git credential stays while its repository does: removing it is
    refused, and the Run is left as it was."""
    git_server.create("cred", {"a.txt": "x\n"})
    runners.start(hosts[0])
    run_id = lux.submit(generic(ALPINE_IMAGE, "sh", "-c", "sleep 300",
                                volumes=[{"name": "workspace", "path": "/workspace", "kind": "state"}],
                                git={"repositories": [{"name": "cred", "url": git_server.url("cred"), "credential": "GIT_TOKEN"}]},
                                secrets=[{"name": "GIT_TOKEN", "value": git_server.token}]))
    lux.wait_state(run_id, "running")
    lux.run("stop", run_id, "--wait")
    with pytest.raises(CLIError) as e:
        lux.run("resume", run_id, "--remove-secret", "GIT_TOKEN")
    assert e.value.code == 4 and "GIT_TOKEN" in e.value.stderr and "credential" in e.value.stderr, e.value.stderr
    run = lux.get(run_id)
    assert run["state"] == "stopped" and [s["name"] for s in run["secrets"]] == ["GIT_TOKEN"], run


def test_secrets_lost_with_luxd_stop_the_run(env, lux, runners, hosts):
    """luxd keeps secret values only in memory. A queued Run whose values a
    restarted luxd no longer has stops (after a grace period), resumable
    with them, and runs once they are supplied."""
    run_id = lux.submit(generic(ALPINE_IMAGE, "sh", "-c", 'echo "len=${#TOKEN}"',
                                secrets=[{"name": "TOKEN", "value": TOKEN}]))
    assert lux.get(run_id)["state"] == "submitted"
    env.stop_luxd()
    env.start_luxd()
    runners.start(hosts[0])
    run = wait_until(lambda: (r := lux.get(run_id))["state"] == "stopped" and r, 90, 1, "the Run did not stop")
    assert "secrets" in run.get("stateReason", ""), run
    lux.run("resume", run_id, "--secret", f"TOKEN={TOKEN}")
    lux.wait_state(run_id, "succeeded")
    assert f"len={len(TOKEN)}" in lux.logs(run_id)

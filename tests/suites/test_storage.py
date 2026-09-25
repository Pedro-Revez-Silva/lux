"""Step 5: where bytes live. Blobs stream through luxd into S3 (runners
hold no S3 credentials); downloads are presigned; hosts drop local copies
they no longer need; retention deletes finished Runs' blobs; storage
quotas are enforced."""

from __future__ import annotations

import pytest

from conftest import CLIError, fake_agent, generic
from env import ALPINE_IMAGE, MINIO_PASSWORD, wait_until


def s3_keys(env, run_id: str) -> list[str]:
    objs = env.s3().list_objects_v2(Bucket=env.bucket).get("Contents", [])
    return [o["Key"] for o in objs if f"/runs/{run_id}/" in o["Key"]]


def host_blob_files(host) -> list[str]:
    return host.exec("sh", "-c", "ls /var/lib/lux/snapshots 2>/dev/null; true").split()


def test_blobs_land_in_s3_scoped_by_tenant(env, lux, runners, hosts, fake_image):
    runners.start(hosts[0])
    run_id = lux.submit(fake_agent(fake_image, "write f.txt x\necho ok"))
    lux.wait_activity(run_id, "idle")
    lux.run("stop", run_id, "--wait")
    snap = lux.wait_uploaded(run_id)
    keys = s3_keys(env, run_id)
    assert keys and all(k.startswith(f"tenants/{lux.tenant_id}/runs/{run_id}/") for k in keys), keys
    vol_blobs = {v["blobId"] for v in snap["manifest"]["volumes"]}
    assert vol_blobs <= {k.rsplit("/", 1)[1] for k in keys}


def test_runners_hold_no_s3_credentials(env, runners, hosts, lux):
    runners.start(hosts[0])
    procenv = hosts[0].exec("sh", "-c", "cat /proc/$(cat /run/lux-runner.pid)/environ | tr '\\0' '\\n'")
    assert MINIO_PASSWORD not in procenv
    assert "S3" not in procenv


def test_another_tenant_cannot_see_snapshots(env, tenant_factory, runners, hosts, lux):
    runners.start(hosts[0])
    run_id = lux.submit(generic(ALPINE_IMAGE, "true"))
    lux.wait_state(run_id, "succeeded")
    lux.wait_placement_uploaded(run_id)
    assert tenant_factory().api(f"/v1/runs/{run_id}/snapshots").status_code == 404


def test_a_runner_cannot_download_a_run_it_does_not_hold(env, lux, runners, hosts, fake_image):
    """GET /runner/v1/blobs/{id} only serves blobs of Runs placed on the asking
    host, and redirects to a presigned URL."""
    a, b = hosts[0], hosts[1]
    runners.start(a)
    run_id = lux.submit(fake_agent(fake_image, "echo hi"))
    lux.wait_activity(run_id, "idle")
    lux.run("stop", run_id, "--wait")
    snap = lux.wait_uploaded(run_id)
    blob = snap["manifest"]["volumes"][0]["blobId"]
    runners.start(b)
    tok_b = runners.tokens[b.name]
    r = lux.api(f"/runner/v1/blobs/{blob}?host={b.name}", tok_b, allow_redirects=False)
    assert r.status_code == 404, r.text
    # Once the Run is placed on b, b may fetch it, through a presigned URL.
    runners.stop(a)
    lux.run("resume", run_id, "--wait")
    r = lux.api(f"/runner/v1/blobs/{blob}?host={b.name}", tok_b, allow_redirects=False)
    assert r.status_code == 302 and "X-Amz-Signature" in r.headers["Location"], r.status_code


def test_old_host_drops_its_copy_after_a_move(lux, runners, hosts, fake_image):
    """When a Run resumes elsewhere from S3, the host that held its previous
    snapshot deletes it."""
    a, b = hosts[0], hosts[1]
    runners.start(a)
    run_id = lux.submit(fake_agent(fake_image, "write f.txt x\necho ok"))
    lux.wait_activity(run_id, "idle")
    lux.run("stop", run_id, "--wait")
    lux.wait_uploaded(run_id)
    assert host_blob_files(a), "host a kept no local copy"
    # Make a unavailable for placement but alive to hear the discard.
    runners.start(b)
    lux.run("hosts", "drain", a.name)
    lux.run("resume", run_id, "--wait")
    wait_until(lambda: not host_blob_files(a), 30, 0.5, "host a kept its copy")
    vols = a.podman("volume", "ls", "--format", "{{.Name}}")
    assert run_id not in vols
    assert not lux.json("snapshots", run_id)[0].get("onHost")


def test_host_ttl_removes_local_copies(lux, runners, hosts):
    runners.start(hosts[0], "--host-ttl", "2s")
    run_id = lux.submit(generic(ALPINE_IMAGE, "echo", "x", volumes=[{"name": "d", "path": "/d"}]))
    lux.wait_state(run_id, "succeeded")
    lux.wait_placement_uploaded(run_id)
    wait_until(lambda: not host_blob_files(hosts[0]), 30, 0.5, "local copy outlived the TTL")
    # luxd hears about it with the next heartbeat: no stale affinity.
    wait_until(lambda: not lux.json("snapshots", run_id)[0].get("onHost"), 30, 0.5,
               "luxd still thinks the host holds the copy")
    # Output still served, from S3.
    assert lux.logs(run_id).strip() == "x"


def test_retention_deletes_finished_runs_blobs(env, lux, runners, hosts):
    """Retention is per tenant, in days; 0 deletes a finished Run's blobs
    as soon as they are uploaded."""
    env.luxd_admin("set-quota", "--tenant", lux.tenant_id, "--retention-days", "0")
    runners.start(hosts[0])
    run_id = lux.submit(generic(ALPINE_IMAGE, "echo", "gone-soon"))
    lux.wait_state(run_id, "succeeded")
    wait_until(lambda: not s3_keys(env, run_id) and lux.get(run_id)["placements"][0].get("uploadedAt"),
               60, 1, "blobs were not uploaded and then deleted")
    # The Run itself (and its events) stays; only its bytes are gone, and
    # its snapshots say so: resuming it is refused, not broken.
    assert lux.get(run_id)["state"] == "succeeded"
    assert not any(sn["available"] for sn in lux.json("snapshots", run_id))


def test_storage_quota(env, lux, runners, hosts):
    runners.start(hosts[0])
    run_id = lux.submit(generic(ALPINE_IMAGE, "sh", "-c", "head -c 200000 /dev/urandom > /d/f",
                                volumes=[{"name": "d", "path": "/d"}]))
    lux.wait_state(run_id, "succeeded")
    lux.wait_placement_uploaded(run_id)
    env.luxd_admin("set-quota", "--tenant", lux.tenant_id, "--max-storage", "100000")
    with pytest.raises(CLIError) as e:
        lux.submit(generic(ALPINE_IMAGE, "true"))
    assert e.value.code == 5 and "quota" in e.value.stderr


def test_resuming_a_failed_run_after_retention_says_why(env, lux, runners, hosts):
    """A failed Run is resumable; once retention has taken its snapshot,
    resuming it ends as lost with the reason, rather than failing to
    restore on a host."""
    env.luxd_admin("set-quota", "--tenant", lux.tenant_id, "--retention-days", "0")
    runners.start(hosts[0])
    run_id = lux.submit(generic(ALPINE_IMAGE, "sh", "-c", "exit 1", volumes=[{"name": "d", "path": "/d"}]))
    lux.wait_state(run_id, "failed")
    wait_until(lambda: not any(sn["available"] for sn in lux.json("snapshots", run_id)), 60, 1,
               "retention never took the snapshot")
    lux.run("resume", run_id)
    run = lux.wait_state(run_id, "lost")
    assert "snapshot" in run["stateReason"], run

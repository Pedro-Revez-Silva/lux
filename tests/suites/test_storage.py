"""Step 5: where bytes live. Blobs stream through luxd into S3 (runners
hold no S3 credentials); downloads are presigned; hosts drop local copies
they no longer need; retention deletes finished Runs' blobs; storage
quotas are enforced."""

from __future__ import annotations

import pytest

from conftest import CLIError, fake_agent, generic
from env import ALPINE_IMAGE, S3_SECRET_KEY, wait_until


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
    assert S3_SECRET_KEY not in procenv
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
    """Retention is per tenant, in days; 0 deletes a terminated Run's blobs
    as soon as they are uploaded (retention runs once a minute)."""
    env.luxd_admin("set-quota", "--tenant", lux.tenant_id, "--retention-days", "0")
    runners.start(hosts[0])
    run_id = lux.submit(generic(ALPINE_IMAGE, "echo", "gone-soon"))
    lux.wait_state(run_id, "succeeded")
    lux.wait_placement_uploaded(run_id)
    lux.run("terminate", run_id, "--wait")
    wait_until(lambda: not s3_keys(env, run_id), 120, 1, "blobs were not deleted")
    # The Run itself (and its events) stays; only its bytes are gone, and
    # its snapshots say so.
    assert lux.get(run_id)["state"] == "terminated"
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


def test_retention_spares_resumable_runs(env, lux, runners, hosts):
    """A failed or succeeded Run is resumable, so retention leaves its
    snapshot alone even at 0 days, while a terminated Run of the same tenant
    loses its own. The failed one still resumes from its own state. It goes
    only once it expires (terminated), then after retention like any
    terminated Run."""
    env.luxd_admin("set-quota", "--tenant", lux.tenant_id, "--retention-days", "0")
    runners.start(hosts[0])
    failed = lux.submit(generic(ALPINE_IMAGE, "sh", "-c", "test -f /d/f && exit 0; touch /d/f; exit 1",
                                volumes=[{"name": "d", "path": "/d"}]))
    lux.wait_state(failed, "failed")
    lux.wait_placement_uploaded(failed)
    ok = lux.submit(generic(ALPINE_IMAGE, "echo", "kept"))
    lux.wait_state(ok, "succeeded")
    lux.wait_placement_uploaded(ok)
    done = lux.submit(generic(ALPINE_IMAGE, "echo", "gone-soon"))
    lux.wait_state(done, "succeeded")
    lux.wait_placement_uploaded(done)
    lux.run("terminate", done, "--wait")
    # Retention (once a minute) has run over this tenant after the others ended.
    wait_until(lambda: not s3_keys(env, done), 120, 1, "retention never deleted the terminated Run")
    assert s3_keys(env, ok), "retention deleted a succeeded Run's blobs"
    assert any(sn["available"] for sn in lux.json("snapshots", failed)), lux.json("snapshots", failed)
    assert s3_keys(env, failed)
    lux.run("resume", failed)
    # /d/f came back from the snapshot, so the script exits 0 this time.
    lux.wait_state(failed, "succeeded")

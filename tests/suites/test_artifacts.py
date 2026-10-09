"""Step 12: artifacts. Files a Run produces: published while it runs with
`lux-shim publish`, or collected from artifacts.paths on every exit;
uploaded like snapshots, versioned per path, listed and downloaded through
lux — as the files the Run wrote."""

from __future__ import annotations

import hashlib
import json
import shlex

import pytest

from conftest import CLIError, generic
from env import ALPINE_IMAGE, wait_until

WORKSPACE = [{"name": "workspace", "path": "/workspace", "kind": "state"}]
PUBLISH = "/.lux/bin/lux-shim publish"


def artifacts_run(lux, script: str, paths: list[str], user: str | None = None) -> str:
    spec = generic(ALPINE_IMAGE, "sh", "-c", script, volumes=WORKSPACE, artifacts={"paths": paths})
    if user:
        spec["workload"]["user"] = user
    return lux.submit(spec)


def long_run(lux, runners, hosts, user: str | None = None) -> str:
    """A Run that keeps running, ready for lux exec."""
    runners.start(hosts[0])
    run_id = artifacts_run(lux, "mkdir -p /workspace && echo ready && sleep 600", [], user=user)
    lux.wait_output(run_id, "ready")
    return run_id


def wait_available(lux, run_id: str, n: int, *args: str) -> list[dict]:
    return wait_until(lambda: (a := lux.json("artifacts", run_id, *args)) and len(a) >= n and all(x["available"] for x in a) and a,
                      60, 0.5, "artifacts never became available")


def sh(lux, run_id: str, script: str, check: bool = True):
    """script in the Run's container, as its workload user."""
    return lux.run("exec", run_id, "-T", "--", "sh", "-c", script, input="", check=check)


def publish(lux, run_id: str, path: str, *flags: str, check: bool = True):
    return sh(lux, run_id, " ".join([PUBLISH, shlex.quote(path), *map(shlex.quote, flags)]), check=check)


def published_events(lux, run_id: str) -> list[dict]:
    return [e["data"] for e in lux.events(run_id, "artifact.published")]


def test_collected_listed_and_downloaded_as_written(lux, runners, hosts, tmp_path):
    runners.start(hosts[0])
    script = ("mkdir -p /workspace/out/sub && echo report > /workspace/out/report.txt && "
              "head -c 200000 /dev/urandom > /workspace/out/sub/data.bin && "
              "echo '{\"ok\":true}' > /workspace/out/result.json && echo skip > /workspace/other.txt && "
              f"echo published > /tmp/note.md && {PUBLISH} /tmp/note.md")
    run_id = artifacts_run(lux, script, ["/workspace/out/**"])
    lux.wait_state(run_id, "succeeded")
    arts = wait_available(lux, run_id, 4)
    paths = sorted(a["path"] for a in arts)
    assert paths == ["/.lux/artifacts/note.md", "/workspace/out/report.txt", "/workspace/out/result.json",
                     "/workspace/out/sub/data.bin"], paths
    types = {a["path"]: a["contentType"] for a in arts}
    assert types["/workspace/out/result.json"].startswith("application/json"), types

    out = lux.run("artifacts", run_id, "--download", str(tmp_path)).stdout
    assert "report.txt" in out, out
    base = tmp_path / "1"
    assert (base / "workspace/out/report.txt").read_text() == "report\n"
    assert (base / ".lux/artifacts/note.md").read_text() == "published\n"
    assert len((base / "workspace/out/sub/data.bin").read_bytes()) == 200000
    # Each became downloadable once, published and globbed alike.
    evs = wait_until(lambda: (e := published_events(lux, run_id)) and len(e) >= 4 and e, 30, 0.5, "artifact.published events")
    assert sorted(e["path"] for e in evs) == paths, evs


def test_artifacts_match_what_the_workload_wrote(lux, runners, hosts, tmp_path):
    runners.start(hosts[0])
    script = ("mkdir -p /workspace/out && head -c 65536 /dev/urandom > /workspace/out/r.bin && "
              "sha256sum /workspace/out/r.bin | cut -d' ' -f1")
    run_id = artifacts_run(lux, script, ["/workspace/out/*.bin"])
    lux.wait_state(run_id, "succeeded")
    want = lux.logs(run_id).strip()
    art = wait_available(lux, run_id, 1)[0]
    # The listing describes the file, not how lux stores it.
    assert art["sha256"] == want and art["size"] == 65536, art
    lux.run("artifacts", run_id, "--download", str(tmp_path))
    got = tmp_path / "1/workspace/out/r.bin"
    assert hashlib.sha256(got.read_bytes()).hexdigest() == want
    assert got.stat().st_mode & 0o044, "downloaded files are readable like any other"


def test_published_while_the_run_keeps_running(lux, runners, hosts, tmp_path):
    """A published file is listed with its description and downloads as
    written while the Run goes on; artifact.published says so, once."""
    run_id = long_run(lux, runners, hosts, user="nobody")
    sh(lux, run_id, "head -c 70000 /dev/urandom > /tmp/notes.md")
    want = sh(lux, run_id, "sha256sum /tmp/notes.md | cut -d' ' -f1").stdout.strip()
    res = publish(lux, run_id, "/tmp/notes.md", "--name", "design/notes.md", "--description", "The invoice design")
    reply = json.loads(res.stdout)
    assert reply["name"] == "design/notes.md" and reply["size"] == 70000 and reply["sha256"] == want, reply
    assert reply["id"].startswith("art_"), reply

    art = wait_available(lux, run_id, 1)[0]
    assert art["id"] == reply["id"] and art["path"] == "/.lux/artifacts/design/notes.md", art
    assert art["description"] == "The invoice design" and art["version"] == 1 and art["sha256"] == want, art
    lux.run("artifacts", run_id, "--download", str(tmp_path))
    assert hashlib.sha256((tmp_path / "1/.lux/artifacts/design/notes.md").read_bytes()).hexdigest() == want
    evs = wait_until(lambda: published_events(lux, run_id), 30, 0.5, "no artifact.published event")
    assert evs == [{"artifactId": reply["id"], "path": "/.lux/artifacts/design/notes.md", "name": "design/notes.md", "version": 1,
                    "description": "The invoice design", "size": 70000, "sha256": want, "contentType": art["contentType"]}], evs
    assert lux.get(run_id)["state"] == "running"

    # Still exactly once after the Run stops and its placement is uploaded.
    lux.run("stop", run_id, "--wait")
    lux.wait_placement_uploaded(run_id)
    assert len(published_events(lux, run_id)) == 1


def test_republishing_a_name_is_a_new_version(lux, runners, hosts, tmp_path):
    """Nothing is overwritten. Changing or deleting the source right after
    publishing changes nothing either: the shim took its own copy."""
    run_id = long_run(lux, runners, hosts)
    sh(lux, run_id, "echo one > /tmp/a.txt")
    publish(lux, run_id, "/tmp/a.txt", "--name", "a.txt")
    sh(lux, run_id, f"echo two > /tmp/a.txt && {PUBLISH} /tmp/a.txt --description second && echo three > /tmp/a.txt")
    sh(lux, run_id, f"echo gone > /tmp/b.txt && {PUBLISH} /tmp/b.txt && rm /tmp/b.txt")

    all_versions = wait_available(lux, run_id, 3, "--all-versions")
    assert sorted((a["path"], a["version"]) for a in all_versions) == [
        ("/.lux/artifacts/a.txt", 1), ("/.lux/artifacts/a.txt", 2), ("/.lux/artifacts/b.txt", 1)], all_versions
    latest = lux.json("artifacts", run_id)
    assert sorted((a["path"], a["version"], a["description"]) for a in latest) == [
        ("/.lux/artifacts/a.txt", 2, "second"), ("/.lux/artifacts/b.txt", 1, "")], latest
    table = lux.run("artifacts", run_id).stdout
    assert "second" in table and "VERSION" in table, table

    lux.run("artifacts", run_id, "--download", str(tmp_path / "latest"))
    assert (tmp_path / "latest/1/.lux/artifacts/a.txt").read_text() == "two\n"
    assert (tmp_path / "latest/1/.lux/artifacts/b.txt").read_text() == "gone\n"
    lux.run("artifacts", run_id, "--all-versions", "--download", str(tmp_path / "all"))
    assert (tmp_path / "all/1/.lux/artifacts/a.txt.v1").read_text() == "one\n"
    assert (tmp_path / "all/1/.lux/artifacts/a.txt.v2").read_text() == "two\n"
    wait_until(lambda: len(published_events(lux, run_id)) >= 3, 30, 0.5, "artifact.published events")
    assert len(published_events(lux, run_id)) == 3
    # Still exactly three once every upload is in: no late duplicate.
    lux.run("stop", run_id, "--wait")
    lux.wait_placement_uploaded(run_id)
    assert len(published_events(lux, run_id)) == 3


def test_only_publishing_makes_an_artifact(lux, runners, hosts):
    """$LUX_ARTIFACTS is gone; a non-root workload cannot write into the
    shim's staging directory, and a file written anywhere without
    publishing it (the old directory included) is collected by nobody."""
    run_id = long_run(lux, runners, hosts, user="nobody")
    assert sh(lux, run_id, 'echo "[${LUX_ARTIFACTS-unset}]"').stdout.strip() == "[unset]"
    assert sh(lux, run_id, "touch /.lux/run/artifacts/x", check=False).returncode != 0
    assert sh(lux, run_id, "mkdir -p /.lux/run/artifacts/sub", check=False).returncode != 0
    sh(lux, run_id, "echo x > /workspace/not-published.txt")
    lux.run("stop", run_id, "--wait")
    lux.wait_placement_uploaded(run_id)
    assert lux.json("artifacts", run_id, "--all-versions") == []

    # A root workload can write in the old place; nothing collects that.
    script = "mkdir -p /.lux/run/artifacts && echo old > /.lux/run/artifacts/old.txt && echo wrote"
    run_id = artifacts_run(lux, script, [])
    lux.wait_state(run_id, "succeeded")
    assert "wrote" in lux.logs(run_id)
    lux.wait_placement_uploaded(run_id)
    assert lux.json("artifacts", run_id, "--all-versions") == []


def test_bad_names_and_oversized_files_are_refused(lux, runners, hosts):
    run_id = long_run(lux, runners, hosts, user="nobody")
    sh(lux, run_id, "echo x > /tmp/x")
    for name, why in [("../x", ".."), ("/etc/x", "relative"), ("a//b", "clean")]:
        res = publish(lux, run_id, "/tmp/x", "--name", name, check=False)
        assert res.returncode != 0 and why in res.stderr, (name, res.stdout, res.stderr)
    # Over the 1 GiB cap by one byte (sparse: nothing is written to disk);
    # refused from its size, never truncated.
    sh(lux, run_id, "truncate -s 1073741825 /tmp/big")
    res = publish(lux, run_id, "/tmp/big", check=False)
    assert res.returncode != 0 and "over the 1073741824-byte limit" in res.stderr, (res.stdout, res.stderr)
    res = publish(lux, run_id, "/tmp/missing", check=False)
    assert res.returncode != 0 and "no such file" in res.stderr, res.stderr
    publish(lux, run_id, "/tmp/x", "--name", "fine/x")
    assert [a["path"] for a in wait_available(lux, run_id, 1, "--all-versions")] == ["/.lux/artifacts/fine/x"]


def test_a_runner_restart_does_not_duplicate_published_artifacts(lux, runners, hosts):
    """A restarted runner reads the output file from its start again: it
    sees the publish's record again and reports nothing new for it."""
    run_id = long_run(lux, runners, hosts)
    sh(lux, run_id, f"echo before > /tmp/a && {PUBLISH} /tmp/a")
    wait_available(lux, run_id, 1)
    runners.stop(hosts[0], "KILL")
    runners.start(hosts[0])
    # A harmless exec until the runner is back, then one publish: a retried
    # publish could be a second version of b. Records are handled in
    # order: once this one is in, the re-read reached the first again.
    wait_until(lambda: sh(lux, run_id, "true", check=False).returncode == 0, 30, 0.5, "exec after the restart")
    sh(lux, run_id, f"echo after > /tmp/b && {PUBLISH} /tmp/b")
    arts = wait_available(lux, run_id, 2, "--all-versions")
    assert sorted((a["path"], a["version"]) for a in arts) == [("/.lux/artifacts/a", 1), ("/.lux/artifacts/b", 1)], arts
    wait_until(lambda: len(published_events(lux, run_id)) >= 2, 30, 0.5, "artifact.published events")
    lux.run("stop", run_id, "--wait")
    lux.wait_placement_uploaded(run_id)
    assert len(lux.json("artifacts", run_id, "--all-versions")) == 2
    assert sorted(e["name"] for e in published_events(lux, run_id)) == ["a", "b"]


def test_every_exit_collects(lux, runners, hosts):
    """A stop collects too, and each placement's are listed with its
    epoch: a changed file is the path's next version, an unchanged one is
    not recorded again. Publishing the same name again is a new version."""
    runners.start(hosts[0])
    script = ("mkdir -p /workspace/out; date +%s%N > /workspace/out/stamp; echo same > /workspace/out/same; "
              f"echo x > /tmp/once.txt; {PUBLISH} /tmp/once.txt; sleep 300")
    run_id = artifacts_run(lux, script, ["/workspace/out/*"])
    wait_until(lambda: lux.run("exec", run_id, "-T", "--", "test", "-f", "/workspace/out/stamp", input="", check=False).returncode == 0,
               30, 0.5, "the workload never wrote its stamp")
    lux.run("stop", run_id, "--wait")
    lux.run("resume", run_id)
    lux.wait_state(run_id, "running")
    # The script has run again (its publish is in) before the terminate.
    wait_until(lambda: any(a["version"] == 2 for a in lux.json("artifacts", run_id)), 30, 0.5, "epoch 2 never published")
    lux.run("terminate", run_id)
    lux.wait_state(run_id, "terminated")
    arts = wait_available(lux, run_id, 5, "--all-versions")
    got = sorted((a["path"], a["version"], a["epoch"]) for a in arts)
    assert got == [("/.lux/artifacts/once.txt", 1, 1), ("/.lux/artifacts/once.txt", 2, 2),
                   ("/workspace/out/same", 1, 1),
                   ("/workspace/out/stamp", 1, 1), ("/workspace/out/stamp", 2, 2)], got
    assert sorted((a["path"], a["version"]) for a in lux.json("artifacts", run_id)) == [
        ("/.lux/artifacts/once.txt", 2), ("/workspace/out/same", 1), ("/workspace/out/stamp", 2)]


def test_symlinks_are_not_followed(lux, runners, hosts):
    """A workload's symlink could point anywhere on the host: it is never
    collected (and cannot lead the collection out of the volume)."""
    host = runners.start(hosts[0])
    host.exec("sh", "-c", "echo host-secret > /root/host-secret.txt")
    script = ("mkdir -p /workspace/out && ln -s /root/host-secret.txt /workspace/out/leak.txt && "
              "ln -s / /workspace/out/rootdir && echo fine > /workspace/out/fine.txt")
    run_id = artifacts_run(lux, script, ["/workspace/out/**"])
    lux.wait_state(run_id, "succeeded")
    arts = wait_available(lux, run_id, 1)
    assert [a["path"] for a in arts] == ["/workspace/out/fine.txt"], arts


def test_another_tenant_cannot_see_them(lux, tenant_factory, runners, hosts):
    runners.start(hosts[0])
    run_id = artifacts_run(lux, "mkdir -p /workspace/out && echo a > /workspace/out/a.txt", ["/workspace/out/*"])
    lux.wait_state(run_id, "succeeded")
    art = wait_available(lux, run_id, 1)[0]
    other = tenant_factory()
    with pytest.raises(CLIError):
        other.run("artifacts", run_id)
    assert other.api(f"/v1/artifacts/{art['id']}").status_code == 404


def test_collected_after_a_runner_restart(lux, runners, hosts):
    """A Run re-adopted after its runner restarts still has its spec: its
    artifacts are collected when it exits."""
    runners.start(hosts[0])
    script = "mkdir -p /workspace/out && echo late > /workspace/out/late.txt && sleep 5"
    run_id = artifacts_run(lux, script, ["/workspace/out/*"])
    lux.wait_state(run_id, "running")
    runners.stop(hosts[0], "KILL")
    runners.start(hosts[0])
    lux.wait_state(run_id, "succeeded", timeout=60)
    assert [a["path"] for a in wait_available(lux, run_id, 1)] == ["/workspace/out/late.txt"]


def test_a_symlinked_staging_dir_cannot_reach_the_host(lux, runners, hosts):
    """A root workload replaces the shim's staging directory with a link to
    a host path and publishes: the runner neither collects from it nor
    touches anything there."""
    host = runners.start(hosts[0])
    host.exec("sh", "-c", "mkdir -p /srv/precious && echo keep > /srv/precious/file")
    script = (f"rm -rf /.lux/run/artifacts && ln -s /srv/precious /.lux/run/artifacts && mkdir -p /srv/precious && "
              f"echo x > /tmp/x && {PUBLISH} /tmp/x; echo linked")
    run_id = artifacts_run(lux, script, [])
    lux.wait_state(run_id, "succeeded")
    assert "linked" in lux.logs(run_id)
    lux.wait_placement_uploaded(run_id)
    assert host.exec("cat", "/srv/precious/file").strip() == "keep"
    assert lux.json("artifacts", run_id, "--all-versions") == []


def test_nested_volumes(lux, runners, hosts):
    """A file is taken from the volume the container sees it on: not from
    the outer volume's directory the inner one hides."""
    host = runners.start(hosts[0])
    vols = [{"name": "workspace", "path": "/workspace", "kind": "state"},
            {"name": "repos", "path": "/workspace/repos", "kind": "state"}]
    script = ("mkdir -p /workspace/repos/app && echo r > /workspace/repos/app/report.xml && "
              "echo t > /workspace/top.xml && echo ready && sleep 300")
    run_id = lux.submit(generic(ALPINE_IMAGE, "sh", "-c", script, volumes=vols,
                                artifacts={"paths": ["/workspace/**/*.xml"]}))
    lux.wait_output(run_id, "ready")
    # A stale file in the outer volume's own repos directory, which the
    # inner volume hides from the container: it must not be collected.
    outer = host.podman("volume", "inspect", "--format", "{{.Mountpoint}}", f"lux-{run_id}-workspace").strip()
    host.exec("sh", "-c", f"mkdir -p {outer}/repos && echo stale > {outer}/repos/stale.xml")
    lux.run("stop", run_id, "--wait")
    paths = sorted(a["path"] for a in wait_available(lux, run_id, 2))
    assert paths == ["/workspace/repos/app/report.xml", "/workspace/top.xml"], paths

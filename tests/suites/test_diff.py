"""lux diff: what a Run changed in its repositories, computed in its
container while it runs and saved with every snapshot once it stops."""

from __future__ import annotations

import json
import time

import pytest

from env import sh, wait_until
from conftest import CLIError, fake_agent


def diff_spec(image: str, git_server, prompt: str, repo: str) -> dict:
    spec = fake_agent(image, prompt)
    spec["git"] = {"repositories": [{"name": repo, "url": git_server.url(repo), "ref": "main", "credential": "GIT_TOKEN"}]}
    spec["secrets"] = [{"name": "GIT_TOKEN", "value": git_server.token}]
    spec["workload"]["workdir"] = f"/workspace/repos/{repo}"
    return spec


def diff_json(lux, run_id: str, *args: str) -> dict:
    return lux.json("diff", run_id, *args)


def wait_diff(lux, run_id: str, *args: str, timeout: float = 90) -> dict:
    """The stopped Run's diff, once its snapshot's diff is computed (after
    the snapshot is reported: lux diff --wait) and its patches uploaded."""
    def ready():
        p = lux.run("diff", run_id, "-o", "json", "--wait", "--timeout", "60s", *args, check=False)
        if p.returncode == 4 and "not_uploaded" not in p.stderr and "still being uploaded" not in p.stderr:
            raise AssertionError(f"lux diff --wait: {p.stderr}")
        if p.returncode == 4:  # its patches not uploaded yet
            return None
        return json.loads(p.stdout)
    return wait_until(ready, timeout, 0.5, f"no diff for {run_id}")


# A working tree as git sees it, .git aside: per path, its type, executable
# bit, and a symlink's target or a file's sha256. Run with the tree as the
# working directory (in the Run's container, or in the git server's).
MANIFEST = r"""
find . -path ./.git -prune -o \( -type f -o -type l \) -print | LC_ALL=C sort | while IFS= read -r p; do
  if [ -L "$p" ]; then echo "link $p -> $(readlink "$p")"
  elif [ -x "$p" ]; then echo "exec $p $(sha256sum < "$p" | cut -d' ' -f1)"
  else echo "file $p $(sha256sum < "$p" | cut -d' ' -f1)"; fi
done
"""


def workload_manifest(hosts, run_id: str, path: str) -> str:
    return hosts[0].exec("podman", "exec", "--user", "agent", "--workdir", path, f"lux-{run_id}", "sh", "-c", MANIFEST)


def apply_and_compare(git_server, repo: str, base: str, patch: str, want: str):
    """Applies patch (lux diff's output) to a fresh clone of repo at base,
    and checks the result is the workload's tree, want (MANIFEST's): every
    path, its content, symlink target and executable bit."""
    got = sh("docker", "exec", "-i", git_server.container, "sh", "-c",
             f"set -e; rm -rf /tmp/ac && git clone -q /repos/{repo}.git /tmp/ac && cd /tmp/ac && "
             f"git checkout -q {base} && git apply --allow-empty - && {MANIFEST}", input=patch.encode())
    assert got == want, f"applied:\n{got}\nworkload:\n{want}"


def test_diff_live_then_from_the_snapshot_then_across_a_resume(lux, runners, hosts, fake_image, git_server):
    """The workload commits one change, edits a file and adds another. The
    diff shows all three while it runs, the same from its snapshot once it
    stops, and after a resume both rounds against the original base; the
    CLI's output applies to a fresh clone at that base."""
    base = git_server.create("dapp", {"a.txt": "one\n", "b.txt": "bee\n"})
    runners.start(hosts[0])
    script = "write c.txt committed\ncommit add c\nwrite a.txt edited\nwrite new.txt added"
    run_id = lux.submit(diff_spec(fake_image, git_server, script, "dapp"))
    lux.wait_output(run_id, "wrote new.txt")
    lux.wait_activity(run_id, "idle")

    live = diff_json(lux, run_id)["repos"][0]
    assert live["source"] == "live" and live["base"] == base and live["head"] != base, live
    names = {f["path"] for f in live["fileStats"]}
    assert names == {"a.txt", "c.txt", "new.txt"}, live["fileStats"]
    for want in ("+committed", "+edited", "-one", "+added"):
        assert want in live["patch"], live["patch"]
    text = lux.run("diff", run_id).stdout
    assert text.startswith(f"# repo dapp: {base[:12]}..{live['head'][:12]} (live)\n"), text
    tree = workload_manifest(hosts, run_id, "/workspace/repos/dapp")
    apply_and_compare(git_server, "dapp", base, text, tree)
    # Against HEAD: the commit is not there.
    head = diff_json(lux, run_id, "--base", "head")["repos"][0]
    assert {f["path"] for f in head["fileStats"]} == {"a.txt", "new.txt"}, head
    stat = lux.run("diff", run_id, "--stat").stdout
    assert " 3 files changed" in stat and "new.txt" in stat, stat

    lux.run("stop", run_id, "--wait")
    lux.wait_uploaded(run_id)
    snap = wait_diff(lux, run_id)["repos"][0]
    snapshot_id = lux.json("snapshots", run_id)[-1]["id"]
    assert snap["source"] == "snapshot" and snap["snapshotId"] == snapshot_id, snap
    assert snap["patch"] == live["patch"] and snap["base"] == base and snap["head"] == live["head"], snap
    text = lux.run("diff", run_id).stdout
    assert f"(snapshot, snapshot {snapshot_id} at " in text.splitlines()[0], text
    apply_and_compare(git_server, "dapp", base, text, tree)
    # Both kinds are stored.
    assert {f["path"] for f in diff_json(lux, run_id, "--base", "head")["repos"][0]["fileStats"]} == {"a.txt", "new.txt"}

    lux.run("resume", run_id, "--wait", "--secret", f"GIT_TOKEN={git_server.token}", "--input", "write b.txt round two")
    lux.wait_output(run_id, "wrote b.txt")
    lux.wait_activity(run_id, "idle")
    again = diff_json(lux, run_id)["repos"][0]
    assert again["source"] == "live" and again["base"] == base, again
    assert {f["path"] for f in again["fileStats"]} == {"a.txt", "b.txt", "c.txt", "new.txt"}, again["fileStats"]
    apply_and_compare(git_server, "dapp", base, lux.run("diff", run_id).stdout,
                      workload_manifest(hosts, run_id, "/workspace/repos/dapp"))


def test_the_patch_reproduces_symlinks_modes_and_binaries(lux, runners, hosts, fake_image, git_server):
    """The workload makes a symlink, an executable, a binary file and an
    export-ignore'd one, changes a symlink's target and a file's mode.
    Applied at the base, lux diff's output (live, then the snapshot's) is
    the workload's tree, byte for byte."""
    base = git_server.create("dfaith", {"a.txt": "one\n", "run.sh": "#!/bin/sh\necho hi\n",
                                        ".gitattributes": "internal/** export-ignore\n"})
    runners.start(hosts[0])
    run_id = lux.submit(diff_spec(fake_image, git_server, "echo ready", "dfaith"))
    lux.wait_activity(run_id, "idle")
    hosts[0].exec("podman", "exec", "--user", "agent", "--workdir", "/workspace/repos/dfaith", f"lux-{run_id}", "sh", "-c",
                  "set -e; ln -s a.txt link; chmod +x run.sh; printf '\\000\\001\\377bin\\000' > data.bin; "
                  "mkdir -p internal && echo private > internal/x.txt; mkdir sub && ln -s ../run.sh sub/tool; "
                  "printf 'one\\ntwo\\n' > a.txt")
    tree = workload_manifest(hosts, run_id, "/workspace/repos/dfaith")
    assert "link ./link -> a.txt" in tree and "exec ./run.sh" in tree and "./internal/x.txt" in tree, tree
    live = diff_json(lux, run_id)["repos"][0]
    assert "internal/x.txt" in {f["path"] for f in live["fileStats"]}, live["fileStats"]
    apply_and_compare(git_server, "dfaith", base, lux.run("diff", run_id).stdout, tree)
    lux.run("stop", run_id, "--wait")
    lux.wait_uploaded(run_id)
    wait_diff(lux, run_id)
    apply_and_compare(git_server, "dfaith", base, lux.run("diff", run_id).stdout, tree)


def test_no_diff_before_a_snapshot_and_nothing_when_unchanged(lux, runners, hosts, fake_image, git_server):
    git_server.create("dsame", {"a.txt": "one\n"})
    runners.start(hosts[0])
    run_id = lux.submit(diff_spec(fake_image, git_server, "echo ready", "dsame"))
    lux.wait_activity(run_id, "idle")
    # Unchanged: empty output, exit 0.
    p = lux.run("diff", run_id)
    assert p.stdout == "" and p.returncode == 0, p
    lux.run("stop", run_id, "--wait")
    lux.wait_uploaded(run_id)
    wait_diff(lux, run_id)
    assert lux.run("diff", run_id).stdout == ""
    # A Run with no repositories has no diff.
    other = lux.submit(fake_agent(fake_image, "echo hi"))
    with pytest.raises(CLIError) as e:
        lux.run("diff", other)
    assert e.value.code == 3, e.value.stderr


def test_a_failed_diff_does_not_fail_the_stop(lux, runners, hosts, fake_image, git_server):
    """The workload deletes its checkout: the repository's diff fails, the
    Run still stops cleanly, with a diff.failed event."""
    git_server.create("dgone", {"a.txt": "one\n"})
    runners.start(hosts[0])
    run_id = lux.submit(diff_spec(fake_image, git_server, "echo ready", "dgone"))
    lux.wait_activity(run_id, "idle")
    hosts[0].exec("podman", "exec", "--user", "agent", f"lux-{run_id}", "rm", "-rf", "/workspace/repos/dgone")
    lux.run("stop", run_id, "--wait")
    assert lux.get(run_id)["state"] == "stopped"
    failed = wait_until(lambda: lux.events(run_id, "diff.failed"), 30, 0.5, "no diff.failed event")
    assert failed[0]["data"]["repo"] == "dgone" and failed[0]["data"]["error"], failed
    # Snapshotted as usual; the diff says why it has none.
    assert lux.json("snapshots", run_id), "no snapshot"
    wait_diff(lux, run_id)
    p = lux.run("diff", run_id, check=False)
    assert p.returncode == 1 and "dgone" in p.stderr, (p.stdout, p.stderr)
    # -o json says so too, and fails the same way.
    p = lux.run("diff", run_id, "-o", "json", check=False)
    assert p.returncode == 1 and json.loads(p.stdout)["repos"][0]["error"], p.stdout


def test_a_hanging_diff_holds_up_nothing(lux, runners, hosts, fake_image, git_server):
    """The workload makes its checkout's index a FIFO, on which git blocks.
    The Run still stops as fast as one whose diff is quick; while its
    snapshot's diff hangs, lux diff says it is pending (exit 4); after the
    diff's one-minute budget, diff.failed, and the diff is unavailable
    (exit 3), never an older one's."""
    git_server.create("dhang", {"a.txt": "one\n"})
    runners.start(hosts[0])
    # A baseline: the same Run whose diff is quick, stopped on this host.
    quick = lux.submit(diff_spec(fake_image, git_server, "write a.txt changed", "dhang"))
    lux.wait_output(quick, "wrote a.txt")
    lux.wait_activity(quick, "idle")
    start = time.time()
    lux.run("stop", quick, "--wait")
    baseline = time.time() - start

    run_id = lux.submit(diff_spec(fake_image, git_server, "write a.txt changed", "dhang"))
    lux.wait_output(run_id, "wrote a.txt")
    lux.wait_activity(run_id, "idle")
    hosts[0].exec("podman", "exec", "--user", "agent", f"lux-{run_id}", "sh", "-c",
                  "cd /workspace/repos/dhang && rm .git/index && mkfifo .git/index")
    start = time.time()
    lux.run("stop", run_id, "--wait")
    took = time.time() - start
    assert took < baseline + 15, f"the stop took {took:.1f}s, a quick one {baseline:.1f}s: it waited for the diff"
    assert lux.get(run_id)["state"] == "stopped"
    # The diff's helper is up (and hanging on the FIFO).
    wait_until(lambda: f"lux-diff-{run_id}" in hosts[0].podman("ps", "--filter", f"name=lux-diff-{run_id}", "--format", "{{.Names}}"),
               30, 0.5, "no diff helper container")
    with pytest.raises(CLIError) as e:
        lux.run("diff", run_id)
    if e.value.code == 4:
        assert "is still being computed; try again in" in e.value.stderr, e.value.stderr
    else:
        # Its budget ran out already: failed, and so unavailable.
        assert e.value.code == 3 and "has no diff" in e.value.stderr, e.value.stderr
    failed = wait_until(lambda: lux.events(run_id, "diff.failed"), 120, 1, "no diff.failed event")
    assert "did not finish" in failed[0]["data"]["error"], failed
    with pytest.raises(CLIError) as e:
        lux.run("diff", run_id)
    assert e.value.code == 3 and "did not finish" in e.value.stderr, e.value.stderr
    wait_until(lambda: f"lux-diff-{run_id}" not in hosts[0].podman("ps", "-a", "--format", "{{.Names}}"),
               30, 0.5, "the diff helper container is still there")

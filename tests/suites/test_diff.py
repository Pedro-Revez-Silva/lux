"""lux diff: what a running Run changed in its repositories, computed in its
container. A Run that is not running has no diff; a patch kept with
workload.beforeStop is how its changes outlive a stop."""

from __future__ import annotations

import json

import pytest

from env import sh, wait_until
from conftest import CLIError, fake_agent

# The message of a stopped Run's lux diff (after "lux: ").
STOPPED = ("the Run is stopped: its diff is available only while the Run is running; resume it, or save a patch at stop "
           "with workload.beforeStop (git add -N . && git diff --binary <base> > $LUX_ARTIFACTS/final.patch) and fetch it with lux artifacts")


def diff_spec(image: str, git_server, prompt: str, repo: str) -> dict:
    spec = fake_agent(image, prompt)
    spec["git"] = {"repositories": [{"name": repo, "url": git_server.url(repo), "ref": "main", "credential": "GIT_TOKEN"}]}
    spec["secrets"] = [{"name": "GIT_TOKEN", "value": git_server.token}]
    spec["workload"]["workdir"] = f"/workspace/repos/{repo}"
    return spec


def diff_json(lux, run_id: str, *args: str) -> dict:
    return lux.json("diff", run_id, *args)


def patch_paths(patch: str) -> set[str]:
    """The paths a patch touches, from its `diff --git a/P b/P` lines."""
    return {line[len("diff --git a/"):].split(" b/", 1)[0] for line in patch.splitlines() if line.startswith("diff --git a/")}


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


def in_checkout(hosts, run_id: str, repo: str, script: str) -> str:
    return hosts[0].exec("podman", "exec", "--user", "agent", "--workdir", f"/workspace/repos/{repo}", f"lux-{run_id}", "sh", "-c", script)


def workload_manifest(hosts, run_id: str, repo: str) -> str:
    return in_checkout(hosts, run_id, repo, MANIFEST)


def apply_and_compare(git_server, repo: str, base: str, patch: str | bytes, want: str):
    """Applies patch (lux diff's output) to a fresh clone of repo at base,
    and checks the result is the workload's tree, want (MANIFEST's): every
    path, its content, symlink target and executable bit."""
    got = sh("docker", "exec", "-i", git_server.container, "sh", "-c",
             f"set -e; rm -rf /tmp/ac && git clone -q /repos/{repo}.git /tmp/ac && cd /tmp/ac && "
             f"git checkout -q {base} && git apply --allow-empty - && {MANIFEST}", input=patch.encode() if isinstance(patch, str) else patch)
    assert got == want, f"applied:\n{got}\nworkload:\n{want}"


def test_live_diff_stop_and_resume(lux, runners, hosts, fake_image, git_server, tmp_path):
    """The workload commits a change; then staged, unstaged, untracked,
    binary, symlink and mode changes are made in its checkout. lux diff
    (against the clone, and against HEAD) is all of it, and applied to a
    fresh clone at the base gives the workload's tree. Stopped, the Run has
    no diff (exit 4, saying why and how to keep a patch), and the patch its
    beforeStop hook saved applies the same. Resumed and edited again, the
    diff has both rounds, against the original base."""
    base = git_server.create("dapp", {"a.txt": "one\n", "b.txt": "bee\n", "run.sh": "#!/bin/sh\necho hi\n",
                                      "gone.txt": "bye\n", ".gitattributes": "internal/** export-ignore\n"})
    runners.start(hosts[0])
    spec = diff_spec(fake_image, git_server, "write c.txt committed\ncommit add c", "dapp")
    # The documented recipe for keeping a patch past a stop.
    spec["workload"]["beforeStop"] = {"command": [
        "sh", "-c", "cd /workspace/repos/dapp && git add -N . && git diff --binary origin/main > $LUX_ARTIFACTS/final.patch"]}
    run_id = lux.submit(spec)
    lux.wait_output(run_id, "committed")
    lux.wait_activity(run_id, "idle")
    in_checkout(hosts, run_id, "dapp",
                "set -e; printf 'one\\nstaged\\n' > a.txt && git add a.txt && printf 'bee\\nunstaged\\n' > b.txt && "
                "echo new > new.txt && printf '\\000\\001\\377bin\\000' > data.bin && git add data.bin && ln -s a.txt link && "
                "chmod +x run.sh && rm gone.txt && mkdir -p internal sub && echo private > internal/x.txt && "
                "ln -s ../run.sh sub/tool")
    tree = workload_manifest(hosts, run_id, "dapp")
    before = in_checkout(hosts, run_id, "dapp", "git status --porcelain")
    assert "link ./link -> a.txt" in tree and "exec ./run.sh" in tree and "./data.bin" in tree, tree

    live = diff_json(lux, run_id)["repos"][0]
    assert live["base"] == base and live["head"] != base and not live.get("error"), live
    names = patch_paths(live["patch"])
    assert names == {"a.txt", "b.txt", "c.txt", "new.txt", "data.bin", "link", "run.sh", "gone.txt",
                     "internal/x.txt", "sub/tool"}, live["patch"]
    assert live["files"] == 10 and not live["truncated"], live
    for want in ("+committed", "+staged", "+unstaged", "+new", "-bye", "GIT binary patch"):
        assert want in live["patch"], (want, live["patch"])
    text = lux.run("diff", run_id).stdout
    assert text.startswith(f"# repo dapp: {base[:12]}..{live['head'][:12]}\n"), text
    apply_and_compare(git_server, "dapp", base, text, tree)
    # The checkout was not touched: nothing new staged, nothing untracked added.
    status = in_checkout(hosts, run_id, "dapp", "git status --porcelain")
    assert status == before, (before, status)
    assert "?? new.txt" in status and "M  a.txt" in status and " M b.txt" in status, status

    # Against HEAD: the commit is not there.
    head = diff_json(lux, run_id, "--base", "head")["repos"][0]
    assert head["base"] == live["head"], head
    assert patch_paths(head["patch"]) == names - {"c.txt"} and head["files"] == 9, head
    stat = lux.run("diff", run_id, "--stat").stdout
    assert " 10 files changed" in stat and "new.txt" in stat, stat

    lux.run("stop", run_id, "--wait")
    with pytest.raises(CLIError) as e:
        lux.run("diff", run_id)
    assert e.value.code == 4 and e.value.stderr.strip() == f"lux: {STOPPED}", e.value.stderr
    p = lux.run("diff", run_id, "-o", "json", check=False)
    assert p.returncode == 4 and "only while the Run is running" in p.stderr, (p.returncode, p.stderr)
    # The beforeStop patch: the same working tree.
    wait_until(lambda: (a := lux.json("artifacts", run_id)) and all(x["available"] for x in a) and
               "/.lux/artifacts/final.patch" in [x["path"] for x in a], 60, 0.5, "no final.patch artifact")
    lux.run("artifacts", run_id, "--download", str(tmp_path))
    apply_and_compare(git_server, "dapp", base, (tmp_path / "1/.lux/artifacts/final.patch").read_bytes(), tree)

    lux.run("resume", run_id, "--wait", "--secret", f"GIT_TOKEN={git_server.token}", "--input", "write b.txt round two")
    lux.wait_output(run_id, "wrote b.txt")
    lux.wait_activity(run_id, "idle")
    again = diff_json(lux, run_id)["repos"][0]
    assert again["base"] == base, again
    assert patch_paths(again["patch"]) == names, again["patch"]
    assert "+round two" in again["patch"] and "+committed" in again["patch"] and "+staged" in again["patch"], again["patch"]
    apply_and_compare(git_server, "dapp", base, lux.run("diff", run_id).stdout, workload_manifest(hosts, run_id, "dapp"))


def test_unchanged_and_no_repositories(lux, runners, hosts, fake_image, git_server):
    git_server.create("dsame", {"a.txt": "one\n"})
    runners.start(hosts[0])
    run_id = lux.submit(diff_spec(fake_image, git_server, "echo ready", "dsame"))
    lux.wait_activity(run_id, "idle")
    # Unchanged: empty output, exit 0.
    p = lux.run("diff", run_id)
    assert p.stdout == "" and p.returncode == 0, p
    assert diff_json(lux, run_id)["repos"][0]["files"] == 0
    # A Run with no repositories has no diff, running or not.
    other = lux.submit(fake_agent(fake_image, "echo hi"))
    lux.wait_activity(other, "idle")
    with pytest.raises(CLIError) as e:
        lux.run("diff", other)
    assert e.value.code == 3 and "no repositories" in e.value.stderr, e.value.stderr


def test_a_failed_repository(lux, runners, hosts, fake_image, git_server):
    """The workload deletes its checkout: that repository's diff fails
    (exit 1, with -o json too), and says so."""
    git_server.create("dgone", {"a.txt": "one\n"})
    runners.start(hosts[0])
    run_id = lux.submit(diff_spec(fake_image, git_server, "echo ready", "dgone"))
    lux.wait_activity(run_id, "idle")
    hosts[0].exec("podman", "exec", "--user", "agent", f"lux-{run_id}", "rm", "-rf", "/workspace/repos/dgone")
    p = lux.run("diff", run_id, check=False)
    assert p.returncode == 1 and "dgone" in p.stderr, (p.stdout, p.stderr)
    p = lux.run("diff", run_id, "-o", "json", check=False)
    assert p.returncode == 1 and json.loads(p.stdout)["repos"][0]["error"], p.stdout


def test_a_large_untracked_file_is_truncated(lux, runners, hosts, fake_image, git_server):
    """An untracked file over 32 KiB carries its first 32 KiB and a marker;
    the repository is flagged truncated, and lux diff says so on stderr. An
    untracked binary file is named, not carried, and flagged the same."""
    git_server.create("dbig", {"a.txt": "one\n"})
    runners.start(hosts[0])
    run_id = lux.submit(diff_spec(fake_image, git_server, "echo ready", "dbig"))
    lux.wait_activity(run_id, "idle")
    in_checkout(hosts, run_id, "dbig", "yes 0123456789abcdef | head -c 40000 > big.txt")
    d = diff_json(lux, run_id)["repos"][0]
    assert d["truncated"] and d["files"] == 1, d
    kept = ("0123456789abcdef\n" * 3000)[:32768]
    want = "@@ -0,0 +1,%d @@\n" % (kept.count("\n") + 1) + "".join("+" + l + "\n" for l in kept.split("\n"))
    assert d["patch"].endswith(want + "\\ lux: truncated at 32768 of 40000 bytes\n"), d["patch"][-300:]
    p = lux.run("diff", run_id)
    assert "big.txt" in p.stdout and "repo dbig" in p.stderr and "will not apply" in p.stderr, (p.stdout[-200:], p.stderr)

    in_checkout(hosts, run_id, "dbig", "rm big.txt && printf 'x\\000y' > blob.bin")
    d = diff_json(lux, run_id)["repos"][0]
    assert d["truncated"] and "Binary files /dev/null and b/blob.bin differ" in d["patch"], d

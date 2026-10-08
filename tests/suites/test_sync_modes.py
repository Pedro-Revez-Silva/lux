"""Sync modes on a checkout the workload edits itself: fast-forward moves
it only when nothing in it can be lost, and leaves the ref's commit as
refs/remotes/lux/<branch> for the workload to merge or rebase onto; a kept
checkout's files, index and commits are as they were. Servers run
afterSync only after a checkout moved."""

from __future__ import annotations

import json

from conftest import generic
from env import ALPINE_IMAGE, wait_until
from suites.test_server_wake import create, exec_in, get, preview_spec, resume_sync, wait_state

GIT = ["git", "-C", "/workspace/app"]
COMMIT_AS = ["-c", "user.name=c", "-c", "user.email=c@c"]
# The server's afterSync appends a line each time it runs.
AFTER_SYNC_LOG = "/workspace/after-sync.log"


def git(lux, run_id: str, *args: str) -> str:
    return exec_in(lux, run_id, *GIT, *args).stdout.strip()


def has_ref(lux, run_id: str, ref: str) -> bool:
    return exec_in(lux, run_id, *GIT, "rev-parse", "--verify", "-q", ref, check=False).returncode == 0


def after_syncs(lux, run_id: str) -> int:
    return len(exec_in(lux, run_id, "sh", "-c", f"cat {AFTER_SYNC_LOG} 2>/dev/null || true").stdout.splitlines())


def with_server(lux, run_id: str) -> dict:
    sv = create(lux, name="cold", port=8081, command=["lux-fake", "serve", "8081", "cold"],
                afterSync=["sh", "-c", f"echo ran >> {AFTER_SYNC_LOG}"], runId=run_id)
    wait_state(lux, sv["id"], "ready")
    return sv


def test_fast_forward_keeps_local_work_and_moves_a_clean_checkout(lux, runners, hosts, fake_image, git_server):
    git_server.create("app", {"message.txt": "one\n"})
    runners.start(hosts[0])
    run_id = lux.submit(preview_spec(fake_image, git_server))
    lux.wait_state(run_id, "running")
    sv = with_server(lux, run_id)
    # The conductor commits its own work in the checkout, and leaves an
    # untracked file.
    exec_in(lux, run_id, "sh", "-c",
            "cd /workspace/app && echo mine > mine.txt && git add mine.txt"
            " && git -c user.name=c -c user.email=c@c commit -qm mine && echo scratch > scratch.txt")
    local = git(lux, run_id, "rev-parse", "HEAD")
    # Someone else pushes to the branch meanwhile.
    pushed = git_server.commit_on("app", "main", "message.txt", "two\n")
    out = lux.json("sync", run_id, "app=main", "--mode", "fast-forward", "--wait", timeout=120)
    r = out[0]
    assert r["status"] == "kept" and r["diverged"] and not r.get("dirty") and r["mode"] == "fast-forward", out
    assert r["from"] == local and r["to"] == pushed and r["ahead"] == 1 and r["behind"] == 1 and not r.get("saved"), out
    # Nothing of the conductor's moved; the pushed commit is lux/main.
    assert git(lux, run_id, "rev-parse", "HEAD") == local
    assert git(lux, run_id, "symbolic-ref", "--short", "HEAD") == "main"
    files = exec_in(lux, run_id, "cat", "/workspace/app/mine.txt", "/workspace/app/message.txt", "/workspace/app/scratch.txt")
    assert files.stdout == "mine\none\nscratch\n", files.stdout
    assert git(lux, run_id, "rev-parse", "refs/remotes/lux/main") == pushed
    assert not has_ref(lux, run_id, "refs/lux/pre-sync")
    done = [e["data"] for e in lux.events(run_id, "sync.done") if e["data"].get("requestId") == r["requestId"]]
    assert done == [{"requestId": r["requestId"], "changed": False}], done
    assert after_syncs(lux, run_id) == 0 and get(lux, sv["id"])["state"] == "ready"
    # The conductor can see and take in what was pushed, itself.
    assert git(lux, run_id, "log", "--format=%H", "HEAD..lux/main") == pushed
    git(lux, run_id, *COMMIT_AS, "merge", "-q", "--no-edit", "lux/main")
    assert exec_in(lux, run_id, "cat", "/workspace/app/message.txt").stdout == "two\n"

    # A clean checkout behind the branch: it fast-forwards, and the
    # server's afterSync runs once.
    git(lux, run_id, "reset", "-q", "--hard", "lux/main")
    at = git(lux, run_id, "rev-parse", "HEAD")
    three = git_server.commit_on("app", "main", "message.txt", "three\n")
    out = lux.json("sync", run_id, "app=main", "--mode", "fast-forward", "--wait", timeout=120)
    r = out[0]
    assert r["status"] == "fast-forward" and r["from"] == at and r["to"] == three and r["ahead"] == 0 and r["behind"] == 1, out
    assert git(lux, run_id, "rev-parse", "HEAD") == three
    assert git(lux, run_id, "symbolic-ref", "--short", "HEAD") == "main"
    assert git(lux, run_id, "rev-parse", "refs/remotes/lux/main") == three
    assert exec_in(lux, run_id, "cat", "/workspace/app/message.txt", "/workspace/app/scratch.txt").stdout == "three\nscratch\n"
    assert not has_ref(lux, run_id, "refs/lux/pre-sync")
    wait_until(lambda: after_syncs(lux, run_id) == 1, 60, 1, "afterSync never ran after the fast-forward")

    # Mode fetch: never moves, counts both ways.
    exec_in(lux, run_id, "sh", "-c", "cd /workspace/app && echo more > more.txt && git add more.txt"
            " && git -c user.name=c -c user.email=c@c commit -qm more")
    mine = git(lux, run_id, "rev-parse", "HEAD")
    four = git_server.commit_on("app", "main", "message.txt", "four\n")
    out = lux.json("sync", run_id, "app=main", "--mode", "fetch", "--wait", timeout=120)
    r = out[0]
    assert r["status"] == "fetched" and r["ahead"] == 1 and r["behind"] == 1 and r["to"] == four, out
    assert git(lux, run_id, "rev-parse", "HEAD") == mine and git(lux, run_id, "rev-parse", "lux/main") == four
    lux.run("terminate", run_id)


def test_resume_fast_forward_keeps_a_dirty_checkout_and_moves_a_clean_one(lux, runners, hosts, fake_image, git_server):
    git_server.create("app", {"message.txt": "one\n"})
    runners.start(hosts[0])
    run_id = lux.submit(preview_spec(fake_image, git_server))
    lux.wait_state(run_id, "running")
    sv = with_server(lux, run_id)
    base = git(lux, run_id, "rev-parse", "HEAD")
    exec_in(lux, run_id, "sh", "-c",
            "echo local edit > /workspace/app/message.txt && echo new > /workspace/app/untracked.txt")
    lux.run("stop", run_id, "--wait", timeout=120)
    pushed = git_server.commit_on("app", "main", "other.txt", "theirs\n")
    lux.run("resume", run_id, "--sync", "app=main", "--sync-mode", "fast-forward",
            "--secret", f"GIT_TOKEN={git_server.token}", "--wait", timeout=180)
    r = resume_sync(lux, run_id, pushed)
    assert r["status"] == "kept" and r["dirty"] and not r.get("diverged") and r["mode"] == "fast-forward", r
    assert r["from"] == base and r["ahead"] == 0 and r["behind"] == 1 and not r.get("saved"), r
    assert git(lux, run_id, "rev-parse", "HEAD") == base
    assert git(lux, run_id, "status", "--porcelain") == "M message.txt\n?? untracked.txt"
    files = exec_in(lux, run_id, "cat", "/workspace/app/message.txt", "/workspace/app/untracked.txt")
    assert files.stdout == "local edit\nnew\n", files.stdout
    assert exec_in(lux, run_id, "test", "-e", "/workspace/app/other.txt", check=False).returncode != 0
    assert git(lux, run_id, "rev-parse", "refs/remotes/lux/main") == pushed
    assert not has_ref(lux, run_id, "refs/lux/pre-sync")
    # Nothing moved: the server started without its afterSync.
    wait_state(lux, sv["id"], "ready")
    assert after_syncs(lux, run_id) == 0

    # A clean checkout behind the branch: the resume fast-forwards it, and
    # the server runs its afterSync once before it starts.
    git(lux, run_id, "checkout", "-q", "--", "message.txt")
    lux.run("stop", run_id, "--wait", timeout=120)
    three = git_server.commit_on("app", "main", "message.txt", "three\n")
    lux.run("resume", run_id, "--sync", "app=main", "--sync-mode", "fast-forward",
            "--secret", f"GIT_TOKEN={git_server.token}", "--wait", timeout=180)
    r = resume_sync(lux, run_id, three)
    assert r["status"] == "fast-forward" and r["from"] == base and r["ahead"] == 0 and r["behind"] == 2, r
    assert git(lux, run_id, "rev-parse", "HEAD") == three
    assert git(lux, run_id, "symbolic-ref", "--short", "HEAD") == "main"
    files = exec_in(lux, run_id, "cat", "/workspace/app/message.txt", "/workspace/app/other.txt", "/workspace/app/untracked.txt")
    assert files.stdout == "three\ntheirs\nnew\n", files.stdout
    wait_state(lux, sv["id"], "ready")
    wait_until(lambda: after_syncs(lux, run_id) >= 1, 60, 1, "afterSync never ran after the resume's fast-forward")
    assert after_syncs(lux, run_id) == 1
    lux.run("terminate", run_id)


def push(lux, run_id: str) -> dict:
    """`lux push --wait`'s result for app."""
    p = lux.run("push", run_id, "--wait", "-o", "json", check=False, timeout=180)
    results = {r["repo"]: r for r in json.loads(p.stdout)}
    assert (p.returncode == 0) == (results["app"]["status"] in ("pushed", "up-to-date")), (p.returncode, results)
    return results["app"]


def test_a_rebase_in_progress_moves_to_another_host_and_is_kept(lux, runners, hosts, fake_image, git_server):
    """A conductor stopped mid-rebase and resumed on another host finds the
    rebase as it left it: the resume's sync names it and moves nothing, a
    push is refused until it is finished, and then it pushes."""
    git_server.create("app", {"message.txt": "one\n"})
    a, b = hosts[0], hosts[1]
    runners.start(a, "--max-runs", "1")
    spec = preview_spec(fake_image, git_server)
    spec["git"]["push"] = {"branch": "lux/work"}
    run_id = lux.submit(spec)
    lux.wait_state(run_id, "running")

    # The conductor commits; someone else pushes a conflicting commit; the
    # conductor fetches it and rebases onto it, which stops on the conflict.
    exec_in(lux, run_id, "sh", "-c", "cd /workspace/app && echo mine > message.txt"
            " && git -c user.name=c -c user.email=c@c commit -qam mine")
    theirs = git_server.commit_on("app", "main", "message.txt", "theirs\n")
    out = lux.json("sync", run_id, "app=main", "--mode", "fetch", "--wait", timeout=120)
    assert out[0]["status"] == "fetched" and out[0]["to"] == theirs and not out[0].get("operation"), out
    rebase = exec_in(lux, run_id, *GIT, *COMMIT_AS, "rebase", "lux/main", check=False)
    assert rebase.returncode != 0, rebase.stdout
    assert exec_in(lux, run_id, "test", "-d", "/workspace/app/.git/rebase-merge", check=False).returncode == 0
    assert git(lux, run_id, "diff", "--name-only", "--diff-filter=U") == "message.txt"
    conflicted = exec_in(lux, run_id, "cat", "/workspace/app/message.txt").stdout
    assert "<<<<<<<" in conflicted and "theirs" in conflicted and "mine" in conflicted, conflicted
    head = git(lux, run_id, "rev-parse", "HEAD")
    assert head == theirs
    assert exec_in(lux, run_id, *GIT, "symbolic-ref", "-q", "HEAD", check=False).returncode != 0

    # Stopped; A is occupied, so the resume goes to B, with a newer push
    # to sync to.
    blocker = lux.submit(generic(ALPINE_IMAGE, "sleep", "600"))
    lux.run("stop", run_id, "--wait", timeout=120)
    lux.wait_state(blocker, "running")
    newest = git_server.commit_on("app", "main", "other.txt", "newest\n")
    runners.start(b)
    lux.run("resume", run_id, "--sync", "app=main", "--sync-mode", "fast-forward",
            "--secret", f"GIT_TOKEN={git_server.token}", "--wait", timeout=180)
    assert lux.get(run_id)["placements"][-1]["hostName"] == b.name

    # The sync names the rebase and moves nothing; the rebase is as it was.
    r = resume_sync(lux, run_id, newest)
    assert r["status"] == "kept" and r["operation"] == "rebase" and r["mode"] == "fast-forward", r
    assert r["from"] == head and r["ahead"] == 0 and r["behind"] == 1 and not r.get("saved"), r
    assert exec_in(lux, run_id, "test", "-d", "/workspace/app/.git/rebase-merge", check=False).returncode == 0
    assert git(lux, run_id, "rev-parse", "HEAD") == head
    assert exec_in(lux, run_id, *GIT, "symbolic-ref", "-q", "HEAD", check=False).returncode != 0
    assert exec_in(lux, run_id, "cat", "/workspace/app/message.txt").stdout == conflicted
    assert git(lux, run_id, "diff", "--name-only", "--diff-filter=U") == "message.txt"
    assert git(lux, run_id, "rev-parse", "refs/remotes/lux/main") == newest

    # A push of the half-done rebase is refused; the branch is not created.
    res = push(lux, run_id)
    assert res["status"] == "refused" and res["operation"] == "rebase", res
    assert res["error"] == "a rebase is in progress in the checkout: finish or abort it, then push", res
    assert git_server.rev("app", "lux/work") == ""

    # The conductor resolves the conflict and finishes the rebase.
    exec_in(lux, run_id, "sh", "-c", "cd /workspace/app && echo resolved > message.txt && git add message.txt"
            " && GIT_EDITOR=true git -c user.name=c -c user.email=c@c rebase --continue")
    assert exec_in(lux, run_id, "test", "-e", "/workspace/app/.git/rebase-merge", check=False).returncode != 0
    assert git(lux, run_id, "symbolic-ref", "--short", "HEAD") == "main"
    assert git(lux, run_id, "rev-parse", "HEAD^") == theirs
    rebased = git(lux, run_id, "rev-parse", "HEAD")

    res = push(lux, run_id)
    assert res["status"] == "pushed" and res["commit"] == rebased and not res.get("operation"), res
    assert git_server.rev("app", "lux/work") == rebased

    # It takes in the newest push itself; a sync then names no operation.
    git(lux, run_id, *COMMIT_AS, "merge", "-q", "--no-edit", "lux/main")
    assert exec_in(lux, run_id, "cat", "/workspace/app/message.txt", "/workspace/app/other.txt").stdout == "resolved\nnewest\n"
    out = lux.json("sync", run_id, "app=main", "--mode", "fast-forward", "--wait", timeout=120)
    assert out[0]["status"] == "ahead" and out[0]["behind"] == 0 and "operation" not in out[0], out
    lux.run("terminate", blocker)
    lux.run("terminate", run_id)

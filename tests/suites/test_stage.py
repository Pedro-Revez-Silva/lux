"""A Run's stage: luxd knows which phase of its start a Run is in, and since
when, as the runner reaches each one, and announces each change as a stage
event."""

from __future__ import annotations

import json
import threading
import time
from datetime import datetime

import requests

from conftest import generic
from env import ALPINE_IMAGE, wait_until

STAGES = ["waiting", "image", "volumes", "repositories", "container", "running", "succeeded"]
# The placement column each start stage begins at.
SINCE_COLUMN = {"image": "acceptedAt", "volumes": "imageReadyAt", "repositories": "volumesRestoredAt",
                "container": "reposReadyAt", "running": "containerStartedAt"}
# How long the clone waits: the repositories stage outlasts the report of
# its start and several GET polls.
CLONE_DELAY = 3


def at(ts: str) -> datetime:
    return datetime.fromisoformat(ts.replace("Z", "+00:00"))


class Feed:
    """GET /v1/events, followed in a thread: every stage event, in the order
    the stream delivers them."""

    def __init__(self, lux):
        self.stages: list[dict] = []
        self.ready = threading.Event()
        self._r = requests.get(f"{lux.env.luxd_url}/v1/events", headers={"Authorization": f"Bearer {lux.api_key}"},
                               stream=True, timeout=(10, 120))
        self._t = threading.Thread(target=self._read, daemon=True)
        self._t.start()

    def _read(self):
        self.ready.set()
        try:
            for line in self._r.iter_lines(decode_unicode=True):
                if line and line.startswith("data: "):
                    e = json.loads(line[6:])
                    if e.get("type") == "stage":
                        self.stages.append(e)
        except (requests.RequestException, AttributeError):
            pass  # closed by close(): a closed response reads from no socket

    def of(self, run_id: str) -> list[dict]:
        return [e["data"] for e in list(self.stages) if e.get("runId") == run_id]

    def close(self):
        self._r.close()
        self._t.join(5)


def follow(lux, run_id: str, until: str, timeout: float = 120) -> list[tuple[str, datetime, str]]:
    """Polls GET every 0.2 s until the stage is until: each (stage, since,
    state) read, once per change."""
    seen: list[tuple[str, datetime, str]] = []
    deadline = time.time() + timeout
    while time.time() < deadline:
        r = lux.api(f"/v1/runs/{run_id}").json()
        entry = (r["stage"], at(r["stageSince"]), r["state"])
        if not seen or seen[-1][:2] != entry[:2]:
            seen.append(entry)
        if r["stage"] == until:
            break
        time.sleep(0.2)
    assert seen and seen[-1][0] == until, seen
    print("stages read:", [(s, since.isoformat(), state) for s, since, state in seen])
    return seen


def assert_ordered(events: list[dict], what: str):
    stages = [e["stage"] for e in events]
    idx = [STAGES.index(s) for s in stages]
    assert idx == sorted(set(idx)), f"{what}: stage events out of order or repeated: {stages}"
    ev_since = [at(e["since"]) for e in events]
    assert ev_since == sorted(ev_since), f"{what}: since went back: {events}"


def assert_since_columns(events: list[dict], pl: dict):
    """Each start stage's event begins at its placement column."""
    for e in events:
        col = SINCE_COLUMN.get(e["stage"])
        if col:
            assert pl.get(col) and at(e["since"]) == at(pl[col]), (e, col, pl)


def stage_spec(git_server, repo: str, image: str = ALPINE_IMAGE) -> dict:
    spec = generic(image, "sleep", "3")
    spec["volumes"] = [{"name": "workspace", "path": "/workspace", "kind": "state"}]
    spec["git"] = {"repositories": [{"name": repo, "url": git_server.url(repo), "ref": "main", "credential": "GIT_TOKEN",
                                     "path": f"/workspace/{repo}"}]}
    spec["secrets"] = [{"name": "GIT_TOKEN", "value": git_server.token}]
    return spec


def test_a_starting_run_reports_its_stage(lux, runners, hosts, git_server):
    """A Run whose image takes seconds to build, with a repository whose
    clone takes seconds: while it builds, GET says stage image; while it
    clones, repositories; the stage moves forward through the start,
    stageSince never going back, and the event stream carries each change
    once, in order, each since its placement mark."""
    git_server.create("stage", {"README.md": "stage\n"})
    git_server.slow("stage", CLONE_DELAY)
    runners.start(hosts[0])
    feed = Feed(lux)
    feed.ready.wait(10)
    time.sleep(0.5)  # the stream is open before the Run exists
    try:
        spec = stage_spec(git_server, "stage")
        cf = f"FROM {ALPINE_IMAGE}\nRUN sleep 6 && echo {time.time_ns()} > /built\n"
        spec["image"] = {"build": {"containerfile": cf}}
        run_id = lux.submit(spec)

        seen = follow(lux, run_id, "succeeded")
        # Read while it started: the build as image, the clone as
        # repositories, both while the Run was starting.
        assert any(s == "image" and state in ("scheduled", "starting") for s, _, state in seen), seen
        assert any(s == "repositories" and state == "starting" for s, _, state in seen), seen
        order = [STAGES.index(s) for s, _, _ in seen]
        assert order == sorted(order), f"stage went back: {seen}"
        sinces = [since for _, since, _ in seen]
        assert sinces == sorted(sinces), f"stageSince went back: {seen}"

        # The placement's marks as the API reports them, reposReady included.
        pl = lux.get(run_id)["placements"][0]
        for k in ("acceptedAt", "imageReadyAt", "volumesRestoredAt", "reposReadyAt", "containerStartedAt"):
            assert pl.get(k), (k, pl)
        assert at(pl["imageReadyAt"]) <= at(pl["volumesRestoredAt"]) <= at(pl["reposReadyAt"]) <= at(pl["containerStartedAt"]), pl
        assert (at(pl["reposReadyAt"]) - at(pl["volumesRestoredAt"])).total_seconds() >= CLONE_DELAY - 0.5, pl
        # reposReady follows the clone: the runner reports git.clone before it.
        clone = lux.events(run_id, "git.clone")
        assert clone and clone[0]["data"]["status"] == "cloned", clone

        # The events: one per change, in order, through the build and to the end.
        events = wait_until(lambda: (lambda e: e if e and e[-1]["stage"] == "succeeded" else None)(feed.of(run_id)),
                            20, 0.2, "the feed never announced succeeded")
        stages = [e["stage"] for e in events]
        assert_ordered(events, "first start")
        for want in ("waiting", "image", "repositories", "running", "succeeded"):
            assert want in stages, stages
        assert all(e["epoch"] == 1 for e in events[1:]), events
        assert_since_columns(events, pl)
        # The image stage the event announced is the one GET showed.
        image = next(e for e in events if e["stage"] == "image")
        assert any(s == "image" and since == at(image["since"]) for s, since, _ in seen), (image, seen)
        # The Run's own event list has the same, once each.
        assert [e["data"]["stage"] for e in lux.events(run_id, "stage")] == stages
    finally:
        feed.close()


def test_a_resume_reports_its_stage_through_restored_checkouts(lux, runners, hosts, git_server, fake_image):
    """A resume with a sync restores the state volumes, with the checkout in
    them, then fetches the sync: repositories ends after the volumes, and
    the resume's stage events start at waiting and run forward in order.
    (The fake image has git: the shim applies the sync in the container.)"""
    git_server.create("again", {"README.md": "again\n"})
    runners.start(hosts[0])
    run_id = lux.submit(stage_spec(git_server, "again", fake_image))
    lux.wait_state(run_id, "succeeded", timeout=120)

    feed = Feed(lux)
    feed.ready.wait(10)
    time.sleep(0.5)
    try:
        git_server.commit_on("again", "main", "README.md", "pushed\n")
        pushed = git_server.rev("again", "main")
        git_server.slow("again", CLONE_DELAY)
        lux.run("resume", run_id, "--sync", "again=main", "--secret", f"GIT_TOKEN={git_server.token}")
        seen = follow(lux, run_id, "succeeded")
        assert seen[0][0] == "waiting", seen
        order = [STAGES.index(s) for s, _, _ in seen]
        assert order == sorted(order), f"stage went back: {seen}"

        run = lux.get(run_id)
        assert len(run["placements"]) == 2, run["placements"]
        pl = run["placements"][1]
        for k in ("acceptedAt", "imageReadyAt", "volumesRestoredAt", "reposReadyAt", "containerStartedAt"):
            assert pl.get(k), (k, pl)
        assert at(pl["reposReadyAt"]) >= at(pl["volumesRestoredAt"]), pl
        assert (at(pl["reposReadyAt"]) - at(pl["volumesRestoredAt"])).total_seconds() >= CLONE_DELAY - 0.5, pl
        sync = lux.events(run_id, "git.sync")
        assert sync and sync[-1]["data"].get("to") == pushed and sync[-1]["data"]["status"] != "failed", sync

        events = wait_until(lambda: (lambda e: e if e and e[-1]["stage"] == "succeeded" else None)(feed.of(run_id)),
                            20, 0.2, "the feed never announced the resume's end")
        stages = [e["stage"] for e in events]
        assert stages[0] == "waiting", stages
        assert_ordered(events, "resume")
        assert "repositories" in stages, stages
        # waiting is announced as the Run is resumed, before the next
        # placement exists; every later one is that placement's.
        assert all(e["epoch"] == 2 for e in events[1:]), events
        assert_since_columns(events, pl)
    finally:
        feed.close()

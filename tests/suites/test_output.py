"""Step 2: output. Live from the host, relayed by luxd; after exit from S3;
one cursor across placements."""

from __future__ import annotations

import json
import socket
import threading
import time

import requests

from conftest import generic
from env import ALPINE_IMAGE, wait_until


def test_logs_follow_streams_live(lux, runners, hosts):
    runners.start(hosts[0])
    run_id = lux.submit(generic(ALPINE_IMAGE, "sh", "-c", "for i in 1 2 3 4 5; do echo line-$i; sleep 1; done"))
    p = lux.popen("logs", run_id, "-f")
    first_at = None
    lines = []
    start = time.time()
    for line in p.stdout:
        lines.append(line.strip())
        if first_at is None:
            first_at = time.time() - start
    p.wait(timeout=30)
    assert lines == [f"line-{i}" for i in range(1, 6)], lines
    # Streamed, not delivered at the end: the first line arrived well before
    # the Run's five seconds were up.
    assert first_at is not None and first_at < 4, first_at


def test_output_after_exit_comes_from_s3(env, lux, runners, hosts):
    runners.start(hosts[0])
    run_id = lux.submit(generic(ALPINE_IMAGE, "sh", "-c", "echo from-the-container; echo to-stderr >&2"))
    assert lux.wait_state(run_id, "succeeded")["state"] == "succeeded"
    # Wait for the upload, then take the host away: output must still be there.
    lux.wait_placement_uploaded(run_id)
    runners.stop(hosts[0])
    recs = lux.records(run_id)
    assert [r["data"] for r in recs if r["ch"] == "stdout"] == ["from-the-container\n"]
    assert [r["data"] for r in recs if r["ch"] == "stderr"] == ["to-stderr\n"]
    keys = [o["Key"] for o in env.s3().list_objects_v2(Bucket=env.bucket).get("Contents", [])]
    assert any(run_id in k for k in keys)


def test_cursor_resumes_where_it_left_off(lux, runners, hosts):
    runners.start(hosts[0])
    run_id = lux.submit(generic(ALPINE_IMAGE, "sh", "-c", "for i in $(seq 1 20); do echo n$i; done"))
    lux.wait_state(run_id, "succeeded")
    recs = lux.records(run_id)
    assert len(recs) >= 1
    mid = recs[len(recs) // 2]
    rest = lux.records(run_id, "--since", mid["cursor"])
    assert rest == recs[len(recs) // 2 + 1:]
    assert "".join(r["data"] for r in recs).split() == [f"n{i}" for i in range(1, 21)]


def test_events_and_lifecycle_in_the_stream(lux, runners, hosts):
    runners.start(hosts[0])
    run_id = lux.submit(generic(ALPINE_IMAGE, "true"))
    lux.wait_state(run_id, "succeeded")
    out = lux.run("logs", run_id, "--events", "-o", "json").stdout
    items = [json.loads(l) for l in out.splitlines() if l.strip()]
    events = [i["event"]["type"] for i in items if i.get("ch") == "event"]
    assert "lux.workload" in events
    states = [i["lux"]["data"].get("state") for i in items if "lux" in i and i["lux"]["type"] == "state"]
    assert states[-1] == "succeeded", states


def test_follow_across_a_restart_of_the_runner(lux, runners, hosts):
    """The runner restarts mid-Run: the container keeps going (Podman is
    daemonless), the new runner re-adopts it, and the output stream picks
    up where it was."""
    runners.start(hosts[0])
    run_id = lux.submit(generic(ALPINE_IMAGE, "sh", "-c", "for i in $(seq 1 12); do echo tick-$i; sleep 1; done"))
    lux.wait_output(run_id, "tick-2")
    runners.stop(hosts[0], "KILL")
    runners.start(hosts[0])
    run = lux.wait_state(run_id, "succeeded", "failed", timeout=60)
    assert run["state"] == "succeeded", run
    assert len(run["placements"]) == 1, "re-adopted, not rescheduled"
    out = lux.logs(run_id)
    assert out.split() == [f"tick-{i}" for i in range(1, 13)], out


class OutputFollower:
    """One raw `GET /v1/runs/{id}/output?follow=true&events=true` stream,
    read on a thread. The caller owns the response: close() ends it even
    if the reader is blocked. Its SSE events go to `events` as (name, data);
    an exception or a non-200 answer goes to `errors`, which check()
    raises."""

    def __init__(self, lux, run_id: str, since: str):
        self.events: list[tuple[str, dict]] = []
        self.errors: list[str] = []
        self.ended = threading.Event()
        self.closing = False
        self.thread: threading.Thread | None = None
        self.resp = requests.get(f"{lux.env.luxd_url}/v1/runs/{run_id}/output", stream=True, timeout=(10, 120),
                                 params={"follow": "true", "events": "true", "since": since},
                                 headers={"Authorization": f"Bearer {lux.api_key}"})
        if self.resp.status_code != 200:
            self.errors.append(f"output: HTTP {self.resp.status_code}: {self.resp.text[:500]}")
            self.ended.set()
            return
        self.thread = threading.Thread(target=self._read, daemon=True)
        self.thread.start()

    def _read(self):
        try:
            name = ""
            for line in self.resp.iter_lines(decode_unicode=True):
                if line.startswith("event: "):
                    name = line[len("event: "):]
                elif line.startswith("data: "):
                    self.events.append((name, json.loads(line[len("data: "):])))
                    if name == "error":
                        self.errors.append(f"output error event: {self.events[-1][1]}")
        except Exception as e:  # noqa: BLE001: any failure of the reader fails the test
            if not self.closing:
                self.errors.append(f"output reader: {e!r}")
        finally:
            self.ended.set()

    def check(self):
        if self.errors:
            raise AssertionError("; ".join(self.errors))

    def records(self, epoch: int | None = None) -> list[dict]:
        return [d for n, d in list(self.events) if n == "record" and (epoch is None or d["epoch"] == epoch)]

    def end(self) -> dict | None:
        return next((d for n, d in list(self.events) if n == "end"), None)

    def saw_state(self, epoch: int, state: str) -> bool:
        return any(n == "lux" and d.get("type") == "state" and d.get("epoch") == epoch
                   and d.get("data", {}).get("state") == state for n, d in list(self.events))

    def close(self):
        """End the stream, from this thread, and wait for its reader."""
        self.closing = True
        conn = getattr(self.resp.raw, "connection", None)
        sock = getattr(conn, "sock", None)
        if sock is not None:
            # Wakes a reader blocked in recv(); close() alone may not.
            try:
                sock.shutdown(socket.SHUT_RDWR)
            except OSError:
                pass
        self.resp.close()
        if self.thread is not None:
            self.thread.join(timeout=10)
            assert not self.thread.is_alive(), "the output reader did not stop"


def test_follow_across_same_host_resumes(lux, runners, hosts):
    """A follower that reconnects from its cursor at each stop, as `lux logs -f`
    does, gets each resumed placement's output, also when it reaches the
    placement before the runner has taken it up (held by LUX_TEST_ASSIGN_HOLD
    until the runner has answered the follower "not mine"). In the end it has
    had every record once."""
    hold = "/tmp/lux-assign-hold"
    host = hosts[0]
    runners.start(host, environ={"LUX_TEST_ASSIGN_HOLD": hold})
    spec = generic(ALPINE_IMAGE, "sh", "-c", 'i=0; while :; do i=$((i+1)); echo "e$LUX_EPOCH-$i"; sleep 0.3; done',
                   volumes=[{"name": "data", "path": "/data", "kind": "state"}])
    run_id = lux.submit(spec)
    followers: list[OutputFollower] = []

    def wait_for(f: OutputFollower, cond, timeout: float, message: str):
        def check():
            f.check()
            return cond()
        return wait_until(check, timeout, 0.2, message)

    def finish(f: OutputFollower) -> str:
        """Wait for the stream's end (the Run stopped), close it and return
        the cursor to resume from."""
        wait_for(f, lambda: f.ended.is_set() or f.end(), 60, "the follower's stream never ended at the stop")
        f.close()
        f.check()
        end = f.end()
        assert end is not None, f"the stream closed without an end event: {f.events[-3:]}"
        return end["cursor"]

    def placement(epoch: int) -> dict | None:
        return next((p for p in lux.get(run_id)["placements"] if p["epoch"] == epoch), None)

    def assert_still_assigned(epoch: int):
        held = placement(epoch)
        assert held is not None and held["state"] == "assigned", held

    def unknown_epoch_answered(epoch: int) -> bool:
        """The runner's <hold>.unknown has a line for this Run and epoch."""
        out = host.exec("cat", hold + ".unknown", check=False)
        return any(l.split()[:2] == [run_id, str(epoch)] for l in out.splitlines())

    try:
        missed: list[int] = []
        try:
            lux.wait_output(run_id, "e1-2")
            f = OutputFollower(lux, run_id, "")
            followers.append(f)
            wait_for(f, lambda: f.records(1), 30, "the first placement's output never reached the follower")
            for epoch in range(2, 5):
                host.exec("touch", hold)
                lux.run("stop", run_id, "--wait")
                cursor = finish(f)
                lux.run("resume", run_id)
                wait_until(lambda: (placement(epoch) or {}).get("state") == "assigned", 30, 0.2,
                           f"placement {epoch} never became assigned")
                f = OutputFollower(lux, run_id, cursor)
                followers.append(f)
                # The request is live: it has sent this epoch's scheduled
                # event, and the runner still holds the assignment.
                wait_for(f, lambda: f.saw_state(epoch, "scheduled"), 30,
                         f"the follower from {cursor} never sent placement {epoch}'s scheduled event")
                assert_still_assigned(epoch)
                # And the runner has answered a subscription to it with
                # "not mine": the case the follower must not take as the end.
                wait_until(lambda: unknown_epoch_answered(epoch), 30, 0.2,
                           f"the runner never ended a subscription to placement {epoch} as unknown")
                assert_still_assigned(epoch)
                host.exec("rm", "-f", hold)
                # A miss is noted and the next cycle still runs, so a
                # regression shows on every cycle, not only the first.
                try:
                    wait_for(f, lambda: f.records(epoch), 20, "")
                except AssertionError:
                    f.check()
                    missed.append(epoch)
            lux.run("stop", run_id, "--wait")
            finish(f)
        finally:
            host.exec("rm", "-f", hold, check=False)
            failures = []
            for f in followers:
                try:
                    f.close()
                except AssertionError as e:
                    failures.append(str(e))
            assert not failures, failures
        assert not missed, f"placements {missed}: output never reached the follower that was waiting on them"
        run = lux.get(run_id)
        assert {p["hostName"] for p in run["placements"]} == {host.name}, run["placements"]
        got = [(r["cursor"], r["data"]) for f in followers for r in f.records() if r["ch"] == "stdout"]
        want = [(r["cursor"], r["data"]) for r in lux.records(run_id) if r["ch"] == "stdout"]
        assert got == want
    finally:
        lux.run("cancel", run_id, "--wait", check=False)

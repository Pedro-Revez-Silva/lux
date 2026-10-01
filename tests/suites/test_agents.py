"""Coding agents through lux, one set of tests for every harness.

Each test takes a `harness` (see tests/harnesses.py) and runs once per
agent: against lux-fake speaking that agent's protocol (always), and
against the real CLI (with -m agents and its credentials). Behaviour that
legitimately differs between protocols is asserted from the harness's
capabilities, never its name.
"""

from __future__ import annotations

import json
import time

from conftest import harnesses
from env import wait_until


def test_starts_reports_a_session_and_waits_for_input(lux, runners, hosts, harness):
    runners.start(hosts[0])
    run_id = lux.submit(harness.spec(harness.say("ready")))
    run = lux.wait_activity(run_id, "idle", timeout=harness.timeout)
    assert run["state"] == "running"
    assert run["sessionId"], "the adapter reported no session id"
    assert "ready" in lux.logs(run_id).lower()
    assert "waiting for input" in lux.run("ls").stdout


def test_steering_is_delivered_and_acknowledged(lux, runners, hosts, harness):
    runners.start(hosts[0])
    run_id = lux.submit(harness.spec(harness.say("first")))
    lux.wait_activity(run_id, "idle", timeout=harness.timeout)
    lux.run("steer", run_id, harness.say("second"), "--request-id", "req-1")
    wait_until(lambda: "second" in lux.logs(run_id).lower(), harness.timeout, 0.5, "steer never answered")
    def delivered():
        return {e["data"]["requestId"]: e["data"] for e in lux.events(run_id, "input.delivered")}
    by_id = wait_until(lambda: (d := delivered()) and "req-1" in d and d, 20, 0.3, "no delivery ack")
    # The first prompt is acknowledged too, and each ack carries what was
    # delivered.
    assert by_id["prompt"]["text"] == harness.say("first"), by_id
    assert by_id["req-1"]["text"] == harness.say("second"), by_id


def test_replies_read_one_per_line(lux, runners, hosts, harness):
    """Agents stream replies in pieces; each still reads as its own line."""
    runners.start(hosts[0])
    run_id = lux.submit(harness.spec(harness.say("alpha")))
    lux.wait_activity(run_id, "idle", timeout=harness.timeout)
    lux.run("steer", run_id, harness.say("beta"))
    wait_until(lambda: "beta" in lux.logs(run_id).lower(), harness.timeout, 0.5, "no second reply")
    lux.wait_activity(run_id, "idle", timeout=harness.timeout)
    lines = [l.strip().strip(".").lower() for l in lux.logs(run_id).splitlines()]
    assert "alpha" in lines and "beta" in lines, lines


def test_interrupt_ends_the_turn_not_the_run(lux, runners, hosts, harness):
    runners.start(hosts[0])
    run_id = lux.submit(harness.spec(harness.say("ready")))
    lux.wait_activity(run_id, "idle", timeout=harness.timeout)
    lux.run("steer", run_id, harness.long_turn())
    lux.wait_activity(run_id, "busy", timeout=harness.timeout)
    lux.run("interrupt", run_id)
    run = lux.wait_activity(run_id, "idle", timeout=harness.timeout)
    assert run["state"] == "running"
    assert "never" not in lux.logs(run_id).lower().split()


TURN_ENDS = ("codex.turn/completed", "claude.result", "acp.turn_end")


INPUT_RECORDS = {"lux.input", "lux.input.consumed", "lux.input.failed"}


def _input_records(lux, run_id: str, request_id: str) -> list[dict]:
    """An input's records, in output order, each with "record" (its type)
    and "step": lux.input's phase (accepted or failed), then consumed or
    failed from lux.input.consumed / lux.input.failed."""
    out = []
    for r in lux.records(run_id, "--events"):
        ev = r.get("event", {})
        if ev.get("type") in INPUT_RECORDS and ev["data"].get("requestId") == request_id:
            d = dict(ev["data"], record=ev["type"])
            d["step"] = d["phase"] if ev["type"] == "lux.input" else ev["type"].removeprefix("lux.input.")
            out.append(d)
    return out


def _check_phases(lux, run_id: str, harness, request_id: str) -> None:
    """accepted, then consumed where the adapter has a receipt: once each,
    in order, in the output and as luxd events. lux.input itself is written
    exactly once: a consumer that ignores the later records sees one
    answer per input."""
    want = ["accepted", "consumed"] if harness.caps.steer_receipt else ["accepted"]
    try:
        recs = wait_until(lambda: (r := _input_records(lux, run_id, request_id)) and len(r) >= len(want) and r,
                          harness.timeout, 0.5, f"no lux.input records for {request_id}")
    except AssertionError as e:
        warnings = [r["event"]["data"] for r in lux.records(run_id, "--events")
                    if r.get("event", {}).get("type") == "lux.warning"]
        raise AssertionError(f"{e}: {_input_records(lux, run_id, request_id)}; warnings: {warnings}") from None
    assert [r["step"] for r in recs] == want, recs
    assert [r["record"] for r in recs].count("lux.input") == 1, recs
    lands = "next_step" if harness.caps.steer_joins_turn else "next_turn"
    assert recs[0]["lands"] == lands and recs[0]["receipt"] == harness.caps.steer_receipt, recs[0]
    events = [e["type"] for e in lux.json("events", run_id)
              if e["type"].startswith("input.") and e["data"].get("requestId") == request_id]
    assert events == ["input.delivered", "input.consumed"][:len(want)], events


def _tool_calls(records: list[dict]) -> list[dict]:
    """The agent's shell tool calls, from its protocol's own events, in
    output order: {"id", "command", "start", "done", "output"}, where start
    and done are record indexes (done None while it runs).

    Claude Code: a Bash tool_use, then its tool_result. Codex: a
    commandExecution item, started then completed. OpenCode (ACP): an
    execute tool_call, an in_progress update naming the command, then
    completed or failed with its output."""
    calls: dict[str, dict] = {}

    def started(i, cid, command):
        c = calls.setdefault(cid, {"id": cid, "command": None, "start": None, "done": None, "output": None})
        if command and c["start"] is None:
            c["command"], c["start"] = command, i

    def finished(i, cid, output):
        c = calls.get(cid)
        if c is not None and c["done"] is None:
            c["done"], c["output"] = i, output or ""

    for i, r in enumerate(records):
        ev = r.get("event") or {}
        typ, d = ev.get("type"), ev.get("data") or {}
        if typ == "claude.assistant":
            for b in d.get("message", {}).get("content", []):
                if b.get("type") == "tool_use" and b.get("name") == "Bash":
                    started(i, b["id"], b.get("input", {}).get("command"))
        elif typ == "claude.user":
            for b in d.get("message", {}).get("content", []) if isinstance(d.get("message", {}).get("content"), list) else []:
                if b.get("type") == "tool_result":
                    out = b.get("content")
                    if isinstance(out, list):
                        out = "".join(x.get("text", "") for x in out)
                    finished(i, b["tool_use_id"], out)
        elif typ in ("codex.item/started", "codex.item/completed"):
            item = d.get("item", {})
            if item.get("type") == "commandExecution":
                started(i, item["id"], item.get("command"))
                if typ == "codex.item/completed":
                    finished(i, item["id"], item.get("aggregatedOutput"))
        elif typ in ("acp.tool_call", "acp.tool_call_update") and (d.get("kind") in (None, "execute")):
            cid = d.get("toolCallId")
            if not cid:
                continue
            if cid not in calls and d.get("kind") != "execute":
                continue
            started(i, cid, (d.get("rawInput") or {}).get("command"))
            if d.get("status") in ("completed", "failed"):
                out = (d.get("rawOutput") or {}).get("output")
                if out is None:
                    out = "".join((c.get("content") or {}).get("text", "") for c in d.get("content") or [])
                finished(i, cid, out)
    return sorted((c for c in calls.values() if c["start"] is not None), key=lambda c: c["start"])


def _tool(calls: list[dict], word: str) -> dict | None:
    """The first tool call whose command has word in it."""
    return next((c for c in calls if word in c["command"]), None)


def _wait_tool(lux, harness, run_id: str, word: str, what: str) -> dict:
    """Wait for the tool call whose command has word in it to finish; on
    timeout, say what the agent did instead."""
    try:
        return wait_until(lambda: (t := _tool(_tool_calls(lux.records(run_id, "--events")), word)) and t["done"] is not None and t,
                          harness.timeout, 0.5, what)
    except AssertionError as e:
        records = lux.records(run_id, "--events")
        seen = {"tools": [{k: c[k] for k in ("command", "start", "done")} for c in _tool_calls(records)],
                "inputs": [r["event"] for r in records if r.get("event", {}).get("type", "").startswith("lux.input")],
                "turn_ends": [i for i, r in enumerate(records) if r.get("event", {}).get("type") in TURN_ENDS],
                "reply": lux.logs(run_id)[-1500:]}
        raise AssertionError(f"{e}; {json.dumps(seen)[:6000]}") from None


def _steer_mid_tool(lux, harness, run_id: str, sleep: str, steer: str, request_id: str) -> dict:
    """Steer once the agent's tool running `sleep` has started (by its
    protocol's own tool-start event); returns that tool call."""
    try:
        tool = wait_until(lambda: _tool(_tool_calls(lux.records(run_id, "--events")), sleep),
                          harness.timeout, 0.3, f"the agent never started its `{sleep}` tool")
    except AssertionError as e:
        seen = [json.dumps(r.get("event"))[:300] for r in lux.records(run_id, "--events") if r.get("event")]
        raise AssertionError(f"{e}; events: {seen[-40:]}") from None
    lux.run("steer", run_id, steer, "--request-id", request_id)
    return tool


def _prompts(harness, token: str) -> tuple[str, str]:
    """The turn and the steer of the steering tests: FIRST after a sleep,
    then SECOND, each its own shell tool call; the steer runs STEER."""
    if harness.real:
        return (("Run `sleep 20 && echo FIRST` with your shell tool, in the foreground; do not background it. "
                 "Wait for its output; only after that, in a separate tool call, run `echo SECOND`. "
                 "Then reply DONE. One tool call at a time."),
                f"Before anything else after the current command, run `echo STEER-{token}` with your shell tool, then continue.")
    return "sh sleep 5 && echo FIRST\nsh echo SECOND", f"sh echo STEER-{token}"


def _skip_if_backgrounded(call: dict) -> None:
    """These tests need the agent's tool running while they steer: skip,
    rather than fail on that precondition, when the agent ran it as a
    background task (its tool call returns at once, saying so)."""
    out = (call.get("output") or "").lower()
    if call.get("done") is not None and "first" not in out and (
            "background" in out or "process running with session" in out):
        import pytest
        pytest.skip(f"skipped: agent ran the tool in the background: {call['output'][:200]}")


def test_mid_turn_steering(lux, runners, hosts, harness):
    """Input sent while the agent's tool runs is never lost, and the tool is
    not cancelled. Where the protocol can, it is read at the agent's next
    step, in the running turn: its tool runs after the running one ends and
    before the turn's next one. Otherwise it runs after the turn. lux says
    when the input was accepted and, where it can, when the agent read it."""
    runners.start(hosts[0])
    token = f"{int(time.time() * 1000) % 1000000:06d}"
    prompt, steer = _prompts(harness, token)
    steered = f"STEER-{token}"
    run_id = lux.submit(harness.foreground_spec(prompt))
    first = _steer_mid_tool(lux, harness, run_id, "sleep", steer, "mid-1")
    _wait_tool(lux, harness, run_id, steered, "the steer's tool never ran")
    lux.wait_activity(run_id, "idle", timeout=harness.timeout)
    records = lux.records(run_id, "--events")
    calls = _tool_calls(records)
    first = next(c for c in calls if c["id"] == first["id"])
    _skip_if_backgrounded(first)
    steer_call, second = _tool(calls, steered), _tool(calls, "SECOND")
    # The tool that was running when the steer came finished, and ran.
    assert first["done"] is not None and "FIRST" in first["output"], first
    assert steered in steer_call["output"], steer_call
    assert second is not None and "SECOND" in second["output"], calls
    turn_ends = [i for i, r in enumerate(records) if r.get("event", {}).get("type") in TURN_ENDS]
    if harness.caps.steer_joins_turn:
        # Read at the next step: after the running tool, before the next.
        assert first["done"] < steer_call["start"] < second["start"], calls
        assert len(turn_ends) == 1, turn_ends
    else:
        assert second["done"] < turn_ends[0] < steer_call["start"], (calls, turn_ends)
        assert len(turn_ends) == 2, turn_ends
    _check_phases(lux, run_id, harness, "mid-1")
    # Once each: the prompt's too.
    assert [r["step"] for r in _input_records(lux, run_id, "prompt")][0] == "accepted"
    lux.run("cancel", run_id)


@harnesses(lambda h: h.caps.steer_joins_turn)
def test_interrupt_carries_an_unread_steer(lux, runners, hosts, harness):
    """"Interrupt now": a steer accepted during a long tool, then an
    interrupt with no text. The running turn ends, its tool cut short; the
    steer is not failed but run in the turn after it, consumed exactly
    once."""
    runners.start(hosts[0])
    token = f"{int(time.time() * 1000) % 1000000:06d}"
    steered = f"STEER-{token}"
    # Claude Code refuses a `sleep` of a minute before another command
    # (it asks for its Monitor tool instead); 20 s is the mid-turn test's.
    if harness.real:
        prompt = ("Run `sleep 20 && echo FIRST` with your shell tool, in the foreground; do not background it. "
                  "Then reply DONE.")
        steer = f"Stop what you were doing and just run `echo {steered}` with your shell tool."
    else:
        prompt, steer = "sh sleep 60 && echo FIRST", f"sh echo {steered}"
    run_id = lux.submit(harness.foreground_spec(prompt))
    first = _steer_mid_tool(lux, harness, run_id, "sleep", steer, "carry-1")
    wait_until(lambda: any(r["step"] == "accepted" for r in _input_records(lux, run_id, "carry-1")),
               harness.timeout, 0.3, "the steer was never accepted")
    running = next(c for c in _tool_calls(lux.records(run_id, "--events")) if c["id"] == first["id"])
    _skip_if_backgrounded(running)
    assert running["done"] is None, f"the tool was not running when the interrupt was sent: {running}"
    # What POST input {"interrupt": true} with no text does (dude's
    # "Interrupt now").
    lux.run("interrupt", run_id)
    _wait_tool(lux, harness, run_id, steered, "the carried steer's tool never ran")
    lux.wait_activity(run_id, "idle", timeout=harness.timeout)
    _check_phases(lux, run_id, harness, "carry-1")
    records = lux.records(run_id, "--events")
    calls = _tool_calls(records)
    first = next(c for c in calls if c["id"] == first["id"])
    steer_call = _tool(calls, steered)
    turn_ends = [i for i, r in enumerate(records) if r.get("event", {}).get("type") in TURN_ENDS]
    consumed = [i for i, r in enumerate(records) if r.get("event", {}).get("type") == "lux.input.consumed"
                and r["event"]["data"] == {"requestId": "carry-1"}]
    # The interrupted tool never finished its work.
    assert "FIRST" not in [l.strip() for l in (first["output"] or "").splitlines()], first
    # Read and run in the turn after the interrupted one: after its end,
    # before the next one's.
    assert len(turn_ends) >= 2 and turn_ends[0] < consumed[0] < turn_ends[1], (turn_ends, consumed)
    assert turn_ends[0] < steer_call["start"] < turn_ends[1] and steered in steer_call["output"], (turn_ends, steer_call)
    lux.run("cancel", run_id)


@harnesses(lambda h: h.caps.steer_joins_turn)
def test_steer_in_the_final_step(lux, runners, hosts, harness):
    """A steer that arrives during the turn's final step (no tool call
    follows) is still read: in the same turn, or as the next turn where the
    agent ends its turn first (Claude Code)."""
    if harness.real:
        import pytest
        pytest.skip("depends on scripted timing")
    runners.start(hosts[0])
    run_id = lux.submit(harness.spec("echo last-step\nsleep 3"))
    lux.wait_output(run_id, "last-step")
    lux.run("steer", run_id, "echo after-last", "--request-id", "final-1")
    lux.wait_output(run_id, "after-last")
    lux.wait_activity(run_id, "idle")
    turn_ends = [r for r in lux.records(run_id, "--events") if r.get("event", {}).get("type") in TURN_ENDS]
    assert len(turn_ends) == (2 if harness.caps.steer_in_final_step_is_next_turn else 1), turn_ends
    _check_phases(lux, run_id, harness, "final-1")


def test_stop_resume_elsewhere_and_remember(lux, runners, hosts, harness):
    """The core story: write a file, be steered, stop, resume on another
    host, and answer from the restored conversation."""
    a, b = hosts[0], hosts[1]
    runners.start(a)
    spec = harness.spec(harness.write("/workspace/word.txt", "PINEAPPLE"))
    run_id = lux.submit(spec)
    session = lux.wait_activity(run_id, "idle", timeout=harness.timeout)["sessionId"]
    lux.run("steer", run_id, harness.say("steered"))
    wait_until(lambda: "steered" in lux.logs(run_id).lower(), harness.timeout, 0.5, "steer never answered")
    lux.wait_activity(run_id, "idle", timeout=harness.timeout)

    lux.run("stop", run_id, "--wait")
    run = lux.get(run_id)
    assert run["state"] == "stopped"
    # A graceful stop: the adapter wound the agent down; it was not killed
    # after the grace period (137), nor, where the protocol wants SIGINT,
    # sent SIGTERM (143).
    code = run["placements"][0]["exitCode"]
    assert code != 137, "the agent had to be killed: its graceful stop did not work"
    if harness.caps.stop_is_sigint:
        assert code != 143, "stopped with SIGTERM; this agent needs SIGINT to end its turn cleanly"
    lux.wait_uploaded(run_id)
    runners.stop(a)
    runners.start(b)

    since = lux.records(run_id)[-1]["cursor"]
    lux.run("resume", run_id, "--wait", "--input", harness.recall("word.txt"), *harness.resume_secrets(spec))
    wait_until(lambda: "PINEAPPLE" in lux.logs(run_id, "--since", since).upper(), harness.timeout * 1.5, 1,
               "resumed session did not remember")
    run = lux.get(run_id)
    assert [p["hostName"] for p in run["placements"]] == [a.name, b.name]
    assert run["sessionId"] == session, "the conversation restarted instead of continuing"
    out = lux.logs(run_id)
    for v in harness.secret_values():
        assert v not in out, "a credential leaked into output"
    lux.run("cancel", run_id)


@harnesses(lambda h: h.name == "codex")
def test_codex_key_as_an_env_secret(lux, runners, hosts, harness):
    """Codex reads its key from ~/.codex/auth.json; the adapter writes that
    file from an OPENAI_API_KEY secret, on the secrets tmpfs, so it never
    reaches disk. (Fake only: it reads the file back.)"""
    if harness.real:
        import pytest
        pytest.skip("the real variant covers this by authenticating at all")
    runners.start(hosts[0])
    spec = harness.spec("cat-file /home/agent/.codex/auth.json",
                        secrets=[{"name": "OPENAI_API_KEY", "value": "sk-test-abcdef"}])
    run_id = lux.submit(spec)
    lux.wait_activity(run_id, "idle")
    out = lux.logs(run_id)
    assert '"auth_mode":"apikey"' in out and "[REDACTED:OPENAI_API_KEY]" in out, out
    assert "sk-test-abcdef" not in out
    host = hosts[0]
    link = host.exec("sh", "-c", "readlink $(podman volume inspect --format '{{.Mountpoint}}' "
                     f"lux-{run_id}-home)/.codex/auth.json").strip()
    assert link.startswith("/.lux/secrets/"), link
    lux.run("stop", run_id, "--wait")
    assert host.exec("sh", "-c", "grep -rl sk-test-abcdef /var/lib/lux 2>/dev/null; true").strip() == ""

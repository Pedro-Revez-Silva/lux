"""Steering that does not depend on the agent: delivery exactly once,
generic workloads (stdin, SIGINT), and input before a Run starts. What
every agent must do is in test_agents.py, run against every harness."""

from __future__ import annotations

import time

import pytest

from conftest import CLIError, fake_agent, generic
from env import ALPINE_IMAGE, wait_until


def test_retried_input_is_delivered_once(lux, runners, hosts, fake_image):
    runners.start(hosts[0])
    run_id = lux.submit(fake_agent(fake_image, "echo hi"))
    lux.wait_activity(run_id, "idle")
    for _ in range(3):
        lux.run("steer", run_id, "echo once", "--request-id", "same")
    lux.wait_output(run_id, "once")
    lux.wait_activity(run_id, "idle")
    # All three reached luxd; the shim delivered one.
    assert len(lux.events(run_id, "input")) == 3
    steered = lambda: [e for e in lux.events(run_id, "input.delivered") if e["data"]["requestId"] == "same"]
    wait_until(steered, 20, 0.3, "no delivery ack")
    assert len(steered()) == 1
    assert lux.logs(run_id).count("once") == 1


def test_generic_input_goes_to_stdin(lux, runners, hosts, fake_image):
    runners.start(hosts[0])
    run_id = lux.submit(generic(fake_image, "lux-fake", "plain"))
    lux.wait_state(run_id, "running")
    lux.run("steer", run_id, "echo via-stdin")
    lux.wait_output(run_id, "via-stdin")
    lux.run("steer", run_id, "exit 4")
    assert lux.wait_state(run_id, "failed")["exitCode"] == 4


def test_generic_interrupt_is_sigint(lux, runners, hosts):
    runners.start(hosts[0])
    run_id = lux.submit(generic(ALPINE_IMAGE, "sh", "-c", "trap 'echo got-int; exit 0' INT; echo ready; while :; do sleep 0.2; done"))
    lux.wait_output(run_id, "ready")
    lux.run("interrupt", run_id)
    assert lux.wait_state(run_id, "succeeded", "failed")["state"] == "succeeded"
    assert "got-int" in lux.logs(run_id)


def test_input_before_start_is_rejected(lux, fake_image):
    run_id = lux.submit(fake_agent(fake_image, "echo hi"))  # no hosts: stays submitted
    with pytest.raises(CLIError) as e:
        lux.run("steer", run_id, "hello")
    assert "not started" in e.value.stderr


def test_fake_conditionals(lux, runners, hosts, fake_image):
    """lux-fake's if-exists / unless-exists run the rest of the line on a
    path's presence (for scripted agents that react to the tree)."""
    runners.start(hosts[0])
    script = "\n".join([
        "unless-exists FIXED.md echo finding: not fixed",
        "if-exists FIXED.md echo WRONG-1",
        "write FIXED.md done",
        "unless-exists FIXED.md echo WRONG-2",
        "if-exists FIXED.md echo fixed now",
        "if-exists /etc/os-release unless-exists /nope echo nested-ok",
    ])
    run_id = lux.submit(fake_agent(fake_image, script))
    lux.wait_activity(run_id, "idle")
    out = lux.logs(run_id)
    assert "finding: not fixed" in out and "fixed now" in out and "nested-ok" in out, out
    assert "WRONG" not in out, out
    lux.run("terminate", run_id)


def test_acp_turn_end_carries_the_agents_usage(lux, runners, hosts, fake_image):
    """The prompt response's usage (OpenCode reports it there) reaches the
    turn's acp.turn_end event, as the agent sent it."""
    runners.start(hosts[0])
    run_id = lux.submit(fake_agent(fake_image, "echo hi", adapter="acp"))
    lux.wait_activity(run_id, "idle")
    ends = [r["event"] for r in lux.records(run_id, "--events") if r.get("ch") == "event"
            and r["event"].get("type") == "acp.turn_end"]
    assert ends and ends[0]["data"]["usage"] == {"inputTokens": 2, "outputTokens": 10, "totalTokens": 12}, ends


@pytest.mark.parametrize("adapter,event,usage", [
    ("claude-code", "claude.turn_end", {"input_tokens": 3, "output_tokens": 9}),
    ("codex", "codex.turn_end", {"last": {"inputTokens": 5, "outputTokens": 7, "cachedInputTokens": 0,
                                          "reasoningOutputTokens": 0, "totalTokens": 12},
                                 "total": {"inputTokens": 5, "outputTokens": 7, "cachedInputTokens": 0,
                                           "reasoningOutputTokens": 0, "totalTokens": 12},
                                 "modelContextWindow": 200000}),
])
def test_turn_end_carries_the_agents_usage(lux, runners, hosts, fake_image, adapter, event, usage):
    """Each agent adapter ends a turn with <adapter>.turn_end carrying the
    agent's own usage report, untranslated (lux-fake reports it as the
    real agent does)."""
    runners.start(hosts[0])
    run_id = lux.submit(fake_agent(fake_image, "echo hi", adapter=adapter))
    lux.wait_activity(run_id, "idle")
    ends = [r["event"] for r in lux.records(run_id, "--events") if r.get("ch") == "event"
            and r["event"].get("type") == event]
    assert ends and ends[0]["data"]["usage"] == usage, ends

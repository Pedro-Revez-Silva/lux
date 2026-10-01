"""Agent harnesses, described once, so every agent test runs against all of
them.

A harness is a coding agent lux can drive: which adapter speaks its
protocol, how to build a spec for it, what credentials it needs, and what
its protocol can do. Each has two variants:

  fake  lux-fake speaking the agent's protocol. No model; always runs.
  real  the real CLI. Makes model calls; runs only when its credentials are
        set (and its suite is selected, so the harness builds its image).

Tests take a `harness` fixture and assert from capabilities, never from a
harness's name. Adding an agent is one entry in HARNESSES.
"""

from __future__ import annotations

import json
import os
from dataclasses import dataclass, field
from typing import Callable

from conftest import AGENT_VOLUMES


@dataclass(frozen=True)
class Caps:
    """What an agent's protocol does, as seen through its adapter."""

    # Input sent during a turn joins that turn, read at the agent's next
    # step (Codex turn/steer, OpenCode), rather than running after it as a
    # turn of its own (generic ACP).
    steer_joins_turn: bool = False
    # A steer is reported read (lux.input.consumed) when the agent's
    # model step has it.
    steer_receipt: bool = False
    # With steer_joins_turn: a steer that arrives during the turn's final
    # step (no tool call follows it) runs as the next turn instead (Claude
    # Code ends the turn, then runs the queued message).
    steer_in_final_step_is_next_turn: bool = False
    # Stopping must be SIGINT, which ends the running turn cleanly; SIGTERM
    # would leave it unfinished (Claude Code).
    stop_is_sigint: bool = False


@dataclass(frozen=True)
class Harness:
    name: str  # also the image name for the real variant
    adapter: str
    caps: Caps
    # The real CLI: command, secrets and env, built from the environment.
    real_command: Callable[[], list[str]] = field(default=lambda: [])
    real_secrets: Callable[[], list[dict]] = field(default=lambda: [])
    real_env: Callable[[], dict] = field(default=lambda: {})
    # Env for the real variant that keeps a shell tool in the foreground,
    # for tests that need it still running when they steer or interrupt.
    foreground_env: dict = field(default_factory=dict)
    # Environment variables the real variant needs.
    credentials: tuple[str, ...] = ()
    # The fake variant's command, when lux-fake needs arguments to act as
    # this agent (opencode's adapter adds --port only to an `acp` command).
    fake_command: list[str] = field(default_factory=lambda: ["lux-fake"])
    # Credential values that must never appear in output.
    secret_values: Callable[[], list[str]] = field(default=lambda: [])


def _env(name: str) -> str:
    return os.environ.get(name, "")


def _opencode_config() -> str:
    cfg = json.loads(_env("LUX_TEST_OPENCODE_CONFIG") or "{}")
    cfg["model"] = _env("LUX_TEST_OPENCODE_MODEL")
    return json.dumps(cfg)


def _opencode_keys() -> list[str]:
    auth = json.loads(_env("LUX_TEST_OPENCODE_AUTH") or "{}")
    return [p["key"] for p in auth.values() if isinstance(p, dict) and p.get("key")]


def _codex_command() -> list[str]:
    cmd = ["codex", "-c", "approval_policy=never", "-c", "sandbox_mode=danger-full-access"]
    if base := _env("LUX_TEST_OPENAI_BASE_URL"):
        cmd += ["-c", f'openai_base_url="{base}"']
    if model := _env("LUX_TEST_CODEX_MODEL"):
        cmd += ["-c", f"model={model}"]
    return cmd


HARNESSES = [
    Harness(
        name="claude",
        adapter="claude-code",
        caps=Caps(steer_joins_turn=True, steer_receipt=True, steer_in_final_step_is_next_turn=True, stop_is_sigint=True),
        real_command=lambda: ["claude", "--model", _env("LUX_TEST_CLAUDE_MODEL") or "haiku", "--permission-mode", "bypassPermissions"],
        real_secrets=lambda: [{"name": "ANTHROPIC_API_KEY", "value": _env("LUX_TEST_ANTHROPIC_API_KEY")}],
        real_env=lambda: {"ANTHROPIC_BASE_URL": b} if (b := _env("LUX_TEST_ANTHROPIC_BASE_URL")) else {},
        # 2.1.280: drops run_in_background from Bash and never
        # auto-backgrounds a long command.
        foreground_env={"CLAUDE_CODE_DISABLE_BACKGROUND_TASKS": "1"},
        credentials=("LUX_TEST_ANTHROPIC_API_KEY",),
        secret_values=lambda: [_env("LUX_TEST_ANTHROPIC_API_KEY")],
    ),
    Harness(
        name="codex",
        adapter="codex",
        caps=Caps(steer_joins_turn=True, steer_receipt=True),
        real_command=_codex_command,
        # An ordinary env secret: the codex adapter writes the auth.json
        # Codex reads.
        real_secrets=lambda: [{"name": "OPENAI_API_KEY", "value": _env("LUX_TEST_OPENAI_API_KEY")}],
        credentials=("LUX_TEST_OPENAI_API_KEY",),
        secret_values=lambda: [_env("LUX_TEST_OPENAI_API_KEY")],
    ),
    Harness(
        name="opencode",
        adapter="opencode",
        caps=Caps(steer_joins_turn=True, steer_receipt=True),
        fake_command=["lux-fake", "acp"],
        real_command=lambda: ["opencode", "acp"],
        # Providers in opencode.json, keys in auth.json: real user config, so
        # both are file secrets (tmpfs, never snapshotted).
        real_secrets=lambda: [
            {"name": "opencode_auth", "value": _env("LUX_TEST_OPENCODE_AUTH"), "as": "file",
             "path": "/home/agent/.local/share/opencode/auth.json"},
            {"name": "opencode_config", "value": _opencode_config(), "as": "file",
             "path": "/home/agent/.config/opencode/opencode.json"},
        ],
        credentials=("LUX_TEST_OPENCODE_AUTH", "LUX_TEST_OPENCODE_CONFIG", "LUX_TEST_OPENCODE_MODEL"),
        secret_values=_opencode_keys,
    ),
    # Any other ACP agent: its fake variant is the generic ACP path.
    Harness(name="acp", adapter="acp", caps=Caps()),
]

BY_NAME = {h.name: h for h in HARNESSES}


@dataclass
class Variant:
    """A harness made concrete: an image and a way to build specs."""

    harness: Harness
    real: bool
    image: str

    @property
    def caps(self) -> Caps:
        return self.harness.caps

    @property
    def id(self) -> str:
        return f"{self.harness.name}-{'real' if self.real else 'fake'}"

    # Real models are slow; fakes answer at once.
    @property
    def timeout(self) -> float:
        return 240 if self.real else 30

    def spec(self, prompt: str, **extra) -> dict:
        h = self.harness
        spec = {
            "image": {"ref": self.image},
            "workload": {"adapter": h.adapter, "prompt": prompt, "workdir": "/workspace",
                         "command": h.real_command() if self.real else list(h.fake_command)},
            "volumes": [dict(v) for v in AGENT_VOLUMES],
            "secrets": h.real_secrets() if self.real else [],
        }
        if self.real:
            spec["network"] = {"unrestricted": True}
            if env := h.real_env():
                spec["env"] = env
        for k, v in extra.items():
            if isinstance(v, dict) and isinstance(spec.get(k), dict):
                spec[k] = {**spec[k], **v}
            elif isinstance(v, list) and isinstance(spec.get(k), list):
                spec[k] = spec[k] + v
            else:
                spec[k] = v
        return spec

    def foreground_spec(self, prompt: str) -> dict:
        """spec, with the agent's shell tool kept in the foreground."""
        if self.real and self.harness.foreground_env:
            return self.spec(prompt, env=dict(self.harness.foreground_env))
        return self.spec(prompt)

    def resume_secrets(self, spec: dict) -> list[str]:
        """--secret arguments to resume a Run built from spec."""
        args = []
        for sec in spec.get("secrets", []):
            args += ["--secret", f"{sec['name']}={sec['value']}"]
        return args

    def secret_values(self) -> list[str]:
        return [v for v in self.harness.secret_values() if v] if self.real else []

    # Prompts. A fake agent follows a script; a real one is asked in words.
    def say(self, word: str) -> str:
        return f"echo {word}" if not self.real else f"Reply with just the word: {word}"

    def write(self, path: str, word: str) -> str:
        if not self.real:
            return f"write {path} {word}\necho done"
        return f"Create the file {path} containing exactly the word {word}, then reply with just: done"

    def recall(self, path: str) -> str:
        """Ask for what was written earlier, from the conversation."""
        if not self.real:
            return "history"
        return f"Without using any tools: what word did you write to {path} earlier? Reply with just the word."

    def long_turn(self) -> str:
        """A turn that runs long enough to be steered or interrupted."""
        if not self.real:
            return "echo long-turn\nsleep 60\necho never"
        return "Count slowly from 1 to 200, one number per line, then reply: never"


def variants(env) -> list[tuple[Harness, bool]]:
    """Every (harness, real?) pair, fakes first."""
    return [(h, False) for h in HARNESSES] + [(h, True) for h in HARNESSES if h.credentials]

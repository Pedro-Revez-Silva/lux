import { describe, expect, test } from "bun:test";
import { mkdtempSync, symlinkSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { SHELL_COMMAND } from "./exec.ts";

// Runs SHELL_COMMAND with PATH set to dir, as the terminal would, and
// returns what it printed for input.
function runShell(dir: string, input: string): string[] {
  const home = mkdtempSync(join(tmpdir(), "lux-home-"));
  const r = Bun.spawnSync(SHELL_COMMAND, { env: { PATH: dir, HOME: home }, stdin: Buffer.from(input), stderr: "pipe" });
  expect(r.exitCode).toBe(0);
  return r.stdout.toString().split("\n");
}

describe("SHELL_COMMAND", () => {
  test("falls back to sh when there is no bash", () => {
    // Neither bash nor sh on PATH: a failed exec would end the shell (127).
    const dir = mkdtempSync(join(tmpdir(), "lux-path-"));
    expect(runShell(dir, "echo shell-$((20+22)); exit\n")).toContain("shell-42");
  });

  test("prefers bash", () => {
    const bash = Bun.which("bash");
    if (!bash) return;
    const dir = mkdtempSync(join(tmpdir(), "lux-path-"));
    symlinkSync(bash, join(dir, "bash"));
    expect(runShell(dir, "echo bash-${BASH_VERSION:+yes}; exit\n")).toContain("bash-yes");
  });
});

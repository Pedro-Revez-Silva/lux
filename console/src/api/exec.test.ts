import { afterEach, describe, expect, test } from "bun:test";
import { mkdtempSync, rmSync, symlinkSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { SHELL_COMMAND } from "./exec.ts";

const dirs: string[] = [];
function tempDir(): string {
  const d = mkdtempSync(join(tmpdir(), "lux-shell-"));
  dirs.push(d);
  return d;
}
afterEach(() => {
  for (const d of dirs.splice(0)) rmSync(d, { recursive: true, force: true });
});

// Runs SHELL_COMMAND with PATH set to dir, as the terminal would, and
// returns what it printed for input.
function runShell(dir: string, input: string): string[] {
  const home = tempDir();
  const r = Bun.spawnSync(SHELL_COMMAND, { env: { PATH: dir, HOME: home }, stdin: Buffer.from(input), stderr: "pipe" });
  expect(r.exitCode).toBe(0);
  return r.stdout.toString().split("\n");
}

describe("SHELL_COMMAND", () => {
  test("falls back to sh when there is no bash", () => {
    // Neither bash nor sh on PATH: a failed exec would end the shell (127).
    expect(runShell(tempDir(), "echo shell-$((20+22)); exit\n")).toContain("shell-42");
  });

  const bash = Bun.which("bash");
  test.skipIf(!bash)("prefers bash", () => {
    const dir = tempDir();
    symlinkSync(bash!, join(dir, "bash"));
    // $0 too: bash as /bin/sh (macOS) sets BASH_VERSION in the fallback.
    const out = runShell(dir, "echo bash-${BASH_VERSION:+yes}; echo \"0=$0\"; exit\n");
    expect(out).toContain("bash-yes");
    expect(out).toContain("0=bash");
  });
});

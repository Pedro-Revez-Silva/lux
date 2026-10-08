import { expect, test } from "bun:test";
import { durationWords, idleText, lifetimeText, wakeText } from "./serverText.ts";

test("Go durations read as a person writes them; zero is never", () => {
  expect(durationWords("10m0s")).toBe("10m");
  expect(durationWords("720h0m0s")).toBe("30d");
  expect(durationWords("1h30m0s")).toBe("1h 30m");
  expect(durationWords("4s")).toBe("4s");
  expect(durationWords("0s")).toBe("never");
  expect(durationWords(null)).toBe("never");
});

test("the idle countdown counts to idleAt, then says now; nothing when not counting", () => {
  const now = Date.parse("2026-10-01T12:00:00Z");
  expect(idleText({ idleAt: "2026-10-01T12:07:42Z" }, now)).toBe("7:42");
  expect(idleText({ idleAt: "2026-10-01T11:00:00Z" }, now)).toBe("now");
  expect(idleText({}, now)).toBeNull();
});

test("wake and lifetime in words, without naming any orchestrator", () => {
  expect(wakeText({ wake: "request" })).toBe("On request: its owner is asked; lux never starts a Run itself");
  expect(wakeText({ wake: "never" })).toBe("Never: it runs only while its run does");
  expect(lifetimeText({ lifetime: "owner", expireAfter: "720h0m0s" })).toBe("Until its owner deletes it, or 30d without a request");
  expect(lifetimeText({ lifetime: "owner", expireAfter: null })).toBe("Until its owner deletes it");
  expect(lifetimeText({ lifetime: "run", expireAfter: null })).toBe("Ends with its run (terminated)");
});

import { describe, expect, test } from "bun:test";
import { renderToStaticMarkup } from "react-dom/server";
import { formatCountdown, IdleCountdown } from "./IdleCountdown.tsx";
import { servedStateStyle } from "./states.ts";

describe("formatCountdown", () => {
  test("minutes and seconds, hours when there are any, never negative", () => {
    expect(formatCountdown(462_000)).toBe("7:42");
    expect(formatCountdown(5_000)).toBe("0:05");
    expect(formatCountdown(3_723_000)).toBe("1:02:03");
    expect(formatCountdown(-10_000)).toBe("0:00");
  });
});

describe("IdleCountdown", () => {
  const now = Date.parse("2026-10-01T12:00:00Z");
  test("counts down to idleAt, says when it is due, and shows a dash with nothing to count", () => {
    expect(renderToStaticMarkup(<IdleCountdown idleAt="2026-10-01T12:07:42Z" idleAfter="10m" now={now} />)).toContain("Idle in 7:42");
    expect(renderToStaticMarkup(<IdleCountdown idleAt="2026-10-01T11:59:00Z" now={now} />)).toContain("Idle now");
    expect(renderToStaticMarkup(<IdleCountdown idleAt={null} now={now} />)).toContain("—");
  });
});

describe("servedStateStyle", () => {
  test("every server state has its own label; waking is live; an unknown one is labelled as given", () => {
    expect(servedStateStyle("asleep").label).toBe("Asleep");
    expect(servedStateStyle("waking").live).toBe(true);
    expect(servedStateStyle("no answer").label).toBe("No answer");
    expect(servedStateStyle("weird").label).toBe("weird");
  });
});

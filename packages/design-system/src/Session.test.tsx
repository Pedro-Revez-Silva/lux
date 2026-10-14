import { expect, test } from "bun:test";
import { renderToStaticMarkup } from "react-dom/server";
import { agentColor, agentSlot } from "./AgentMark.tsx";
import { Markdown } from "./Turn.tsx";
import { ToolUsage } from "./ToolUsage.tsx";
import { SessionMarker } from "./SessionMarker.tsx";
import { ToolCall, toolIcon } from "./ToolCall.tsx";

test("an agent type keeps the slot it was first seen in; slots wrap after five", () => {
  const slots = new Map<string, number>();
  const seen = ["explore", "general", "explore", "review", "a", "b", "c", "general"].map((a) => agentSlot(slots, a));
  expect(seen).toEqual([0, 1, 0, 2, 3, 4, 5, 1]);
  expect([0, 4, 5].map(agentColor)).toEqual(["var(--agent-1)", "var(--agent-5)", "var(--agent-1)"]);
});

test("ToolUsage: most used first, each bar its share of the largest, the count beside it", () => {
  const html = renderToStaticMarkup(<ToolUsage counts={[{ name: "read", count: 2 }, { name: "bash", count: 4 }, { name: "edit", count: 2 }]} />);
  const rows = [...html.matchAll(/toolusage-name" title="([^"]+)".*?width:([\d.]+)%.*?toolusage-n">(\d+)</g)].map((m) => [m[1], Number(m[2]), Number(m[3])]);
  expect(rows).toEqual([
    ["bash", 100, 4],
    ["edit", 50, 2],
    ["read", 50, 2],
  ]);
});

test("Markdown: agent text is escaped, never parsed as HTML", () => {
  const html = renderToStaticMarkup(<Markdown text={"run `rm -rf <dir>` **now**\n\n<script>x</script>"} />);
  expect(html).toBe("<p>run <code>rm -rf &lt;dir&gt;</code> <b>now</b></p><p>&lt;script&gt;x&lt;/script&gt;</p>");
});

test("Markdown: fenced code, bullet and numbered lists", () => {
  const html = renderToStaticMarkup(<Markdown text={"Callers:\n- `a.go`\n- b.go\n\n1. one\n2. two\n\n```go\nx := 1\n```"} />);
  expect(html).toBe("<p>Callers:</p><ul><li><code>a.go</code></li><li>b.go</li></ul><ol><li>one</li><li>two</li></ol><pre>x := 1</pre>");
});

test("ToolCall: a failed call shows its error line while folded, and says its status in words", () => {
  const html = renderToStaticMarkup(<ToolCall name="bash" summary="go test ./..." status="error" start={0} end={2500} error={"exit status 1\nmore"} />);
  expect(html).toContain('class="toolcall-error">exit status 1</div>');
  expect(html).toContain('<span class="sr-only">error</span>');
  expect(html).toContain('class="toolcall-dur">2.5s<');
  expect(renderToStaticMarkup(<>{toolIcon("Task")}</>)).toBe(renderToStaticMarkup(<>{toolIcon("spawn_agent")}</>));
});

test("SessionMarker: a turn marker is a separator with its text", () => {
  const html = renderToStaticMarkup(<SessionMarker type="turn">turn ended · 3s</SessionMarker>);
  expect(html).toContain('class="marker is-turn" role="separator"');
  expect(html).toContain("turn ended · 3s");
});

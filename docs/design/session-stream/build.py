#!/usr/bin/env python3
"""Build mockup.html: the console's real shell (DOM captured from `bun mock.ts`)
and compiled CSS, plus proposed.css and app.js, inlined into one file."""
import json, re, pathlib
d = pathlib.Path(__file__).parent
dom = (d / "lsm-dom.html").read_text()
css = (d / "lsm-all.css").read_text()
body = dom[dom.find('<div class="shell">'):dom.rfind('<div class="toasts"')]
# Drop the action dialogs, the old tabs+output; keep header + facts.
body = re.sub(r'<dialog.*?</dialog>', '', body, flags=re.S)
cut = body.find('<div role="tablist" class="tabs tabs-md">')
end_page = body.find('</div></main>')
tabs = '''<div role="tablist" class="tabs tabs-md">
<button type="button" role="tab" class="tab" data-tab="session">Session<span class="pill pill-teal pill-live pill-compact" id="session-tab-live" style="margin-left:6px" title="streaming"><span class="pill-dot"></span><span class="sr-only">live</span></span><span class="tab-count num" id="session-tab-count"></span></button>
<button type="button" role="tab" class="tab" data-tab="output">Output</button>
<button type="button" role="tab" class="tab" data-tab="servers">Servers<span class="tab-count num">3</span></button>
<button type="button" role="tab" class="tab" data-tab="timeline">Timeline<span class="tab-count num">3</span></button>
<button type="button" role="tab" class="tab" data-tab="resources">Resources &amp; cost</button>
<button type="button" role="tab" class="tab" data-tab="events">Events</button>
<button type="button" role="tab" class="tab" data-tab="snapshots">Snapshots &amp; artifacts</button>
<button type="button" role="tab" class="tab" data-tab="spec">Spec</button>
</div><div id="tab-body"></div>'''
body = body[:cut] + tabs + body[end_page:]
body = body.replace('<span class="pill pill-teal  pill-live" title="Running · busy" data-state="running">', '<span class="pill pill-teal pill-live" id="run-pill" title="Running · busy" data-state="running">', 1)
body = body.replace('<span class="badge badge-neutral badge-outline">acp</span>', '<span class="badge badge-neutral badge-outline" id="adapter-badge">acp</span>', 1)
body = body.replace('aria-label="Switch to dark theme"', 'id="theme-btn" aria-label="Toggle theme"', 1)
body = body.replace('aria-label="Compact density"', 'id="density-btn" aria-label="Toggle density"', 1)
body = body.replace('<span class="topbar-section">Run</span>', '<span class="topbar-section">Runs › agent-refactor-42</span>', 1)
ctl = '''<div class="mock-ctl" role="region" aria-label="Mockup controls">
<h4><span>Mockup controls</span><button class="btn btn-ghost btn-sm" id="mock-min" style="height:18px;padding:0 6px">–</button></h4>
<div class="row"><select id="mock-scn" aria-label="Scenario">
<option value="opencode">opencode · live, 3 subagents (one nested)</option>
<option value="acp">acp · finished, no subagent reporting</option>
<option value="starting">claude-code · starting, empty</option>
</select></div>
<div class="row"><button class="btn btn-default btn-sm" id="mock-play"><span class="btn-label">Pause stream</span></button><button class="btn btn-default btn-sm" id="mock-restart"><span class="btn-label">Restart</span></button>
<select id="mock-speed" aria-label="Speed" style="flex:0 0 70px"><option value="0.5">0.5×</option><option value="1" selected>1×</option><option value="2">2×</option></select></div>
<div class="muted" style="font-size:11px">Not part of the design. Data is invented; the stream replays a scripted turn.</div>
</div>'''
icons = (d / "icons.json").read_text()
html = f'''<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>lux · Run · Session (mockup)</title>
<style>{css}</style>
<style>{(d / "proposed.css").read_text()}</style>
</head><body><div id="root">{body}{ctl}</div>
<script>window.__ICONS = {icons};</script>
<script>{(d / "app.js").read_text()}</script>
</body></html>'''
(d / "mockup.html").write_text(html)
print("wrote", len(html))

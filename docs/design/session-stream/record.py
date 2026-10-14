#!/usr/bin/env python3
"""Record the lux Session-tab mockup at 1920x1080 with the shared walkthrough
overlay (abs tests/suites/walkthrough/overlay.js), writing video.webm and
timeline.json for abs tests/tools/walkthrough_render.py."""
import json, pathlib, shutil, sys, time
from playwright.sync_api import sync_playwright

HERE = pathlib.Path(__file__).parent
OUT = HERE / "take"
URL = (HERE / "mockup.html").resolve().as_uri()
OVERLAY = (HERE / "overlay.js").read_text()

timeline, t0, caption = [], [None], [""]

def ev(kind, text):
    timeline.append({"t": round(time.time() - t0[0], 3), "kind": kind, "text": text})

def main():
    shutil.rmtree(OUT, ignore_errors=True); OUT.mkdir()
    with sync_playwright() as p:
        b = p.chromium.launch()
        ctx = b.new_context(viewport={"width": 1920, "height": 1080}, record_video_dir=str(OUT / "raw"),
                            record_video_size={"width": 1920, "height": 1080}, bypass_csp=True, color_scheme="light")
        # Clicks are logged by the script (exact), not by the overlay's binding.
        ctx.add_init_script(OVERLAY)
        pg = ctx.new_page()
        t0[0] = time.time()
        pg.goto(URL + "?ctl=0&paused=1")
        pg.wait_for_timeout(600)

        def cap(text):
            caption[0] = text
            pg.evaluate("t => window.__setCaption(t)", text)
            ev("caption", text)
        def pause(s): pg.wait_for_timeout(int(s * 1000))
        def center(loc):
            for _ in range(20):
                try:
                    bb = loc.first.evaluate("e => { e.scrollIntoView({block: 'nearest'}); const r = e.getBoundingClientRect(); return {x: r.x, y: r.y, w: r.width, h: r.height}; }", timeout=3000)
                    return bb["x"] + bb["w"] / 2, bb["y"] + bb["h"] / 2
                except Exception:
                    pause(0.1)
            raise RuntimeError("no element")
        def move(loc, steps=25):
            x, y = center(loc); pg.mouse.move(x, y, steps=steps)
        def click(loc, after=1.0, what=""):
            move(loc); pause(0.15)
            x, y = center(loc)
            pg.mouse.move(x, y, steps=2)
            ev("click", what)
            pg.mouse.down(); pg.mouse.up(); pause(after)
        def scroll_transcript(dy):
            sc = pg.locator(".transcript-scroll")
            move(sc, 12)
            for _ in range(abs(dy) // 120):
                pg.mouse.wheel(0, 120 if dy > 0 else -120); pause(0.06)
        def play():
            pg.evaluate("() => { const s = window.__mock.st; s.speed = 0.7; if (!s.playing) document.querySelector('#mock-play').click(); }")
        def wait_for(js, timeout=30000):
            pg.wait_for_function(js, timeout=timeout)

        pg.mouse.move(960, 620)
        cap("lux Run page, proposed: a new Session tab, first, for any Run with an agent adapter")
        pause(3.2)
        cap("The current Output tab stays as it is: raw records, plus input and interrupt")
        click(pg.locator("[data-tab='output']"), 2.6, "Output tab")
        cap("Session reads those same records as a conversation. It's read-only for now")
        click(pg.locator("[data-tab='session']"), 2.4, "Session tab")
        cap("Every lux event lands in place: each epoch's move or resume, and a context compaction")
        scroll_transcript(-1200); pause(0.6)
        click(pg.locator(".marker-body button").first, 2.4, "summary")
        cap("Inputs and steers carry who sent them and lux's delivery phase")
        pause(2.4)
        cap("Tool calls fold to one line: name, what it touched, duration, result. A failure always shows its error")
        click(pg.locator(".toolcall-row").nth(4), 2.6, "bash go test")
        cap("Opened, a call shows its input, output or diff, and links to its record in Output")
        pause(2.8)
        click(pg.locator(".toolcall-row").nth(4), 0.6, "close bash")
        cap("A task call that runs a subagent gets a stub in that agent's colour")
        click(pg.locator(".subthread").first, 2.4, "Explore subagent")
        cap("Open it: the subagent's own transcript replaces the rail. The parent keeps streaming beside it")
        pause(3.0)
        cap("Esc or the X closes it and puts the rail back")
        click(pg.locator("[data-close-sub]").first, 1.6, "Close subagent")

        pg.evaluate("() => { window.__mock.st.holdAt = window.__mock.streamIndex(); }")
        cap("Now live: the agent is mid-turn, and the transcript follows the tail")
        play(); pause(5.5)
        pg.evaluate("() => { window.__mock.st.speed = 0.45; }")
        cap("The rail lists every subagent as a tree. A subagent's own subagent nests under it")
        move(pg.locator(".subtree-row").nth(1), 20); pause(4.0)
        click(pg.locator(".subtree-row").nth(2), 3.2, "nested Explore")
        cap("A nested one opens with a path back to its parent")
        click(pg.locator("[data-back-sub]").first, 3.4, "back to parent")
        cap("lux answers a subagent's permission requests by policy; the stub records what it allowed")
        pause(3.4)
        click(pg.locator("[data-close-sub]").first, 1.0, "Close subagent")
        cap("Scrolling up stops following; new turns are counted until you jump back")
        scroll_transcript(-720); pause(4.0)
        # hold the stream at the reply until we are back at the tail

        j = pg.locator("[data-jump]")
        if j.is_visible():
            click(j, 1.4, "Jump to latest")
        else:
            click(pg.locator("[data-follow]"), 1.4, "Follow")
        wait_for("() => window.__mock.st.step === window.__mock.st.holdAt", 60000)
        pg.evaluate("() => { const m = window.__mock; m.st.holdAt = -1; m.st.speed = 0.7; m.schedule(); }")
        wait_for("() => document.querySelector('.turn-msg.is-streaming') || window.__mock.st.S.idle", 30000)
        cap("The agent's reply streams in as it is written")
        pause(3.5)
        cap("Messages hides tool calls and thoughts, leaving inputs, replies and lux events")
        click(pg.locator("[data-filter='messages']"), 3.0, "Messages")
        click(pg.locator("[data-filter='all']"), 1.4, "Everything")
        wait_for("() => window.__mock.st.S.idle === true", 30000)
        pause(0.4)
        cap("Turn over: the run goes idle and every subagent is done")
        pause(3.0)
        cap("Dark theme uses the console's own dark tokens")
        click(pg.locator("#theme-btn"), 2.8, "theme")
        cap("An acp Run has no subagent records, and the rail says so")
        pg.evaluate("() => window.__mock.load('acp')"); pause(3.4)
        cap("Before the first record: an empty state, not a blank pane")
        pg.evaluate("() => window.__mock.load('starting')"); pause(3.0)
        cap("")
        pause(0.8)
        vid = pg.video
        ctx.close(); b.close()
        shutil.move(vid.path(), OUT / "video.webm")
    shutil.rmtree(OUT / "raw", ignore_errors=True)
    (OUT / "timeline.json").write_text(json.dumps(timeline, indent=1))
    print("ok", len(timeline), "events")

if __name__ == "__main__":
    main()

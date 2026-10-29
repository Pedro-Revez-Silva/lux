import { afterAll, beforeAll, expect, test } from "bun:test";
import { GlobalRegistrator } from "@happy-dom/global-registrator";
import { act } from "react";
import { createRoot } from "react-dom/client";

// The page's imports (the router) touch window at load: register the DOM first.
let RunPage: typeof import("./RunPage.tsx").RunPage;
let ScopeProvider: typeof import("../scope.tsx").ScopeProvider;
let ToastProvider: typeof import("@lux/design-system").ToastProvider;
let fakeApi: typeof import("../testing.ts").fakeApi;
let until: typeof import("../testing.ts").until;
let type: typeof import("../testing.ts").type;
let api: typeof import("../../api/index.ts");
beforeAll(async () => {
  GlobalRegistrator.register();
  ({ RunPage } = await import("./RunPage.tsx"));
  ({ ScopeProvider } = await import("../scope.tsx"));
  ({ ToastProvider } = await import("@lux/design-system"));
  ({ fakeApi, until, type } = await import("../testing.ts"));
  api = await import("../../api/index.ts");
  (globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;
});
afterAll(async () => {
  await GlobalRegistrator.unregister();
});

const IMAGE = "ghcr.io/acme/jervasion-agent@sha256:2c567002fb22e0d9c1b7a4f35e6d2c8b90a1f4e7d3c2b1a09f8e7d6cd76856cf";
const run = (adapter: string, extra: Record<string, unknown> = {}) => ({ id: "run_1", tenant: "t", state: "succeeded", epoch: 1, labels: {}, createdAt: new Date(0).toISOString(), spec: { image: { ref: IMAGE }, resources: {}, workload: { adapter } }, ...extra });

async function render(adapter: string, url: string, extra: Record<string, unknown> = {}, input?: (body: unknown) => unknown) {
  api.signIn("k");
  const fake = fakeApi((path, method, body) => {
    if (path.startsWith("/v1/runs/run_1/output")) return new Response("event: end\ndata: {}\n\n", { headers: { "Content-Type": "text/event-stream" } });
    if (path === "/v1/runs/run_1/input" && method === "POST") return input ? input(body) : { requestId: "in_1" };
    if (path.startsWith("/v1/runs/run_1")) return run(adapter, extra);
    return {};
  });
  (window as unknown as { happyDOM: { setURL: (u: string) => void } }).happyDOM.setURL(url);
  const el = document.createElement("div");
  document.body.appendChild(el);
  const root = createRoot(el);
  await act(async () =>
    root.render(
      <ToastProvider>
        <ScopeProvider>
          <RunPage id="run_1" />
        </ScopeProvider>
      </ToastProvider>,
    ),
  );
  await until(() => el.querySelector('[role="tab"]') != null, "Run tabs");
  const tabs = () => [...el.querySelectorAll('[role="tab"]')].map((t) => t.textContent?.trim().split("\n")[0]);
  const selected = () => el.querySelector('[role="tab"][aria-selected="true"]')?.textContent?.trim();
  return {
    el,
    fake,
    tabs,
    selected,
    done: async () => {
      await act(async () => root.unmount());
      el.remove();
      fake.restore();
      api.signOut();
    },
  };
}

const button = (scope: Element, name: string) => [...scope.querySelectorAll<HTMLButtonElement>("button")].find((b) => b.textContent === name);
const conflict = () => new Response(JSON.stringify({ error: { code: "not_running", message: "run is stopping: not started yet" } }), { status: 409, headers: { "Content-Type": "application/json" } });
const toasts = (el: Element) => [...el.ownerDocument.querySelectorAll('.toasts [role="status"]')].map((t) => t.textContent);
// The texts that describe a field, through its aria-describedby ids.
const described = (n: Element) => (n.getAttribute("aria-describedby") ?? "").split(" ").filter(Boolean).map((id) => document.getElementById(id)?.textContent);

test("a generic Run opens on Output, and its Session tab comes after it: no conversation, input to stdin, Interrupt", async () => {
  const r = await render("generic", "http://localhost/runs/run_1?tab=session", { state: "running" });
  try {
    expect(r.tabs().slice(0, 2)).toEqual(["Output", "Session"]);
    expect(r.selected()).toBe("Session");
    const session = r.el.querySelector('section[aria-label="Agent session"]')!;
    expect(session.textContent).toContain("No agent conversation");
    const form = session.querySelector<HTMLFormElement>('form[aria-label="Send to stdin"]')!;
    expect(button(form, "Interrupt")?.disabled).toBe(false);
    await type(form.querySelector("textarea")!, "later");
    await act(async () => button(form, "Send")!.click());
    await until(() => r.fake.posts.length > 0, "the input POST");
    expect(r.fake.posts).toEqual([{ path: "/v1/runs/run_1/input", method: "POST", body: { text: "later" } }]);

    await act(async () => button(form, "Interrupt")!.click());
    await until(() => r.fake.posts.length === 2, "the interrupt POST");
    expect(r.fake.posts[1]).toEqual({ path: "/v1/runs/run_1/input", method: "POST", body: { interrupt: true } });
    await until(() => toasts(r.el).length > 0, "the toast");
    expect(toasts(r.el)).toEqual(["Interrupt sent"]);
  } finally {
    await r.done();
  }
});

test("an Interrupt the server refuses toasts Interrupt failed with the server's reason", async () => {
  const r = await render("opencode", "http://localhost/runs/run_1", { state: "running" }, (body) => ((body as { interrupt?: boolean }).interrupt ? conflict() : { requestId: "in_1" }));
  try {
    const form = r.el.querySelector<HTMLFormElement>('form[aria-label="Message the agent"]')!;
    await act(async () => button(form, "Interrupt")!.click());
    await until(() => toasts(r.el).length > 0, "the toast");
    expect(r.fake.posts).toEqual([{ path: "/v1/runs/run_1/input", method: "POST", body: { interrupt: true } }]);
    expect(toasts(r.el)).toEqual(["Interrupt failedrun is stopping: not started yet"]);
    expect(form.querySelector('[role="alert"]')).toBeNull();
  } finally {
    await r.done();
  }
});

test("a generic Run without ?tab opens on Output", async () => {
  const r = await render("generic", "http://localhost/runs/run_1");
  try {
    expect(r.selected()).toBe("Output");
  } finally {
    await r.done();
  }
});

test("an opencode Run without ?tab opens on Session", async () => {
  const r = await render("opencode", "http://localhost/runs/run_1");
  try {
    expect(r.tabs()[0]).toBe("Session");
    expect(r.selected()).toBe("Session");
  } finally {
    await r.done();
  }
});

test("the Output tab is read-only: no input form, field, Send or Interrupt", async () => {
  const r = await render("opencode", "http://localhost/runs/run_1?tab=output", { state: "running" });
  try {
    expect(r.selected()).toBe("Output");
    await until(() => r.el.querySelector(".logview") != null, "the log");
    // The tab's content: what follows the Run's tab list (the header's dialogs hold their own forms).
    const tablist = r.el.querySelector('[role="tablist"]')!;
    const panel = [...tablist.parentElement!.children].slice([...tablist.parentElement!.children].indexOf(tablist) + 1);
    expect(panel.length).toBeGreaterThan(0);
    expect(panel.some((n) => n.contains(r.el.querySelector(".logview")))).toBe(true);
    expect(panel.flatMap((n) => [...n.querySelectorAll("form, textarea, input")]).map((x) => x.outerHTML)).toEqual([]);
    expect(panel.flatMap((n) => [...n.querySelectorAll("button")]).map((b) => b.textContent)).not.toContain("Send");
    expect(panel.flatMap((n) => [...n.querySelectorAll("button")]).map((b) => b.textContent)).not.toContain("Interrupt");
    // And no pointer back to Session: no link, no text naming it.
    expect(panel.flatMap((n) => [...n.querySelectorAll("a")]).map((a) => a.outerHTML)).toEqual([]);
    expect(panel.map((n) => n.textContent).join("")).not.toContain("Session");
  } finally {
    await r.done();
  }
});

test("an agent Run's Session composer posts to /v1/runs/{id}/input and clears the draft; a refusal keeps the draft and shows why", async () => {
  let refuse = false;
  const r = await render("opencode", "http://localhost/runs/run_1", { state: "running" }, () => (refuse ? conflict() : { requestId: "in_1" }));
  try {
    const form = r.el.querySelector<HTMLFormElement>('section[aria-label="Agent session"] form[aria-label="Message the agent"]')!;
    const area = form.querySelector("textarea")!;
    await type(area, "  also add jitter ");
    await act(async () => button(form, "Send")!.click());
    await until(() => area.value === "", "the draft to clear");
    expect(r.fake.posts).toEqual([{ path: "/v1/runs/run_1/input", method: "POST", body: { text: "also add jitter" } }]);

    refuse = true;
    await type(area, "keep me");
    await act(async () => button(form, "Send")!.click());
    await until(() => form.querySelector('[role="alert"]') != null, "the alert");
    expect(form.querySelector('[role="alert"]')!.textContent).toBe("run is stopping: not started yet");
    expect(area.value).toBe("keep me");
    expect(r.fake.posts).toEqual([
      { path: "/v1/runs/run_1/input", method: "POST", body: { text: "also add jitter" } },
      { path: "/v1/runs/run_1/input", method: "POST", body: { text: "keep me" } },
    ]);
  } finally {
    await r.done();
  }
});

test("a stopped, resumable agent Run: the composer is off and points to Resume", async () => {
  const r = await render("opencode", "http://localhost/runs/run_1", { state: "stopped", resumable: true });
  try {
    const form = r.el.querySelector<HTMLFormElement>('form[aria-label="Message the agent"]')!;
    expect(form.querySelector("textarea")!.disabled).toBe(true);
    expect(form.querySelector("textarea")!.placeholder).toBe("Resume this Run to send a message");
    expect(described(form.querySelector("textarea")!)).toEqual(["Run stopped · Resume is in the page actions"]);
    expect(button(form, "Send")!.disabled).toBe(true);
    expect(button(form, "Interrupt")!.disabled).toBe(true);
  } finally {
    await r.done();
  }
});

test("a running agent Run whose spec sets steerable false: the composer is off and says why, and the header says not steerable", async () => {
  const badges = (el: Element) => [...el.querySelectorAll(".badge")].map((b) => b.textContent);
  const r = await render("opencode", "http://localhost/runs/run_1", { state: "running", spec: { image: { ref: IMAGE }, resources: {}, workload: { adapter: "opencode", steerable: false } } });
  try {
    const form = r.el.querySelector<HTMLFormElement>('form[aria-label="Message the agent"]')!;
    expect(form.querySelector("textarea")!.disabled).toBe(true);
    expect(form.querySelector("textarea")!.placeholder).toBe("This Run takes no messages");
    expect(described(form.querySelector("textarea")!)).toEqual(["Not steerable · its spec sets workload.steerable false"]);
    expect(button(form, "Send")!.disabled).toBe(true);
    expect(button(form, "Interrupt")!.disabled).toBe(true);
    expect(badges(r.el)).toContain("not steerable");
  } finally {
    await r.done();
  }
});

test("a starting agent Run whose adapter reads input at the turn's end: the composer is on and says so", async () => {
  const r = await render("opencode", "http://localhost/runs/run_1", { state: "starting", steer: { lands: "next_turn" } });
  try {
    const area = r.el.querySelector<HTMLTextAreaElement>('form[aria-label="Message the agent"] textarea')!;
    expect(area.disabled).toBe(false);
    expect(described(area)).toEqual(["Root session · read when the agent's current turn ends"]);
  } finally {
    await r.done();
  }
});

test("a non-preemptible Run carries a non-preemptible badge; an ordinary one does not", async () => {
  const badges = (el: Element) => [...el.querySelectorAll(".badge")].map((b) => b.textContent);
  const spec = (placement: Record<string, unknown>) => ({ spec: { image: { ref: IMAGE }, resources: {}, workload: { adapter: "generic" }, placement } });
  let r = await render("generic", "http://localhost/runs/run_1", spec({ pool: "steady", preemptible: false }));
  try {
    expect(badges(r.el)).toContain("non-preemptible");
  } finally {
    await r.done();
  }
  r = await render("generic", "http://localhost/runs/run_1", spec({ pool: "steady", preemptible: true }));
  try {
    expect(badges(r.el)).not.toContain("non-preemptible");
  } finally {
    await r.done();
  }
});

test("a Run built from a Containerfile: the Image row says so", async () => {
  const r = await render("generic", "http://localhost/runs/run_1", { spec: { image: { build: { containerfile: "Containerfile" } }, resources: {}, workload: { adapter: "generic" } } });
  try {
    const image = [...r.el.querySelectorAll('.kv-facts > [role="group"]')].find((g) => r.el.querySelector(`#${CSS.escape(g.getAttribute("aria-labelledby")!)}`)?.textContent === "Image")!;
    expect(image.querySelector(".fact-value")?.textContent).toBe("built from Containerfile");
  } finally {
    await r.done();
  }
});

test("the facts: the grid in its order, Image and Labels as full-width rows outside it", async () => {
  const labels = { app: "jervasion", "a.b": "x", "jervasion.pr": "45", "jervasion.repository": "acme/aiverse", "jervasion.target": "pr:PR_kwDOQUrH8Es8AAAABIcOqGg" };
  const r = await render("opencode", "http://localhost/runs/run_1", { labels });
  try {
    // The facts card: the grid and its full-width rows, found through the Image row's group.
    const facts = r.el.querySelector('[role="group"]')!.parentElement!;
    expect(facts.classList.contains("kv-facts")).toBe(true);
    const grid = facts.querySelector(":scope > dl")!;
    expect([...grid.querySelectorAll(":scope > div > dt")].map((d) => d.textContent)).toEqual(["Created", "First started", "Finished", "Resources", "Usage", "Submitted by", "Session", "Snapshot"]);
    const rows = [...facts.querySelectorAll(':scope > [role="group"]')];
    expect(rows.map((g) => r.el.querySelector(`#${CSS.escape(g.getAttribute("aria-labelledby")!)}`)?.textContent)).toEqual(["Image", "Labels 5"]);
    expect(rows[0]!.querySelector(`[title="${IMAGE}"]`)?.textContent).toBe("ghcr.io/acme/jervasion-agent@sha256:2c567002fb22…d76856cf");
    // app leads the preview and the list, ahead of a key that sorts before it.
    expect([...rows[1]!.querySelectorAll(".label-chip")].map((c) => c.textContent?.split("=")[0])).toEqual(["app", "a.b", "jervasion.pr", "+2"]);
    const toggle = rows[1]!.querySelector<HTMLButtonElement>("button[aria-expanded]")!;
    expect(toggle.textContent).toBe("Show all");
    expect(toggle.getAttribute("aria-expanded")).toBe("false");
    await act(async () => toggle.click());
    expect(toggle.getAttribute("aria-expanded")).toBe("true");
    expect(toggle.textContent).toBe("Hide");
    expect([...rows[1]!.querySelectorAll("dl dt")].map((d) => d.textContent)).toEqual(["app", "a.b", "jervasion.pr", "jervasion.repository", "jervasion.target"]);
    expect(rows[1]!.querySelector("dl")?.textContent).toContain("pr:PR_kwDOQUrH8Es8AAAABIcOqGg");
  } finally {
    await r.done();
  }
});

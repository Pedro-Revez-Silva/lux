import { afterAll, beforeAll, expect, test } from "bun:test";
import { GlobalRegistrator } from "@happy-dom/global-registrator";
import { act } from "react";
import { createRoot } from "react-dom/client";

// The page's imports (the router) touch window at load: register the DOM first.
let PreviewAuth: typeof import("./PreviewAuth.tsx").PreviewAuth;
let previewAuthUrl: typeof import("./previewTarget.ts").previewAuthUrl;
let fakeApi: typeof import("../testing.ts").fakeApi;
let api: typeof import("../../api/index.ts");
beforeAll(async () => {
  GlobalRegistrator.register();
  ({ PreviewAuth } = await import("./PreviewAuth.tsx"));
  ({ previewAuthUrl } = await import("./previewTarget.ts"));
  ({ fakeApi } = await import("../testing.ts"));
  api = await import("../../api/index.ts");
  (globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;
});
afterAll(async () => {
  await GlobalRegistrator.unregister();
});

const sleep = (ms: number) => act(() => new Promise<void>((r) => setTimeout(r, ms)));

/** Polls (10 ms steps, inside act) until ok() holds; fails after 5 s. */
async function until(ok: () => boolean, what: string) {
  for (const end = Date.now() + 5000; !ok(); ) {
    if (Date.now() > end) throw new Error(`never: ${what}`);
    await sleep(10);
  }
}

const REFUSED = "No server of yours answers to web.pr1.lux.example.com: it was deleted, or belongs to another tenant.";

const TO = "https://web.pr1.lux.example.com/goals?tab=a";
const WHOAMI = { tenantId: "t1", scopes: ["run"], previewDomain: "lux.example.com", previewScheme: "https" };

/** /preview-auth?to=TO with a fake API whose server lookup answers servers. */
async function render(servers: { id: string }[]) {
  api.signIn("k");
  const fake = fakeApi((path) => {
    if (path.startsWith("/v1/whoami")) return WHOAMI;
    if (path.startsWith("/v1/servers?")) return { servers, counts: {} };
    if (/^\/v1\/servers\/[^/]+\/tickets/.test(path)) return { ticket: "tkt_1", expiresAt: new Date(Date.now() + 60_000).toISOString() };
    return {};
  });
  const replaced: string[] = [];
  const loc = window.location as unknown as { replace: (u: string) => void };
  const realReplace = loc.replace;
  loc.replace = (u: string) => {
    replaced.push(u);
  };
  (window as unknown as { happyDOM: { setURL: (u: string) => void } }).happyDOM.setURL(
    `http://localhost/preview-auth?to=${encodeURIComponent(TO)}`,
  );
  const el = document.createElement("div");
  document.body.appendChild(el);
  const root = createRoot(el);
  await act(async () => root.render(<PreviewAuth />));
  return {
    fake,
    replaced,
    text: () => el.textContent ?? "",
    tickets: () => fake.calls.filter((c) => /^\/v1\/servers\/[^/]+\/tickets/.test(c)),
    done: async () => {
      await act(async () => root.unmount());
      el.remove();
      loc.replace = realReplace;
      fake.restore();
      api.signOut();
    },
  };
}

test("a hostname with no server of this session's is refused, and no ticket is minted", async () => {
  const p = await render([]);
  try {
    // The refusal is the page's last step: once it shows, no call is pending.
    await until(() => p.text().includes(REFUSED), "the refusal");
    await sleep(50);
    expect(p.fake.calls).toContain("/v1/servers?hostname=web.pr1.lux.example.com");
    expect(p.text()).toContain(REFUSED);
    expect(p.tickets()).toEqual([]);
    expect(p.replaced).toEqual([]);
  } finally {
    await p.done();
  }
});

test("a found server gets a ticket, and the browser goes to /.lux/auth on the preview host", async () => {
  const p = await render([{ id: "srv_abc" }]);
  try {
    await until(() => p.replaced.length === 1, "the redirect");
    expect(p.tickets()).toEqual(["/v1/servers/srv_abc/tickets"]);
    const want = previewAuthUrl({ url: new URL(TO), hostname: "web.pr1.lux.example.com" }, "tkt_1");
    expect(want).toBe("https://web.pr1.lux.example.com/.lux/auth?ticket=tkt_1&to=%2Fgoals%3Ftab%3Da");
    expect(p.replaced).toEqual([want]);
  } finally {
    await p.done();
  }
});

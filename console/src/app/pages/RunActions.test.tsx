import { afterAll, beforeAll, expect, test } from "bun:test";
import { GlobalRegistrator } from "@happy-dom/global-registrator";
import { act } from "react";
import { createRoot } from "react-dom/client";
import type { Run } from "../../api/index.ts";

// The page's imports (the router) touch window at load: register the DOM first.
let RunActions: typeof import("./RunActions.tsx").RunActions;
let ToastProvider: typeof import("@lux/design-system").ToastProvider;
let fakeApi: typeof import("../testing.ts").fakeApi;
let api: typeof import("../../api/index.ts");
beforeAll(async () => {
  GlobalRegistrator.register();
  ({ RunActions } = await import("./RunActions.tsx"));
  ({ ToastProvider } = await import("@lux/design-system"));
  ({ fakeApi } = await import("../testing.ts"));
  api = await import("../../api/index.ts");
  (globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;
});
afterAll(async () => {
  await GlobalRegistrator.unregister();
});

const run = (state: string, more: { resumable?: boolean; resumePolicy?: string } = {}): Run =>
  ({ id: "run_1", state, stateReason: "", resumable: more.resumable ?? false, spec: { workload: {}, resumePolicy: more.resumePolicy } }) as unknown as Run;

/** RunActions for a Run in state; returns its buttons by label and a cleanup. */
async function render(state: string, more: { resumable?: boolean; resumePolicy?: string } = {}) {
  api.signIn("k");
  const fake = fakeApi(() => ({}));
  const el = document.createElement("div");
  document.body.appendChild(el);
  const root = createRoot(el);
  await act(async () =>
    root.render(
      <ToastProvider>
        <RunActions run={run(state, more)} operator={false} onChanged={() => {}} />
      </ToastProvider>,
    ),
  );
  // The action buttons, not those of the (closed) dialogs.
  const button = (label: string) =>
    [...el.querySelectorAll("button")].find((b) => !b.closest("dialog") && b.textContent?.trim() === label) as HTMLButtonElement | undefined;
  const done = async () => {
    await act(async () => root.unmount());
    el.remove();
    fake.restore();
  };
  return { el, fake, button, done };
}

test("a running Run offers Terminate (no Cancel), which opens the terminate dialog", async () => {
  const r = await render("running");
  try {
    expect(r.button("Cancel")).toBeUndefined();
    const terminate = r.button("Terminate")!;
    expect(terminate.disabled).toBe(false);
    await act(async () => terminate.click());
    const dialog = [...r.el.querySelectorAll("dialog")].find((d) => d.textContent?.includes("Terminate run?"))!;
    expect(dialog.open).toBe(true);
  } finally {
    await r.done();
  }
});

test("a succeeded Run can be resumed or terminated, not stopped", async () => {
  const r = await render("succeeded", { resumable: true });
  try {
    expect(r.button("Resume")!.disabled).toBe(false);
    expect(r.button("Terminate")!.disabled).toBe(false);
    expect(r.button("Stop")!.disabled).toBe(true);
  } finally {
    await r.done();
  }
});

test("a resumePolicy never Run that is not resumable cannot be resumed", async () => {
  const r = await render("stopped", { resumable: false, resumePolicy: "never" });
  try {
    expect(r.button("Resume")!.disabled).toBe(true);
    expect(r.button("Terminate")!.disabled).toBe(false);
  } finally {
    await r.done();
  }
});

test("a stopped Run left without a snapshot can still be resumed from an older one", async () => {
  const r = await render("stopped", { resumable: false });
  try {
    expect(r.button("Resume")!.disabled).toBe(false);
  } finally {
    await r.done();
  }
});

test("a succeeded Run not resumable as it is can still be resumed, unless its resumePolicy is never", async () => {
  for (const [resumePolicy, disabled] of [[undefined, false], ["never", true]] as const) {
    const r = await render("succeeded", { resumable: false, resumePolicy });
    try {
      expect(r.button("Resume")!.disabled).toBe(disabled);
    } finally {
      await r.done();
    }
  }
});

test("a terminated Run can neither be terminated nor resumed", async () => {
  const r = await render("terminated");
  try {
    expect(r.button("Terminate")!.disabled).toBe(true);
    expect(r.button("Resume")!.disabled).toBe(true);
    expect(r.button("Stop")!.disabled).toBe(true);
  } finally {
    await r.done();
  }
});

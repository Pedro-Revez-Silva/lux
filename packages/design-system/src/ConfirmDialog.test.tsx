import { afterAll, beforeAll, describe, expect, test } from "bun:test";
import { GlobalRegistrator } from "@happy-dom/global-registrator";
import type { act as Act } from "react";
import type { createRoot as CreateRoot } from "react-dom/client";
import type { ConfirmDialog as Dialog, ConfirmDialogProps } from "./ConfirmDialog.tsx";

// react-dom decides at load whether the DOM has input events (which its
// onChange needs), so it is loaded once the DOM is there.
let act: typeof Act;
let createRoot: typeof CreateRoot;
let ConfirmDialog: typeof Dialog;

/** Mounts an open dialog in a DOM; `confirmed` counts onConfirm calls. */
async function mount(props: Partial<ConfirmDialogProps>) {
  const el = document.createElement("div");
  document.body.appendChild(el);
  const root = createRoot(el);
  const calls: unknown[][] = [];
  const render = (p: Partial<ConfirmDialogProps>) =>
    act(async () => root.render(<ConfirmDialog open title="Rename lab?" onConfirm={(...a) => calls.push(a)} onCancel={() => {}} {...props} {...p} />));
  await render({});
  const form = el.querySelector("form") as HTMLFormElement;
  const submitButton = el.querySelector('button[type="submit"]') as HTMLButtonElement;
  /** Submits the form as Enter in one of its fields does, whatever the button's state. */
  const submit = () => act(async () => form.requestSubmit());
  const type = (input: HTMLInputElement, value: string) =>
    act(async () => {
      // React tracks the value it last set; set it past React, then say so.
      Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, "value")!.set!.call(input, value);
      input.dispatchEvent(new Event("input", { bubbles: true }));
    });
  const unmount = () => act(async () => root.unmount());
  return { el, calls, render, submit, submitButton, type, unmount };
}

describe("ConfirmDialog in a DOM", () => {
  beforeAll(async () => {
    GlobalRegistrator.register();
    (globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;
    ({ act } = await import("react"));
    ({ createRoot } = await import("react-dom/client"));
    ({ ConfirmDialog } = await import("./ConfirmDialog.tsx"));
  });
  afterAll(async () => {
    await GlobalRegistrator.unregister();
  });

  test("submitted while loading, it does not confirm; once loaded, it does", async () => {
    const d = await mount({ loading: true });
    expect(d.submitButton.disabled).toBe(true);
    await d.submit();
    expect(d.calls).toHaveLength(0);
    await d.render({ loading: false });
    expect(d.submitButton.disabled).toBe(false);
    await d.submit();
    expect(d.calls).toHaveLength(1);
    await d.unmount();
  });

  test("while loading, typing the confirmation and the input does not unblock it", async () => {
    const d = await mount({ loading: true, confirmText: "lab", input: { label: "New name", required: true } });
    const [name, confirm] = [...d.el.querySelectorAll("input")] as HTMLInputElement[];
    await d.type(name!, "lab2");
    await d.type(confirm!, "lab");
    await d.submit();
    expect(d.calls).toHaveLength(0);
    await d.render({ loading: false });
    await d.submit();
    expect(d.calls).toEqual([["lab2", undefined]]);
    await d.unmount();
  });

  test("without loading, the typed confirmation decides", async () => {
    const d = await mount({ confirmText: "lab" });
    const confirm = d.el.querySelector("input") as HTMLInputElement;
    await d.type(confirm, "la");
    await d.submit();
    expect(d.calls).toHaveLength(0);
    await d.type(confirm, "lab");
    await d.submit();
    expect(d.calls).toHaveLength(1);
    await d.unmount();
  });
});

import { expect, test } from "bun:test";
import { renderToStaticMarkup } from "react-dom/server";
import { ConfirmDialog, confirmBlocked, type ConfirmDialogProps } from "./ConfirmDialog.tsx";

const noop = () => {};

/** The confirm (submit) button's opening tag. */
function submitButton(props: Partial<ConfirmDialogProps>): string {
  const html = renderToStaticMarkup(<ConfirmDialog open title="t" onConfirm={noop} onCancel={noop} {...props} />);
  const m = /<button type="submit"[^>]*>/.exec(html);
  if (!m) throw new Error(`no submit button in ${html}`);
  return m[0];
}

test("ConfirmDialog: while loading, confirming is refused, whatever was typed", () => {
  expect(confirmBlocked({ loading: true, typed: "", text: "" })).toBe(true);
  expect(confirmBlocked({ loading: true, confirmText: "lab", typed: "lab", inputRequired: true, text: "lab2" })).toBe(true);
  expect(submitButton({ loading: true })).toContain("disabled");
});

test("ConfirmDialog: once loaded, the typed confirmation and a required input decide", () => {
  expect(confirmBlocked({ typed: "", text: "" })).toBe(false);
  expect(confirmBlocked({ loading: false, confirmText: "lab", typed: "la", text: "" })).toBe(true);
  expect(confirmBlocked({ confirmText: "lab", typed: "lab", inputRequired: true, text: "  " })).toBe(true);
  expect(confirmBlocked({ confirmText: "lab", typed: "lab", inputRequired: true, text: "lab2" })).toBe(false);
  expect(submitButton({})).not.toContain("disabled");
});

import { useEffect, useRef, useState, type ReactNode } from "react";
import { Button } from "./Button.tsx";

export interface ConfirmDialogProps {
  open: boolean;
  title: ReactNode;
  description?: ReactNode;
  confirmLabel?: string;
  cancelLabel?: string;
  /** "danger" for destructive actions (drain, cancel, terminate). */
  tone?: "default" | "danger";
  /** Require the user to type this exact text (e.g. the host id) before confirming. */
  confirmText?: string;
  /** Prompt for free text (e.g. a reason) and pass it to onConfirm. */
  input?: { label: string; placeholder?: string; required?: boolean };
  /** An opt-in checkbox (e.g. force-evict); its state is passed to onConfirm. */
  checkbox?: { label: string; help?: string; checked?: boolean; locked?: boolean };
  loading?: boolean;
  onConfirm: (input?: string, checked?: boolean) => void;
  onCancel: () => void;
}

export function ConfirmDialog(props: ConfirmDialogProps) {
  const { open, title, description, confirmLabel = "Confirm", cancelLabel = "Cancel", tone = "default", confirmText, input, checkbox, loading, onConfirm, onCancel } = props;
  const ref = useRef<HTMLDialogElement>(null);
  const [typed, setTyped] = useState("");
  const [text, setText] = useState("");
  const [checked, setChecked] = useState(false);

  useEffect(() => {
    const d = ref.current;
    if (!d) return;
    if (open && !d.open) {
      setTyped("");
      setText("");
      setChecked(!!checkbox?.checked);
      d.showModal();
    } else if (!open && d.open) d.close();
  }, [open, checkbox?.checked]);

  const blocked = (confirmText != null && typed !== confirmText) || (input?.required && text.trim() === "");

  return (
    <dialog
      ref={ref}
      className="dialog"
      onCancel={(e) => {
        e.preventDefault();
        onCancel();
      }}
      onClick={(e) => {
        if (e.target === ref.current) onCancel();
      }}
    >
      <form
        method="dialog"
        className="dialog-form"
        onSubmit={(e) => {
          e.preventDefault();
          if (!blocked) onConfirm(input ? text : undefined, checkbox ? checked : undefined);
        }}
      >
        <h2 className="dialog-title">{title}</h2>
        {description && <div className="dialog-desc">{description}</div>}
        {input && (
          <label className="field">
            <span className="field-label">{input.label}</span>
            <input className="input" value={text} placeholder={input.placeholder} onChange={(e) => setText(e.target.value)} autoFocus />
          </label>
        )}
        {checkbox && (
          <label className="check">
            <input type="checkbox" checked={checked} disabled={checkbox.locked} onChange={(e) => setChecked(e.target.checked)} />
            {checkbox.label}
          </label>
        )}
        {checkbox?.help && <div className="muted">{checkbox.help}</div>}
        {confirmText != null && (
          <label className="field">
            <span className="field-label">
              Type <code className="code">{confirmText}</code> to confirm
            </span>
            <input className="input mono" value={typed} onChange={(e) => setTyped(e.target.value)} autoComplete="off" spellCheck={false} autoFocus={!input} />
          </label>
        )}
        <div className="dialog-actions">
          <Button variant="ghost" onClick={onCancel} disabled={loading}>
            {cancelLabel}
          </Button>
          <Button type="submit" variant={tone === "danger" ? "danger" : "primary"} disabled={blocked} loading={loading}>
            {confirmLabel}
          </Button>
        </div>
      </form>
    </dialog>
  );
}

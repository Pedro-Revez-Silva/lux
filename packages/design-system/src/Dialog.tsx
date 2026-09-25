import { useEffect, useRef, type FormEvent, type ReactNode } from "react";
import { Button } from "./Button.tsx";

export interface DialogProps {
  open: boolean;
  title: ReactNode;
  description?: ReactNode;
  children?: ReactNode;
  confirmLabel?: string;
  cancelLabel?: string;
  tone?: "default" | "danger";
  disabled?: boolean;
  loading?: boolean;
  onConfirm: () => void;
  onCancel: () => void;
  width?: number;
}

/** Generic modal form: the ConfirmDialog chrome with arbitrary fields inside. */
export function Dialog({ open, title, description, children, confirmLabel = "Confirm", cancelLabel = "Cancel", tone = "default", disabled, loading, onConfirm, onCancel, width }: DialogProps) {
  const ref = useRef<HTMLDialogElement>(null);
  useEffect(() => {
    const d = ref.current;
    if (!d) return;
    if (open && !d.open) d.showModal();
    else if (!open && d.open) d.close();
  }, [open]);
  const submit = (e: FormEvent) => {
    e.preventDefault();
    if (!disabled && !loading) onConfirm();
  };
  return (
    <dialog
      ref={ref}
      className="dialog"
      style={width ? { width } : undefined}
      onCancel={(e) => {
        e.preventDefault();
        onCancel();
      }}
      onClick={(e) => {
        if (e.target === ref.current) onCancel();
      }}
    >
      <form method="dialog" className="dialog-form" onSubmit={submit}>
        <h2 className="dialog-title">{title}</h2>
        {description && <div className="dialog-desc">{description}</div>}
        {children}
        <div className="dialog-actions">
          <Button variant="ghost" onClick={onCancel} disabled={loading}>
            {cancelLabel}
          </Button>
          <Button type="submit" variant={tone === "danger" ? "danger" : "primary"} disabled={disabled} loading={loading}>
            {confirmLabel}
          </Button>
        </div>
      </form>
    </dialog>
  );
}

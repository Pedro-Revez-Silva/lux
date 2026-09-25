import { createContext, useCallback, useContext, useMemo, useState, type ReactNode } from "react";
import { IconCheck, IconClose, IconInfo, IconWarning } from "./icons.tsx";

export type ToastTone = "info" | "success" | "warn" | "danger";

export interface ToastOptions {
  title: ReactNode;
  description?: ReactNode;
  tone?: ToastTone;
  /** ms; 0 keeps it until dismissed. */
  duration?: number;
}

interface ToastItem extends ToastOptions {
  id: number;
}

const ToastCtx = createContext<((o: ToastOptions) => void) | null>(null);

export function useToast(): (o: ToastOptions) => void {
  const push = useContext(ToastCtx);
  if (!push) throw new Error("useToast: wrap the app in <ToastProvider>");
  return push;
}

export function ToastProvider({ children }: { children: ReactNode }) {
  const [items, setItems] = useState<ToastItem[]>([]);
  const dismiss = useCallback((id: number) => setItems((xs) => xs.filter((x) => x.id !== id)), []);
  const push = useCallback(
    (o: ToastOptions) => {
      const id = Date.now() + Math.random();
      setItems((xs) => [...xs.slice(-4), { ...o, id }]);
      const d = o.duration ?? 4000;
      if (d > 0) setTimeout(() => dismiss(id), d);
    },
    [dismiss],
  );
  const value = useMemo(() => push, [push]);
  return (
    <ToastCtx.Provider value={value}>
      {children}
      <div className="toasts" aria-live="polite">
        {items.map((t) => (
          <Toast key={t.id} {...t} onDismiss={() => dismiss(t.id)} />
        ))}
      </div>
    </ToastCtx.Provider>
  );
}

export function Toast({ title, description, tone = "info", onDismiss }: ToastOptions & { onDismiss?: () => void }) {
  const Icon = tone === "success" ? IconCheck : tone === "info" ? IconInfo : IconWarning;
  return (
    <div className={`toast toast-${tone}`} role="status">
      <span className="toast-icon">
        <Icon size={14} />
      </span>
      <div className="toast-body">
        <div className="toast-title">{title}</div>
        {description && <div className="toast-desc">{description}</div>}
      </div>
      {onDismiss && (
        <button type="button" className="toast-close" onClick={onDismiss} aria-label="Dismiss">
          <IconClose size={12} />
        </button>
      )}
    </div>
  );
}

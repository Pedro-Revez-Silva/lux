import type { ButtonHTMLAttributes, ReactNode } from "react";
import { Spinner } from "./Spinner.tsx";

export type ButtonVariant = "default" | "primary" | "danger" | "ghost";
export type ButtonSize = "sm" | "md" | "lg";

export interface ButtonProps extends Omit<ButtonHTMLAttributes<HTMLButtonElement>, "children"> {
  variant?: ButtonVariant;
  size?: ButtonSize;
  loading?: boolean;
  icon?: ReactNode;
  children?: ReactNode;
}

export function Button({ variant = "default", size = "md", loading = false, icon, children, className, disabled, ...rest }: ButtonProps) {
  const cls = ["btn", `btn-${variant}`, `btn-${size}`, loading ? "btn-loading" : "", className ?? ""].join(" ").trim();
  return (
    <button type="button" className={cls} disabled={disabled || loading} aria-busy={loading || undefined} {...rest}>
      {loading ? <Spinner size={size === "sm" ? 12 : 14} /> : icon ? <span className="btn-icon">{icon}</span> : null}
      {children != null && <span className="btn-label">{children}</span>}
    </button>
  );
}

export interface IconButtonProps extends Omit<ButtonHTMLAttributes<HTMLButtonElement>, "children"> {
  /** Required: icon-only buttons need an accessible name. */
  label: string;
  size?: ButtonSize;
  variant?: ButtonVariant;
  active?: boolean;
  children: ReactNode;
}

export function IconButton({ label, size = "md", variant = "ghost", active, children, className, ...rest }: IconButtonProps) {
  const cls = ["btn", "btn-iconOnly", `btn-${variant}`, `btn-${size}`, active ? "is-active" : "", className ?? ""].join(" ").trim();
  return (
    <button type="button" className={cls} aria-label={label} title={label} aria-pressed={active} {...rest}>
      {children}
    </button>
  );
}

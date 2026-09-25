import { useState, type FormEvent } from "react";
import { Button, Logo } from "@lux/design-system";
import { signIn } from "../api/index.ts";

/** Asks for an API key. Shown when there is none, or after a 401. */
export function SignIn({ reason }: { reason?: string }) {
  const [key, setKey] = useState("");
  const submit = (e: FormEvent) => {
    e.preventDefault();
    const k = key.trim();
    if (k) signIn(k);
  };
  return (
    <div className="signin">
      <form className="signin-card" onSubmit={submit}>
        <div className="brand signin-brand">
          <Logo className="brand-mark" />
          <span className="brand-name">lux</span>
          <span className="brand-sub">console</span>
        </div>
        <p className="secondary">Sign in with an API key. Operator keys see every tenant; tenant keys see their own.</p>
        {reason && (
          <div className="error-strip" role="alert">
            {reason}
          </div>
        )}
        <label className="field">
          <span className="field-label">API key</span>
          <input className="input mono" type="password" value={key} onChange={(e) => setKey(e.target.value)} autoFocus autoComplete="off" spellCheck={false} placeholder="lux_…" />
        </label>
        <div className="dialog-actions">
          <Button type="submit" variant="primary" disabled={key.trim() === ""}>
            Sign in
          </Button>
        </div>
        <p className="muted signin-note">The key stays in this tab's session storage and is sent as a bearer token to this server only.</p>
      </form>
    </div>
  );
}

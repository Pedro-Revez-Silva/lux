import { useState, type FormEvent, type ReactNode } from "react";
import { Button, Logo } from "@lux/design-system";
import { IconCloud } from "@lux/design-system/icons";
import { signIn } from "../api/index.ts";

/** The frame of a page shown before or beside the console proper (sign-in, a preview hand-off): one centred card under the brand. */
export function AuthScreen({ children, as: As = "div", ...rest }: { children: ReactNode; as?: "div" | "form"; onSubmit?: (e: FormEvent) => void }) {
  return (
    <div className="signin">
      <As className="signin-card" {...rest}>
        <div className="brand signin-brand">
          <Logo size={26} className="brand-mark" />
          <span className="brand-name">Lux</span>
        </div>
        {children}
      </As>
    </div>
  );
}

export interface SignInProps {
  /** Why sign-in is needed again (an expired session, a page that needs a key). */
  reason?: string;
  /** Where the person was headed, shown so they know sign-in continues there. */
  next?: { icon?: ReactNode; text: ReactNode };
  /** luxd sits behind Cloudflare Access too: offer it beside the key. */
  access?: boolean;
}

/** Asks for an API key. Shown when there is none, or after a 401. */
export function SignIn({ reason, next, access }: SignInProps) {
  const [key, setKey] = useState("");
  const submit = (e: FormEvent) => {
    e.preventDefault();
    const k = key.trim();
    if (k) signIn(k);
  };
  return (
    <AuthScreen as="form" onSubmit={submit}>
      <p className="secondary">Sign in with an API key. Operator keys see every tenant; tenant keys see their own.</p>
      {next && (
        <div className="signin-next">
          {next.icon && <span className="signin-next-icon">{next.icon}</span>}
          <span>{next.text}</span>
        </div>
      )}
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
      {access && (
        <>
          <div className="signin-or">or</div>
          <Button icon={<IconCloud size={16} />} onClick={() => window.location.reload()}>
            Continue with Cloudflare Access
          </Button>
        </>
      )}
      <p className="muted signin-note">The key stays in this tab's session storage and is sent as a bearer token to this server only.</p>
    </AuthScreen>
  );
}

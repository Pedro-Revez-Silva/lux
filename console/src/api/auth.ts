// The session: an API key, kept in sessionStorage so a reload keeps you
// signed in but closing the tab does not; or, when luxd is behind Cloudflare
// Access, the person Access signed in (no key: the browser's Access cookie
// authenticates every call). Whether it is an operator's, and who, is
// learned from GET /v1/whoami.
import { useSyncExternalStore } from "react";

const KEY = "lux.key";
const listeners = new Set<() => void>();

export type Role = "unknown" | "operator" | "tenant";

/** A person signed in by Cloudflare Access; picture: their photo's URL, if the identity provider has one. */
export interface User {
  email: string;
  name: string;
  picture?: string;
}

interface Session {
  key: string | null;
  role: Role;
  /** Signed in by Cloudflare Access, as this person (no key). */
  user: User | null;
  /** How luxd's console signs people in, once whoami has said (null until then). */
  consoleAuth: "key" | "cloudflare-access" | null;
}

let session: Session = { key: readKey(), role: "unknown", user: null, consoleAuth: null };

function readKey(): string | null {
  try {
    return sessionStorage.getItem(KEY);
  } catch {
    return null;
  }
}

function emit() {
  for (const l of listeners) l();
}

export function getSession(): Session {
  return session;
}

export function getKey(): string | null {
  return session.key;
}

export function signIn(key: string) {
  try {
    sessionStorage.setItem(KEY, key);
  } catch {}
  session = { ...session, key, role: "unknown", user: null };
  emit();
}

/** Signed in by the console auth luxd sits behind (Cloudflare Access). */
export function signInAs(user: User, role: Role) {
  session = { ...session, key: null, role, user, consoleAuth: "cloudflare-access" };
  emit();
}

export function signOut() {
  try {
    sessionStorage.removeItem(KEY);
  } catch {}
  session = { ...session, key: null, role: "unknown", user: null };
  emit();
}

export function setRole(role: Role, consoleAuth?: Session["consoleAuth"]) {
  if (session.role === role && (consoleAuth == null || session.consoleAuth === consoleAuth)) return;
  session = { ...session, role, consoleAuth: consoleAuth ?? session.consoleAuth };
  emit();
}

function subscribe(cb: () => void) {
  listeners.add(cb);
  return () => listeners.delete(cb);
}

export function useSession(): Session {
  return useSyncExternalStore(subscribe, () => session, () => session);
}

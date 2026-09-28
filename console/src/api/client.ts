// Fetch wrapper for the luxd API. Same origin; every call carries the key.
// The tenant scope is explicit: calls that list across tenants take it as an
// argument (undefined: the key's own view, which is every tenant for operators).
import { getKey, signOut } from "./auth.ts";
import type { ApiErrorBody } from "./types.ts";

const API_BASE = "/v1";

export class ApiError extends Error {
  status: number;
  code: string;
  details: unknown;
  constructor(status: number, code: string, message: string, details?: unknown) {
    super(message);
    this.name = "ApiError";
    this.status = status;
    this.code = code;
    this.details = details;
  }
  /** What to show a person. */
  get display(): string {
    if (this.status === 403) return `Your key cannot do this: ${this.message}`;
    return this.message;
  }
}

export type Query = Record<string, string | number | boolean | string[] | null | undefined>;

export interface RequestOptions {
  method?: "GET" | "POST" | "PUT" | "DELETE";
  body?: unknown;
  query?: Query;
  signal?: AbortSignal;
  /** Narrow an operator's request to this tenant (id or name); undefined: no narrowing. */
  tenant?: string;
  headers?: Record<string, string>;
}

function buildUrl(path: string, query: Query = {}, tenant?: string): string {
  const u = new URL(API_BASE + path, window.location.origin);
  for (const [k, v] of Object.entries(query)) {
    if (v == null || v === "" || v === false) continue;
    if (Array.isArray(v)) {
      for (const x of v) u.searchParams.append(k, x);
    } else u.searchParams.set(k, String(v));
  }
  if (tenant) u.searchParams.set("tenant", tenant);
  return u.pathname + u.search;
}

function authHeaders(extra: Record<string, string> = {}): Record<string, string> {
  const key = getKey();
  return key ? { Authorization: `Bearer ${key}`, ...extra } : extra;
}

async function toError(res: Response): Promise<ApiError> {
  let body: ApiErrorBody | null = null;
  try {
    body = (await res.json()) as ApiErrorBody;
  } catch {}
  const e = body?.error;
  return new ApiError(res.status, e?.code ?? "http_error", e?.message ?? `${res.status} ${res.statusText}`, e?.details);
}

/** Raw fetch with auth; a 401 signs out (the sign-in screen takes over). */
export async function apiFetch(path: string, opts: RequestOptions = {}): Promise<Response> {
  const res = await fetch(buildUrl(path, opts.query, opts.tenant), {
    method: opts.method ?? "GET",
    headers: authHeaders({ ...(opts.body !== undefined ? { "Content-Type": "application/json" } : {}), ...(opts.headers ?? {}) }),
    body: opts.body !== undefined ? JSON.stringify(opts.body) : undefined,
    signal: opts.signal,
  });
  if (res.status === 401) {
    signOut();
    throw await toError(res);
  }
  if (!res.ok) throw await toError(res);
  return res;
}

export async function request<T>(path: string, opts: RequestOptions = {}): Promise<T> {
  const res = await apiFetch(path, opts);
  if (res.status === 204) return undefined as T;
  return (await res.json()) as T;
}

/** Download a file through fetch (EventSource/anchors cannot send the key). */
export async function download(path: string, filename: string): Promise<void> {
  const res = await apiFetch(path);
  const blob = await res.blob();
  const url = URL.createObjectURL(blob);
  const a = document.createElement("a");
  a.href = url;
  a.download = filename;
  document.body.appendChild(a);
  a.click();
  a.remove();
  setTimeout(() => URL.revokeObjectURL(url), 10_000);
}

export function isApiError(e: unknown): e is ApiError {
  return e instanceof ApiError;
}

export function errorText(e: unknown): string {
  if (isApiError(e)) return e.display;
  if (e instanceof Error) return e.message;
  return String(e);
}

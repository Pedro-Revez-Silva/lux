// The exec stream: GET /v1/runs/{id}/exec upgraded to a WebSocket (see
// internal/server/stream.go). The first message is a proto.StreamOpen; then
// proto.StreamData both ways: {data: base64} for bytes, {rows, cols} to
// resize, and the stream ends with {exitCode} or {error}, or the socket
// closing. A browser cannot put a header on a WebSocket: behind Cloudflare
// Access the cookie rides along; with a key, a single-use ticket
// (POST /tickets) goes in the query.
import { getKey } from "./auth.ts";
import { apiFetch, apiUrl } from "./client.ts";
import { api } from "./endpoints.ts";

/** What lux shell runs: bash as a login shell, sh when there is none. */
export const SHELL_COMMAND = ["/bin/sh", "-c", "command -v bash >/dev/null && exec bash -l; exec /bin/sh -l"];

export interface ExecOptions {
  command: string[];
  /** The grid at the moment the socket opens (a resize during the round trips is not lost). */
  size: () => { rows: number; cols: number };
  signal: AbortSignal;
  /** The socket is open and the shell requested. */
  onOpen: () => void;
  /** Bytes from the process. */
  onData: (data: Uint8Array) => void;
  /** The process ended (the stream closes after). */
  onExit: (code: number) => void;
  /** The stream could not be opened, or ended in an error. */
  onError: (message: string) => void;
  /** The socket closed: after an exit or error, or on its own (the host went away). */
  onClose: (code: number, reason: string) => void;
}

export interface ExecSession {
  /** Keystrokes, as text (UTF-8 on the wire). */
  send: (text: string) => void;
  resize: (cols: number, rows: number) => void;
  close: () => void;
}

/** proto.StreamData, the fields a terminal uses. */
interface StreamData {
  data?: string;
  rows?: number;
  cols?: number;
  exitCode?: number;
  error?: string;
}

const enc = new TextEncoder();

function toBase64(bytes: Uint8Array): string {
  let s = "";
  for (const b of bytes) s += String.fromCharCode(b);
  return btoa(s);
}

function fromBase64(s: string): Uint8Array {
  const bin = atob(s);
  const out = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
  return out;
}

/**
 * Open a shell in a running Run. Checks first, without upgrading, that the
 * stream can be opened (the same URL answers the error it would get: not
 * running, no host, not allowed; a 401 signs out); with a key session a
 * ticket is minted meanwhile. Then dials.
 */
export async function openExec(runId: string, opts: ExecOptions): Promise<ExecSession> {
  const path = `/runs/${encodeURIComponent(runId)}/exec`;
  // Independent of the check, so the two round trips overlap; a ticket
  // minted for a check that fails is single-use and expires in a minute.
  const ticket = getKey() ? api.ticket(runId, "exec", opts.signal) : null;
  ticket?.catch(() => {});
  await apiFetch(path, { signal: opts.signal });
  const url = new URL(apiUrl(path), window.location.origin);
  url.protocol = url.protocol === "https:" ? "wss:" : "ws:";
  if (ticket) url.searchParams.set("ticket", (await ticket).ticket);
  opts.signal.throwIfAborted();

  const ws = new WebSocket(url);
  let ended = false;
  const send = (m: StreamData) => {
    if (ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify(m));
  };
  ws.onopen = () => {
    ws.send(JSON.stringify({ command: opts.command, tty: true, ...opts.size() }));
    opts.onOpen();
  };
  ws.onmessage = (ev) => {
    let d: StreamData;
    try {
      d = JSON.parse(String(ev.data)) as StreamData;
    } catch {
      return;
    }
    if (d.data) opts.onData(fromBase64(d.data));
    if (d.error) {
      ended = true;
      opts.onError(d.error);
    } else if (d.exitCode != null) {
      ended = true;
      opts.onExit(d.exitCode);
    }
  };
  ws.onclose = (ev) => {
    if (opts.signal.aborted) return;
    opts.onClose(ev.code, ended ? "" : ev.reason);
  };
  const close = () => {
    if (ws.readyState === WebSocket.OPEN || ws.readyState === WebSocket.CONNECTING) ws.close(1000, "closed");
  };
  opts.signal.addEventListener("abort", close, { once: true });
  return {
    send: (text) => send({ data: toBase64(enc.encode(text)) }),
    resize: (cols, rows) => send({ cols, rows }),
    close,
  };
}

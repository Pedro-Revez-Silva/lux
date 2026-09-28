import type { ReactNode } from "react";
import { Spinner } from "./Spinner.tsx";

export type ConnectionStatus = "connecting" | "connected" | "exited" | "disconnected";

export interface ConnectionBadgeProps {
  status: ConnectionStatus;
  /** The shell's exit code, once it exited. */
  exitCode?: number | null;
  /** Replace the default words (e.g. "Reconnecting"). */
  label?: ReactNode;
  className?: string;
}

const TONE: Record<ConnectionStatus, string> = {
  connecting: "badge-warn",
  connected: "badge-success",
  exited: "badge-neutral",
  disconnected: "badge-danger",
};

const WORDS: Record<ConnectionStatus, string> = {
  connecting: "Connecting",
  connected: "Connected",
  exited: "Shell exited",
  disconnected: "Disconnected",
};

/** A live link's state beside a title: a dot (a spinner while connecting) and a word. */
export function ConnectionBadge({ status, exitCode, label, className }: ConnectionBadgeProps) {
  const text = label ?? (status === "exited" && exitCode != null ? `${WORDS.exited} · code ${exitCode}` : WORDS[status]);
  return (
    <span className={["badge", TONE[status], "conn", className ?? ""].join(" ").trim()} role="status">
      {status === "connecting" ? <Spinner size={10} className="conn-spinner" /> : <span className="conn-dot" aria-hidden="true" />}
      {text}
    </span>
  );
}

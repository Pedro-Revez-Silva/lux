import type { ReactNode } from "react";
import { Badge, type BadgeTone } from "./Badge.tsx";
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

const TONE: Record<ConnectionStatus, BadgeTone> = {
  connecting: "warn",
  connected: "success",
  exited: "neutral",
  disconnected: "danger",
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
    <Badge tone={TONE[status]} className={["conn", className ?? ""].join(" ").trim()} role="status">
      {status === "connecting" ? <Spinner size={10} className="conn-spinner" /> : <span className="conn-dot" aria-hidden="true" />}
      {text}
    </Badge>
  );
}

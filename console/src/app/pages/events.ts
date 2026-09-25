// One-line summaries of lifecycle events, from their data payloads.
import type { Event } from "../../api/index.ts";
import { formatBytes } from "@lux/design-system";

function str(v: unknown): string | undefined {
  return typeof v === "string" && v !== "" ? v : undefined;
}

export function eventSummary(e: Event): string {
  const d = e.data ?? {};
  switch (e.type) {
    case "state": {
      const parts = [str(d.state) ?? "?"];
      if (str(d.reason)) parts.push(`(${str(d.reason)})`);
      if (str(d.host)) parts.push(`on ${str(d.host)}`);
      return parts.join(" ");
    }
    case "submitted":
      return `by ${str(d.by) ?? "?"}`;
    case "exited": {
      const code = typeof d.exitCode === "number" ? `exit ${d.exitCode}` : "exited";
      return [code, str(d.reason), str(d.message)].filter(Boolean).join(" · ");
    }
    case "snapshot":
      return `snapshot ${str(d.snapshotId) ?? ""} · ${typeof d.bytes === "number" ? `${d.bytes} bytes` : ""} · ${typeof d.volumes === "number" ? `${d.volumes} volumes` : ""}`.replace(/( · )+$/, "");
    case "snapshot.failed":
      return `snapshot failed: ${str(d.error) ?? "?"}`;
    case "activity":
      return str(d.activity) ?? "";
    case "session":
      return `session ${str(d.sessionId) ?? ""}`;
    case "input":
      return d.interrupt === true && !str(d.text) ? "interrupt" : `input: ${str(d.text) ?? `${typeof d.rawBytes === "number" ? d.rawBytes : 0} raw bytes`}`;
    case "input.delivered":
      return `input delivered${str(d.text) ? `: ${str(d.text)}` : ""}`;
    case "input.failed":
      return `input failed: ${str(d.error) ?? "?"}`;
    case "stop.requested":
    case "cancel.requested":
      return `by ${str(d.by) ?? "?"}`;
    case "migrate.requested":
      return `from ${str(d.from) ?? "?"} to ${str(d.to) ?? "any other host"}`;
    case "disk.exceeded":
      return `disk ${typeof d.usedBytes === "number" ? formatBytes(d.usedBytes) : "?"} over its limit of ${typeof d.limitBytes === "number" ? formatBytes(d.limitBytes) : "?"}`;
    case "push.requested":
      return `push ${str(d.requestId) ?? ""}`;
    case "resume.requested": {
      const added = Array.isArray(d.addedRepositories) && d.addedRepositories.length > 0 ? ` · adding ${d.addedRepositories.join(", ")}` : "";
      return `by ${str(d.by) ?? "?"}${added}`;
    }
    case "git.clone":
      return d.status === "failed"
        ? `${str(d.repo) ?? "?"} not cloned: ${str(d.error) ?? "?"}`
        : `${str(d.repo) ?? "?"} cloned at ${(str(d.commit) ?? "?").slice(0, 12)}`;
    default: {
      const keys = Object.keys(d);
      if (keys.length === 0) return "";
      return keys
        .slice(0, 4)
        .map((k) => `${k}=${typeof d[k] === "object" ? JSON.stringify(d[k]) : String(d[k])}`)
        .join(" ");
    }
  }
}

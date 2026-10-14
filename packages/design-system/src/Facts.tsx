import { useId, useMemo, useState, type ReactNode } from "react";
import { Button } from "./Button.tsx";
import { sortLabels, LabelChips } from "./CostPanel.tsx";
import { useCopy } from "./IdChip.tsx";
import { IconCheck, IconChevronDown, IconChevronUp, IconCopy } from "./icons.tsx";

export interface FactRowProps {
  label: ReactNode;
  /** A muted figure after the label ("Labels 8"). */
  count?: number;
  /** The value, on one line beside the label. */
  children: ReactNode;
  /** A control at the row's right end (a disclosure toggle). */
  aside?: ReactNode;
  /** Full-width content under the line (what a disclosure opens). */
  detail?: ReactNode;
  className?: string;
}

/**
 * A full-width fact under a KeyValue grid, for a value that can be long (an
 * image reference, labels, hashes): one line, the rest on hover, copy or
 * behind a disclosure, so it never stretches the grid's rows.
 */
export function FactRow({ label, count, children, aside, detail, className }: FactRowProps) {
  const id = useId();
  return (
    <div className={["fact-row", className ?? ""].join(" ").trim()} role="group" aria-labelledby={id}>
      <div className="fact-line">
        <span id={id} className="fact-key">
          {label}
          {count != null && <span className="num"> {count}</span>}
        </span>
        <div className="fact-value">{children}</div>
        {aside}
      </div>
      {detail}
    </div>
  );
}

export interface ImageRefParts {
  /** Registry and path up to the last slash, with it ("ghcr.io/acme/"); empty for a bare name. */
  path: string;
  name: string;
  /** With its colon (":1.4"), shown whole. */
  tag: string;
  /** Without its "@" ("sha256:2c56…"). */
  digest: string;
}

export function splitImageRef(ref: string): ImageRefParts {
  const at = ref.indexOf("@");
  const repo = at < 0 ? ref : ref.slice(0, at);
  const digest = at < 0 ? "" : ref.slice(at + 1);
  const slash = repo.lastIndexOf("/");
  const colon = repo.lastIndexOf(":");
  // A colon before the last slash is a registry port, not a tag.
  const tagAt = colon > slash ? colon : -1;
  return {
    path: repo.slice(0, slash + 1),
    name: repo.slice(slash + 1, tagAt < 0 ? undefined : tagAt),
    tag: tagAt < 0 ? "" : repo.slice(tagAt),
    digest,
  };
}

/** The algorithm + the first 12 and last 8 hex digits of a digest; a short or unrecognised one whole. */
export function shortDigest(digest: string): string {
  const m = /^([a-z0-9+._-]+):([0-9a-f]+)$/i.exec(digest);
  if (!m || m[2]!.length <= 24) return digest;
  return `${m[1]}:${m[2]!.slice(0, 12)}…${m[2]!.slice(-8)}`;
}

function CopyButton({ value, label }: { value: string; label: string }) {
  const { copied, copy } = useCopy(value);
  return (
    <button type="button" className={copied ? "fact-copy idchip-copy is-copied" : "fact-copy idchip-copy"} onClick={copy} aria-label={copied ? "Copied" : label} title={label}>
      {copied ? <IconCheck size={12} /> : <IconCopy size={12} />}
    </button>
  );
}

/**
 * An image reference on one line: the registry path muted, the name in the
 * foreground, a digest shortened in the middle (tags whole). The whole
 * reference is the title and what the copy affordance copies.
 */
export function ImageRef({ value }: { value: string }) {
  const p = splitImageRef(value);
  return (
    <span className="imageref">
      <span className="imageref-text mono" title={value}>
        {p.path && <span className="imageref-path">{p.path}</span>}
        <span className="imageref-name">{p.name}</span>
        {p.tag && <span className="imageref-path">{p.tag}</span>}
        {p.digest && <span className="imageref-path">@{shortDigest(p.digest)}</span>}
      </span>
      <CopyButton value={value} label="Copy image reference" />
    </span>
  );
}

export interface LabelsFactProps {
  labels: Record<string, string> | undefined;
  /** The row's name, for other key/value sets (Versions, Template); default "Labels". */
  label?: ReactNode;
  /** Keys shown first, in the preview and the list (LabelChips' `first`). */
  first?: string[];
  /** Chips in the collapsed preview. */
  max?: number;
  /** Open on mount (the gallery); the console starts collapsed. */
  defaultOpen?: boolean;
}

/**
 * A Run's labels as a FactRow: a count, a few LabelChips and a toggle that
 * opens every pair, key over a complete, selectable value with a copy affordance.
 * Starts collapsed; no labels reads "–" with no toggle.
 */
export function LabelsFact({ labels, label = "Labels", first, max = 3, defaultOpen = false }: LabelsFactProps) {
  const [open, setOpen] = useState(defaultOpen);
  const listId = useId();
  // `first` is usually an inline array literal: key the memo on its contents.
  const firstKey = first?.join("\0");
  const pairs = useMemo(() => sortLabels(labels, first), [labels, firstKey]);
  if (pairs.length === 0) {
    return (
      <FactRow label={label} className="labels-fact">
        <span className="muted">–</span>
      </FactRow>
    );
  }
  return (
    <FactRow
      label={label}
      count={pairs.length}
      className="labels-fact"
      aside={
        <Button variant="ghost" size="sm" className="fact-toggle" aria-expanded={open} aria-controls={open ? listId : undefined} onClick={() => setOpen(!open)}>
          {open ? "Hide" : "Show all"}
          {open ? <IconChevronUp size={12} /> : <IconChevronDown size={12} />}
        </Button>
      }
      detail={
        open && (
          <dl id={listId} className="label-list">
            {pairs.map(([k, v]) => (
              <div key={k} className="label-pair">
                <dt>{k}</dt>
                <dd>
                  <span className="mono">{v}</span>
                  <CopyButton value={v} label={`Copy ${k}`} />
                </dd>
              </div>
            ))}
          </dl>
        )
      }
    >
      <LabelChips entries={pairs} max={max} />
    </FactRow>
  );
}

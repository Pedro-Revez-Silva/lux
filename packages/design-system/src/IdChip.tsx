import { useCallback, useEffect, useState, type MouseEvent } from "react";
import { IconCheck, IconCopy } from "./icons.tsx";

/** Copy `value` to the clipboard; `copied` is true for a moment after, for the affordance to say so. */
export function useCopy(value: string): { copied: boolean; copy: () => void } {
  const [copied, setCopied] = useState(false);
  useEffect(() => {
    if (!copied) return;
    const t = setTimeout(() => setCopied(false), 1200);
    return () => clearTimeout(t);
  }, [copied]);
  const copy = useCallback(() => {
    void navigator.clipboard?.writeText(value).then(() => setCopied(true));
  }, [value]);
  return { copied, copy };
}

export interface IdChipProps {
  value: string;
  /** Show only the first n characters (with an ellipsis); the full id copies. */
  truncate?: number;
  /** Prefix label, e.g. "run". */
  prefix?: string;
  href?: string;
  /** Click handler for the link (e.g. client-side navigation). */
  onLinkClick?: (e: MouseEvent<HTMLAnchorElement>) => void;
  className?: string;
}

/** A quiet id: plain mono, muted, with a copy affordance on hover or focus. Click the icon (or the text when no href) to copy. */
export function IdChip({ value, truncate, prefix, href, onLinkClick, className }: IdChipProps) {
  const { copied, copy: copyValue } = useCopy(value);

  const shown = truncate && value.length > truncate ? value.slice(0, truncate) + "…" : value;
  // Chips sit inside clickable rows: their own clicks never bubble.
  const copy = (e: MouseEvent) => {
    e.stopPropagation();
    copyValue();
  };
  const follow = (e: MouseEvent<HTMLAnchorElement>) => {
    e.stopPropagation();
    onLinkClick?.(e);
  };
  const text = href ? (
    <a href={href} className="idchip-text" onClick={follow}>
      {shown}
    </a>
  ) : (
    <span className="idchip-text" onClick={copy}>
      {shown}
    </span>
  );
  return (
    <span className={["idchip", copied ? "is-copied" : "", className ?? ""].join(" ").trim()} title={value}>
      {prefix && <span className="idchip-prefix">{prefix}</span>}
      {text}
      <button type="button" className="idchip-copy" onClick={copy} aria-label={copied ? "Copied" : "Copy"}>
        {copied ? <IconCheck size={12} /> : <IconCopy size={12} />}
      </button>
    </span>
  );
}

/** Inline code with the same look, no copy affordance. */
export function Code({ children, className }: { children: string; className?: string }) {
  return <code className={["code", className ?? ""].join(" ").trim()}>{children}</code>;
}

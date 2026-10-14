import { useId, useState, type FormEvent, type KeyboardEvent, type ReactNode } from "react";
import { Button } from "./Button.tsx";

export interface SessionComposerProps {
  /** The field's visible label ("Message the agent", "Send to stdin"); also the form's name. */
  label: string;
  /** Delivers the trimmed text. Resolving clears the draft; rejecting keeps it and shows the error. */
  onSend: (text: string) => Promise<unknown>;
  /** Interrupt the current turn; omitted, no Interrupt button. A rejection is swallowed: report it yourself. */
  onInterrupt?: () => Promise<unknown> | void;
  /** What Interrupt does, as its title. */
  interruptTitle?: string;
  /** Textarea, Send and Interrupt are all off (the Run cannot take input). */
  disabled?: boolean;
  placeholder?: string;
  /** The draft it starts with (a restored draft). */
  defaultValue?: string;
  /** One quiet line left of the buttons: where input lands, or why it cannot be sent. */
  hint?: ReactNode;
  /** Turns a rejected send into the alert's text. */
  errorText?: (err: unknown) => string;
}

const defaultErrorText = (err: unknown) => (err instanceof Error ? err.message : String(err));

/**
 * The input form under a SessionTranscript: a labelled textarea, a hint, Interrupt
 * and Send. Ctrl/⌘+Enter in the textarea sends; Enter is a newline.
 */
export function SessionComposer({ label, onSend, onInterrupt, interruptTitle = "Interrupt the agent's current turn", disabled = false, placeholder, defaultValue = "", hint, errorText = defaultErrorText }: SessionComposerProps) {
  const id = useId();
  const [draft, setDraft] = useState(defaultValue);
  const [sending, setSending] = useState(false);
  const [interrupting, setInterrupting] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const text = draft.trim();

  const send = async () => {
    if (disabled || sending || !text) return;
    setSending(true);
    try {
      await onSend(text);
      setDraft("");
      setError(null);
    } catch (err) {
      setError(errorText(err));
    } finally {
      setSending(false);
    }
  };
  const onSubmit = (e: FormEvent) => {
    e.preventDefault();
    void send();
  };
  const onKeyDown = (e: KeyboardEvent<HTMLTextAreaElement>) => {
    // An IME confirming a candidate with the chord reports isComposing (keyCode 229 in older engines).
    if (e.nativeEvent.isComposing || e.keyCode === 229) return;
    if (e.key === "Enter" && (e.ctrlKey || e.metaKey)) {
      e.preventDefault();
      void send();
    }
  };
  const interrupt = async () => {
    if (!onInterrupt) return;
    setInterrupting(true);
    try {
      await onInterrupt();
    } catch {
      // Feedback for a failed interrupt is the caller's (the console toasts it).
    } finally {
      setInterrupting(false);
    }
  };

  return (
    <form className="composer" aria-label={label} onSubmit={onSubmit}>
      <label htmlFor={id} className="field-label">
        {label}
      </label>
      <textarea
        id={id}
        className="input composer-input"
        rows={2}
        value={draft}
        placeholder={placeholder}
        disabled={disabled}
        readOnly={sending}
        aria-describedby={[hint ? `${id}-hint` : "", error ? `${id}-error` : ""].filter(Boolean).join(" ") || undefined}
        aria-invalid={error ? true : undefined}
        aria-keyshortcuts="Control+Enter Meta+Enter"
        onChange={(e) => {
          setDraft(e.target.value);
          setError(null);
        }}
        onKeyDown={onKeyDown}
      />
      <div className="composer-actions">
        {hint && (
          <span id={`${id}-hint`} className="composer-hint">
            {hint}
          </span>
        )}
        <span className="grow" />
        {onInterrupt && (
          <Button disabled={disabled} loading={interrupting} onClick={() => void interrupt()} title={interruptTitle}>
            Interrupt
          </Button>
        )}
        <Button type="submit" variant="primary" disabled={disabled || !text} loading={sending}>
          Send
        </Button>
      </div>
      {error && (
        <p id={`${id}-error`} className="composer-error" role="alert">
          {error}
        </p>
      )}
    </form>
  );
}

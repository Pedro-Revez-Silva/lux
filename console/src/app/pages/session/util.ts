// Small readers for untyped record data.
export type Obj = Record<string, unknown>;

export function obj(v: unknown): Obj | undefined {
  return v && typeof v === "object" && !Array.isArray(v) ? (v as Obj) : undefined;
}

export function str(v: unknown): string | undefined {
  return typeof v === "string" ? v : undefined;
}

export function num(v: unknown): number | undefined {
  return typeof v === "number" && Number.isFinite(v) ? v : undefined;
}

export function arr(v: unknown): unknown[] {
  return Array.isArray(v) ? v : [];
}

// The text of a list of content blocks ({type:"text", text}) or a string.
export function blocksText(v: unknown): string {
  if (typeof v === "string") return v;
  return arr(v)
    .map((b) => {
      const o = obj(b);
      if (!o) return "";
      if (typeof o.text === "string") return o.text;
      const inner = obj(o.content);
      return inner && typeof inner.text === "string" ? inner.text : "";
    })
    .filter((s) => s !== "")
    .join("\n");
}

// A todo list (todowrite input, ACP plan entries) as plan steps.
export function planSteps(list: unknown): { text: string; status: "done" | "now" | "todo" }[] {
  return arr(list).flatMap((e) => {
    const o = obj(e);
    const text = str(o?.content) ?? str(o?.step) ?? "";
    if (!text) return [];
    const s = str(o?.status) ?? "";
    const status = s === "completed" ? "done" : s === "in_progress" || s === "inProgress" ? "now" : "todo";
    return [{ text, status }] as const;
  });
}

export function plural(n: number, one: string, many = `${one}s`): string {
  return `${n} ${n === 1 ? one : many}`;
}

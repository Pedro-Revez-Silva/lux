// The Session tab's model: a Run's output records folded into a
// conversation. Pure data; projection.ts builds it, one adapter per file.
import type { OutputRecord } from "../../../api/index.ts";

export type Adapter = "acp" | "opencode" | "claude-code" | "codex" | string;

export type ToolStatus = "pending" | "running" | "completed" | "error";

export interface InputItem {
  kind: "input";
  id: string;
  t: number;
  requestId: string;
  // "prompt" is the workload's first prompt; anything else was steered in.
  steer: boolean;
  sender?: string;
  text: string;
  truncated?: boolean;
  phase: "sent" | "accepted" | "consumed" | "failed";
  error?: string;
  attachments?: number;
}

export interface MessageItem {
  kind: "message";
  id: string;
  t: number;
  text: string;
  streaming: boolean;
  // Cut by lux before it reached the console (an OpenCode child's message).
  truncated?: boolean;
}

export interface ThoughtItem {
  kind: "thought";
  id: string;
  t: number;
  // Last chunk's time: "Thought for Ns" is end - t.
  end: number;
  text: string;
}

export interface PlanStep {
  text: string;
  status: "done" | "now" | "todo";
}

export interface PlanItem {
  kind: "plan";
  id: string;
  t: number;
  steps: PlanStep[];
}

export interface ToolItem {
  kind: "tool";
  id: string;
  t: number;
  end?: number;
  name: string;
  title?: string;
  status: ToolStatus;
  input?: unknown;
  // Input as lux relayed it when it is a cut JSON string (opencode.part).
  inputText?: string;
  output?: string;
  diff?: string;
  error?: string;
  truncated?: boolean;
  // The record that first named this call: "<epoch>.<seq>".
  cursor: string;
  // A subagent this call runs (a thread id).
  child?: string;
}

export type MarkerType = "epoch" | "turn" | "compacted" | "warning" | "ended";

export interface MarkerItem {
  kind: "marker";
  id: string;
  t: number;
  type: MarkerType;
  text: string;
  summary?: string;
  summaryTruncated?: boolean;
  tokens?: { pre?: number; post?: number };
  trigger?: string;
}

export interface NoteItem {
  kind: "note";
  id: string;
  t: number;
  tone: "permission" | "warning";
  text: string;
  detail?: string;
}

export type SessionItem = InputItem | MessageItem | ThoughtItem | PlanItem | ToolItem | MarkerItem | NoteItem;

export type ThreadStatus = "running" | "completed" | "error" | "idle";

export interface SessionThread {
  id: string;
  parent?: string;
  // The tool call in the parent that runs it.
  callId?: string;
  title: string;
  agent?: string;
  model?: string;
  status: ThreadStatus;
  startedAt?: number;
  endedAt?: number;
  // The task's input, as the parent briefed it.
  brief?: string;
  briefTruncated?: boolean;
  items: SessionItem[];
  children: string[];
  tools: number;
  cost?: number;
  tokens?: number;
}

// Whether the adapter reports subagents.
export type SubagentSupport = "reported" | "unsupported";

export interface Session {
  adapter: Adapter;
  subagents: SubagentSupport;
  root: string;
  threads: Record<string, SessionThread>;
  sessionId?: string;
  model?: string;
  activity?: "busy" | "idle";
  turns: number;
  compactions: number;
  records: number;
  cursor: string;
  // Changes on every fold that changed anything, for memoised views.
  version: number;
}

export type { OutputRecord };

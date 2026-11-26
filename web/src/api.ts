export type Event = {
  id?: string;
  kind: "user" | "assistant" | "thinking" | "tool" | "error" | "note";
  text?: string;
  tool?: string;
  input?: string;
  detail?: string;
  output?: string;
  failed?: boolean;
  attachments?: Attachment[];
  // The local pictures the text names that the server can show.
  images?: Attachment[];
  time: string;
};

export type Attachment = { name: string; url: string; path?: string; image?: boolean };

export type Conversation = {
  id: string;
  role?: string;
  // The inbox source the session checks for kk, such as gmail. kk starts its runs and limits its tools.
  integration?: string;
  harness: string;
  model?: string;
  dir: string;
  // The directory the list groups the conversation under; for a worktree session the directory it was made from.
  project: string;
  // The agent works in a git worktree, apart from the main checkout.
  in_worktree: boolean;
  // The feature the agent works on. Empty until kk or konduktor has decided.
  group?: string;
  title: string;
  created: string;
  last_message: string;
  activity: string;
  // A queued session waits with its prompt for a start; no agent runs yet.
  status: "queued" | "starting" | "working" | "background" | "idle" | "exited";
  // The session this one follows in a chain.
  after?: string;
  prompt?: string;
  attention?: string;
  // Who has to act on an agent that stopped; empty while it works.
  waiting_for: "" | "user" | "conductor";
  // The konduktor the session belongs to; empty for konduktors and kk's own inbox sessions.
  konduktor?: string;
  // The directory a konduktor other than the main one is responsible for.
  scope?: string;
  // Why konduktor handed the agent to the user.
  escalation?: string;
  closed?: boolean;
  closed_at?: string;
  // Stopped by kk after a long time without activity; waits for nobody.
  sleeping?: boolean;
  schedule?: Schedule;
  last_run?: string;
  next_run?: string;
  // A scheduled run finished and the user has not seen it yet.
  run_done: boolean;
  context_tokens: number;
  // What the models used over every transcript of the conversation; null for a harness that does not report it.
  usage: Usage | null;
  unread: boolean;
  actions: string[];
  // Messages the agent holds until it gets to them.
  queued: Event[];
  events: Event[];
  // The agents the session's agent started, oldest first.
  subagents: Subagent[];
  // Set when this is a subagent, which is shown read only.
  subagent?: Subagent;
};

// A subagent of a session. Its id is the session's id and the subagent's, joined by a colon; parent is the id of the
// subagent that started it, empty when the session's agent did. Its usage is part of the session's.
export type Subagent = {
  id: string;
  of: string;
  parent?: string;
  title: string;
  type?: string;
  model?: string;
  state: "running" | "done" | "failed" | "stopped";
  started: string;
  last: string;
  worktree?: string;
  branch?: string;
  usage: Usage | null;
};

// Days are weekdays, 0 for Sunday; times are local, "09:30".
export type Schedule = { days: number[]; times: string[]; prompt: string };

// Input excludes cache reads and writes. Cost is in US dollars at API list prices, for the models with a known price.
export type Usage = { input: number; cache_read: number; cache_write: number; output: number; cost: number; unpriced?: string[] };

export type PastSession = { id: string; title: string; dir: string; modified: string; messages: number; started: string; ended: string };

export type Repo = { git: boolean; branch: string };

export type Skill = { name: string; description: string; insert: string; scope: string };

export type Settings = {
  addr: string;
  default_dir: string;
  skill_dirs: string[];
  coordinator: boolean;
  coordinator_model: string;
  coordinator_prompt: string;
  worker_prompt: string;
  source: string;
  start_timeout_seconds: number;
  compact_after_minutes: number;
  compact_tokens: number;
  inbox: Record<string, { interval_minutes: number; methods: Record<string, boolean> }>;
};

// Rewind says what going back to an earlier message does in a harness, with and without restoring files.
export type Rewind = { conversation: string; files?: string };

export type Harness = { name: string; label: string; models: string[]; hooks: boolean; rewind?: Rewind };

export async function api<T = unknown>(method: string, path: string, body?: unknown): Promise<T> {
  const res = await fetch(path, {
    method,
    headers: body === undefined ? {} : { "Content-Type": "application/json" },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(data.error || res.statusText);
  return data as T;
}

export async function upload(file: File): Promise<Attachment> {
  const res = await fetch(`/api/attachments?name=${encodeURIComponent(file.name)}`, { method: "POST", body: file });
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(data.error || res.statusText);
  return data as Attachment;
}

let flashTimer = 0;
export function flash(message: string) {
  let el = document.getElementById("flash");
  if (!el) {
    el = document.createElement("div");
    el.id = "flash";
    el.onclick = () => el!.remove();
    document.body.append(el);
  }
  el.textContent = message;
  clearTimeout(flashTimer);
  flashTimer = window.setTimeout(() => el!.remove(), 6000);
}

export async function act(fn: () => Promise<unknown>) {
  try {
    await fn();
  } catch (e) {
    flash((e as Error).message);
  }
}

const months = ["Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"];

export function when(iso: string): string {
  const d = new Date(iso);
  if (!iso || d.getFullYear() < 2000) return "-";
  const hm = `${String(d.getHours()).padStart(2, "0")}:${String(d.getMinutes()).padStart(2, "0")}`;
  const now = new Date();
  if (d.toDateString() === now.toDateString()) return hm;
  return `${d.getDate()} ${months[d.getMonth()]} ${hm}`;
}

export function harnessLabel(name: string): string {
  return { claude: "Claude Code", codex: "Codex", cursor: "Cursor" }[name] ?? name;
}

export const usageTokens = (u: Usage) => u.input + u.cache_read + u.cache_write + u.output;

// newTokens leaves out cache reads, which repeat the context on every request and dwarf the rest.
export const newTokens = (u: Usage) => u.input + u.cache_write + u.output;

export function count(n: number): string {
  if (n < 1000) return String(n);
  const [d, unit] = n < 1e6 ? [1e3, "k"] : n < 1e9 ? [1e6, "M"] : [1e9, "B"];
  const v = n / d;
  return (v < 10 ? v.toFixed(1).replace(/\.0$/, "") : Math.round(v).toString()) + unit;
}

export function dollars(v: number): string {
  if (v > 0 && v < 0.005) return "<$0.01";
  return "$" + (v < 100 ? v.toFixed(2) : Math.round(v).toLocaleString("en-US"));
}

const ESTIMATE_NOTE = "With a subscription it is an estimate, not what you pay.";

// usageLabel is the short form for a session card; usageTitle the breakdown for its tooltip.
export function usageLabel(u: Usage | null): string {
  if (!u) return "Usage unavailable";
  const tokens = `${count(newTokens(u))} tokens`;
  return u.unpriced?.length ? tokens : `${tokens} · ${dollars(u.cost)}`;
}

export function usageTitle(u: Usage | null, harness: string): string {
  if (!u) return `${harnessLabel(harness)} does not report token usage`;
  const n = (v: number) => v.toLocaleString("en-US");
  return [
    `${n(newTokens(u))} tokens without cache reads`,
    `New input ${n(u.input)}`,
    `Cache writes ${n(u.cache_write)}`,
    `Output ${n(u.output)}`,
    `Cache reads ${n(u.cache_read)}, the context read again on each request`,
    u.unpriced?.length ? `No list price for ${u.unpriced.join(", ")}, so no cost is shown` : `Cost ${dollars(u.cost)} at API list prices. ${ESTIMATE_NOTE}`,
  ].join("\n");
}

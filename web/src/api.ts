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
  time: string;
};

export type Attachment = { name: string; url: string; path?: string };

export type Conversation = {
  id: string;
  role?: string;
  harness: string;
  model?: string;
  dir: string;
  // The directory the list groups the conversation under; for a worktree session the directory it was made from.
  project: string;
  // The feature the agent works on. Empty until kk or the coordinator has decided.
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
  // Why the conductor handed the agent to the user.
  escalation?: string;
  closed?: boolean;
  closed_at?: string;
  context_tokens: number;
  // What the models used over every transcript of the conversation; null for a harness that does not report it.
  usage: Usage | null;
  unread: boolean;
  actions: string[];
  // Messages the agent holds until it gets to them.
  queued: Event[];
  events: Event[];
};

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

const ESTIMATE = "Cost at API list prices. With a subscription it is an estimate, not what you pay.";

// usageLabel is the short form for a session card; usageTitle the breakdown for its tooltip.
export function usageLabel(u: Usage | null): string {
  if (!u) return "Usage unavailable";
  const tokens = `${count(usageTokens(u))} tokens`;
  return u.unpriced?.length ? tokens : `${tokens} · ${dollars(u.cost)}`;
}

export function usageTitle(u: Usage | null, harness: string): string {
  if (!u) return `${harnessLabel(harness)} does not report token usage`;
  const n = (v: number) => v.toLocaleString("en-US");
  const lines = [
    `${n(usageTokens(u))} tokens`,
    `Input ${n(u.input)} · cache read ${n(u.cache_read)} · cache write ${n(u.cache_write)} · output ${n(u.output)}`,
    u.unpriced?.length ? `No list price for ${u.unpriced.join(", ")}, so no cost is shown` : ESTIMATE,
  ];
  return lines.join("\n");
}

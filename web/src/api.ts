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
  status: "starting" | "working" | "background" | "idle" | "exited";
  attention?: string;
  // Who has to act on an agent that stopped; empty while it works.
  waiting_for: "" | "user" | "conductor";
  // Why the conductor handed the agent to the user.
  escalation?: string;
  closed?: boolean;
  closed_at?: string;
  context_tokens: number;
  unread: boolean;
  actions: string[];
  // Messages the agent holds until it gets to them.
  queued: Event[];
  events: Event[];
};

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

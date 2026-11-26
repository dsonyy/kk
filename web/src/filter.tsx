import { useEffect, useRef, useState } from "preact/hooks";
import type { Conversation } from "./api";
import { Button, Check } from "./mnemonic";

export type Filter = { dirs: string[]; states: string[] };

// The states overlap: an exited session also waits for someone, and a session that needs action is its own and someone's turn.
const states: Record<string, (c: Conversation) => boolean> = {
  Working: (c) => !c.closed && (c.status === "working" || c.status === "starting"),
  "Background working": (c) => !c.closed && c.status === "background",
  "Your turn": (c) => !c.closed && c.waiting_for === "user",
  "Conductor's turn": (c) => !c.closed && c.waiting_for === "conductor",
  "Action required": (c) => !c.closed && !!c.attention,
  Idle: (c) => !c.closed && c.status === "idle",
  Queued: (c) => !c.closed && c.status === "queued",
  Exited: (c) => !c.closed && c.status === "exited" && !c.sleeping,
  Sleeping: (c) => !c.closed && !!c.sleeping,
  Scheduled: (c) => !!c.schedule,
  Closed: (c) => !!c.closed,
  "Closed < 48h": (c) => !!c.closed && !!c.closed_at && Date.now() - Date.parse(c.closed_at) < 48 * 3600e3,
};

export function matches(f: Filter, c: Conversation) {
  return (!f.dirs.length || f.dirs.includes(c.project)) && (!f.states.length || f.states.some((s) => states[s]?.(c)));
}

export function useFilter(): [Filter, (f: Filter) => void] {
  const [filter, setFilter] = useState<Filter>(() => ({ dirs: [], states: [], ...JSON.parse(localStorage.getItem("kk.filter") || "{}") }));
  return [
    filter,
    (f) => {
      localStorage.setItem("kk.filter", JSON.stringify(f));
      setFilter(f);
    },
  ];
}

export type Sort = "active" | "created" | "title" | "state";

const sorts: Record<Sort, string> = { active: "Last active", created: "Created", title: "Title", state: "State" };

export function useSort(): [Sort, (s: Sort) => void] {
  const [sort, setSort] = useState<Sort>(() => {
    const saved = localStorage.getItem("kk.sort") as Sort;
    return saved in sorts ? saved : "active";
  });
  return [
    sort,
    (s) => {
      localStorage.setItem("kk.sort", s);
      setSort(s);
    },
  ];
}

export const time = (iso: string) => Date.parse(iso) || 0;

// The same signal the server orders its list by.
export const lastActive = (c: Conversation) => Math.max(time(c.created), time(c.last_message), time(c.activity));

const inProgress = (list: Conversation[]) => list.some((c) => !c.closed && c.status !== "exited");
const finished = (c: Conversation) => Math.max(lastActive(c), time(c.closed_at ?? ""));

// When a group was last active, or, once none of its sessions is in progress, when it finished.
export const groupTime = (list: Conversation[]) => Math.max(...list.map(inProgress(list) ? lastActive : finished));

// The default order of groups: those with a session in progress first, then the finished ones, each the most recent first.
export const byActivity = (a: Conversation[], b: Conversation[]) => Number(inProgress(b)) - Number(inProgress(a)) || groupTime(b) - groupTime(a);

const collator = new Intl.Collator("en", { sensitivity: "base", numeric: true });
export const byName = (a: string, b: string) => collator.compare(a, b);

// Needs action first, then the user's turn, at work, at work in the background, the conductor's turn, queued, exited, sleeping and closed.
function stateRank(c: Conversation) {
  if (c.closed) return 8;
  if (c.sleeping) return 7;
  if (c.attention || c.run_done) return 0;
  if (c.status === "exited") return 6;
  if (c.status === "queued") return 5;
  if (c.status === "working" || c.status === "starting") return 2;
  if (c.status === "background") return 3;
  return c.waiting_for === "conductor" ? 4 : 1;
}

export function compare(sort: Sort): (a: Conversation, b: Conversation) => number {
  const recent = (a: Conversation, b: Conversation) => lastActive(b) - lastActive(a);
  return {
    active: recent,
    created: (a: Conversation, b: Conversation) => time(b.created) - time(a.created),
    title: (a: Conversation, b: Conversation) => byName(a.title || "Untitled", b.title || "Untitled") || recent(a, b),
    state: (a: Conversation, b: Conversation) => stateRank(a) - stateRank(b) || recent(a, b),
  }[sort];
}

// A directory goes by its last segment, and by its last two when another group ends the same way.
export function shortName(project: string, all: string[]) {
  const parts = project.split("/").filter(Boolean);
  const last = parts[parts.length - 1] ?? project;
  return all.filter((p) => p.endsWith(`/${last}`)).length > 1 ? parts.slice(-2).join("/") : last;
}

// A plain menu shows its label alone, for choices that are not a filter.
type MenuProps = { label: string; options: [string, string][]; selected: string[]; mono?: boolean; single?: boolean; plain?: boolean; onChange: (selected: string[]) => void };

export function Menu({ label, options, selected, mono, single, plain, onChange }: MenuProps) {
  const [open, setOpen] = useState(false);
  const ref = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (!open) return;
    const close = (e: Event) => {
      if (e instanceof KeyboardEvent ? e.key === "Escape" : !ref.current?.contains(e.target as Node)) setOpen(false);
    };
    document.addEventListener("pointerdown", close);
    document.addEventListener("keydown", close);
    return () => {
      document.removeEventListener("pointerdown", close);
      document.removeEventListener("keydown", close);
    };
  }, [open]);
  const toggle = (value: string, on: boolean) => onChange(options.map(([v]) => v).filter((v) => (v === value ? on : selected.includes(v))));
  const pick = (value: string) => {
    setOpen(false);
    onChange([value]);
  };
  const text = single ? `${label}: ${options.find(([v]) => v === selected[0])?.[1]}` : selected.length && !plain ? `${label} (${selected.length})` : label;
  return (
    <div class="menu" ref={ref}>
      <Button label={text} class={selected.length && !single && !plain ? "small on" : "small"} aria-expanded={open} onClick={() => setOpen(!open)} />
      {open && (
        <div class={mono ? "menu-list mono" : "menu-list"}>
          {options.map(([value, text]) => (
            <div key={value} title={mono ? value : undefined}>
              {single ? (
                <Check label={text} type="radio" checked={selected.includes(value)} onClick={() => pick(value)} />
              ) : (
                <Check label={text} checked={selected.includes(value)} onChange={(e) => toggle(value, e.currentTarget.checked)} />
              )}
            </div>
          ))}
        </div>
      )}
    </div>
  );
}

type FiltersProps = { sessions: Conversation[]; shown: number; filter: Filter; onChange: (f: Filter) => void; sort: Sort; onSort: (s: Sort) => void };

export function Filters({ sessions, shown, filter, onChange, sort, onSort }: FiltersProps) {
  const projects = [...new Set([...sessions.map((c) => c.project), ...filter.dirs])].sort();
  const active = filter.dirs.length + filter.states.length > 0;
  return (
    <div class="filters">
      <Menu label="Directory" mono options={projects.map((p) => [p, shortName(p, projects)])} selected={filter.dirs} onChange={(dirs) => onChange({ ...filter, dirs })} />
      <Menu label="State" options={Object.keys(states).map((s) => [s, s])} selected={filter.states} onChange={(s) => onChange({ ...filter, states: s })} />
      <Menu label="Sort" single options={Object.entries(sorts)} selected={[sort]} onChange={([s]) => onSort(s as Sort)} />
      {active && <Button label="Clear" class="small" title="Show all sessions" onClick={() => onChange({ dirs: [], states: [] })} />}
      {active && (
        <span class="muted">
          {shown} of {sessions.length}
        </span>
      )}
    </div>
  );
}

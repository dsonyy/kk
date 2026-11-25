import { useEffect, useRef, useState } from "preact/hooks";
import type { Conversation } from "./api";
import { Button, Check } from "./mnemonic";

export type Filter = { dirs: string[]; states: string[] };

// The states overlap: an exited session also waits for someone, and a session that needs action is its own and someone's turn.
const states: Record<string, (c: Conversation) => boolean> = {
  Working: (c) => !c.closed && (c.status === "working" || c.status === "starting"),
  "Your turn": (c) => !c.closed && c.waiting_for === "user",
  "Conductor's turn": (c) => !c.closed && c.waiting_for === "conductor",
  "Action required": (c) => !c.closed && !!c.attention,
  Idle: (c) => !c.closed && c.status === "idle",
  Exited: (c) => !c.closed && c.status === "exited",
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

// A directory goes by its last segment, and by its last two when another group ends the same way.
export function shortName(project: string, all: string[]) {
  const parts = project.split("/").filter(Boolean);
  const last = parts[parts.length - 1] ?? project;
  return all.filter((p) => p.endsWith(`/${last}`)).length > 1 ? parts.slice(-2).join("/") : last;
}

type MenuProps = { label: string; options: [string, string][]; selected: string[]; mono?: boolean; onChange: (selected: string[]) => void };

function Menu({ label, options, selected, mono, onChange }: MenuProps) {
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
  return (
    <div class="menu" ref={ref}>
      <Button label={selected.length ? `${label} (${selected.length})` : label} class={selected.length ? "small on" : "small"} aria-expanded={open} onClick={() => setOpen(!open)} />
      {open && (
        <div class={mono ? "menu-list mono" : "menu-list"}>
          {options.map(([value, text]) => (
            <div key={value} title={mono ? value : undefined}>
              <Check label={text} checked={selected.includes(value)} onChange={(e) => toggle(value, e.currentTarget.checked)} />
            </div>
          ))}
        </div>
      )}
    </div>
  );
}

export function Filters({ sessions, shown, filter, onChange }: { sessions: Conversation[]; shown: number; filter: Filter; onChange: (f: Filter) => void }) {
  const projects = [...new Set([...sessions.map((c) => c.project), ...filter.dirs])].sort();
  const active = filter.dirs.length + filter.states.length > 0;
  return (
    <div class="filters">
      <Menu label="Directory" mono options={projects.map((p) => [p, shortName(p, projects)])} selected={filter.dirs} onChange={(dirs) => onChange({ ...filter, dirs })} />
      <Menu label="State" options={Object.keys(states).map((s) => [s, s])} selected={filter.states} onChange={(s) => onChange({ ...filter, states: s })} />
      {active && <Button label="Clear" class="small" title="Show all sessions" onClick={() => onChange({ dirs: [], states: [] })} />}
      {active && (
        <span class="muted">
          {shown} of {sessions.length}
        </span>
      )}
    </div>
  );
}

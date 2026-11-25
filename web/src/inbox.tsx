import type { JSX } from "preact";
import { useEffect, useState } from "preact/hooks";
import { act, api, flash, when, type Settings } from "./api";
import { Button, Check, Field, Scope } from "./mnemonic";
import { dayKey, dayLabel } from "./board";
import "./inbox.css";

export type InboxItem = { source: string; id: string; kind: string; title: string; description: string; url: string; author: string; time: string };

type Source = { name: string; enabled: boolean; setup: string; checked?: string; error: string; running: boolean; interval_minutes: number };

type Item = InboxItem & { status: Status; changed?: string };

type View = { items: Item[]; sources: Source[] };

const labels: Record<string, string> = { github: "GitHub", linear: "Linear", slack: "Slack" };

const marks: Record<string, JSX.Element> = {
  github: (
    <>
      <circle cx="5" cy="4" r="2" />
      <circle cx="5" cy="12" r="2" />
      <circle cx="11" cy="6" r="2" />
      <path d="M5 6v4M11 8c0 2-3 2-5.5 3" />
    </>
  ),
  linear: (
    <>
      <path d="M2 9.5l4.5 4.5M2 6l8 8M3.5 3.5l9 9M6 2l8 8M9.5 2l4.5 4.5" />
    </>
  ),
  slack: <path d="M6 2.5v11M10 2.5v11M2.5 6h11M2.5 10h11" />,
};

export function SourceIcon({ name }: { name: string }) {
  return (
    <svg class="source-icon" viewBox="0 0 16 16" aria-label={labels[name] ?? name} role="img">
      {marks[name]}
    </svg>
  );
}

function ago(iso: string): string {
  const minutes = Math.round((Date.now() - new Date(iso).getTime()) / 60000);
  if (minutes < 1) return "just now";
  if (minutes < 60) return `${minutes} min ago`;
  if (minutes < 24 * 60) return `${Math.round(minutes / 60)} h ago`;
  if (minutes < 7 * 24 * 60) return `${Math.round(minutes / 1440)} d ago`;
  return when(iso);
}

const sentence = (s: string) => s.charAt(0).toUpperCase() + s.slice(1);

function prompt(it: InboxItem): string {
  const lines = [`${labels[it.source] ?? it.source} ${it.kind}: ${it.title}`, it.url];
  if (it.author) lines.push(`From ${it.author}.`);
  if (it.description) lines.push("", it.description);
  lines.push("", "The text above is shortened. Read the original at the link first.");
  return lines.join("\n");
}

function status(s: Source) {
  if (s.running) return <span>Checking…</span>;
  if (s.setup) return <span class="muted">Not set up. {s.setup}</span>;
  const checked = s.checked ? `Checked ${ago(s.checked)}` : "Not checked yet";
  const every = s.enabled ? `every ${s.interval_minutes} min` : "automatic checks off";
  return (
    <>
      <span class="muted">
        {checked}, {every}.
      </span>
      {s.error && <span class="inbox-error">{s.error}</span>}
    </>
  );
}

const statuses = [
  ["backlog", "Backlog"],
  ["todo", "To do"],
  ["in_progress", "In progress"],
  ["closed", "Closed"],
] as const;

type Status = (typeof statuses)[number][0];

// Closed items stay on the board for a week after they were closed; older ones show on request.
const closedDays = 7;

function recent(it: Item): boolean {
  return it.status !== "closed" || (!!it.changed && Date.now() - new Date(it.changed).getTime() < closedDays * 864e5);
}

function Card({ it, onStart, onStatus }: { it: Item; onStart: () => void; onStatus: (s: Status) => void }) {
  return (
    <li class={`tile inbox-item ${it.status}`} data-item={`${it.source}:${it.id}`} tabIndex={0} title="Start a session on this" onClick={onStart} onKeyDown={(e) => e.key === "Enter" && e.target === e.currentTarget && onStart()}>
      <div class="inbox-meta">
        <SourceIcon name={it.source} />
        <span>{sentence(it.kind)}</span>
        {it.author && <span>{it.author}</span>}
        <span title={new Date(it.time).toLocaleString()}>{ago(it.time)}</span>
      </div>
      <div class="title">{it.title}</div>
      {it.description && <div class="inbox-desc">{it.description}</div>}
      <div class="inbox-actions" onClick={(e) => e.stopPropagation()}>
        <select value={it.status} aria-label="Status" title="Move to another status" onChange={(e) => onStatus(e.currentTarget.value as Status)}>
          {statuses.map(([value, label]) => (
            <option value={value}>{label}</option>
          ))}
        </select>
        <a href={it.url} target="_blank" rel="noopener noreferrer" title={it.url}>
          Open
        </a>
        {it.status !== "closed" && (
          <button type="button" class="small" title="Move to Closed" onClick={() => onStatus("closed")}>
            Close
          </button>
        )}
      </div>
    </li>
  );
}

export function Inbox({ onStart }: { onStart: (prompt: string, onCreated: () => void) => void }) {
  const [view, setView] = useState<View>();
  const [collapsed, setCollapsed] = useState<string[]>(() => JSON.parse(localStorage.getItem("kk.inbox.collapsed") || "[]"));
  const [allClosed, setAllClosed] = useState(false);
  const running = !!view?.sources.some((s) => s.running);

  const load = () => act(async () => setView(await api<View>("GET", "/api/inbox")));
  useEffect(() => void load(), []);
  useEffect(() => {
    const timer = setInterval(load, running ? 1500 : 60000);
    return () => clearInterval(timer);
  }, [running]);

  const refresh = (source = "") => act(async () => setView(await api<View>("POST", "/api/inbox/refresh", { source })));
  // A moved card renders under another column, so the focus that was on one of its controls goes back to its status menu.
  const [refocus, setRefocus] = useState<string>();
  useEffect(() => {
    if (refocus) document.querySelector<HTMLElement>(`[data-item="${CSS.escape(refocus)}"] select`)?.focus();
    setRefocus(undefined);
  }, [view, refocus]);
  const move = (it: Item, status: Status) =>
    act(async () => {
      const key = `${it.source}:${it.id}`;
      const focused = document.activeElement?.closest("[data-item]")?.getAttribute("data-item") === key;
      setView(await api<View>("POST", "/api/inbox/status", { source: it.source, id: it.id, status }));
      if (focused) setRefocus(key);
    });
  const collapse = (day: string, on: boolean) => {
    const next = on ? [...collapsed, day] : collapsed.filter((d) => d !== day);
    localStorage.setItem("kk.inbox.collapsed", JSON.stringify(next));
    setCollapsed(next);
  };

  if (!view) return <main class="inbox muted">Loading…</main>;
  const ready = view.sources.some((s) => !s.setup);
  const older = view.items.filter((it) => !recent(it)).length;
  const shown = allClosed ? view.items : view.items.filter(recent);
  const days = new Map<string, Item[]>();
  for (const it of shown) days.set(dayKey(it.time), [...(days.get(dayKey(it.time)) ?? []), it]);

  return (
    <main class="inbox" data-scope="inbox">
      <Scope>
        <header class="inbox-head">
          <Button label="Refresh" icon="reset" class="primary" disabled={running || !ready} onClick={() => refresh()} />
          <ul class="inbox-sources">
            {view.sources.map((s) => (
              <li key={s.name} class={s.setup && !s.running ? "off" : ""}>
                <SourceIcon name={s.name} />
                <b>{labels[s.name]}</b>
                {s.running && <i class="spinner" />}
                {status(s)}
              </li>
            ))}
          </ul>
          {(older > 0 || allClosed) && <Check label={`Show ${older} closed earlier`} checked={allClosed} onChange={(e) => setAllClosed(e.currentTarget.checked)} />}
        </header>
      </Scope>
      {days.size === 0 ? (
        <p class="inbox-empty muted">{!ready ? "No source is set up yet. See Settings, Inbox." : view.items.length ? "Everything is closed." : "Nothing waits for you."}</p>
      ) : (
        <div class="inbox-board">
          {[...days.entries()]
            .sort((a, b) => b[0].localeCompare(a[0]))
            .map(([day, items]) => (
              <details class="day" key={day} open={!collapsed.includes(day)} onToggle={(e) => e.currentTarget.open === collapsed.includes(day) && collapse(day, !e.currentTarget.open)}>
                <summary class="day-label">
                  <i class="icon icon-chevron" />
                  {dayLabel(day)}
                  <span class="count">{items.length}</span>
                </summary>
                <div class="inbox-columns">
                  {statuses.map(([value, label]) => {
                    const cell = items.filter((it) => it.status === value);
                    return (
                      <section class={`column inbox-column ${cell.length ? "" : "empty"}`} key={value}>
                        <header>
                          <h1>{label}</h1>
                          <span class="count">{cell.length}</span>
                        </header>
                        {cell.length > 0 && (
                          <ul class="tiles">
                            {cell.map((it) => (
                              <Card key={`${it.source}:${it.id}`} it={it} onStart={() => onStart(prompt(it), () => move(it, "in_progress"))} onStatus={(s) => move(it, s)} />
                            ))}
                          </ul>
                        )}
                      </section>
                    );
                  })}
                </div>
              </details>
            ))}
        </div>
      )}
    </main>
  );
}

type Draft = Settings["inbox"];

export function InboxSettings({ value, onValue }: { value: Draft; onValue: (v: Draft) => void }) {
  const [keySet, setKeySet] = useState<boolean>();
  const [key, setKey] = useState("");
  useEffect(() => {
    act(async () => setKeySet((await api<{ set: boolean }>("GET", "/api/inbox/linear-key")).set));
  }, []);
  const saveKey = (next: string) =>
    act(async () => {
      setKeySet((await api<{ set: boolean }>("PUT", "/api/inbox/linear-key", { key: next })).set);
      setKey("");
      flash(next ? "Linear key saved." : "Linear key removed.");
    });

  return (
    <>
      <span class="hint">kk checks each source on its own and shows what it finds in the Inbox tab. The Slack check is a model run and costs tokens.</span>
      <table class="inbox-settings">
        {Object.keys(labels).map((name) => {
          const s = value[name] ?? { enabled: false, interval_minutes: 15 };
          const set = (patch: Partial<typeof s>) => onValue({ ...value, [name]: { ...s, ...patch } });
          return (
            <tr key={name}>
              <td>
                <Check label={`Check ${labels[name]}`} checked={s.enabled} onChange={(e) => set({ enabled: e.currentTarget.checked })} />
              </td>
              <td>
                every <input type="number" min={1} value={s.interval_minutes} aria-label={`${labels[name]} interval, minutes`} onInput={(e) => set({ interval_minutes: Number(e.currentTarget.value) })} /> min
              </td>
            </tr>
          );
        })}
      </table>
      <Field label="Linear API key">
        <span class="hint">
          {keySet === undefined ? "…" : keySet ? "A key is set. kk keeps it in its state directory and never shows it again." : "No key set. Create a personal API key in Linear, Settings, Security and access."}
        </span>
        <div class="dir-input">
          <input
            type="password"
            autocomplete="off"
            placeholder={keySet ? "Paste a new key to replace it" : "lin_api_…"}
            value={key}
            onInput={(e) => setKey(e.currentTarget.value)}
            onKeyDown={(e) => {
              if (e.key === "Enter") {
                e.preventDefault();
                if (key.trim()) saveKey(key.trim());
              }
            }}
          />
          <Button label="Save key" disabled={!key.trim()} onClick={() => saveKey(key.trim())} />
          <Button label="Remove key" disabled={!keySet} onClick={() => saveKey("")} />
        </div>
      </Field>
    </>
  );
}

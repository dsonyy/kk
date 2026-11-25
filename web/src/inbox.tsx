import { useEffect, useState } from "preact/hooks";
import { act, api, flash, when, type Settings } from "./api";
import { Button, Check, Field, Scope } from "./mnemonic";
import { dayKey, dayLabel } from "./board";
import { Menu } from "./filter";
import "./inbox.css";

export type InboxItem = { source: string; id: string; kind: string; title: string; description: string; url: string; author: string; time: string };

type Source = { name: string; enabled: boolean; setup: string; checked?: string; error: string; running: boolean; methods: Record<string, boolean>; interval_minutes: number };

type Item = InboxItem & { status: Status; changed?: string };

type View = { items: Item[]; sources: Source[] };

const labels: Record<string, string> = { github: "GitHub", linear: "Linear", slack: "Slack", gmail: "Gmail" };

const methods: Record<string, string> = { gh: "GitHub CLI (gh)", api_key: "API key", connector: "claude.ai connector" };

export function SourceIcon({ name }: { name: string }) {
  return <i class={`source-icon source-${name}`} aria-label={labels[name] ?? name} role="img" />;
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
  const every = s.enabled ? `every ${s.interval_minutes} min while kk has focus` : "switched off in Settings";
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
  ["in_progress", "In progress"],
  ["dismissed", "Dismissed"],
  ["closed", "Closed"],
] as const;

type Status = (typeof statuses)[number][0];

const dates: Record<string, [string, number]> = { any: ["Any time", Infinity], today: ["Today", 0], week: ["Last 7 days", 6], month: ["Last 30 days", 29] };

type Filter = { date: string; kinds: string[]; sources: string[] };

const noFilter: Filter = { date: "any", kinds: [], sources: [] };

function matches(f: Filter, it: Item) {
  const days = dates[f.date]?.[1] ?? Infinity;
  const today = new Date();
  const since = new Date(today.getFullYear(), today.getMonth(), today.getDate() - days).getTime();
  return (days === Infinity || Date.parse(it.time) >= since) && (!f.kinds.length || f.kinds.includes(it.kind)) && (!f.sources.length || f.sources.includes(it.source));
}

function useStored<T>(key: string, initial: T): [T, (v: T) => void] {
  const [value, setValue] = useState<T>(() => JSON.parse(localStorage.getItem(key) ?? "null") ?? initial);
  return [
    value,
    (v) => {
      localStorage.setItem(key, JSON.stringify(v));
      setValue(v);
    },
  ];
}

type Drag = { key: string; day: string; status: Status };

function Card({ it, dragging, onStart, onStatus, onDrag }: { it: Item; dragging: boolean; onStart: () => void; onStatus: (s: Status) => void; onDrag: (on: boolean) => void }) {
  return (
    <li
      class={`tile inbox-item ${it.status} ${dragging ? "dragging" : ""}`}
      data-item={`${it.source}:${it.id}`}
      tabIndex={0}
      title="Start a session on this, or drag it to another status"
      draggable
      onClick={onStart}
      onKeyDown={(e) => e.key === "Enter" && e.target === e.currentTarget && onStart()}
      onDragStart={(e) => {
        e.dataTransfer!.effectAllowed = "move";
        e.dataTransfer!.setData("text/plain", it.title);
        // The drag image is taken after this handler, so the card is lifted for it now and turns into a placeholder only afterwards.
        e.currentTarget.classList.add("lifted");
        setTimeout(() => onDrag(true));
      }}
      onDragEnd={(e) => {
        e.currentTarget.classList.remove("lifted");
        onDrag(false);
      }}
    >
      <div class="inbox-top">
        <SourceIcon name={it.source} />
        <div class="inbox-meta">
          <span>{sentence(it.kind)}</span>
          {it.author && <span>{it.author}</span>}
          <span title={new Date(it.time).toLocaleString()}>{ago(it.time)}</span>
        </div>
        {it.status !== "dismissed" && (
          <button
            type="button"
            class="inbox-dismiss"
            aria-label="Dismiss"
            title="Move to Dismissed"
            draggable={false}
            onClick={(e) => {
              e.stopPropagation();
              onStatus("dismissed");
            }}
          >
            <i class="icon icon-close" />
          </button>
        )}
      </div>
      <div class="title">{it.title}</div>
      {it.description && <div class="inbox-desc">{it.description}</div>}
      <div class="inbox-actions" onClick={(e) => e.stopPropagation()}>
        <a href={it.url} target="_blank" rel="noopener noreferrer" title={it.url} draggable={false}>
          Open
        </a>
      </div>
    </li>
  );
}

export function Inbox({ onStart }: { onStart: (prompt: string, onCreated: () => void) => void }) {
  const [view, setView] = useState<View>();
  const [collapsed, setCollapsed] = useStored<string[]>("kk.inbox.collapsed", []);
  const [hidden, setHidden] = useStored<string[]>("kk.inbox.hidden", ["dismissed", "closed"]);
  const [filter, setFilter] = useStored<Filter>("kk.inbox.filter", noFilter);
  const running = !!view?.sources.some((s) => s.running);

  const load = () => act(async () => setView(await api<View>("GET", "/api/inbox")));
  useEffect(() => {
    load();
    const onTick = (e: Event) => setView((e as CustomEvent<View>).detail);
    window.addEventListener("kk:inbox", onTick);
    return () => window.removeEventListener("kk:inbox", onTick);
  }, []);
  useEffect(() => {
    const timer = setInterval(load, running ? 1500 : 60000);
    return () => clearInterval(timer);
  }, [running]);

  const refresh = (source = "") => act(async () => setView(await api<View>("POST", "/api/inbox/refresh", { source })));
  // A moved card renders under another column, so the focus that was on one of its controls goes back to the card.
  const [refocus, setRefocus] = useState<string>();
  useEffect(() => {
    if (refocus) document.querySelector<HTMLElement>(`[data-item="${CSS.escape(refocus)}"]`)?.focus();
    setRefocus(undefined);
  }, [view, refocus]);
  const [drag, setDrag] = useState<Drag>();
  const [over, setOver] = useState<Status>();
  const move = (it: Item, status: Status) =>
    act(async () => {
      const key = `${it.source}:${it.id}`;
      const focused = document.activeElement?.closest("[data-item]")?.getAttribute("data-item") === key;
      const set = (s: Status, changed?: string) => setView((v) => v && { ...v, items: v.items.map((x) => (`${x.source}:${x.id}` === key ? { ...x, status: s, changed } : x)) });
      set(status, new Date().toISOString());
      try {
        setView(await api<View>("POST", "/api/inbox/status", { source: it.source, id: it.id, status }));
      } catch (e) {
        set(it.status, it.changed);
        throw e;
      }
      if (focused) setRefocus(key);
    });
  const collapse = (day: string, on: boolean) => setCollapsed(on ? [...collapsed, day] : collapsed.filter((d) => d !== day));

  if (!view) return <main class="inbox muted">Loading…</main>;
  const ready = view.sources.some((s) => !s.setup);
  const columns = statuses.filter(([value]) => !hidden.includes(value));
  const matching = view.items.filter((it) => matches(filter, it));
  const shown = matching.filter((it) => !hidden.includes(it.status));
  const filtered = filter.date !== "any" || filter.kinds.length + filter.sources.length > 0;
  const kinds = [...new Set([...view.items.map((it) => it.kind), ...filter.kinds])].sort();
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
          <div class="filters">
            <Menu label="Date" single options={Object.entries(dates).map(([v, [text]]) => [v, text])} selected={[filter.date]} onChange={([date]) => setFilter({ ...filter, date })} />
            <Menu label="Type" options={kinds.map((k) => [k, sentence(k)])} selected={filter.kinds} onChange={(k) => setFilter({ ...filter, kinds: k })} />
            <Menu label="Source" options={view.sources.map((s) => [s.name, labels[s.name] ?? s.name])} selected={filter.sources} onChange={(s) => setFilter({ ...filter, sources: s })} />
            <Menu
              label="Columns"
              plain
              options={statuses.map(([value, label]) => [value, label])}
              selected={columns.map(([value]) => value)}
              onChange={(shownColumns) => shownColumns.length && setHidden(statuses.map(([value]) => value).filter((v) => !shownColumns.includes(v)))}
            />
            {filtered && <Button label="Clear" class="small" title="Show every item" onClick={() => setFilter(noFilter)} />}
            {filtered && (
              <span class="muted">
                {matching.length} of {view.items.length}
              </span>
            )}
          </div>
        </header>
      </Scope>
      {days.size === 0 ? (
        <p class="inbox-empty muted">
          {!ready ? "No source is set up yet. See Settings, Inbox." : !view.items.length ? "Nothing waits for you." : filtered ? "No items match the filters." : "Nothing in the columns shown."}
        </p>
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
                <div class={`inbox-columns ${drag ? (drag.day === day ? "drag-here" : "drag-elsewhere") : ""}`} data-n={columns.length} style={{ "--n": columns.length }}>
                  {columns.map(([value, label]) => {
                    const cell = items.filter((it) => it.status === value);
                    const target = drag?.day === day && drag.status !== value;
                    return (
                      <section
                        class={`column inbox-column ${cell.length ? "" : "empty"} ${target ? "target" : ""} ${target && over === value ? "over" : ""}`}
                        key={value}
                        onDragOver={(e) => {
                          if (!target) return;
                          e.preventDefault();
                          e.dataTransfer!.dropEffect = "move";
                          setOver(value);
                        }}
                        onDragLeave={(e) => !e.currentTarget.contains(e.relatedTarget as Node) && setOver((o) => (o === value ? undefined : o))}
                        onDrop={(e) => {
                          e.preventDefault();
                          const it = items.find((x) => `${x.source}:${x.id}` === drag?.key);
                          setDrag(undefined);
                          setOver(undefined);
                          if (it && target) move(it, value);
                        }}
                      >
                        <header>
                          <h1>{label}</h1>
                          <span class="count">{cell.length}</span>
                        </header>
                        {cell.length > 0 && (
                          <ul class="tiles">
                            {cell.map((it) => (
                              <Card
                                key={`${it.source}:${it.id}`}
                                it={it}
                                dragging={drag?.key === `${it.source}:${it.id}`}
                                onStart={() => onStart(prompt(it), () => move(it, "in_progress"))}
                                onStatus={(s) => move(it, s)}
                                onDrag={(on) => {
                                  setDrag(on ? { key: `${it.source}:${it.id}`, day, status: it.status } : undefined);
                                  setOver(undefined);
                                }}
                              />
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
      <span class="hint">kk checks each source on its own through each way switched on, only while the kk page has focus, and shows what it finds in the Inbox tab. The claude.ai connector checks are model runs and cost tokens.</span>
      <table class="inbox-settings">
        {Object.keys(labels).map((name) => {
          const s = value[name];
          if (!s) return null;
          const set = (patch: Partial<typeof s>) => onValue({ ...value, [name]: { ...s, ...patch } });
          return (
            <tr key={name}>
              <th>
                <SourceIcon name={name} />
                {labels[name]}
              </th>
              <td>
                {Object.keys(s.methods).map((m) => (
                  <Check key={m} label={methods[m] ?? m} checked={s.methods[m]} onChange={(e) => set({ methods: { ...s.methods, [m]: e.currentTarget.checked } })} />
                ))}
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

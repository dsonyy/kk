import type * as preact from "preact";
import { render } from "preact";
import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from "preact/hooks";
import { marked } from "marked";
import DOMPurify from "dompurify";
import { act, api, flash, harnessLabel, usageLabel, usageTitle, when, type Attachment, type Conversation, type Event, type Harness, type PastSession, type Repo, type Rewind } from "./api";
import { Controls } from "./controls";
import { DirField } from "./dirpicker";
import { Glenz } from "./glenz";
import { Load } from "./load";
import { ImageViewer, linkImages, showImages } from "./images";
import { Board, CardTitle } from "./board";
import { arrange, ChainLinks, ChainRefs, type Chains } from "./chain";
import { Inbox } from "./inbox";
import { byActivity, byName, compare, Filters, matches, shortName, useFilter, useSort } from "./filter";
import { Attachments, PromptInput, useAttachments, useSkills, withPaths } from "./prompt";
import { UserMessage, useRewind } from "./rewind";
import { SettingsView } from "./settings";
import { Button, Check, Field, Scope } from "./mnemonic";
import { Terminal } from "./terminal";
import { Media, media } from "./media";
import { Logs, reportErrors } from "./logs";
import { ScheduleDialog, scheduleText } from "./schedule";
import { Stats } from "./stats";
import "./app.css";

const COORDINATOR = "coordinator";
// A directory shows its sessions in the views of the second level; the global tabs are the same in every directory.
const views = ["Agents", "Sessions"] as const;
const globals = ["Inbox", "Stats", "Logs", "Tmux", "Settings"] as const;
type Tab = (typeof views)[number] | (typeof globals)[number];
const isView = (t: Tab) => (views as readonly string[]).includes(t);

reportErrors();

const mdCache = new Map<string, string>();
function Markdown({ text, images = [], partial }: { text: string; images?: Attachment[]; partial?: boolean }) {
  const key = images.length ? `${text}\0${images.map((i) => i.url).join("\0")}` : text;
  let html = mdCache.get(key);
  if (html === undefined) {
    html = DOMPurify.sanitize(marked.parse(text, { async: false, gfm: true, breaks: true }) as string);
    if (images.length) {
      const box = document.createElement("template");
      box.innerHTML = html;
      showImages(box.content, images);
      html = box.innerHTML;
    }
    if (!partial) mdCache.set(key, html);
  }
  const box = useRef<HTMLDivElement>(null);
  useLayoutEffect(() => linkImages(box.current!), [html]);
  return <div ref={box} class="md" dangerouslySetInnerHTML={{ __html: html }} />;
}

// The open panels in order. Pinned ones stay until hidden; the one marked with ~ is reused by the next session the user
// opens, so at most one panel is unpinned.
type Panel = { id: string; pinned: boolean };

// Where the page is: the tab, the directory (empty for All) and that directory's open panels. The hash holds it as
// tab/directory/panels. A hash from before the directories holds only the panels.
type Place = { tab: Tab; dir: string; panels: Panel[] };

function readHash(): Place & { legacy: boolean } {
  const hash = location.hash.slice(1);
  if (!hash.includes("/")) return { tab: "Agents", dir: "", panels: parsePanels(decodeURIComponent(hash)), legacy: !!hash };
  const [tab, dir, panels = ""] = hash.split("/");
  return { tab: [...views, ...globals].find((t) => t.toLowerCase() === tab) ?? "Agents", dir: decodeURIComponent(dir), panels: parsePanels(panels), legacy: false };
}

const hashOf = (p: Place) => `${p.tab.toLowerCase()}/${encodeURIComponent(p.dir)}/${p.panels.map((x) => (x.pinned ? x.id : `~${x.id}`)).join(",")}`;

// The panels each directory had open, so coming back to it shows them again.
function storedPanels(): Record<string, Panel[]> {
  try {
    return JSON.parse(localStorage.getItem("kk.panels") || "{}");
  } catch {
    return {};
  }
}

function parsePanels(text: string): Panel[] {
  const out: Panel[] = [];
  for (const part of text.split(",")) {
    const id = part.replace(/^~/, "");
    if (id && !out.some((p) => p.id === id)) out.push({ id, pinned: !part.startsWith("~") });
  }
  const slot = out.map((p) => p.pinned).lastIndexOf(false);
  return out.map((p, i) => ({ ...p, pinned: p.pinned || i !== slot }));
}

localStorage.removeItem("kk.konduktorPin");

function App() {
  const [sessions, setList] = useState<Conversation[]>([]);
  const [filter, setFilter] = useFilter();
  const [sort, setSort] = useSort();
  const [place, setPlace] = useState<Place>(readHash);
  const placeRef = useRef(place);
  const { tab, dir, panels } = place;
  const open = panels.map((p) => p.id);
  // A directory opens with the view the user looked at last.
  const lastView = useRef<Tab>(isView(tab) ? tab : "Agents");
  if (isView(tab)) lastView.current = tab;
  // Inside a directory the directory filter has nothing to choose; it stays saved for All.
  const mine = dir ? sessions.filter((c) => c.project === dir) : sessions;
  const scoped = dir ? { ...filter, dirs: [] } : filter;
  const shown = mine.filter((c) => matches(scoped, c)).sort(compare(sort));
  // Closed sessions come after the open ones in the sidebar; both keep the chosen order.
  const list = [...shown.filter((c) => !c.closed), ...shown.filter((c) => c.closed)];
  const empty = mine.length ? "No sessions match the filters." : dir ? "No sessions in this directory." : "No sessions yet.";
  const filters = () => (
    <Filters sessions={mine} shown={shown.length} filter={scoped} dirs={!dir} onChange={(f) => setFilter(dir ? { ...f, dirs: filter.dirs } : f)} sort={sort} onSort={setSort} />
  );
  // Groups whose sessions are all closed start collapsed; opening one lasts until the page reloads.
  const [unfolded, setUnfolded] = useState<string[]>([]);
  const unfold = (key: string, on: boolean) => setUnfolded(on ? [...unfolded, key] : unfolded.filter((k) => k !== key));
  const allClosed = (inGroup: (c: Conversation) => boolean) => sessions.filter(inGroup).every((c) => c.closed);
  const [details, setDetails] = useState<Record<string, Conversation>>({});
  // The prompt a new session starts with; undefined while the dialog is closed.
  const [creating, setCreating] = useState<string>();
  // Called once the session from the dialog exists, for the Inbox item it was started from.
  const created = useRef<(() => void) | undefined>(undefined);
  const create = (prompt: string, then?: () => void) => {
    created.current = then;
    setCreating(prompt);
  };
  const loaded = useRef(new Set<string>());
  const isOpen = (id: string) => placeRef.current.panels.some((p) => p.id === id);

  // Moving to another directory brings back the panels it had open.
  const go = (next: Partial<Place>) => {
    const cur = placeRef.current;
    const p = { ...cur, ...next };
    if (next.dir !== undefined && next.dir !== cur.dir && !next.panels) p.panels = storedPanels()[p.dir] ?? [];
    placeRef.current = p;
    setPlace(p);
    const stored = { ...storedPanels(), [p.dir]: p.panels };
    if (!p.panels.length) delete stored[p.dir];
    localStorage.setItem("kk.panels", JSON.stringify(stored));
    location.hash = hashOf(p);
  };
  const setOpen = (next: Panel[]) => go({ panels: next });
  const hide = (id: string) => setOpen(placeRef.current.panels.filter((p) => p.id !== id));
  // Unpinning a panel makes it the reusable one, so the panel that held that place stays open as a pinned one.
  const pin = (id: string, on: boolean) => setOpen(placeRef.current.panels.map((p) => (p.id === id ? { id, pinned: on } : { ...p, pinned: p.pinned || !on })));
  const pick = (d: string) => {
    setAway(undefined);
    const t = placeRef.current.tab;
    go({ tab: isView(t) ? t : lastView.current, dir: d });
  };

  // The running konduktors; undefined until they are loaded. The panel shows the pinned one, else the one of the directory.
  const [konduktors, setKonduktors] = useState<Conversation[]>();
  // The konduktor the visible panel shows, which the user reads.
  const reading = useRef<string | undefined>(undefined);
  const loadList = useCallback(
    () =>
      act(async () => {
        const [list, ks] = await Promise.all([api<Conversation[]>("GET", "/api/conversations?all=1"), api<Conversation[]>("GET", "/api/konduktors")]);
        setList(list);
        setKonduktors(ks);
      }),
    [],
  );
  const loadDetail = useCallback(async (id: string) => {
    try {
      const c = await api<Conversation>("GET", `/api/conversations/${id}`);
      setDetails((d) => ({ ...d, [id]: c }));
      loaded.current.add(id);
      if (c.unread && (isOpen(id) || id === reading.current) && document.hasFocus()) {
        await api("POST", `/api/conversations/${id}/seen`, {});
      }
    } catch (e) {
      if (!isOpen(id)) return setDetails(({ [id]: _, ...d }) => d);
      // A failed load, such as during a server restart, leaves a pinned panel that has something to show.
      if (!(placeRef.current.panels.find((p) => p.id === id)?.pinned && loaded.current.has(id))) hide(id);
      flash((e as Error).message);
    }
  }, []);

  // A hash from before the directories lists panels of any directory: each goes to its own, and the page to the first one's.
  const legacy = useRef(readHash().legacy);
  useEffect(() => {
    if (!legacy.current || !sessions.length) return;
    legacy.current = false;
    const byDir: Record<string, Panel[]> = {};
    for (const p of placeRef.current.panels) {
      const project = sessions.find((c) => c.id === p.id)?.project;
      if (project) (byDir[project] ??= []).push(p);
    }
    const first = Object.keys(byDir)[0];
    if (first === undefined) return;
    localStorage.setItem("kk.panels", JSON.stringify({ ...storedPanels(), ...byDir }));
    go({ dir: first, panels: byDir[first] });
  }, [sessions]);

  useEffect(() => {
    loadList();
    const pending = new Set<string>();
    let timer = 0;
    const source = new EventSource("/api/events");
    source.onmessage = (e) => {
      pending.add(e.data);
      clearTimeout(timer);
      timer = window.setTimeout(() => {
        loadList();
        for (const id of pending) if (isOpen(id) || id === reading.current) loadDetail(id);
        pending.clear();
      }, 100);
    };
    source.onopen = () => {
      loadList();
      if (reading.current) loadDetail(reading.current);
      for (const p of placeRef.current.panels) loadDetail(p.id);
    };
    const onHash = () => {
      const h = readHash();
      legacy.current ||= h.legacy;
      placeRef.current = h;
      setPlace(h);
    };
    window.addEventListener("hashchange", onHash);
    return () => {
      source.close();
      window.removeEventListener("hashchange", onHash);
    };
  }, []);

  // The inbox checks its sources only while this page has focus, and catches up on the overdue ones when focus comes back.
  useEffect(() => {
    const tick = () => document.hasFocus() && act(async () => window.dispatchEvent(new CustomEvent("kk:inbox", { detail: await api("POST", "/api/inbox/tick", {}) })));
    tick();
    const timer = setInterval(tick, 30000);
    window.addEventListener("focus", tick);
    return () => {
      clearInterval(timer);
      window.removeEventListener("focus", tick);
    };
  }, []);

  useEffect(() => {
    for (const id of open) loadDetail(id);
  }, [open.join(",")]);

  // The directories: a tab for each one with an open session or a running konduktor and for each one the user opened, by name.
  const scopes = (konduktors ?? []).flatMap((k) => (k.scope ? [k.scope] : []));
  const projects = [...new Set([...sessions.map((c) => c.project), ...scopes])];
  const label = (p: string) => shortName(p, projects);
  const inDir = (p: string) => sessions.filter((c) => c.project === p);
  const live = projects.filter((p) => scopes.includes(p) || inDir(p).some((c) => !c.closed));
  const konduktorOf = (d: string) => (konduktors ?? []).find((k) => (d ? k.scope === d : k.id === COORDINATOR));
  // A hidden tab stays in More, and coming back to its directory by any way brings it back.
  const [hidden, setHidden] = useState<string[]>(() => JSON.parse(localStorage.getItem("kk.hiddenTabs") || "[]"));
  const storeHidden = (next: string[]) => {
    localStorage.setItem("kk.hiddenTabs", JSON.stringify(next));
    setHidden(next);
  };
  const hideTab = (d: string) => {
    if (d === placeRef.current.dir) go({ dir: "" });
    storeHidden([...hidden, d]);
  };
  useEffect(() => {
    if (hidden.includes(dir)) storeHidden(hidden.filter((d) => d !== dir));
  }, [dir]);
  // A tab stays until its X is clicked, also when its directory is left with only closed sessions; a gone directory drops out.
  const [kept, setKept] = useState<string[]>(() => JSON.parse(localStorage.getItem("kk.tabs") || "[]"));
  const tabDirs = [...new Set([...live, ...kept.filter((d) => projects.includes(d)), ...(dir ? [dir] : [])])]
    .filter((d) => d === dir || !hidden.includes(d))
    .sort((a, b) => byName(label(a), label(b)));
  useEffect(() => {
    if (!konduktors || tabDirs.join("\n") === kept.join("\n")) return;
    localStorage.setItem("kk.tabs", JSON.stringify(tabDirs));
    setKept(tabDirs);
  }, [konduktors, tabDirs.join("\n")]);
  const hiddenLive = live.filter((d) => d !== dir && hidden.includes(d)).sort((a, b) => byName(label(a), label(b)));
  const finished = projects.filter((p) => !live.includes(p)).sort((a, b) => byActivity(inDir(a), inDir(b)));
  // Alt with a digit picks All and the directories after it.
  const order = useRef<string[]>([]);
  order.current = ["", ...tabDirs];
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      const n = /^Digit([1-9])$/.exec(e.code);
      if (!n || !e.altKey || e.ctrlKey || e.metaKey || (e.target as HTMLElement).closest?.(".xterm")) return;
      const d = order.current[Number(n[1]) - 1];
      if (d === undefined) return;
      e.preventDefault();
      pick(d);
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, []);
  const dirsRef = useRef<HTMLDivElement>(null);
  useEffect(() => {
    const reveal = () => dirsRef.current?.querySelector(".on")?.scrollIntoView({ block: "nearest", inline: "nearest" });
    reveal();
    window.addEventListener("resize", reveal);
    return () => window.removeEventListener("resize", reveal);
  }, [dir, tab]);

  const [collapsed, setCollapsed] = useState<string[]>(() => JSON.parse(localStorage.getItem("kk.collapsed") || "[]"));
  const collapse = (key: string, on: boolean) => {
    const next = on ? [...collapsed, key] : collapsed.filter((p) => p !== key);
    localStorage.setItem("kk.collapsed", JSON.stringify(next));
    setCollapsed(next);
  };
  // In All the sidebar groups sessions by directory. A group follows its first session, its name when sorting by title, and
  // puts work in progress first when sorting by activity. Inside a directory the open sessions form one list, and the closed
  // ones sit below it in a group that starts collapsed.
  const groups = new Map<string, Conversation[]>();
  for (const c of list) groups.set(c.project, [...(groups.get(c.project) ?? []), c]);
  const keys = [...groups.keys()];
  if (sort === "title") keys.sort((a, b) => byName(label(a), label(b)));
  if (sort === "active") keys.sort((a, b) => byActivity(groups.get(a)!, groups.get(b)!));
  const going = list.filter((c) => !c.closed);
  const gone = list.filter((c) => c.closed);
  const goneKey = `${dir}\nclosed`;

  const [konduktor, setKonduktor] = useState(() => localStorage.getItem("kk.conductor") !== "hidden");
  // The panel shows the konduktor of the directory, or of the one picked in its menu until the directory changes.
  const [away, setAway] = useState<string>();
  useEffect(() => setAway(undefined), [dir]);
  const panelDir = away ?? dir;
  const shownKonduktor = konduktorOf(panelDir);
  reading.current = konduktor ? shownKonduktor?.id : undefined;
  useEffect(() => {
    if (reading.current) loadDetail(reading.current);
  }, [reading.current]);
  const konduktorName = (k: Conversation) => (k.id === COORDINATOR ? "main" : label(k.scope ?? ""));
  konduktorNames = new Map(konduktors && konduktors.length > 1 ? konduktors.map((k) => [k.id, k.id === COORDINATOR ? "main konduktor" : `konduktor ${konduktorName(k)}`]) : []);
  const konduktorHead = (
    <KonduktorPick
      dirs={["", ...[...new Set([...projects, ...tabDirs])].sort((a, b) => byName(label(a), label(b)))]}
      shown={panelDir}
      of={konduktorOf}
      label={label}
      tab={away === undefined ? undefined : dir}
      onPick={(d) => setAway(d === dir ? undefined : d)}
    />
  );

  // An object, so that showing the same panel twice scrolls to it again.
  const [focus, setFocus] = useState<{ id: string }>();
  useEffect(() => {
    if (focus) document.getElementById(`panel-${focus.id}`)?.scrollIntoView({ behavior: "smooth", block: "nearest", inline: "nearest" });
  }, [focus]);
  // A session opens in its own directory, unless All is shown or the directory is given.
  const show = (id: string, to?: string) => {
    const cur = placeRef.current;
    const project = sessions.find((c) => c.id === id)?.project;
    const target = to ?? (cur.dir && project ? project : cur.dir);
    const list = target === cur.dir ? cur.panels : (storedPanels()[target] ?? []);
    const slot = list.findIndex((p) => !p.pinned);
    const next = list.some((p) => p.id === id) ? list : slot < 0 ? [...list, { id, pinned: false }] : list.map((p, i) => (i === slot ? { id, pinned: false } : p));
    go({ tab: "Agents", dir: target, panels: next });
    setFocus({ id });
  };

  return (
    <>
      <nav class="tabs">
        <span class="logo">
          <b>k</b>ondu<b>k</b>tor
        </span>
        <div class="dirs" ref={dirsRef}>
          <DirTab name="All" title="Every directory" list={sessions} konduktor={konduktorOf("")} on={isView(tab) && !dir} onPick={() => pick("")} />
          {tabDirs.map((d) => (
            <DirTab key={d} name={label(d)} title={d} mono list={inDir(d)} konduktor={konduktorOf(d)} on={isView(tab) && d === dir} onPick={() => pick(d)} onClose={() => hideTab(d)} />
          ))}
        </div>
        {projects.length > 0 && (
          <MoreDirs
            live={live.filter((d) => !hiddenLive.includes(d)).sort((a, b) => byName(label(a), label(b)))}
            hidden={hiddenLive}
            finished={finished}
            label={label}
            count={(d) => inDir(d)}
            konduktor={konduktorOf}
            onPick={pick}
          />
        )}
        {globals.map((t) => (
          <a class={t === tab ? "tab global on" : "tab global"} onClick={() => go({ tab: t })}>
            {t}
          </a>
        ))}
        <Button
          class={konduktor ? "konduktor-toggle on" : "konduktor-toggle"}
          label="konduktor"
          aria-pressed={konduktor}
          title={konduktor ? "Hide the konduktor panel" : "Show the konduktor panel"}
          onClick={() => {
            localStorage.setItem("kk.conductor", konduktor ? "hidden" : "");
            setKonduktor(!konduktor);
          }}
        />
      </nav>
      {/* The konduktor panel stays mounted on the right of every tab; each tab's view goes before it. */}
      <div class="workspace">
        <div class="view">
          {isView(tab) && (
            <Scope>
              <nav class="views" data-scope="views">
                {views.map((v) => (
                  <Button key={v} label={v} class={v === tab ? "on" : undefined} aria-pressed={v === tab} onClick={() => go({ tab: v })} />
                ))}
                <span class="where mono" title={dir || undefined}>
                  {dir || "Every directory"}
                </span>
              </nav>
            </Scope>
          )}
          <div class="view-body">
            {tab === "Settings" && <SettingsView />}
            {tab === "Inbox" && <Inbox onStart={create} />}
            {tab === "Stats" && <Stats />}
            {tab === "Logs" && <Logs />}
            {tab === "Tmux" && (
              <main class="tmux">
                <Terminal path="/api/tmux" />
              </main>
            )}
            {tab === "Sessions" && (
              <div class="sessions">
                <Scope>
                  <div class="sessions-head">
                    <Button label="New session" icon="plus" class="small primary" onClick={() => create("")} />
                    {filters()}
                  </div>
                </Scope>
                <Board
                  list={shown}
                  all={sessions}
                  sort={sort}
                  empty={empty}
                  projects={!dir}
                  folded={(project, group) => (allClosed((c) => c.project === project && c.group === group) ? !unfolded.includes(`${project}\n${group}`) : undefined)}
                  onFold={(project, group, on) => unfold(`${project}\n${group}`, !on)}
                  onOpen={(id) => show(id)}
                />
              </div>
            )}
            <aside class="sidebar" data-scope="sidebar" hidden={tab !== "Agents"}>
              <Scope>
                <div class="sidebar-head">
                  <Button label="New session" icon="plus" class="primary" onClick={() => create("")} />
                  {filters()}
                </div>
              </Scope>
              <div class="groups">
                {list.length === 0 && <div class="empty">{empty}</div>}
                {dir ? (
                  <>
                    {going.length > 0 && <Tiles list={going} all={sessions} open={open} onOpen={show} />}
                    {gone.length > 0 && (
                      <Group
                        all={sessions}
                        name="Closed"
                        muted
                        list={gone}
                        open={open}
                        collapsed={!unfolded.includes(goneKey)}
                        onCollapse={(on) => unfold(goneKey, !on)}
                        onOpen={show}
                      />
                    )}
                  </>
                ) : (
                  keys.map((key) => {
                    const closed = allClosed((c) => c.project === key);
                    return (
                      <Group
                        key={key}
                        all={sessions}
                        title={key}
                        name={label(key)}
                        mono
                        list={groups.get(key)!}
                        open={open}
                        collapsed={closed ? !unfolded.includes(key) : collapsed.includes(key)}
                        onCollapse={(on) => (closed ? unfold(key, !on) : collapse(key, on))}
                        onOpen={show}
                      />
                    );
                  })
                )}
              </div>
              <Glenz page={`${tab} ${dir} ${konduktor} ${sessions.length} ${open.filter((id) => details[id]).join()}`} />
              <Load />
            </aside>
            <div class="panels" hidden={tab !== "Agents"}>
              {open.length === 0 && <main class="conversation placeholder">Pick a session on the left or start a new one.</main>}
              {panels.map(({ id, pinned }) =>
                details[id] ? (
                  <ConversationView
                    key={id}
                    c={details[id]}
                    before={sessions.find((x) => x.id === details[id].after)}
                    onShow={show}
                    reload={() => loadDetail(id)}
                    onHide={() => hide(id)}
                    pinned={pinned}
                    onPin={(on) => pin(id, on)}
                  />
                ) : (
                  <main key={id} id={`panel-${id}`} class="conversation placeholder">
                    Loading…
                  </main>
                ),
              )}
            </div>
          </div>
        </div>
        <CoordinatorPanel hidden={!konduktor}>
          {konduktors === undefined ? null : !konduktors.length ? (
            <div class="conversation placeholder">konduktor is not running.</div>
          ) : shownKonduktor ? (
            details[shownKonduktor.id] ? (
              <ConversationView c={details[shownKonduktor.id]} reload={() => loadDetail(shownKonduktor.id)} side head={konduktorHead} />
            ) : (
              <div class="conversation placeholder">Loading…</div>
            )
          ) : (
            <KonduktorStart
              key={panelDir}
              dir={panelDir}
              name={label(panelDir)}
              head={konduktorHead}
              onStarted={(k) => {
                loadList();
                loadDetail(k.id);
              }}
            />
          )}
        </CoordinatorPanel>
      </div>
      {creating !== undefined && (
        <NewSession
          initial={creating}
          dir={dir}
          onClose={() => setCreating(undefined)}
          onCreated={(c) => {
            created.current?.();
            loadList();
            setCreating(undefined);
            show(c.id, c.project);
          }}
        />
      )}
      <ImageViewer />
    </>
  );
}

// What a directory asks of the user: attention when one of its sessions needs them, unread when it or its konduktor wrote
// something they have not read.
function dirMark(list: Conversation[], konduktor?: Conversation) {
  if (list.some((c) => !c.closed && c.attention && c.waiting_for !== "conductor")) return "attention";
  return list.some((c) => c.unread) || konduktor?.unread ? "unread" : "";
}

type DirTabProps = { name: string; title: string; mono?: boolean; list: Conversation[]; konduktor?: Conversation; on: boolean; onPick: () => void; onClose?: () => void };

// The tooltip of a directory tab names the state of its loudest session, like a collapsed group.
function DirTab({ name, title, mono, list, konduktor, on, onPick, onClose }: DirTabProps) {
  const loud = loudest(list);
  const cls = ["tab", "dir", on && "on", dirMark(list, konduktor)];
  return (
    <div class={cls.filter(Boolean).join(" ")} title={loud ? `${title}\n${stateLabel(loud)}` : title} onClick={onPick}>
      <span class={mono ? "mono" : undefined}>{name}</span>
      {onClose && (
        <button
          type="button"
          class="tab-close"
          aria-label={`Hide the tab of ${name}`}
          title="Hide this tab, it stays in More and its sessions keep running"
          onClick={(e) => {
            e.stopPropagation();
            onClose();
          }}
        >
          <Icon name="close" />
        </button>
      )}
    </div>
  );
}

type MoreProps = {
  live: string[];
  hidden: string[];
  finished: string[];
  label: (dir: string) => string;
  count: (dir: string) => Conversation[];
  konduktor: (dir: string) => Conversation | undefined;
  onPick: (dir: string) => void;
};

// More lists every directory, so one whose tab is scrolled away or that has only closed sessions is a click away.
function MoreDirs({ live, hidden, finished, label, count, konduktor, onPick }: MoreProps) {
  const [at, setAt] = useState<{ top: number; left: number }>();
  const ref = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (!at) return;
    const close = (e: globalThis.Event) => {
      if (e instanceof KeyboardEvent ? e.key === "Escape" : !ref.current?.contains(e.target as Node)) setAt(undefined);
    };
    document.addEventListener("pointerdown", close);
    document.addEventListener("keydown", close);
    window.addEventListener("resize", close);
    return () => {
      document.removeEventListener("pointerdown", close);
      document.removeEventListener("keydown", close);
      window.removeEventListener("resize", close);
    };
  }, [at]);
  const item = (d: string, closed: boolean) => {
    const list = count(d);
    return (
      <button
        type="button"
        key={d}
        class={["dir-item", hidden.includes(d) && dirMark(list, konduktor(d))].filter(Boolean).join(" ")}
        title={d}
        onClick={() => {
          setAt(undefined);
          onPick(d);
        }}
      >
        <span class="mono">{label(d)}</span>
        <span class="count">{closed ? list.length : list.filter((c) => !c.closed).length}</span>
      </button>
    );
  };
  const marks = hidden.map((d) => dirMark(count(d), konduktor(d)));
  const mark = marks.includes("attention") ? "attention" : marks.includes("unread") ? "unread" : "";
  return (
    <div class="more" ref={ref}>
      <a
        class={["tab", "more-tab", at && "on", mark].filter(Boolean).join(" ")}
        title={mark === "attention" ? "A directory with a hidden tab needs you" : undefined}
        aria-expanded={!!at}
        onClick={(e) => {
          const r = e.currentTarget.getBoundingClientRect();
          setAt(at ? undefined : { top: r.bottom, left: Math.max(0, Math.min(r.left, window.innerWidth - 320)) });
        }}
      >
        More
        <i class="icon icon-chevron" />
      </a>
      {at && (
        <div class="dir-menu" style={{ top: `${at.top}px`, left: `${at.left}px` }}>
          {live.map((d) => item(d, false))}
          {hidden.length > 0 && <div class="dir-menu-label">Hidden tabs</div>}
          {hidden.map((d) => item(d, false))}
          {finished.length > 0 && <div class="dir-menu-label">Only closed sessions</div>}
          {finished.map((d) => item(d, true))}
        </div>
      )}
    </div>
  );
}

// The names of the konduktors, set while more than one runs, so a session says whose turn it is.
let konduktorNames = new Map<string, string>();

function stateLabel(c: Conversation) {
  if (c.closed) return "Closed";
  if (c.sleeping) return "Sleeping";
  if (c.status === "queued") return "Queued";
  if (c.waiting_for === "conductor") {
    const turn = `${(c.konduktor && konduktorNames.get(c.konduktor)) || "konduktor"}'s turn`;
    return c.status === "exited" ? `Exited · ${turn}` : turn;
  }
  if (c.attention) return "Action required";
  if (c.waiting_for === "user") return c.status === "exited" ? "Exited · your turn" : "Your turn";
  if (c.integration && c.status === "idle") return "Idle";
  return { starting: "Starting", working: "Working", background: "Background working", idle: "Your turn", exited: "Exited", queued: "Queued" }[c.status];
}

function Dot({ c }: { c: Conversation }) {
  return <i class={`dot ${c.attention ? "attention" : c.sleeping ? "sleeping" : c.status} ${c.waiting_for} ${c.integration ? "integration" : ""}`} />;
}

function attention(c: Conversation) {
  if (c.attention) return c.waiting_for === "conductor" ? "warn" : "attention";
  return c.run_done ? "called" : "";
}

// A collapsed group speaks for the sessions it hides: one that needs the user first, then one at work, then one konduktor holds.
function loudness(c: Conversation) {
  if (c.waiting_for === "user") return c.attention || c.run_done ? 0 : 1;
  if (c.status === "working" || c.status === "starting" || c.status === "background") return 2;
  return c.waiting_for === "conductor" ? 3 : 4;
}

// The session a collapsed group or a directory tab shows the state of.
const loudest = (list: Conversation[]) => list.filter((c) => !c.closed && loudness(c) < 4).sort((a, b) => loudness(a) - loudness(b))[0];

type GroupProps = {
  all: Conversation[];
  title?: string;
  name: string;
  mono?: boolean;
  muted?: boolean;
  list: Conversation[];
  open: string[];
  collapsed: boolean;
  onCollapse: (on: boolean) => void;
  onOpen: (id: string) => void;
};

function Group({ all, title, name, mono, muted, list, open, collapsed, onCollapse, onOpen }: GroupProps) {
  const loud = loudest(list);
  const unread = list.some((c) => c.unread && !open.includes(c.id));
  const cls = ["group", collapsed && loud && attention(loud), collapsed && unread && "unread"];
  return (
    <details class={cls.filter(Boolean).join(" ")} open={!collapsed} onToggle={(e) => e.currentTarget.open === collapsed && onCollapse(!collapsed)}>
      <summary title={title}>
        <Icon name="chevron" />
        <span class={["name", mono && "mono", muted && "muted"].filter(Boolean).join(" ")}>{name}</span>
        {collapsed && loud && (
          <span class={`state ${loud.waiting_for}`} title={loud.escalation || loud.attention || undefined}>
            <Dot c={loud} />
            {stateLabel(loud)}
          </span>
        )}
        <span class="count">{list.length}</span>
      </summary>
      <Tiles list={list} all={all} open={open} onOpen={onOpen} />
    </details>
  );
}

function Tiles({ list, all, open, onOpen }: { list: Conversation[]; all: Conversation[]; open: string[]; onOpen: (id: string) => void }) {
  const chains = arrange(list, all);
  return (
    <ChainLinks chains={chains} hot={(id) => open.includes(id)}>
      <ul class="tiles">
        {chains.order.map((c) => (
          <Tile key={c.id} c={c} chains={chains} on={open.includes(c.id)} onOpen={() => onOpen(c.id)} />
        ))}
      </ul>
    </ChainLinks>
  );
}

function Tile({ c, chains, on, onOpen }: { c: Conversation; chains: Chains; on: boolean; onOpen: () => void }) {
  const cls = ["tile", on && "on", attention(c) || (["working", "background", "queued"].includes(c.status) && c.status), c.unread && !on && "unread", c.closed ? "closed" : c.status === "exited" && "exited"];
  return (
    <li class={cls.filter(Boolean).join(" ")} data-chain={c.id} onClick={onOpen} title={c.attention || c.escalation || undefined}>
      <CardTitle c={c} />
      <ChainRefs chains={chains} id={c.id} />
      {c.escalation && <div class="reason">{c.escalation}</div>}
      <div class="meta">
        <span>
          {harnessLabel(c.harness)} · {c.model || "default"}
        </span>
        <span class={`state ${c.waiting_for}`}>
          <Dot c={c} />
          {stateLabel(c)}
        </span>
      </div>
      <div class="meta">
        {c.status === "queued" ? (
          <span>Not started</span>
        ) : (
          <>
            <span title={usageTitle(c.usage, c.harness)}>{usageLabel(c.usage)}</span>
            <span>Last {when(c.last_message)}</span>
          </>
        )}
      </div>
    </li>
  );
}

function Icon({ name }: { name: string }) {
  return <i class={`icon icon-${name}`} />;
}

const TOOL_ICONS: [RegExp, string][] = [
  [/^mcp(__|[._:-])/i, "mcp"],
  [/bash|shell|exec|command|terminal/i, "terminal"],
  [/edit|write|patch|notebook/i, "edit"],
  [/grep|glob|search|find|ls\b|list_dir/i, "search"],
  [/read|file|view/i, "file"],
  [/web|fetch|browser|url/i, "globe"],
  [/task|agent/i, "agent"],
  [/todo|plan/i, "list"],
];

function toolIcon(tool = "") {
  return TOOL_ICONS.find(([re]) => re.test(tool))?.[1] ?? "tool";
}

// Agents write whole messages to their transcripts, so a message that arrives while the page is open is revealed
// progressively instead of appearing at once.
function Reveal({ text, images }: { text: string; images?: Attachment[] }) {
  const still = window.matchMedia("(prefers-reduced-motion: reduce)").matches;
  const [shown, setShown] = useState(still ? text.length : 0);
  useEffect(() => {
    if (shown >= text.length) return;
    const t = setTimeout(() => setShown((n) => n + Math.max(6, Math.ceil(text.length / 60))), 16);
    return () => clearTimeout(t);
  }, [shown, text]);
  return shown >= text.length ? <Markdown text={text} images={images} /> : <Markdown text={text.slice(0, shown)} images={images} partial />;
}

function Feed({ c, live, rewind, retry }: { c: Conversation; live: number; rewind?: Rewind; retry?: () => void }) {
  const events = c.events;
  const running = c.status === "working";
  const out: preact.JSX.Element[] = [];
  let steps: [Event, number][] = [];
  const flush = (key: number) => {
    if (steps.length) {
      const [last, at] = steps[steps.length - 1];
      const active = running && at === events.length - 1 && !last.output && !last.failed;
      out.push(
        <details class="steps" key={`s${key}`}>
          <summary>
            {steps.length === 1 ? "1 step" : `${steps.length} steps`}
            <Icon name="chevron" />
          </summary>
          {steps.map(([e, i]) => (
            <Step key={i} e={e} running={active && i === at} />
          ))}
        </details>,
      );
    }
    steps = [];
  };
  events.forEach((e, i) => {
    if (e.kind === "tool" || e.kind === "thinking") {
      steps.push([e, i]);
      return;
    }
    flush(i);
    if (e.kind === "error")
      out.push(
        <div class="error" key={i}>
          {retry && i === events.length - 1 && <Button label="Retry" icon="reset" class="small primary" title={`Send to the agent: ${RETRY}`} onClick={retry} />}
          <Icon name="alert" />
          {e.text}
        </div>,
      );
    else if (e.kind === "note") out.push(<div class="note" key={i}>{e.text}</div>);
    else if (e.kind === "user")
      out.push(
        // Claude Code lists only real prompts in its rewind menu; those carry their transcript id.
        <UserMessage key={i} id={c.id} index={i} event={e} rewind={c.harness !== "claude" || e.id ? rewind : undefined}>
          {e.text && <Markdown text={e.text} images={e.images} />}
          <Attachments items={e.attachments ?? []} />
        </UserMessage>,
      );
    else
      out.push(
        <div class={`msg ${e.kind}`} key={i} title={when(e.time)} data-event={i}>
          {e.kind === "assistant" && i >= live ? <Reveal text={e.text ?? ""} images={e.images} /> : e.text && <Markdown text={e.text} images={e.images} />}
          <Attachments items={e.attachments ?? []} />
        </div>,
      );
  });
  flush(events.length);
  return <>{out}</>;
}

function Step({ e, running }: { e: Event; running: boolean }) {
  if (e.kind === "thinking") {
    return (
      <details class="tool">
        <summary>
          <Icon name="think" />
          <b>Thinking</b>
          <span class="input">{firstWords(e.text ?? "")}</span>
          <Icon name="chevron" />
        </summary>
        <Markdown text={e.text ?? ""} />
      </details>
    );
  }
  return (
    <details class={e.failed ? "tool failed" : "tool"}>
      <summary>
        <Icon name={toolIcon(e.tool)} />
        <b>{e.tool}</b>
        <span class="input mono">{e.input}</span>
        {running && <i class="spinner" />}
        {e.failed && <Icon name="alert" />}
        <Icon name="chevron" />
      </summary>
      {e.detail && <pre>{e.detail}</pre>}
      {e.output && <pre>{e.output}</pre>}
    </details>
  );
}

function firstWords(s: string) {
  const line = s.trim().split("\n")[0];
  return line.length > 120 ? line.slice(0, 119) + "…" : line;
}

const KEYS: [string, string][] = [
  ["Enter", "Enter"],
  ["Esc", "Escape"],
  ["↑", "Up"],
  ["↓", "Down"],
  ["Tab", "Tab"],
  ["1", "1"],
  ["2", "2"],
  ["3", "3"],
  ["Ctrl-C", "C-c"],
];

function useScreen(id: string, active: boolean) {
  const [screen, setScreen] = useState("");
  useEffect(() => {
    if (!active) return;
    let stop = false;
    const poll = async () => {
      try {
        const r = await api<{ screen: string }>("GET", `/api/conversations/${id}/screen`);
        if (!stop) setScreen(r.screen.replace(/\s+$/, ""));
      } catch {}
    };
    poll();
    const t = setInterval(poll, 1000);
    return () => {
      stop = true;
      clearInterval(t);
    };
  }, [id, active]);
  return screen;
}

function Keys({ id }: { id: string }) {
  return (
    <div class="keys">
      {KEYS.map(([label, key]) => (
        <button class="small" key={key} onClick={() => act(() => api("POST", `/api/conversations/${id}/keys`, { keys: [key] }))}>
          {label}
        </button>
      ))}
    </div>
  );
}

const QUICK_REPLIES: [string, string][] = [
  ["Ack", "Acknowledged, I agree with this. Go ahead."],
  ["Explain", "Explain this part: what it means, why it is so, and what follows from it."],
  ["Out-of-scope", "This is out of scope. Drop it and do not work on it."],
];

// Resending the prompt would start a turn over that was cut after many steps, so a retry asks the agent to go on.
const RETRY = "The last turn was cut off by an API error. Continue from where you stopped.";

type Pending = { text: string; files: Attachment[]; expect: number };

type ConversationViewProps = {
  c: Conversation;
  reload: () => void;
  side?: boolean;
  onHide?: () => void;
  pinned?: boolean;
  onPin?: (on: boolean) => void;
  // The session this one follows in a chain.
  before?: Conversation;
  onShow?: (id: string) => void;
  // What the konduktor panel shows in place of the title.
  head?: preact.ComponentChildren;
};

function ConversationView({ c, reload, side, onHide, pinned, onPin, before, onShow, head }: ConversationViewProps) {
  const [chosen, setView] = useState<"chat" | "terminal" | "media">("chat");
  // Closing kills the window, so a closed session has no terminal to show.
  const view = chosen === "terminal" && c.closed ? "chat" : chosen;
  const images = useMemo(() => media(c.events), [c.events]);
  const [confirmClose, setConfirmClose] = useState(false);
  const [scheduling, setScheduling] = useState(false);
  const [pending, setPending] = useState<Pending[]>([]);
  const screen = useScreen(c.id, !!c.attention);
  const feed = useRef<HTMLDivElement>(null);
  const inner = useRef<HTMLDivElement>(null);
  const stick = useRef(true);
  const live = useRef(c.events.length);
  live.current = Math.min(live.current, c.events.length);
  const rewind = useRewind(c);
  const composer = useRef<((text: string) => void) | undefined>(undefined);
  const [quote, setQuote] = useState<{ text: string; x: number; y: number }>();

  useEffect(() => {
    const onSelect = () => {
      const sel = getSelection();
      const text = sel?.toString().trim() ?? "";
      const node = sel?.anchorNode;
      const el = (node instanceof Element ? node : node?.parentElement)?.closest<HTMLElement>(".msg.assistant");
      if (!sel || sel.isCollapsed || !text || !el || !feed.current?.contains(el)) return setQuote(undefined);
      const e = c.events[Number(el.dataset.event)];
      const rect = sel.getRangeAt(0).getBoundingClientRect();
      const box = feed.current.getBoundingClientRect();
      if (rect.bottom < box.top || rect.top > box.bottom) return setQuote(undefined);
      const lines = text.split("\n").map((l) => `> ${l}`).join("\n");
      const source = `(quoted from your message of ${when(e.time)}${e.id ? `, transcript id ${e.id}` : ""})`;
      setQuote({ text: `${lines}\n${source}\n\n`, x: Math.max(4, Math.min(rect.right, window.innerWidth) - 290), y: Math.max(rect.top - 30, box.top) });
    };
    const el = feed.current;
    document.addEventListener("selectionchange", onSelect);
    el?.addEventListener("scroll", onSelect);
    return () => {
      document.removeEventListener("selectionchange", onSelect);
      el?.removeEventListener("scroll", onSelect);
    };
  }, [c.events]);

  useEffect(() => {
    const observer = new ResizeObserver(() => {
      if (stick.current && feed.current) feed.current.scrollTop = feed.current.scrollHeight;
    });
    observer.observe(inner.current!);
    return () => observer.disconnect();
  }, []);

  useEffect(() => {
    if (!confirmClose) return;
    const t = setTimeout(() => setConfirmClose(false), 3000);
    return () => clearTimeout(t);
  }, [confirmClose]);

  const exited = c.status === "exited";
  const queued = c.status === "queued" && !c.closed;
  const users = c.events.filter((e) => e.kind === "user").length + c.queued.length;
  const working = useRef(false);
  working.current = c.status === "working";
  const sending = pending.filter((p) => users < p.expect);
  // A rewind lowers the count again, which must not bring back messages that already arrived.
  if (sending.length < pending.length) setPending(sending);
  const busy = !exited && !c.attention && c.status !== "queued" && (c.status !== "idle" || sending.length > 0);
  const last = c.events[c.events.length - 1];

  const send = async (text: string, files: Attachment[]) => {
    const p = { text, files, expect: users + sending.length + 1 };
    const drop = () => setPending((q) => q.filter((x) => x !== p));
    setPending([...sending, p]);
    // A harness that does not record its queue shows the message only when the agent takes it, after its turn at the latest.
    const expire = () => (working.current ? setTimeout(expire, 30000) : drop());
    setTimeout(expire, 30000);
    try {
      await api("POST", `/api/conversations/${c.id}/send`, { text: withPaths(text, files) });
      return true;
    } catch (e) {
      drop();
      flash((e as Error).message);
      return false;
    }
  };
  const resume = () => act(async () => (await api("POST", `/api/conversations/${c.id}/resume`, {}), reload()));
  const close = () => {
    if (!confirmClose) return setConfirmClose(true);
    act(async () => {
      await api("POST", `/api/conversations/${c.id}/close`, {});
      onHide?.();
    });
  };
  const Tag = side ? "div" : "main";
  return (
    <Tag class={onPin && !pinned ? "conversation unpinned" : "conversation"} id={`panel-${c.id}`} data-scope={side ? "coordinator" : `conversation-${c.id}`}>
      <Scope>
        <header class="conv-head">
          {onPin && (
            <button
              type="button"
              class={pinned ? "pin on" : "pin"}
              aria-label={pinned ? "Unpin" : "Pin"}
              title={pinned ? "Unpin: the next session you open replaces this panel" : "Pin: keep this panel open when you open another session"}
              onClick={() => onPin(!pinned)}
            >
              <Icon name="pin" />
            </button>
          )}
          {head ?? <h1>{side ? "konduktor" : c.title || "Untitled"}</h1>}
          <div class="actions">
            {!queued && (
              <div class="modes">
                <Button label="Chat" class={view === "chat" ? "small on" : "small"} onClick={() => setView("chat")} />
                {!c.closed && <Button label="Terminal" icon="terminal" class={view === "terminal" ? "small on" : "small"} onClick={() => setView("terminal")} />}
                <Button
                  label="Media"
                  icon="image"
                  class={view === "media" ? "small on" : "small"}
                  data-count={images.length || undefined}
                  title={images.length ? `${images.length} ${images.length === 1 ? "image" : "images"}` : "No images"}
                  onClick={() => setView("media")}
                />
              </div>
            )}
            {(exited || c.closed) && !(c.integration && c.closed) && <Button label={c.closed ? "Reopen" : "Resume"} icon="play" class="small primary" onClick={resume} />}
            <Controls c={c} reload={reload} />
            {!side && !c.closed && !queued && !c.integration && (
              <Button label="Schedule" icon="clock" class={c.schedule ? "small on" : "small"} title="Resume the session with a prompt on set days and times" onClick={() => setScheduling(true)} />
            )}
            {(!side || c.id !== COORDINATOR) && !c.closed && !c.integration && (
              <Button
                label={confirmClose ? "Confirm close" : "Close"}
                icon="close"
                class="small"
                title={side ? "Stop this konduktor, your next message to it starts it again" : queued ? "Drop the queued session, it moves to the closed ones" : "Stop the agent and move the session to the closed ones"}
                onClick={close}
              />
            )}
            {onHide && <Button label="Hide" icon="hide" class="small" title="Hide this panel, the agent keeps running" onClick={onHide} />}
          </div>
          <div class="meta">
            <span class={`status ${attention(c)} ${c.waiting_for}`}>
              <Dot c={c} />
              {stateLabel(c)}
            </span>
            <span>{harnessLabel(c.harness)} · {c.model || "default"}</span>
            {c.schedule && (
              <span class="schedule-meta" title={c.schedule.prompt}>
                <Icon name="clock" />
                {scheduleText(c)}
                {c.next_run && ` · next ${when(c.next_run)}`}
              </span>
            )}
            {c.context_tokens > 0 && <span>{Math.round(c.context_tokens / 1000)}k context</span>}
            {!side && (
              <span class="dir mono" title={c.dir}>
                <Icon name="folder" />
                <span class="path">
                  <bdi>{c.dir}</bdi>
                </span>
              </span>
            )}
          </div>
        </header>
        {view === "terminal" && <Terminal path={`/api/conversations/${c.id}/terminal`} />}
        {view === "media" && <Media items={images} />}
        <div
          hidden={view !== "chat"}
          class="feed"
          ref={feed}
          onScroll={(e) => {
            const el = e.currentTarget;
            stick.current = el.scrollHeight - el.scrollTop - el.clientHeight < 40;
          }}
        >
          <div class="feed-inner" ref={inner}>
            {queued && <Queued c={c} before={before} onShow={onShow} reload={reload} />}
            {c.events.length === 0 && !c.attention && !busy && !queued && <div class="empty">No messages yet.</div>}
            <Feed c={c} live={live.current} rewind={rewind} retry={c.status === "idle" && !sending.length && !c.integration ? () => send(RETRY, []) : undefined} />
            {c.queued.map((e, i) => (
              <div class="msg user queued" key={`q${i}`} title="The agent reads this message at its next step">
                {e.text && <Markdown text={e.text} />}
                <Attachments items={e.attachments ?? []} />
                <span class="queued-note">Queued</span>
              </div>
            ))}
            {sending.map((p, i) => (
              <div class="msg user sending" key={`p${i}`}>
                {p.text.trim() && <Markdown text={p.text} />}
                <Attachments items={p.files} />
              </div>
            ))}
            {busy && (
              <div class="busy">
                <i class="pulse" />
                <span>{c.status === "starting" ? "Starting the agent" : c.status === "background" ? "Background working" : last?.kind === "tool" && !last.output ? `Running ${last.tool}` : "Working"}</span>
              </div>
            )}
            {c.escalation && (
              <div class="box-warn">
                <p>
                  <Icon name="alert" />
                  konduktor hands this to you: {c.escalation}
                </p>
              </div>
            )}
            {c.attention && (
              <div class="box-warn">
                <p>
                  <Icon name="alert" />
                  {c.attention}
                </p>
                <Keys id={c.id} />
                {screen && <pre>{screen}</pre>}
              </div>
            )}
          </div>
        </div>
        {quote && view === "chat" && !exited && (
          <div class="quote-bar" style={{ left: `${quote.x}px`, top: `${quote.y}px` }} onMouseDown={(e) => e.preventDefault()}>
            <Button
              label="Quote"
              icon="quote"
              class="small"
              title="Put the quote in the reply box"
              onClick={() => {
                composer.current?.(quote.text);
                getSelection()?.removeAllRanges();
              }}
            />
            {QUICK_REPLIES.map(([label, reply]) => (
              <Button
                key={label}
                label={label}
                class="small"
                title={`Add to the reply box: ${reply}`}
                onClick={() => {
                  composer.current?.(`${quote.text}${reply}\n\n`);
                  getSelection()?.removeAllRanges();
                }}
              />
            ))}
          </div>
        )}
        {view === "chat" && !queued && <Composer c={c} exited={exited || !!c.closed} onResume={resume} onSend={send} insert={composer} />}
        {scheduling && <ScheduleDialog c={c} onClose={() => setScheduling(false)} />}
      </Scope>
    </Tag>
  );
}

// Queued shows the prepared prompt of a session that waits for its start, for the user to edit or start it.
function Queued({ c, before, onShow, reload }: { c: Conversation; before?: Conversation; onShow?: (id: string) => void; reload: () => void }) {
  const saved = c.prompt ?? "";
  const [text, setText] = useState(saved);
  const [base, setBase] = useState(saved);
  const [busy, setBusy] = useState(false);
  const area = useRef<HTMLTextAreaElement>(null);
  const skills = useSkills(c.harness, c.dir);
  const attachments = useAttachments();
  // A prompt changed elsewhere replaces the one shown, unless the user has edited it here.
  if (saved !== base) {
    setBase(saved);
    if (text === base) setText(saved);
  }
  const prompt = withPaths(text, attachments.items as Attachment[]);
  const dirty = prompt !== saved;
  const empty = !prompt.trim();
  useLayoutEffect(() => {
    const el = area.current;
    if (!el) return;
    el.style.height = "auto";
    el.style.height = `${Math.min(el.scrollHeight + 2, 420)}px`;
  }, [text]);
  const run = (start: boolean) => {
    if (busy || empty || attachments.uploading) return;
    setBusy(true);
    act(async () => {
      try {
        if (dirty) {
          await api("POST", `/api/conversations/${c.id}/prompt`, { prompt });
          setText(prompt);
          attachments.setItems([]);
        }
        if (start) await api("POST", `/api/conversations/${c.id}/start`, {});
        reload();
      } finally {
        setBusy(false);
      }
    });
  };
  return (
    <section class="queued-card">
      <header>
        <Icon name="chain" />
        {before ? (
          <span class="after">
            Queued after{" "}
            <button type="button" class="link" title="Open that session" onClick={() => onShow?.(before.id)}>
              {before.title || "Untitled"}
            </button>
            <span class={`state ${before.waiting_for}`}>
              <Dot c={before} />
              {stateLabel(before)}
            </span>
          </span>
        ) : (
          <span class="after">Queued</span>
        )}
      </header>
      <p class="hint">
        Nothing starts by itself. When the session before it has finished, konduktor checks the work and starts this one with a handoff added to the prompt. You can
        start it yourself any time.
      </p>
      <Field label="Prepared prompt">
        <PromptInput
          inputRef={area}
          value={text}
          onValue={setText}
          skills={skills}
          attachments={attachments}
          onEnter={(e) => {
            if (e.ctrlKey || e.metaKey) {
              e.preventDefault();
              run(false);
            }
          }}
        />
      </Field>
      <div class="row">
        {dirty && <Button label="Revert" class="small" title="Go back to the saved prompt" onClick={() => (setText(saved), attachments.setItems([]))} />}
        <Button label="Attach" icon="file" class="small" title="Attach files, or paste or drop them" onClick={attachments.pick} />
        <Button label="Save prompt" class="small" disabled={!dirty || empty || busy || attachments.uploading} onClick={() => run(false)} />
        <Button label="Start" icon="play" class="small primary" title="Start the agent with this prompt now" disabled={empty || busy || attachments.uploading} onClick={() => run(true)} />
      </div>
    </section>
  );
}

type ComposerProps = {
  c: Conversation;
  exited: boolean;
  onResume: () => void;
  onSend: (text: string, files: Attachment[]) => Promise<boolean>;
  insert: preact.RefObject<((text: string) => void) | undefined>;
};

function Composer({ c, exited, onResume, onSend, insert }: ComposerProps) {
  const [text, setText] = useState("");
  const area = useRef<HTMLTextAreaElement>(null);
  const skills = useSkills(c.harness, c.dir);
  const attachments = useAttachments();
  insert.current = (quoted) => {
    setText((cur) => (cur.trim() ? cur.replace(/\n*$/, "\n\n") : "") + quoted);
    requestAnimationFrame(() => {
      area.current?.focus();
      area.current?.setSelectionRange(area.current.value.length, area.current.value.length);
    });
  };
  useLayoutEffect(() => {
    const el = area.current;
    if (!el) return;
    el.style.height = "auto";
    el.style.height = `${el.scrollHeight + 2}px`;
  }, [text]);
  const send = async () => {
    const body = text;
    const files = attachments.items;
    if (attachments.uploading || (!body.trim() && !files.length)) return;
    setText("");
    attachments.setItems([]);
    if (!(await onSend(body, files as Attachment[]))) {
      setText((cur) => cur || body);
      attachments.setItems((cur) => [...files, ...cur]);
    }
  };
  if (c.integration && c.closed) {
    return (
      <div class="composer">
        <span class="muted">This source is switched off in Settings. Once it is on again, its next check reopens the session.</span>
      </div>
    );
  }
  if (exited) {
    return (
      <div class="composer">
        <span class="muted" style="flex:1;align-self:center">{c.closed ? "The session is closed." : c.sleeping ? "The agent sleeps after a long time without activity." : "The agent has exited."}</span>
        <Button label={c.closed ? "Reopen session" : "Resume session"} icon="play" class="primary" onClick={onResume} />
      </div>
    );
  }
  return (
    <div class="composer">
      <PromptInput
        inputRef={area}
        rows={1}
        up
        value={text}
        onValue={setText}
        skills={skills}
        attachments={attachments}
        placeholder={c.role === COORDINATOR ? "Message konduktor" : c.integration ? "Message the agent, it can only read" : "Reply to the agent, / for skills"}
        onEnter={(e) => {
          if (!e.shiftKey && !e.isComposing) {
            e.preventDefault();
            send();
          }
        }}
      />
      {!c.integration && <Button label="Attach" icon="file" title="Attach files, or paste or drop them" onClick={attachments.pick} />}
      <Button label="Send" icon="send" class="primary" disabled={attachments.uploading} onClick={send} />
    </div>
  );
}

function CoordinatorPanel({ hidden, children }: { hidden: boolean; children: preact.ComponentChildren }) {
  const [width, setWidth] = useState(() => Number(localStorage.getItem("kk.panel")) || 440);
  const [dragging, setDragging] = useState(false);
  useEffect(() => {
    if (!dragging) return;
    const move = (e: PointerEvent) => setWidth(Math.max(280, Math.min(window.innerWidth * 0.6, window.innerWidth - e.clientX)));
    const up = () => setDragging(false);
    window.addEventListener("pointermove", move);
    window.addEventListener("pointerup", up);
    return () => {
      window.removeEventListener("pointermove", move);
      window.removeEventListener("pointerup", up);
      localStorage.setItem("kk.panel", String(width));
    };
  }, [dragging, width]);
  return (
    <aside class="side-panel" hidden={hidden} style={{ "--panel-width": `${width}px` }}>
      <div class={dragging ? "panel-resize dragging" : "panel-resize"} onPointerDown={() => setDragging(true)} />
      {children}
    </aside>
  );
}

type PickProps = {
  dirs: string[];
  shown: string;
  of: (dir: string) => Conversation | undefined;
  label: (dir: string) => string;
  tab?: string;
  onPick: (dir: string) => void;
};

// The head of the konduktor panel: a menu of All and every directory, and while the menu's choice differs from the tab,
// the way back to the tab's directory.
function KonduktorPick({ dirs, shown, of, label, tab, onPick }: PickProps) {
  const [at, setAt] = useState<{ top: number; left: number }>();
  const ref = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (!at) return;
    const close = (e: globalThis.Event) => {
      if (e instanceof KeyboardEvent ? e.key === "Escape" : !ref.current?.contains(e.target as Node)) setAt(undefined);
    };
    document.addEventListener("pointerdown", close);
    document.addEventListener("keydown", close);
    return () => {
      document.removeEventListener("pointerdown", close);
      document.removeEventListener("keydown", close);
    };
  }, [at]);
  const k = of(shown);
  const name = (d: string) => (d ? label(d) : "All");
  return (
    <div class="konduktor-pick" ref={ref}>
      <h1>
        <button
          type="button"
          class="konduktor-name small"
          aria-expanded={!!at}
          title={`${shown || "Every directory without its own konduktor"}\n${k ? stateLabel(k) : "not started"}`}
          onClick={(e) => {
            const r = e.currentTarget.getBoundingClientRect();
            setAt(at ? undefined : { top: r.bottom + 2, left: Math.max(0, Math.min(r.left, window.innerWidth - 320)) });
          }}
        >
          <span class="mono">{name(shown)}</span>
          <Icon name="chevron" />
        </button>
        {!k && <span class="muted">not started</span>}
      </h1>
      {tab !== undefined && (
        <div class="konduktor-away">
          <span>
            Showing <span class="mono">{name(shown)}</span>, not <span class="mono">{name(tab)}</span>.
          </span>
          <Button label={`Back to ${name(tab)}`} class="small" onClick={() => onPick(tab)} />
        </div>
      )}
      {at && (
        <div class="dir-menu" style={{ top: `${at.top}px`, left: `${at.left}px` }}>
          {dirs.map((d) => {
            const k = of(d);
            return (
              <button
                type="button"
                key={d}
                class={["dir-item", k?.unread && "unread", d === shown && "on"].filter(Boolean).join(" ")}
                title={d || "Every directory without its own konduktor"}
                onClick={() => {
                  setAt(undefined);
                  onPick(d);
                }}
              >
                <span>
                  {k ? <Dot c={k} /> : <i class="dot none" />}
                  <span class="mono">{name(d)}</span>
                </span>
                <span class="count">{k ? stateLabel(k) : "not started"}</span>
              </button>
            );
          })}
        </div>
      )}
    </div>
  );
}

// The panel of a directory whose konduktor has not started: the first message starts it.
function KonduktorStart({ dir, name, head, onStarted }: { dir: string; name: string; head: preact.ComponentChildren; onStarted: (k: Conversation) => void }) {
  const [text, setText] = useState("");
  const [busy, setBusy] = useState(false);
  const skills = useSkills("claude", dir);
  const attachments = useAttachments();
  const send = () => {
    if (busy || attachments.uploading || (!text.trim() && !attachments.items.length)) return;
    setBusy(true);
    act(async () => {
      try {
        const k = await api<Conversation>("POST", "/api/konduktors", { dir, prompt: withPaths(text, attachments.items as Attachment[]) });
        setText("");
        attachments.setItems([]);
        onStarted(k);
      } finally {
        setBusy(false);
      }
    });
  };
  return (
    <div class="conversation" data-scope="coordinator">
      <Scope>
        <header class="conv-head">{head}</header>
        <div class="feed">
          <p class="konduktor-hint">The konduktor of {name} starts with your first message.</p>
        </div>
        <div class="composer">
          <PromptInput
            rows={1}
            up
            value={text}
            onValue={setText}
            skills={skills}
            attachments={attachments}
            placeholder="Message konduktor"
            onEnter={(e) => {
              if (!e.shiftKey && !e.isComposing) {
                e.preventDefault();
                send();
              }
            }}
          />
          <Button label="Attach" icon="file" title="Attach files, or paste or drop them" onClick={attachments.pick} />
          <Button label="Send" icon="send" class="primary" disabled={busy || attachments.uploading} onClick={send} />
        </div>
      </Scope>
    </div>
  );
}

// span is how long a session ran, from its first message to its last.
function span(from: string, to: string): string {
  const minutes = Math.round((new Date(to).getTime() - new Date(from).getTime()) / 60000);
  if (!(minutes >= 1)) return "under 1 min";
  if (minutes < 60) return `${minutes} min`;
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return `${hours} h ${minutes % 60} min`;
  return `${Math.floor(hours / 24)} d ${hours % 24} h`;
}

function NewSession({ initial, dir: here, onClose, onCreated }: { initial: string; dir: string; onClose: () => void; onCreated: (c: Conversation) => void }) {
  const [harnesses, setHarnesses] = useState<Harness[]>([]);
  const [dirs, setDirs] = useState<string[]>([]);
  const [prompt, setPrompt] = useState(initial);
  const [harness, setHarness] = useState(localStorage.getItem("kk.harness") || "claude");
  const [models, setModels] = useState<Record<string, string>>(() => JSON.parse(localStorage.getItem("kk.models") || "{}"));
  // The directory shown, or in All the one used last.
  const [dir, setDir] = useState(here || localStorage.getItem("kk.dir") || "");
  const [busy, setBusy] = useState(false);
  const [past, setPast] = useState<PastSession[]>([]);
  const [resume, setResume] = useState<PastSession>();
  const [filter, setFilter] = useState("");
  const [worktree, setWorktree] = useState(localStorage.getItem("kk.worktree") === "1");
  const [managed, setManaged] = useState(localStorage.getItem("kk.managed") === "1");
  const [repo, setRepo] = useState<Repo>();
  const promptRef = useRef<HTMLTextAreaElement>(null);
  // Cursor keeps its history per directory; the others have one history for all directories.
  const pastDir = harness === "cursor" ? dir : "";

  useEffect(() => {
    let stop = false;
    setResume(undefined);
    const timer = setTimeout(
      () =>
        act(async () => {
          const list = await api<PastSession[]>("GET", `/api/sessions?harness=${harness}&dir=${encodeURIComponent(pastDir)}`);
          if (!stop) setPast(list);
        }),
      200,
    );
    return () => {
      stop = true;
      clearTimeout(timer);
    };
  }, [harness, pastDir]);

  useEffect(() => {
    let stop = false;
    const timer = setTimeout(
      () =>
        act(async () => {
          const info = await api<Repo>("GET", `/api/repo?dir=${encodeURIComponent(dir)}`);
          if (!stop) setRepo(info);
        }),
      200,
    );
    return () => {
      stop = true;
      clearTimeout(timer);
    };
  }, [dir]);

  useEffect(() => {
    promptRef.current?.focus();
    act(async () => setHarnesses(await api<Harness[]>("GET", "/api/harnesses")));
    act(async () => {
      const d = await api<string[]>("GET", "/api/dirs");
      setDirs(d);
      if (d[0]) setDir((cur) => cur || d[0]);
    });
  }, []);

  const h = harnesses.find((x) => x.name === harness);
  const skills = useSkills(harness, dir);
  const attachments = useAttachments();
  const empty = !prompt.trim() && !attachments.items.length;
  const blocked = busy || attachments.uploading || (!resume && empty);
  const model = h && h.models.includes(models[harness] ?? "") ? models[harness] ?? "" : h?.models[0] ?? "";
  const inWorktree = worktree && !!repo?.git && !resume;
  const submit = (e?: SubmitEvent) => {
    e?.preventDefault();
    if (blocked) return;
    setBusy(true);
    act(async () => {
      try {
        const c = await api<Conversation>("POST", "/api/conversations", { harness, model, dir, prompt: withPaths(prompt, attachments.items as Attachment[]), session: resume?.id, worktree: inWorktree, conductor: managed });
        localStorage.setItem("kk.harness", harness);
        localStorage.setItem("kk.models", JSON.stringify({ ...models, [harness]: model }));
        localStorage.setItem("kk.dir", dir);
        localStorage.setItem("kk.worktree", worktree ? "1" : "");
        localStorage.setItem("kk.managed", managed ? "1" : "");
        onCreated(c);
      } finally {
        setBusy(false);
      }
    });
  };

  return (
    <div class="overlay" data-scope="new" onPointerDown={(e) => e.target === e.currentTarget && onClose()}>
      <Scope>
        <form class="new-session" onSubmit={submit as any} onKeyDown={(e) => e.key === "Escape" && onClose()}>
          <h1>New session</h1>
          <div class="fields">
            <Field label="Program">
              <select value={harness} onChange={(e) => setHarness(e.currentTarget.value)}>
                {(harnesses.length ? harnesses : [{ name: "claude", label: "Claude Code" } as Harness]).map((x) => (
                  <option value={x.name} key={x.name}>
                    {x.label}
                  </option>
                ))}
              </select>
            </Field>
            <Field label="Model">
              <select value={model} onChange={(e) => setModels({ ...models, [harness]: e.currentTarget.value })}>
                {(h?.models ?? [""]).map((m) => (
                  <option value={m} key={m}>
                    {m || "Default"}
                  </option>
                ))}
              </select>
            </Field>
          </div>
          <DirField label="Directory" hint="The folder the agent starts in and works on." value={dir} onValue={setDir} recent={dirs} />
          <div class="field">
            <Check label="Worktree" checked={inWorktree} disabled={!repo?.git || !!resume} onChange={(e) => setWorktree(e.currentTarget.checked)} />
            <span class="hint">
              {resume
                ? "An earlier session resumes in its own directory."
                : !repo
                  ? "Checking the directory."
                  : !repo.git
                    ? "The directory is not a git repository."
                    : `The agent works in its own git worktree on a new branch off ${repo.branch}, apart from this checkout.`}
            </span>
          </div>
          <div class="field">
            <Check label="Hand over to konduktor" checked={managed} onChange={(e) => setManaged(e.currentTarget.checked)} />
            <span class="hint">The konduktor of {dir.replace(/\/+$/, "").split("/").pop() || "this directory"} drives the session when it stops, until you write into it yourself. It starts if it is not running.</span>
          </div>
          <Field label={resume ? "Prompt, optional" : "Prompt"}>
            <PromptInput
              inputRef={promptRef}
              value={prompt}
              onValue={setPrompt}
              skills={skills}
              attachments={attachments}
              onEnter={(e) => {
                if (e.ctrlKey || e.metaKey) {
                  e.preventDefault();
                  submit();
                }
              }}
            />
            <span class="hint">
              {skills.length} skills available to {h?.label ?? harness} in this directory. Type {harness === "codex" ? "$" : "/"} to insert one.
            </span>
          </Field>
          <div class="row">
            <Button label="Attach" icon="file" title="Attach files, or paste or drop them" onClick={attachments.pick} />
            <Button label="Cancel" onClick={onClose} />
            <Button label={resume ? "Resume" : "Start"} icon="play" class="primary" type="submit" disabled={blocked} />
          </div>
          <h1>Earlier sessions</h1>
          <input placeholder={`Filter ${past.length} earlier ${h?.label ?? harness} sessions`} value={filter} onInput={(e) => setFilter(e.currentTarget.value)} />
          <ul class="past">
            {past
              .filter((p) => `${p.title} ${p.dir}`.toLowerCase().includes(filter.toLowerCase()))
              .map((p) => (
                <li
                  key={p.id}
                  class={resume?.id === p.id ? "tile on" : "tile"}
                  onClick={() => {
                    setResume(resume?.id === p.id ? undefined : p);
                    setDir(p.dir);
                  }}
                >
                  <div class="title">{p.title}</div>
                  <div class="meta">
                    <span class="mono" title={p.dir}>
                      <bdi>{p.dir}</bdi>
                    </span>
                  </div>
                  <div class="meta">
                    <span>
                      {p.messages} {p.messages === 1 ? "message" : "messages"} · {span(p.started, p.ended)}
                    </span>
                    <span>Last {when(p.ended)}</span>
                  </div>
                  {resume?.id === p.id && <Button label="Resume this session" icon="play" class="small primary" type="submit" disabled={blocked} />}
                </li>
              ))}
          </ul>
        </form>
      </Scope>
    </div>
  );
}

const root = document.createElement("div");
root.id = "app";
document.body.prepend(root);
render(<App />, root);

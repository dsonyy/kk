import type * as preact from "preact";
import { render } from "preact";
import { useCallback, useEffect, useLayoutEffect, useRef, useState } from "preact/hooks";
import { marked } from "marked";
import DOMPurify from "dompurify";
import { act, api, flash, harnessLabel, when, type Attachment, type Conversation, type Event, type Harness, type PastSession, type Repo } from "./api";
import { Controls } from "./controls";
import { Load } from "./load";
import { Attachments, PromptInput, useAttachments, useSkills, withPaths } from "./prompt";
import { SettingsView } from "./settings";
import { Button, Check, Field, Scope } from "./mnemonic";
import { Terminal } from "./terminal";
import "./app.css";

const COORDINATOR = "coordinator";

const mdCache = new Map<string, string>();
function Markdown({ text, partial }: { text: string; partial?: boolean }) {
  let html = mdCache.get(text);
  if (html === undefined) {
    html = DOMPurify.sanitize(marked.parse(text, { async: false, gfm: true, breaks: true }) as string);
    if (!partial) mdCache.set(text, html);
  }
  return <div class="md" dangerouslySetInnerHTML={{ __html: html }} />;
}

function openIds(): string[] {
  return [...new Set(decodeURIComponent(location.hash.slice(1)).split(",").filter(Boolean))];
}

function App() {
  const [list, setList] = useState<Conversation[]>([]);
  const [details, setDetails] = useState<Record<string, Conversation>>({});
  const [open, setOpenState] = useState(openIds);
  const [tab, setTab] = useState<"Agents" | "Settings">("Agents");
  const [creating, setCreating] = useState(false);
  const openRef = useRef(open);
  openRef.current = open;

  const setOpen = (ids: string[]) => {
    location.hash = ids.join(",");
  };
  const hide = (id: string) => setOpen(openRef.current.filter((x) => x !== id));

  const loadList = useCallback(() => act(async () => setList(await api<Conversation[]>("GET", "/api/conversations"))), []);
  const loadDetail = useCallback(async (id: string) => {
    try {
      const c = await api<Conversation>("GET", `/api/conversations/${id}`);
      setDetails((d) => ({ ...d, [id]: c }));
      if (c.unread && (openRef.current.includes(id) || id === COORDINATOR) && document.hasFocus()) {
        await api("POST", `/api/conversations/${id}/seen`, {});
      }
    } catch (e) {
      if (id === COORDINATOR) return setDetails(({ [COORDINATOR]: _, ...d }) => d);
      hide(id);
      flash((e as Error).message);
    }
  }, []);

  useEffect(() => {
    loadList();
    loadDetail(COORDINATOR);
    const pending = new Set<string>();
    let timer = 0;
    const source = new EventSource("/api/events");
    source.onmessage = (e) => {
      pending.add(e.data);
      clearTimeout(timer);
      timer = window.setTimeout(() => {
        loadList();
        for (const id of pending) if (openRef.current.includes(id) || id === COORDINATOR) loadDetail(id);
        pending.clear();
      }, 100);
    };
    source.onopen = () => {
      loadList();
      loadDetail(COORDINATOR);
      for (const id of openRef.current) loadDetail(id);
    };
    const onHash = () => setOpenState(openIds());
    window.addEventListener("hashchange", onHash);
    return () => {
      source.close();
      window.removeEventListener("hashchange", onHash);
    };
  }, []);

  useEffect(() => {
    for (const id of open) loadDetail(id);
  }, [open.join(",")]);

  const show = (id: string) => {
    if (open.includes(id)) document.getElementById(`panel-${id}`)?.scrollIntoView({ behavior: "smooth", inline: "nearest" });
    else setOpen([...open, id]);
  };

  return (
    <>
      <nav class="tabs">
        {(["Agents", "Settings"] as const).map((t) => (
          <a class={t === tab ? "tab on" : "tab"} onClick={() => setTab(t)}>
            {t}
          </a>
        ))}
      </nav>
      {tab === "Settings" && <SettingsView />}
      <div class="workspace" hidden={tab !== "Agents"}>
        <aside class="sidebar" data-scope="sidebar">
          <Scope>
            <div class="sidebar-head">
              <Button label="New session" icon="plus" class="primary" onClick={() => setCreating(true)} />
            </div>
          </Scope>
          <ul class="tiles">
            {list.length === 0 && <li class="empty">No sessions yet.</li>}
            {list.map((c) => (
              <Tile key={c.id} c={c} on={open.includes(c.id)} onOpen={() => show(c.id)} />
            ))}
          </ul>
          <Load />
        </aside>
        <div class="panels">
          {open.length === 0 && <main class="conversation placeholder">Pick a session on the left or start a new one.</main>}
          {open.map((id) =>
            details[id] ? (
              <ConversationView key={id} c={details[id]} reload={() => loadDetail(id)} onHide={() => hide(id)} />
            ) : (
              <main key={id} class="conversation placeholder">Loading…</main>
            ),
          )}
        </div>
        <CoordinatorPanel c={details[COORDINATOR]} reload={() => loadDetail(COORDINATOR)} />
      </div>
      {creating && (
        <NewSession
          onClose={() => setCreating(false)}
          onCreated={() => {
            loadList();
            setCreating(false);
          }}
        />
      )}
    </>
  );
}

function stateLabel(c: Conversation) {
  if (c.attention) return "Action required";
  return { starting: "Starting", working: "Working", idle: "Your turn", exited: "Exited" }[c.status];
}

function Dot({ c }: { c: Conversation }) {
  return <i class={`dot ${c.attention ? "attention" : c.status}`} />;
}

function Tile({ c, on, onOpen }: { c: Conversation; on: boolean; onOpen: () => void }) {
  const cls = ["tile", on && "on", c.attention ? "attention" : c.status === "working" && "working", c.unread && !on && "unread", c.status === "exited" && "exited"];
  return (
    <li class={cls.filter(Boolean).join(" ")} onClick={onOpen} title={c.attention || undefined}>
      <div class="title">{c.title || "Untitled"}</div>
      <div class="meta">
        <span>
          {harnessLabel(c.harness)} · {c.model || "default"}
        </span>
        <span class="state">
          <Dot c={c} />
          {stateLabel(c)}
        </span>
      </div>
      <div class="meta">
        <span>Created {when(c.created)}</span>
        <span>Last {when(c.last_message)}</span>
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
function Reveal({ text }: { text: string }) {
  const still = window.matchMedia("(prefers-reduced-motion: reduce)").matches;
  const [shown, setShown] = useState(still ? text.length : 0);
  useEffect(() => {
    if (shown >= text.length) return;
    const t = setTimeout(() => setShown((n) => n + Math.max(6, Math.ceil(text.length / 60))), 16);
    return () => clearTimeout(t);
  }, [shown, text]);
  return shown >= text.length ? <Markdown text={text} /> : <Markdown text={text.slice(0, shown)} partial />;
}

function Feed({ events, live, running }: { events: Event[]; live: number; running: boolean }) {
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
          <Icon name="alert" />
          {e.text}
        </div>,
      );
    else if (e.kind === "note") out.push(<div class="note" key={i}>{e.text}</div>);
    else
      out.push(
        <div class={`msg ${e.kind}`} key={i} title={when(e.time)} data-event={i}>
          {e.kind === "assistant" && i >= live ? <Reveal text={e.text ?? ""} /> : e.text && <Markdown text={e.text} />}
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

type Pending = { text: string; files: Attachment[]; expect: number };

function ConversationView({ c, reload, side, onHide }: { c: Conversation; reload: () => void; side?: boolean; onHide?: () => void }) {
  const [showTerminal, setShowTerminal] = useState(false);
  const [confirmClose, setConfirmClose] = useState(false);
  const [pending, setPending] = useState<Pending[]>([]);
  const screen = useScreen(c.id, !!c.attention);
  const feed = useRef<HTMLDivElement>(null);
  const inner = useRef<HTMLDivElement>(null);
  const stick = useRef(true);
  const live = useRef(c.events.length);
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
  const users = c.events.filter((e) => e.kind === "user").length;
  const sending = pending.filter((p) => users < p.expect);
  const busy = !exited && !c.attention && (c.status !== "idle" || sending.length > 0);
  const last = c.events[c.events.length - 1];

  const send = async (text: string, files: Attachment[]) => {
    const p = { text, files, expect: users + sending.length + 1 };
    const drop = () => setPending((q) => q.filter((x) => x !== p));
    stick.current = true;
    setPending([...sending, p]);
    setTimeout(drop, 30000);
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
    <Tag class="conversation" id={`panel-${c.id}`} data-scope={side ? "coordinator" : `conversation-${c.id}`}>
      <Scope>
        <header class="conv-head">
          <h1>{side ? "Coordinator" : c.title || "Untitled"}</h1>
          <div class="actions">
            <div class="modes">
              <Button label="Chat" class={showTerminal ? "small" : "small on"} onClick={() => setShowTerminal(false)} />
              <Button label="Terminal" icon="terminal" class={showTerminal ? "small on" : "small"} onClick={() => setShowTerminal(true)} />
            </div>
            {exited && <Button label="Resume" icon="play" class="small primary" onClick={resume} />}
            <Controls c={c} reload={reload} />
            {!side && <Button label={confirmClose ? "Confirm close" : "Close"} icon="close" class="small" title="Stop the agent and remove the session" onClick={close} />}
            {onHide && <Button label="Hide" icon="hide" class="small" title="Hide this panel, the agent keeps running" onClick={onHide} />}
          </div>
          <div class="meta">
            <span class={c.attention ? "status attention" : "status"}>
              <Dot c={c} />
              {stateLabel(c)}
            </span>
            <span>{harnessLabel(c.harness)} · {c.model || "default"}</span>
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
        {showTerminal && <Terminal id={c.id} />}
        <div
          hidden={showTerminal}
          class="feed"
          ref={feed}
          onScroll={(e) => {
            const el = e.currentTarget;
            stick.current = el.scrollHeight - el.scrollTop - el.clientHeight < 40;
          }}
        >
          <div class="feed-inner" ref={inner}>
            {c.events.length === 0 && !c.attention && !busy && <div class="empty">No messages yet.</div>}
            <Feed events={c.events} live={live.current} running={c.status === "working"} />
            {sending.map((p, i) => (
              <div class="msg user sending" key={`p${i}`}>
                {p.text.trim() && <Markdown text={p.text} />}
                <Attachments items={p.files} />
              </div>
            ))}
            {busy && (
              <div class="busy">
                <i class="pulse" />
                <span>{c.status === "starting" ? "Starting the agent" : last?.kind === "tool" && !last.output ? `Running ${last.tool}` : "Working"}</span>
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
        {quote && !showTerminal && !exited && (
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
        {!showTerminal && <Composer c={c} exited={exited} onResume={resume} onSend={send} insert={composer} />}
      </Scope>
    </Tag>
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
  if (exited) {
    return (
      <div class="composer">
        <span class="muted" style="flex:1;align-self:center">The agent has exited.</span>
        <Button label="Resume session" icon="play" class="primary" onClick={onResume} />
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
        placeholder={c.role === COORDINATOR ? "Message the coordinator" : "Reply to the agent, / for skills"}
        onEnter={(e) => {
          if (!e.shiftKey && !e.isComposing) {
            e.preventDefault();
            send();
          }
        }}
      />
      <Button label="Attach" icon="file" title="Attach files, or paste or drop them" onClick={attachments.pick} />
      <Button label="Send" icon="send" class="primary" disabled={attachments.uploading} onClick={send} />
    </div>
  );
}

function CoordinatorPanel({ c, reload }: { c?: Conversation; reload: () => void }) {
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
    <aside class="side-panel" style={{ "--panel-width": `${width}px` }}>
      <div class={dragging ? "panel-resize dragging" : "panel-resize"} onPointerDown={() => setDragging(true)} />
      {c ? <ConversationView c={c} reload={reload} side /> : <div class="conversation placeholder">The coordinator is not running.</div>}
    </aside>
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

function NewSession({ onClose, onCreated }: { onClose: () => void; onCreated: () => void }) {
  const [harnesses, setHarnesses] = useState<Harness[]>([]);
  const [dirs, setDirs] = useState<string[]>([]);
  const [prompt, setPrompt] = useState("");
  const [harness, setHarness] = useState(localStorage.getItem("kk.harness") || "claude");
  const [models, setModels] = useState<Record<string, string>>(() => JSON.parse(localStorage.getItem("kk.models") || "{}"));
  const [dir, setDir] = useState(localStorage.getItem("kk.dir") || "");
  const [busy, setBusy] = useState(false);
  const [past, setPast] = useState<PastSession[]>([]);
  const [resume, setResume] = useState<PastSession>();
  const [filter, setFilter] = useState("");
  const [worktree, setWorktree] = useState(localStorage.getItem("kk.worktree") === "1");
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
        await api<Conversation>("POST", "/api/conversations", { harness, model, dir, prompt: withPaths(prompt, attachments.items as Attachment[]), session: resume?.id, worktree: inWorktree });
        localStorage.setItem("kk.harness", harness);
        localStorage.setItem("kk.models", JSON.stringify({ ...models, [harness]: model }));
        localStorage.setItem("kk.dir", dir);
        localStorage.setItem("kk.worktree", worktree ? "1" : "");
        onCreated();
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
          <Field label="Directory">
            <span class="hint">The folder the agent starts in and works on.</span>
            <input class="mono" list="kk-dirs" value={dir} onInput={(e) => setDir(e.currentTarget.value)} />
            <datalist id="kk-dirs">
              {dirs.map((d) => (
                <option value={d} key={d} />
              ))}
            </datalist>
          </Field>
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

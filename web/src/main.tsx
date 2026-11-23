import type * as preact from "preact";
import { render } from "preact";
import { useCallback, useEffect, useLayoutEffect, useRef, useState } from "preact/hooks";
import { marked } from "marked";
import DOMPurify from "dompurify";
import { act, api, harnessLabel, when, type Conversation, type Event, type Harness } from "./api";
import { Button, Field, Scope } from "./mnemonic";
import "./app.css";

const COORDINATOR = "coordinator";

const mdCache = new Map<string, string>();
function Markdown({ text }: { text: string }) {
  let html = mdCache.get(text);
  if (html === undefined) {
    html = DOMPurify.sanitize(marked.parse(text, { async: false, gfm: true, breaks: true }) as string);
    mdCache.set(text, html);
  }
  return <div class="md" dangerouslySetInnerHTML={{ __html: html }} />;
}

function currentId() {
  return decodeURIComponent(location.hash.slice(1));
}

function App() {
  const [list, setList] = useState<Conversation[]>([]);
  const [details, setDetails] = useState<Record<string, Conversation>>({});
  const [selected, setSelected] = useState(currentId());
  const [creating, setCreating] = useState(false);
  const selectedRef = useRef(selected);
  selectedRef.current = selected;

  const loadList = useCallback(() => act(async () => setList(await api<Conversation[]>("GET", "/api/conversations"))), []);
  const loadDetail = useCallback(
    (id: string) =>
      act(async () => {
        const c = await api<Conversation>("GET", `/api/conversations/${id}`);
        setDetails((d) => ({ ...d, [id]: c }));
        if (c.unread && (id === selectedRef.current || id === COORDINATOR) && document.hasFocus()) {
          await api("POST", `/api/conversations/${id}/seen`, {});
        }
      }),
    [],
  );

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
        for (const id of pending) if (id === selectedRef.current || id === COORDINATOR) loadDetail(id);
        pending.clear();
      }, 100);
    };
    source.onopen = () => {
      loadList();
      loadDetail(COORDINATOR);
      if (selectedRef.current) loadDetail(selectedRef.current);
    };
    const onHash = () => setSelected(currentId());
    window.addEventListener("hashchange", onHash);
    return () => {
      source.close();
      window.removeEventListener("hashchange", onHash);
    };
  }, []);

  useEffect(() => {
    if (selected) loadDetail(selected);
  }, [selected]);

  const open = (id: string) => {
    location.hash = id;
  };

  const current = selected ? details[selected] : undefined;
  return (
    <>
      <nav class="tabs">
        <span class="tab on">Agents</span>
      </nav>
      <div class="workspace">
        <aside class="sidebar" data-scope="sidebar">
          <Scope>
            <div class="sidebar-head">
              <Button label="New session" icon="plus" class="primary" onClick={() => setCreating(true)} />
            </div>
          </Scope>
          <ul class="tiles">
            {list.length === 0 && <li class="empty">No sessions yet.</li>}
            {list.map((c) => (
              <Tile key={c.id} c={c} on={c.id === selected} onOpen={() => open(c.id)} />
            ))}
          </ul>
        </aside>
        {current ? (
          <ConversationView key={current.id} c={current} reload={() => loadDetail(current.id)} />
        ) : (
          <main class="conversation placeholder">{selected ? "Loading…" : "Pick a session on the left or start a new one."}</main>
        )}
        <CoordinatorPanel c={details[COORDINATOR]} reload={() => loadDetail(COORDINATOR)} />
      </div>
      {creating && (
        <NewSession
          onClose={() => setCreating(false)}
          onCreated={(id) => {
            setCreating(false);
            loadList();
            open(id);
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

function Tile({ c, on, onOpen }: { c: Conversation; on: boolean; onOpen: () => void }) {
  const cls = ["tile", on && "on", c.attention ? "attention" : c.status === "working" && "working", c.unread && !on && "unread", c.status === "exited" && "exited"];
  return (
    <li class={cls.filter(Boolean).join(" ")} onClick={onOpen} title={c.attention || undefined}>
      <div class="title">{c.title || "Untitled"}</div>
      <div class="meta">
        <span>
          {harnessLabel(c.harness)} · {c.model || "default"}
        </span>
        <span class="state">{stateLabel(c)}</span>
      </div>
      <div class="meta">
        <span>Created {when(c.created)}</span>
        <span>Last {when(c.last_message)}</span>
      </div>
    </li>
  );
}

function Feed({ events }: { events: Event[] }) {
  const out: preact.JSX.Element[] = [];
  let steps: Event[] = [];
  const flush = (key: number) => {
    if (steps.length) out.push(<div class="steps" key={`s${key}`}>{steps.map((e, i) => <Step key={i} e={e} />)}</div>);
    steps = [];
  };
  events.forEach((e, i) => {
    if (e.kind === "tool" || e.kind === "thinking") {
      steps.push(e);
      return;
    }
    flush(i);
    if (e.kind === "error") out.push(<div class="error" key={i}>{e.text}</div>);
    else if (e.kind === "note") out.push(<div class="note" key={i}>{e.text}</div>);
    else
      out.push(
        <div class={`msg ${e.kind}`} key={i}>
          <header>
            <span>{e.kind === "user" ? "You" : "Agent"}</span>
            <span>{when(e.time)}</span>
          </header>
          <Markdown text={e.text ?? ""} />
        </div>,
      );
  });
  flush(events.length);
  return <>{out}</>;
}

function Step({ e }: { e: Event }) {
  if (e.kind === "thinking") {
    return (
      <details class="tool">
        <summary>
          <b>Thinking</b>
          {firstWords(e.text ?? "")}
        </summary>
        <Markdown text={e.text ?? ""} />
      </details>
    );
  }
  return (
    <details class={e.failed ? "tool failed" : "tool"}>
      <summary>
        <b>{e.tool}</b>
        <span class="mono">{e.input}</span>
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

function ConversationView({ c, reload, side }: { c: Conversation; reload: () => void; side?: boolean }) {
  const [showScreen, setShowScreen] = useState(false);
  const [confirmClose, setConfirmClose] = useState(false);
  const screen = useScreen(c.id, showScreen || !!c.attention);
  const feed = useRef<HTMLDivElement>(null);
  const stick = useRef(true);

  useLayoutEffect(() => {
    if (stick.current && feed.current) feed.current.scrollTop = feed.current.scrollHeight;
  }, [c.events.length, c.attention]);

  useEffect(() => {
    if (!confirmClose) return;
    const t = setTimeout(() => setConfirmClose(false), 3000);
    return () => clearTimeout(t);
  }, [confirmClose]);

  const exited = c.status === "exited";
  const resume = () => act(async () => (await api("POST", `/api/conversations/${c.id}/resume`, {}), reload()));
  const close = () => {
    if (!confirmClose) return setConfirmClose(true);
    act(async () => {
      await api("POST", `/api/conversations/${c.id}/close`, {});
      location.hash = "";
    });
  };
  const Tag = side ? "div" : "main";
  return (
    <Tag class="conversation" data-scope={side ? "coordinator" : "conversation"}>
      <Scope>
        <header class="conv-head">
          <div>
            <h1>{side ? "Coordinator" : c.title || "Untitled"}</h1>
            <div class="meta">
              <span>{harnessLabel(c.harness)} · {c.model || "default"}</span>
              {!side && <span class="mono">{c.dir}</span>}
              <span class={c.attention ? "status attention" : "status"}>{stateLabel(c)}</span>
              {c.context_tokens > 0 && <span>{Math.round(c.context_tokens / 1000)}k context</span>}
            </div>
          </div>
          <div class="actions">
            <Button label={showScreen ? "Hide screen" : "Screen"} class="small" onClick={() => setShowScreen(!showScreen)} />
            {exited && <Button label="Resume" class="small primary" onClick={resume} />}
            {!side && <Button label={confirmClose ? "Confirm close" : "Close"} class="small" onClick={close} />}
          </div>
        </header>
        <div
          class="feed"
          ref={feed}
          onScroll={(e) => {
            const el = e.currentTarget;
            stick.current = el.scrollHeight - el.scrollTop - el.clientHeight < 40;
          }}
        >
          {c.events.length === 0 && !c.attention && <div class="empty">{c.status === "starting" ? "Starting the agent…" : "No messages yet."}</div>}
          <Feed events={c.events} />
          {c.attention && (
            <div class="box-warn">
              <p>{c.attention}</p>
              <Keys id={c.id} />
              {screen && <pre>{screen}</pre>}
            </div>
          )}
        </div>
        {showScreen && !c.attention && (
          <div class="screen">
            <pre>{screen || "No screen."}</pre>
            <Keys id={c.id} />
          </div>
        )}
        <Composer c={c} exited={exited} onResume={resume} />
      </Scope>
    </Tag>
  );
}

function Composer({ c, exited, onResume }: { c: Conversation; exited: boolean; onResume: () => void }) {
  const [text, setText] = useState("");
  const area = useRef<HTMLTextAreaElement>(null);
  useLayoutEffect(() => {
    const el = area.current;
    if (!el) return;
    el.style.height = "auto";
    el.style.height = `${el.scrollHeight + 2}px`;
  }, [text]);
  const send = () => {
    const body = text;
    if (!body.trim()) return;
    act(async () => {
      await api("POST", `/api/conversations/${c.id}/send`, { text: body });
      setText("");
    });
  };
  if (exited) {
    return (
      <div class="composer">
        <span class="muted" style="flex:1;align-self:center">The agent has exited.</span>
        <Button label="Resume session" class="primary" onClick={onResume} />
      </div>
    );
  }
  return (
    <div class="composer">
      <textarea
        ref={area}
        rows={1}
        value={text}
        placeholder={c.role === COORDINATOR ? "Message the coordinator" : "Reply to the agent"}
        onInput={(e) => setText(e.currentTarget.value)}
        onKeyDown={(e) => {
          if (e.key === "Enter" && !e.shiftKey && !e.isComposing) {
            e.preventDefault();
            send();
          }
        }}
      />
      <div class="buttons">
        <Button label="Send" class="primary" onClick={send} />
      </div>
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

function NewSession({ onClose, onCreated }: { onClose: () => void; onCreated: (id: string) => void }) {
  const [harnesses, setHarnesses] = useState<Harness[]>([]);
  const [dirs, setDirs] = useState<string[]>([]);
  const [prompt, setPrompt] = useState("");
  const [harness, setHarness] = useState(localStorage.getItem("kk.harness") || "claude");
  const [models, setModels] = useState<Record<string, string>>(() => JSON.parse(localStorage.getItem("kk.models") || "{}"));
  const [dir, setDir] = useState(localStorage.getItem("kk.dir") || "");
  const [busy, setBusy] = useState(false);
  const promptRef = useRef<HTMLTextAreaElement>(null);

  useEffect(() => {
    promptRef.current?.focus();
    act(async () => {
      setHarnesses(await api<Harness[]>("GET", "/api/harnesses"));
      const d = await api<string[]>("GET", "/api/dirs");
      setDirs(d);
      if (d[0]) setDir((cur) => cur || d[0]);
    });
    const esc = (e: KeyboardEvent) => e.key === "Escape" && onClose();
    window.addEventListener("keydown", esc);
    return () => window.removeEventListener("keydown", esc);
  }, []);

  const h = harnesses.find((x) => x.name === harness);
  const model = h && h.models.includes(models[harness] ?? "") ? models[harness] ?? "" : h?.models[0] ?? "";
  const submit = (e?: SubmitEvent) => {
    e?.preventDefault();
    if (busy || !prompt.trim()) return;
    setBusy(true);
    act(async () => {
      try {
        const c = await api<Conversation>("POST", "/api/conversations", { harness, model, dir, prompt });
        localStorage.setItem("kk.harness", harness);
        localStorage.setItem("kk.models", JSON.stringify({ ...models, [harness]: model }));
        localStorage.setItem("kk.dir", dir);
        onCreated(c.id);
      } finally {
        setBusy(false);
      }
    });
  };

  return (
    <div class="overlay" onClick={(e) => e.target === e.currentTarget && onClose()}>
      <Scope>
        <form class="dialog" onSubmit={submit as any}>
          <h1>New session</h1>
          <Field label="Prompt">
            <textarea
              ref={promptRef}
              value={prompt}
              onInput={(e) => setPrompt(e.currentTarget.value)}
              onKeyDown={(e) => {
                if (e.key === "Enter" && (e.ctrlKey || e.metaKey)) {
                  e.preventDefault();
                  submit();
                }
              }}
            />
          </Field>
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
            <input class="mono" list="kk-dirs" value={dir} onInput={(e) => setDir(e.currentTarget.value)} />
            <datalist id="kk-dirs">
              {dirs.map((d) => (
                <option value={d} key={d} />
              ))}
            </datalist>
          </Field>
          <div class="row">
            <Button label="Cancel" onClick={onClose} />
            <Button label="Start" class="primary" type="submit" disabled={busy || !prompt.trim()} />
          </div>
        </form>
      </Scope>
    </div>
  );
}

const root = document.createElement("div");
root.id = "app";
document.body.prepend(root);
render(<App />, root);

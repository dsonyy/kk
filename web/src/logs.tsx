import { useEffect, useLayoutEffect, useRef, useState } from "preact/hooks";

const keep = 2000;

// Logs shows the server log live, newest at the bottom. It follows new lines while scrolled to the end.
export function Logs() {
  const [lines, setLines] = useState<string[]>([]);
  const box = useRef<HTMLElement>(null);
  const atEnd = useRef(true);
  useEffect(() => {
    const source = new EventSource("/api/logs");
    let pending: string[] = [];
    let frame = 0;
    // A reconnect sends the tail again.
    source.onopen = () => {
      pending = [];
      setLines([]);
    };
    source.onmessage = (e) => {
      pending.push(e.data);
      cancelAnimationFrame(frame);
      frame = requestAnimationFrame(() => {
        const add = pending;
        pending = [];
        setLines((l) => [...l, ...add].slice(-keep));
      });
    };
    return () => {
      cancelAnimationFrame(frame);
      source.close();
    };
  }, []);
  useLayoutEffect(() => {
    if (atEnd.current) box.current!.scrollTop = box.current!.scrollHeight;
  }, [lines]);
  const onScroll = () => {
    const el = box.current!;
    atEnd.current = el.scrollHeight - el.scrollTop - el.clientHeight < 4;
  };
  return (
    <main class="logs" ref={box} onScroll={onScroll}>
      {lines.length === 0 && <div class="muted">No log lines yet.</div>}
      {lines.map((line, i) => (
        <div key={i} class={/ failed \(| error/.test(line) ? "log-line err" : "log-line"}>
          <span class="log-time">{line.slice(0, 19)}</span>
          {line.slice(19)}
        </div>
      ))}
    </main>
  );
}

// reportErrors sends the web client's uncaught errors to the server log.
export function reportErrors() {
  const send = (error: string) => fetch("/api/logs", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ error }) }).catch(() => {});
  window.addEventListener("error", (e) => send(e.error?.stack ?? e.message));
  window.addEventListener("unhandledrejection", (e) => send(String(e.reason?.stack ?? e.reason)));
}

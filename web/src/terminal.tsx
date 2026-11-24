import { useEffect, useRef } from "preact/hooks";
import { Terminal as XTerm } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";
import "@xterm/xterm/css/xterm.css";

export function Terminal({ id }: { id: string }) {
  const box = useRef<HTMLDivElement>(null);
  useEffect(() => {
    const css = getComputedStyle(document.documentElement);
    const term = new XTerm({
      fontFamily: css.getPropertyValue("--font-mono"),
      fontSize: 13,
      cursorBlink: true,
      theme: { background: css.getPropertyValue("--bg"), foreground: css.getPropertyValue("--text") },
    });
    const fit = new FitAddon();
    term.loadAddon(fit);
    term.open(box.current!);
    const ws = new WebSocket(`${location.protocol === "https:" ? "wss" : "ws"}://${location.host}/api/conversations/${id}/terminal`);
    ws.binaryType = "arraybuffer";
    const encoder = new TextEncoder();
    const resize = () => {
      fit.fit();
      if (ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify({ cols: term.cols, rows: term.rows }));
    };
    ws.onopen = () => {
      resize();
      term.focus();
    };
    ws.onmessage = (e) => term.write(new Uint8Array(e.data));
    ws.onclose = () => term.write("\r\n\x1b[2m[disconnected]\x1b[0m\r\n");
    term.onData((d) => ws.readyState === WebSocket.OPEN && ws.send(encoder.encode(d)));
    const observer = new ResizeObserver(resize);
    observer.observe(box.current!);
    return () => {
      observer.disconnect();
      ws.onclose = null;
      ws.close();
      term.dispose();
    };
  }, [id]);
  return <div class="terminal" ref={box} />;
}

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

type browser struct {
	t   *testing.T
	ctx context.Context
	ws  *websocket.Conn
	id  int
}

// newBrowser starts headless Chrome and connects to its first page over the DevTools protocol.
func newBrowser(t *testing.T) *browser {
	t.Helper()
	bin := ""
	for _, name := range []string{"google-chrome", "chromium", "chromium-browser"} {
		if path, err := exec.LookPath(name); err == nil {
			bin = path
			break
		}
	}
	if bin == "" {
		t.Skip("no Chrome or Chromium on PATH")
	}
	cmd := exec.Command(bin, "--headless=new", "--remote-debugging-port=0", "--user-data-dir="+t.TempDir(), "--no-first-run", "about:blank")
	stderr, _ := cmd.StderrPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	var port string
	for scan := bufio.NewScanner(stderr); scan.Scan() && port == ""; {
		if m := regexp.MustCompile(`ws://127\.0\.0\.1:(\d+)/`).FindStringSubmatch(scan.Text()); m != nil {
			port = m[1]
		}
	}
	go io.Copy(io.Discard, stderr)
	resp, err := http.Get("http://127.0.0.1:" + port + "/json")
	if err != nil {
		t.Fatal(err)
	}
	var targets []struct{ Type, WebSocketDebuggerUrl string }
	json.NewDecoder(resp.Body).Decode(&targets)
	resp.Body.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	for _, target := range targets {
		if target.Type == "page" {
			ws, _, err := websocket.Dial(ctx, target.WebSocketDebuggerUrl, nil)
			if err != nil {
				t.Fatal(err)
			}
			ws.SetReadLimit(1 << 24)
			return &browser{t: t, ctx: ctx, ws: ws}
		}
	}
	t.Fatal("Chrome has no page")
	return nil
}

func (b *browser) call(method string, params any) json.RawMessage {
	b.t.Helper()
	b.id++
	data, _ := json.Marshal(map[string]any{"id": b.id, "method": method, "params": params})
	if err := b.ws.Write(b.ctx, websocket.MessageText, data); err != nil {
		b.t.Fatal(err)
	}
	for {
		_, data, err := b.ws.Read(b.ctx)
		if err != nil {
			b.t.Fatal(err)
		}
		var m struct {
			ID     int
			Result json.RawMessage
			Error  *struct{ Message string }
		}
		json.Unmarshal(data, &m)
		if m.ID != b.id {
			continue
		}
		if m.Error != nil {
			b.t.Fatalf("%s: %s", method, m.Error.Message)
		}
		return m.Result
	}
}

func (b *browser) eval(expr string, out any) {
	b.t.Helper()
	var r struct {
		Result struct{ Value json.RawMessage }
	}
	json.Unmarshal(b.call("Runtime.evaluate", map[string]any{"expression": expr, "returnByValue": true, "awaitPromise": true}), &r)
	if err := json.Unmarshal(r.Result.Value, out); err != nil {
		b.t.Fatalf("%s: %v in %s", expr, err, r.Result.Value)
	}
}

func (b *browser) waitFor(what, expr string) {
	b.t.Helper()
	ok := false
	for deadline := time.Now().Add(10 * time.Second); !ok; time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			b.t.Fatalf("timed out waiting for %s", what)
		}
		b.eval(expr, &ok)
	}
}

func (b *browser) open(url string, width, height int, reducedMotion bool) {
	b.t.Helper()
	b.call("Emulation.setDeviceMetricsOverride", map[string]any{"width": width, "height": height, "deviceScaleFactor": 2, "mobile": false})
	value := ""
	if reducedMotion {
		value = "reduce"
	}
	b.call("Emulation.setEmulatedMedia", map[string]any{"features": []map[string]string{{"name": "prefers-reduced-motion", "value": value}}})
	b.call("Page.navigate", map[string]any{"url": url})
	b.waitFor("the sidebar", `!!document.querySelector("aside.sidebar .load")`)
}

// glenzDraws counts the frames the canvas draws, as every frame starts by clearing it, and the animation frames the page asks for.
const glenzDraws = `window.draws = 0;
window.frames = 0;
const request = window.requestAnimationFrame;
window.requestAnimationFrame = (f) => (window.frames++, request(f));
const clear = CanvasRenderingContext2D.prototype.clearRect;
CanvasRenderingContext2D.prototype.clearRect = function (...args) {
  if (this.canvas.classList.contains("glenz")) window.draws++;
  return clear.apply(this, args);
};`

func (b *browser) countIn(counter string, d time.Duration) int {
	b.t.Helper()
	var before, after int
	b.eval("window."+counter, &before)
	time.Sleep(d)
	b.eval("window."+counter, &after)
	return after - before
}

func (b *browser) glenzPixels() string {
	b.t.Helper()
	var sum string
	b.eval(`(() => { const c = document.querySelector("canvas.glenz"); const d = c.getContext("2d").getImageData(0, 0, c.width, c.height).data;
		let lit = 0, hash = 0; for (let i = 3; i < d.length; i += 4) if (d[i]) { lit++; hash = (hash * 31 + d[i - 3] + i) | 0; } return lit + ":" + hash; })()`, &sum)
	return sum
}

func TestGlenz(t *testing.T) {
	e := newEnv(t)
	b := newBrowser(t)
	b.call("Page.enable", nil)
	b.call("Page.addScriptToEvaluateOnNewDocument", map[string]any{"source": glenzDraws})
	b.open(e.url, 1400, 900, false)
	b.waitFor("the first frame", "window.draws > 0")

	var size struct{ Width, Height, CanvasWidth, CanvasHeight, Sidebar float64 }
	b.eval(`(() => { const c = document.querySelector("canvas.glenz"); const r = c.getBoundingClientRect();
		return { width: r.width, height: r.height, canvasWidth: c.width, canvasHeight: c.height, sidebar: document.querySelector("aside.sidebar").clientWidth }; })()`, &size)
	if size.Height != 110 || size.Width != size.Sidebar || size.CanvasWidth != 2*size.Width || size.CanvasHeight != 2*size.Height {
		t.Fatalf("the canvas does not fill the sidebar at the device pixel ratio: %+v", size)
	}
	var above bool
	b.eval(`(() => { const c = document.querySelector("canvas.glenz"); return c.nextElementSibling.classList.contains("load") && c.previousElementSibling.classList.contains("groups"); })()`, &above)
	if !above {
		t.Fatal("the animation is not between the session list and the system load")
	}
	first := b.glenzPixels()
	if n := b.countIn("draws", 2*time.Second); n < 30 || n > 64 {
		t.Fatalf("%d frames in 2 s, want about 30 a second and never more", n)
	}
	if strings.HasPrefix(first, "0:") || b.glenzPixels() == first {
		t.Fatalf("the solid is not drawn or does not move: %s", first)
	}

	b.eval(`[...document.querySelectorAll(".tabs .tab")].find((t) => t.textContent.trim() === "Settings").click(), true`, new(bool))
	time.Sleep(200 * time.Millisecond)
	if n := b.countIn("frames", time.Second); n != 0 {
		t.Fatalf("%d animation frames while the sidebar is hidden", n)
	}
	b.eval(`[...document.querySelectorAll(".tabs .tab")].find((t) => t.textContent.trim() === "Agents").click(), true`, new(bool))
	time.Sleep(200 * time.Millisecond)
	if n := b.countIn("draws", time.Second); n == 0 {
		t.Fatal("the animation did not come back with the Agents tab")
	}

	b.open(e.url, 1400, 600, false)
	var shown bool
	b.eval(`document.querySelector("canvas.glenz").clientHeight > 0`, &shown)
	if n := b.countIn("frames", time.Second); shown || n != 0 {
		t.Fatalf("on a short window the animation takes space or draws: shown %v, %d frames", shown, n)
	}

	b.open(e.url, 1400, 900, true)
	b.waitFor("the still frame", "window.draws > 0")
	still := b.glenzPixels()
	if n := b.countIn("frames", time.Second); n != 0 || strings.HasPrefix(still, "0:") || b.glenzPixels() != still {
		t.Fatalf("under reduced motion: %d more frames, pixels %s", n, still)
	}
}

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/png"
	"io"
	"math"
	"net/http"
	"os/exec"
	"regexp"
	"runtime"
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

// chrome is a headless Chrome or Chromium with the given flags. On macOS Chrome lives in its app bundle, off PATH, and
// a symlink to it fails to start. Without a mock keychain it asks the user for the macOS keychain.
func chrome(t *testing.T, flags ...string) *exec.Cmd {
	t.Helper()
	for _, name := range []string{"google-chrome", "chromium", "chromium-browser", "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"} {
		if path, err := exec.LookPath(name); err == nil {
			return exec.Command(path, append([]string{"--headless=new", "--remote-debugging-port=0", "--no-first-run", "--use-mock-keychain", "--password-store=basic"}, flags...)...)
		}
	}
	t.Skip("needs Chrome")
	return nil
}

// newBrowser starts headless Chrome with the given flags and connects to its first page over the DevTools protocol.
func newBrowser(t *testing.T, flags ...string) *browser {
	t.Helper()
	cmd := chrome(t, append([]string{"--user-data-dir=" + t.TempDir(), "about:blank"}, flags...)...)
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

// glenzProbe counts the frames the canvas draws, the animation frames the page asks for and the page maps it builds, one
// text range each.
const glenzProbe = `window.draws = 0;
window.frames = 0;
window.maps = 0;
const count = (proto, name, counter, mine) => {
  const f = proto[name];
  proto[name] = function (...args) {
    if (mine(this)) window[counter]++;
    return f.apply(this, args);
  };
};
const glenz = (ctx) => ctx.canvas.classList.contains("glenz");
count(CanvasRenderingContext2D.prototype, "clearRect", "draws", glenz);
count(WebGL2RenderingContext.prototype, "drawArrays", "draws", glenz);
count(Document.prototype, "createRange", "maps", () => true);
count(window, "requestAnimationFrame", "frames", () => true);`

func (b *browser) counter(name string) int {
	b.t.Helper()
	var n int
	b.eval("window."+name, &n)
	return n
}

func (b *browser) countIn(name string, d time.Duration) int {
	b.t.Helper()
	before := b.counter(name)
	time.Sleep(d)
	return b.counter(name) - before
}

func (b *browser) tab(name string) {
	b.t.Helper()
	b.eval(`[...document.querySelectorAll(".tabs .tab")].find((t) => t.textContent.trim() === "`+name+`").click(), true`, new(bool))
	time.Sleep(200 * time.Millisecond)
}

// glenzShot returns a screenshot of the canvas and how many of its pixels differ from the sidebar behind it. The rest of the
// sidebar is hidden for the shot, so the sessions and the system load under the overlay do not count.
func (b *browser) glenzShot() (image.Image, int) {
	b.t.Helper()
	var box struct{ X, Y, Width, Height float64 }
	b.eval(`(() => { const s = document.createElement("style"); s.id = "glenz-alone"; s.textContent = "aside.sidebar > :not(.glenz) { visibility: hidden !important; }"; document.head.append(s);
		const r = document.querySelector("canvas.glenz").getBoundingClientRect(); return { x: r.x, y: r.y, width: r.width, height: r.height }; })()`, &box)
	var shot struct{ Data []byte }
	json.Unmarshal(b.call("Page.captureScreenshot", map[string]any{"format": "png", "clip": map[string]any{"x": box.X, "y": box.Y, "width": box.Width, "height": box.Height, "scale": 1}}), &shot)
	b.eval(`document.getElementById("glenz-alone").remove(), true`, new(bool))
	img, err := png.Decode(bytes.NewReader(shot.Data))
	if err != nil {
		b.t.Fatal(err)
	}
	bg := img.At(0, 0)
	lit := 0
	for y := img.Bounds().Min.Y; y < img.Bounds().Max.Y; y++ {
		for x := img.Bounds().Min.X; x < img.Bounds().Max.X; x++ {
			if img.At(x, y) != bg {
				lit++
			}
		}
	}
	return img, lit
}

// litRows returns the first and last rows of a canvas shot that hold drawn pixels, in CSS pixels from the canvas top.
func litRows(img image.Image, height float64) (top, bottom float64) {
	bg, scale := img.At(0, 0), float64(img.Bounds().Dy())/height
	first, last := -1, -1
	for y := img.Bounds().Min.Y; y < img.Bounds().Max.Y; y++ {
		for x := img.Bounds().Min.X; x < img.Bounds().Max.X; x++ {
			if img.At(x, y) != bg {
				if first < 0 {
					first = y
				}
				last = y
				break
			}
		}
	}
	return float64(first-img.Bounds().Min.Y) / scale, float64(last-img.Bounds().Min.Y+1) / scale
}

func same(a, b image.Image) bool {
	for y := a.Bounds().Min.Y; y < a.Bounds().Max.Y; y++ {
		for x := a.Bounds().Min.X; x < a.Bounds().Max.X; x++ {
			if a.At(x, y) != b.At(x, y) {
				return false
			}
		}
	}
	return true
}

// checkGlenz checks the animation in a browser started with the given flags and returns whether it drew with WebGL.
func checkGlenz(t *testing.T, flags ...string) bool {
	e := newEnv(t)
	id := e.mustKK("new", "claude", "-C", e.work, "Under the glenz")
	b := newBrowser(t, flags...)
	b.call("Page.enable", nil)
	b.call("Page.addScriptToEvaluateOnNewDocument", map[string]any{"source": glenzProbe})
	b.open(e.url, 1400, 900, false)
	b.waitFor("the first frame", "window.draws > 0")
	var webgl bool
	b.eval(`!!document.querySelector("canvas.glenz").getContext("webgl2")`, &webgl)

	var size struct{ Width, Height, CanvasWidth, CanvasHeight, SidebarWidth, SidebarHeight, Groups, Below, Floor, Chart float64 }
	measure := `(() => { const c = document.querySelector("canvas.glenz"); const r = c.getBoundingClientRect(); const s = document.querySelector("aside.sidebar");
		const below = document.querySelector(".load").getBoundingClientRect().top;
		return { width: r.width, height: r.height, canvasWidth: c.width, canvasHeight: c.height, sidebarWidth: s.clientWidth, sidebarHeight: s.clientHeight,
			groups: document.querySelector(".groups").getBoundingClientRect().bottom, below, floor: below - r.top,
			chart: document.querySelector(".load svg").getBoundingClientRect().top - r.top }; })()`
	b.eval(measure, &size)
	near := func(a, b float64) bool { return math.Abs(a-b) <= 1 }
	if !near(size.Width, size.SidebarWidth) || !near(size.Height, size.SidebarHeight) || !near(size.CanvasWidth, 2*size.Width) || !near(size.CanvasHeight, 2*size.Height) {
		t.Fatalf("the canvas does not cover the sidebar at the device pixel ratio: %+v", size)
	}
	if size.Groups != size.Below {
		t.Fatalf("the animation takes space between the session list and the system load: %+v", size)
	}

	first, lit := b.glenzShot()
	maps := b.counter("maps")
	if n := b.countIn("draws", 2*time.Second); n < 30 || n > 64 {
		t.Fatalf("%d frames in 2 s, want about 30 a second and never more", n)
	}
	if now, _ := b.glenzShot(); lit < 500 || same(first, now) {
		t.Fatalf("the solid is not drawn or does not move: %d pixels drawn", lit)
	}
	// Over one 6.8 s bounce the solid stays in a low band just above the system load chart: the floor plus 36 px of
	// travel and the solid's own height.
	high, low := size.Height, 0.0
	for end := time.Now().Add(7200 * time.Millisecond); time.Now().Before(end); time.Sleep(250 * time.Millisecond) {
		img, _ := b.glenzShot()
		top, bottom := litRows(img, size.Height)
		high, low = math.Min(high, top), math.Max(low, bottom)
	}
	if low >= size.Chart || high < size.Floor-130 {
		t.Fatalf("the solid reaches from %.0f to %.0f px, want within 130 px above the floor at %.0f and above the chart at %.0f", high, low, size.Floor, size.Chart)
	}
	if n := b.counter("maps") - maps; n != 0 {
		t.Fatalf("the page map was drawn %d times while nothing changed", n)
	}

	b.tab("Settings")
	if n := b.countIn("frames", time.Second); n != 0 {
		t.Fatalf("%d animation frames while the sidebar is hidden", n)
	}
	maps = b.counter("maps")
	b.tab("All")
	if n := b.countIn("draws", time.Second); n == 0 {
		t.Fatal("the animation did not come back with the Agents tab")
	}
	b.call("Emulation.setDeviceMetricsOverride", map[string]any{"width": 1300, "height": 900, "deviceScaleFactor": 2, "mobile": false})
	time.Sleep(200 * time.Millisecond)
	if n := b.counter("maps") - maps; webgl && n == 0 || !webgl && n != 0 {
		t.Fatalf("the page map was drawn %d times after the tab changed and the window resized, with WebGL %v", n, webgl)
	}

	b.open(e.url, 1400, 600, false)
	if n := b.countIn("draws", time.Second); n == 0 {
		t.Fatal("the animation does not draw on a short window")
	}

	b.open(e.url, 1400, 900, true)
	b.waitFor("the still frame", "window.draws > 0")
	still, lit := b.glenzShot()
	if n := b.countIn("frames", time.Second); n != 0 || lit < 500 {
		t.Fatalf("under reduced motion: %d more frames, %d pixels drawn", n, lit)
	}
	if now, _ := b.glenzShot(); !same(still, now) {
		t.Fatal("the still frame changed under reduced motion")
	}
	b.eval(measure, &size)
	if top, bottom := litRows(still, size.Height); bottom >= size.Chart || top < size.Floor-130 {
		t.Fatalf("the still frame spans %.0f to %.0f px, want above the chart at %.0f and near the floor at %.0f", top, bottom, size.Chart, size.Floor)
	}

	b.open(e.url, 1400, 900, false)
	var tile struct {
		X, Y  float64
		Under bool
	}
	b.waitFor("the session card", `!!document.querySelector(".tile[data-chain='`+id+`']")`)
	b.eval(`(() => { const r = document.querySelector(".tile[data-chain='`+id+`']").getBoundingClientRect(); const x = r.x + r.width / 2, y = r.y + r.height / 2;
		const c = document.querySelector("canvas.glenz").getBoundingClientRect();
		return { x, y, under: x > c.left && x < c.right && y > c.top && y < c.bottom && getComputedStyle(document.querySelector("canvas.glenz")).zIndex > 0 }; })()`, &tile)
	if !tile.Under {
		t.Fatalf("the session card is not under the overlay: %+v", tile)
	}
	for _, kind := range []string{"mousePressed", "mouseReleased"} {
		b.call("Input.dispatchMouseEvent", map[string]any{"type": kind, "x": tile.X, "y": tile.Y, "button": "left", "clickCount": 1})
	}
	b.waitFor("the session to open from a click through the overlay", `!!document.querySelector(".tile.on[data-chain='`+id+`']")`)
	return webgl
}

// The machine's GPU draws with WebGL and reflects the page; without one the 2D canvas draws, as software WebGL is slower.
func TestGlenz(t *testing.T) {
	angle := "--use-angle=gl"
	if runtime.GOOS == "darwin" {
		angle = "--use-angle=metal"
	}
	webgl := checkGlenz(t, "--enable-gpu", "--ignore-gpu-blocklist", angle)
	t.Logf("drawn with WebGL: %v", webgl)
}

func TestGlenzWithoutWebGL(t *testing.T) {
	if checkGlenz(t, "--disable-3d-apis") {
		t.Fatal("drawn with WebGL although it is disabled")
	}
}

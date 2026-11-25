package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// page is a tab of a headless Chrome, driven over the DevTools protocol.
type page struct {
	t    *testing.T
	ctx  context.Context
	ws   *websocket.Conn
	last int
}

func (e *env) openPage(width, height int) *page {
	t := e.t
	t.Helper()
	var chrome string
	for _, name := range []string{"google-chrome", "chromium", "chromium-browser"} {
		if path, err := exec.LookPath(name); err == nil {
			chrome = path
			break
		}
	}
	if chrome == "" {
		t.Skip("needs Chrome")
	}
	profile := t.TempDir()
	cmd := exec.Command(chrome, "--headless=new", "--remote-debugging-port=0", "--user-data-dir="+profile, "--no-first-run", "about:blank")
	cmd.Env = append(os.Environ(), "HOME="+profile, "XDG_CONFIG_HOME="+profile, "XDG_CACHE_HOME="+profile)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// Chrome's helpers outlive it for a moment and still write into the profile.
	t.Cleanup(func() {
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		cmd.Wait()
		for end := time.Now().Add(10 * time.Second); syscall.Kill(-cmd.Process.Pid, 0) == nil && time.Now().Before(end); {
			time.Sleep(10 * time.Millisecond)
		}
	})
	var socket string
	for end := time.Now().Add(20 * time.Second); socket == ""; time.Sleep(50 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatal("Chrome did not start")
		}
		data, _ := os.ReadFile(filepath.Join(profile, "DevToolsActivePort"))
		port, _, found := strings.Cut(string(data), "\n")
		if !found {
			continue
		}
		resp, err := http.Get("http://127.0.0.1:" + port + "/json/list")
		if err != nil {
			continue
		}
		var targets []struct {
			Type   string `json:"type"`
			Socket string `json:"webSocketDebuggerUrl"`
		}
		json.NewDecoder(resp.Body).Decode(&targets)
		resp.Body.Close()
		for _, target := range targets {
			if target.Type == "page" {
				socket = target.Socket
			}
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	ws, _, err := websocket.Dial(ctx, socket, nil)
	if err != nil {
		t.Fatal(err)
	}
	ws.SetReadLimit(-1)
	p := &page{t: t, ctx: ctx, ws: ws}
	p.resize(width, height)
	p.call("Page.navigate", map[string]any{"url": e.url})
	p.until(`document.querySelector("nav.tabs")`)
	return p
}

func (p *page) call(method string, params any) json.RawMessage {
	p.t.Helper()
	p.last++
	data, _ := json.Marshal(map[string]any{"id": p.last, "method": method, "params": params})
	if err := p.ws.Write(p.ctx, websocket.MessageText, data); err != nil {
		p.t.Fatalf("%s: %v", method, err)
	}
	for {
		_, data, err := p.ws.Read(p.ctx)
		if err != nil {
			p.t.Fatalf("%s: %v", method, err)
		}
		var answer struct {
			ID     int             `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  json.RawMessage `json:"error"`
		}
		json.Unmarshal(data, &answer)
		if answer.ID != p.last {
			continue
		}
		if answer.Error != nil {
			p.t.Fatalf("%s: %s", method, answer.Error)
		}
		return answer.Result
	}
}

func (p *page) resize(width, height int) {
	p.t.Helper()
	p.call("Emulation.setDeviceMetricsOverride", map[string]any{"width": width, "height": height, "deviceScaleFactor": 1, "mobile": false})
}

// eval runs a JavaScript expression in the page and decodes its value into out.
func (p *page) eval(expression string, out any) {
	p.t.Helper()
	var r struct {
		Result struct {
			Value json.RawMessage `json:"value"`
		} `json:"result"`
		Exception json.RawMessage `json:"exceptionDetails"`
	}
	json.Unmarshal(p.call("Runtime.evaluate", map[string]any{"expression": expression, "returnByValue": true}), &r)
	if r.Exception != nil {
		p.t.Fatalf("%s: %s", expression, r.Exception)
	}
	if out != nil {
		if err := json.Unmarshal(r.Result.Value, out); err != nil {
			p.t.Fatalf("%s: %v: %s", expression, err, r.Result.Value)
		}
	}
}

func (p *page) until(expression string) {
	p.t.Helper()
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		var ok bool
		p.eval("!!("+expression+")", &ok)
		if ok {
			return
		}
	}
	p.t.Fatalf("timed out waiting for %s", expression)
}

func (p *page) click(element string) {
	p.t.Helper()
	p.until(element)
	p.eval(element+".click()", nil)
}

func TestSettingsViewIsCentered(t *testing.T) {
	e := newEnv(t)
	p := e.openPage(1600, 900)
	p.click(`[...document.querySelectorAll("nav.tabs .tab")].find((a) => a.textContent === "Settings")`)
	p.until(`document.querySelector(".settings form")`)
	for _, width := range []int{1600, 900} {
		p.resize(width, 900)
		var gap struct{ Left, Right, Width, Below float64 }
		p.eval(`(() => {
			const view = document.querySelector("main.settings"), box = view.getBoundingClientRect()
			const form = view.querySelector("form").getBoundingClientRect()
			const row = view.querySelector(".row").getBoundingClientRect()
			return { left: form.left - box.left, right: box.left + view.clientWidth - form.right, width: form.width, below: box.bottom - row.bottom }
		})()`, &gap)
		if math.Abs(gap.Left-gap.Right) > 1 || gap.Left < 16 || gap.Width > 720 {
			t.Errorf("%d wide: the form is not centered: %+v", width, gap)
		}
		if math.Abs(gap.Below) > 1 {
			t.Errorf("%d wide: the form shows through below the pinned buttons: %+v", width, gap)
		}
	}
}

func TestConductorToggle(t *testing.T) {
	e := newEnv(t)
	p := e.openPage(1600, 900)
	type toggle struct {
		Text, Pressed, Background, Mark string
		Panel                           bool
	}
	read := func() (got toggle) {
		t.Helper()
		p.eval(`(() => {
			const b = document.querySelector("nav.tabs .conductor-toggle")
			return {
				text: b.textContent,
				pressed: b.getAttribute("aria-pressed"),
				background: getComputedStyle(b).backgroundColor,
				mark: getComputedStyle(b, "::before").backgroundColor,
				panel: document.querySelector(".side-panel").getClientRects().length > 0,
			}
		})()`, &got)
		return got
	}
	press := func() {
		t.Helper()
		p.click(`document.querySelector("nav.tabs .conductor-toggle")`)
	}
	on := read()
	if on.Text != "Conductor" || on.Pressed != "true" || !on.Panel {
		t.Fatalf("at the start: %+v", on)
	}
	press()
	p.until(`!document.querySelector(".side-panel").getClientRects().length`)
	off := read()
	if off.Text != "Conductor" || off.Pressed != "false" {
		t.Fatalf("switched off: %+v", off)
	}
	if off.Background == on.Background || off.Mark == on.Mark {
		t.Fatalf("the two states look the same: on %+v, off %+v", on, off)
	}
	p.call("Page.reload", nil)
	p.until(`document.querySelector("nav.tabs .conductor-toggle") && performance.getEntriesByType("navigation")[0].type === "reload"`)
	if got := read(); got != off {
		t.Fatalf("after a reload: %+v, want %+v", got, off)
	}
	press()
	p.until(`document.querySelector(".side-panel").getClientRects().length`)
	if got := read(); got != on {
		t.Fatalf("switched on again: %+v, want %+v", got, on)
	}
}

func TestNewSessionWindowKeepsItsSize(t *testing.T) {
	e := newEnv(t)
	project := filepath.Join(filepath.Dir(e.home), ".claude", "projects", "-work")
	os.MkdirAll(project, 0o755)
	for i := range 12 {
		line := fmt.Sprintf(`{"type":"user","timestamp":"2026-11-20T10:00:00Z","cwd":%q,"message":{"content":"earlier question %d"}}`, e.work, i)
		os.WriteFile(filepath.Join(project, fmt.Sprintf("11111111-2222-4333-8444-%012d.jsonl", i)), []byte(line+"\n"), 0o644)
	}
	p := e.openPage(1600, 1100)
	p.click(`document.querySelector(".sidebar-head > button")`)
	type window struct {
		Width, Height, Content, Visible, Inner float64
		Overflow                               string
	}
	read := func(state string) (got window) {
		t.Helper()
		p.eval(`(() => {
			const w = document.querySelector(".new-session"), box = w.getBoundingClientRect()
			return { width: box.width, height: box.height, content: w.scrollHeight, visible: w.clientHeight, inner: w.clientWidth, overflow: getComputedStyle(w).overflowY }
		})()`, &got)
		if got.Overflow != "scroll" || got.Content <= got.Visible {
			t.Errorf("%s: the window does not scroll: %+v", state, got)
		}
		return got
	}
	same := func(state string, want window) {
		t.Helper()
		if got := read(state); got.Width != want.Width || got.Height != want.Height || got.Inner != want.Inner {
			t.Errorf("%s: the window changed size: %+v, want %+v", state, got, want)
		}
	}
	program := func(name string) {
		t.Helper()
		p.eval(fmt.Sprintf(`(() => { const s = document.querySelector(".new-session select"); s.value = %q; s.dispatchEvent(new Event("change", { bubbles: true })) })()`, name), nil)
	}
	p.until(`document.querySelectorAll(".past .tile").length === 12`)
	full := read("with earlier sessions")
	if full.Height != 720 {
		t.Errorf("height: %+v", full)
	}
	p.until(`[...document.querySelector(".new-session select").options].some((o) => o.value === "codex")`)
	program("codex")
	p.until(`!document.querySelector(".past .tile")`)
	same("without earlier sessions", full)
	p.click(`[...document.querySelectorAll(".new-session button")].find((b) => b.getAttribute("aria-label") === "Cancel")`)
	p.click(`document.querySelector(".sidebar-head > button")`)
	same("opened again", full)

	// A screen lower than the window limits it to the screen.
	p.resize(1600, 500)
	program("claude")
	p.until(`document.querySelectorAll(".past .tile").length === 12`)
	low := read("on a low screen")
	if low.Height != 500-2*12 {
		t.Errorf("height on a low screen: %+v", low)
	}
	program("codex")
	p.until(`!document.querySelector(".past .tile")`)
	same("on a low screen without earlier sessions", low)
}

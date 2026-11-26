package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
	p.click(`[...document.querySelectorAll(".new-session button")].find((b) => b.getAttribute("aria-label") === "Cancel")`)

	// The schedule dialog borrows the look of the window and keeps the size of its content.
	id := e.mustKK("new", "claude", "-C", e.work, "hello")
	p.eval(fmt.Sprintf(`location.hash = %q`, "~"+id), nil)
	p.click(`[...document.querySelectorAll(".panels button")].find((b) => b.getAttribute("aria-label") === "Schedule")`)
	p.until(`document.querySelector(".new-session.schedule")`)
	var schedule struct{ Height, Content float64 }
	p.eval(`(() => { const w = document.querySelector(".new-session.schedule"); return { height: w.offsetHeight, content: w.scrollHeight + w.offsetHeight - w.clientHeight } })()`, &schedule)
	if schedule.Height >= 720 || math.Abs(schedule.Height-schedule.Content) > 1 {
		t.Errorf("the schedule dialog: %+v", schedule)
	}
	p.click(`[...document.querySelectorAll(".new-session.schedule button")].find((b) => b.getAttribute("aria-label") === "Cancel")`)
	p.click(`document.querySelector(".sidebar-head > button")`)

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

// worktreeSessions starts or resumes sessions in and outside git worktrees and returns, by title, the ids and whether
// each agent works in a worktree.
func (e *env) worktreeSessions() (map[string]string, map[string]bool) {
	t := e.t
	t.Helper()
	root := filepath.Dir(e.home)
	git := func(dir string, args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=kk", "-c", "user.email=kk@example.com"}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git(e.work, "init", "-q", "-b", "trunk")
	os.MkdirAll(filepath.Join(e.work, "sub"), 0o755)
	os.WriteFile(filepath.Join(e.work, "sub", "file"), []byte("x"), 0o644)
	git(e.work, "add", ".")
	git(e.work, "commit", "-q", "-m", "first")
	side, own := filepath.Join(root, "side"), filepath.Join(e.work, ".claude", "worktrees", "own")
	git(e.work, "worktree", "add", "-q", "-b", "side", side)
	git(e.work, "worktree", "add", "-q", "-b", "own", own)
	link, separate, bare := filepath.Join(root, "link"), filepath.Join(root, "separate"), filepath.Join(root, "no-git")
	os.Symlink(filepath.Join(side, "sub"), link)
	os.MkdirAll(bare, 0o755)
	// Like a submodule, this checkout has a .git file and is not a linked worktree.
	git(root, "init", "-q", "--separate-git-dir", filepath.Join(root, "separate.git"), separate)

	ids, inWorktree := map[string]string{}, map[string]bool{}
	start := func(title, dir string, worktree bool, flags ...string) {
		args := append([]string{"new", "codex", "-C", dir}, flags...)
		ids[title], inWorktree[title] = e.mustKK(append(args, title)...), worktree
	}
	start("Investigate why the nightly pipeline fails on the main branch only", e.work, true, "-w")
	start("Rename_the_configuration_loader_and_every_caller_of_it_in_one_go", side, true)
	start("Made by hand", filepath.Join(side, "sub"), true)
	start("Behind a link", link, true)
	start("Main checkout", filepath.Join(e.work, "sub"), false)
	start("Separate git directory", separate, false)
	start("Outside git", bare, false)

	project := filepath.Join(root, ".claude", "projects", "-work")
	os.MkdirAll(project, 0o755)
	resume := func(n int, title, dir string, worktree bool) {
		session := fmt.Sprintf("11111111-2222-4333-8444-%012d", n)
		lines := fmt.Sprintf(`{"type":"user","timestamp":"2026-11-20T10:00:00Z","cwd":%q,"message":{"content":"earlier question"}}`+"\n"+`{"type":"ai-title","aiTitle":%q}`+"\n", dir, title)
		os.WriteFile(filepath.Join(project, session+".jsonl"), []byte(lines), 0o644)
		code, body := e.post("/api/conversations", map[string]string{"harness": "claude", "session": session})
		var c convView
		if err := json.Unmarshal([]byte(body), &c); code != 200 || err != nil {
			t.Fatalf("resume %s: %d %s", title, code, body)
		}
		ids[title], inWorktree[title] = c.ID, worktree
	}
	resume(1, "Resumed in a worktree", own, true)
	resume(2, "Resumed in the checkout", e.work, false)
	return ids, inWorktree
}

func TestWorktreeSessionsAreMarked(t *testing.T) {
	e := newEnv(t)
	ids, want := e.worktreeSessions()
	check := func(state string) {
		t.Helper()
		for title, id := range ids {
			if got := e.conv(id).InWorktree; got != want[title] {
				t.Errorf("%s: %q in a worktree: %v, want %v", state, title, got, want[title])
			}
		}
	}
	check("at the start")
	// A removed worktree stays marked when kk made it or its path says what it was.
	kk := e.conv(ids["Investigate why the nightly pipeline fails on the main branch only"])
	for _, dir := range []string{kk.Dir, e.conv(ids["Resumed in a worktree"]).Dir} {
		if err := os.RemoveAll(dir); err != nil {
			t.Fatal(err)
		}
	}
	check("after the worktrees were removed")
}

func TestWorktreeIcon(t *testing.T) {
	e := newEnv(t)
	ids, want := e.worktreeSessions()
	for title, id := range ids {
		if !strings.HasPrefix(title, "Resumed") {
			e.waitFor(title, id, func(c convView) bool { return c.Status == statusIdle })
		}
	}
	p := e.openPage(1600, 1000)
	type card struct {
		Title, Label, Tooltip, Dir string
		Icon                       bool
		// Distances in pixels: from the title to the icon, from the icon to the card's inner edge, and between the middles of the icon and the title's first line.
		Gap, Edge, Middle float64
		// Start is how far below the top of the title its text begins, past the mark of an unread card.
		Width, Height, Lines, Start float64
	}
	for _, view := range []struct{ tab, cards string }{{"Agents", ".sidebar .tile"}, {"Sessions", ".board .tile"}} {
		p.click(fmt.Sprintf(`[...document.querySelectorAll("nav.tabs .tab")].find((a) => a.textContent === %q)`, view.tab))
		p.until(fmt.Sprintf(`document.querySelectorAll(%q).length === %d`, view.cards, len(ids)))
		var cards []card
		p.eval(fmt.Sprintf(`[...document.querySelectorAll(%q)].map((tile) => {
			const title = tile.querySelector(".title"), icon = tile.querySelector(".icon-worktree")
			const out = { title: title.textContent, icon: !!icon }
			const line = parseFloat(getComputedStyle(title).lineHeight), t = title.getBoundingClientRect()
			out.lines = t.height / line
			const first = document.createRange()
			first.setStart(title.firstChild, 0)
			first.setEnd(title.firstChild, 1)
			out.start = first.getBoundingClientRect().top - t.top
			if (!icon) return out
			const i = icon.getBoundingClientRect(), s = getComputedStyle(tile)
			return { ...out, label: icon.getAttribute("aria-label"), tooltip: icon.title, width: i.width, height: i.height,
				gap: i.left - t.right, edge: tile.getBoundingClientRect().right - parseFloat(s.paddingRight) - parseFloat(s.borderRightWidth) - i.right,
				middle: i.top + i.height / 2 - (t.top + line / 2) }
		})`, view.cards), &cards)
		long := 0
		for _, c := range cards {
			if c.Icon != want[c.Title] {
				t.Errorf("%s: icon on %q: %v, want %v", view.tab, c.Title, c.Icon, want[c.Title])
			}
			if c.Lines > 2.05 || c.Start > 10 {
				t.Errorf("%s: %q takes %.1f lines, starting %.0f px down", view.tab, c.Title, c.Lines, c.Start)
			}
			if !c.Icon {
				continue
			}
			if c.Lines > 1.5 {
				long++
			}
			if dir := e.conv(ids[c.Title]).Dir; c.Label != "Git worktree" || c.Tooltip != "Works in a git worktree: "+dir {
				t.Errorf("%s: tooltip of %q: %q %q", view.tab, c.Title, c.Label, c.Tooltip)
			}
			if c.Width != 12 || c.Height != 12 || c.Gap < 6 || math.Abs(c.Edge) > 0.5 || math.Abs(c.Middle) > 1 {
				t.Errorf("%s: icon of %q is out of place: %+v", view.tab, c.Title, c)
			}
		}
		if long == 0 {
			t.Errorf("%s: no title long enough to wrap next to the icon", view.tab)
		}
	}
}

func TestLogo(t *testing.T) {
	e := newEnv(t)
	p := e.openPage(1600, 900)
	for _, width := range []int{1600, 800, 420} {
		p.resize(width, 900)
		var got struct {
			Text, Bold, Rest, Transform, Line, TabLine string
			// Distances in pixels: of the logo from the left end of the bar and from the first tab, between their bottom edges
			// and between the baselines of their texts. Hidden is how much of the logo does not fit in its box.
			Left, ToTabs, Bottom, Baseline, Hidden float64
		}
		p.eval(`(() => {
			const bar = document.querySelector("nav.tabs"), logo = bar.querySelector(".logo"), tab = bar.querySelector(".tab")
			const weights = { bold: "", rest: "" }
			for (const walk = document.createTreeWalker(logo, NodeFilter.SHOW_TEXT); walk.nextNode(); ) {
				const weight = Number(getComputedStyle(walk.currentNode.parentElement).fontWeight)
				weights[weight >= 700 ? "bold" : weight <= 400 ? "rest" : "between"] += walk.currentNode.data
			}
			// An empty inline block sits on the baseline.
			const baseline = (text) => {
				const mark = text.appendChild(document.createElement("i"))
				mark.style.display = "inline-block"
				const y = mark.getBoundingClientRect().bottom
				mark.remove()
				return y
			}
			const line = (el) => { const s = getComputedStyle(el); return s.borderBottomWidth + " " + s.borderBottomColor }
			const l = logo.getBoundingClientRect(), t = tab.getBoundingClientRect()
			return { text: logo.textContent, ...weights, transform: getComputedStyle(logo).textTransform, line: line(logo), tabLine: line(bar.querySelector(".tab:not(.on)")),
				left: l.left - bar.getBoundingClientRect().left, toTabs: t.left - l.right, bottom: l.bottom - t.bottom,
				baseline: baseline(logo) - baseline(tab), hidden: logo.scrollWidth - logo.clientWidth }
		})()`, &got)
		if got.Text != "konduktor" || got.Bold != "kk" || got.Rest != "ondutor" || got.Transform != "none" {
			t.Errorf("%d wide: the logo reads wrong: %+v", width, got)
		}
		if got.Left != 0 || got.ToTabs != 0 || math.Abs(got.Bottom) > 0.5 || math.Abs(got.Baseline) > 0.5 || got.Hidden > 0 || got.Line != got.TabLine {
			t.Errorf("%d wide: the logo is out of place: %+v", width, got)
		}
	}
}

func TestNoAccentStripes(t *testing.T) {
	e := newEnv(t, "KK_COORDINATOR=1")
	for _, name := range []string{"alpha", "beta"} {
		dir := filepath.Join(filepath.Dir(e.home), ".claude", "skills", name)
		os.MkdirAll(dir, 0o755)
		os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: "+name+"\ndescription: A skill\n---\n"), 0o644)
	}
	idle := func(c convView) bool { return c.Status == statusIdle }
	e.waitFor("conductor start", coordinatorID, idle)
	escalated := e.asConductor("new", "claude", "-C", e.work, "escalated work")
	e.waitFor("a stop to escalate", escalated, idle)
	e.asConductor("escalate", escalated, "Pick a database")
	quoting := e.mustKK("new", "claude", "-C", e.work, "> a quoted line\n\nmy reply")
	e.waitFor("answer to the quote", quoting, idle)

	p := e.openPage(1600, 900)
	p.eval(fmt.Sprintf(`location.hash = %q`, "~"+quoting), nil)
	p.click(`document.querySelector(".panels textarea")`)
	p.eval(`document.querySelector(".panels textarea").focus()`, nil)
	p.call("Input.insertText", map[string]any{"text": "/"})
	p.until(`document.querySelectorAll(".suggest li").length === 2 && document.querySelector(".msg.user blockquote") && document.querySelector(".sidebar .tile.unread .reason")`)

	var got struct {
		Stripes                                                     []string
		Rules                                                       int
		Dot, Shadow, Reason, Quote, Selected, SelectedShadow, Other string
		// The space between the frame of the quote and its text, above it and below it.
		Above, Below float64
	}
	p.eval(`(() => {
		const stripes = []
		let rules = 0
		// A stripe is a left border of 2px or more that the top does not share, or an inset shadow pushed to one side.
		const visit = (list) => {
			for (const rule of list) {
				if (rule.cssRules?.length) visit(rule.cssRules)
				if (!rule.style || rule.selectorText?.includes(".xterm")) continue
				rules++
				const s = rule.style
				// Chrome keeps a shorthand with a variable only in the text of the rule.
				const border = /border-(left|inline-start)(-width)?:\s*([2-9]|[1-9]\d)(\.\d+)?px/.test(s.cssText) ||
					(parseFloat(s.borderLeftWidth) >= 2 && s.borderLeftWidth !== s.borderTopWidth)
				const shadow = s.getPropertyValue("box-shadow").split(/,(?![^(]*\))/).some((part) =>
					part.includes("inset") && part.replace(/(var|rgba?)\([^)]*\)|#\w+|inset/g, "").trim().split(/\s+/).slice(0, 2).some((v) => parseFloat(v)))
				if (border || shadow) stripes.push(rule.selectorText)
			}
		}
		for (const sheet of document.styleSheets) visit(sheet.cssRules)
		const style = (selector, pseudo) => getComputedStyle(document.querySelector(selector), pseudo)
		const frame = (s) => [s.borderTopWidth, s.borderRightWidth, s.borderBottomWidth, s.borderLeftWidth, s.borderLeftStyle].join(" ")
		const outline = (s) => s.outlineStyle + " " + s.outlineWidth
		const quote = document.querySelector(".msg.user blockquote"), box = quote.getBoundingClientRect()
		return {
			above: quote.firstElementChild.getBoundingClientRect().top - box.top,
			below: box.bottom - quote.lastElementChild.getBoundingClientRect().bottom,
			stripes, rules,
			dot: style(".sidebar .tile.unread .title", "::before").content,
			shadow: style(".sidebar .tile.unread").boxShadow,
			reason: frame(style(".sidebar .tile .reason")),
			quote: frame(style(".msg.user blockquote")),
			selected: outline(style(".suggest li.on")),
			selectedShadow: style(".suggest li.on").boxShadow,
			other: style(".suggest li:not(.on)").outlineStyle,
		}
	})()`, &got)
	if got.Rules < 100 || len(got.Stripes) > 0 {
		t.Errorf("accent stripes in %d rules: %q", got.Rules, got.Stripes)
	}
	const framed = "1px 1px 1px 1px solid"
	if !strings.HasPrefix(got.Dot, `"●`) || got.Shadow != "none" {
		t.Errorf("unread card: dot %q, shadow %q", got.Dot, got.Shadow)
	}
	if got.Reason != framed || got.Quote != framed {
		t.Errorf("escalation reason %q and quote %q are not framed", got.Reason, got.Quote)
	}
	if math.Abs(got.Above-got.Below) > 0.5 {
		t.Errorf("the quote has %.0f px above its text and %.0f px below", got.Above, got.Below)
	}
	if got.Selected != "solid 1px" || got.SelectedShadow != "none" || got.Other != "none" {
		t.Errorf("selected suggestion: %q, shadow %q, others %q", got.Selected, got.SelectedShadow, got.Other)
	}
}

// press sends a key to the page the way a keyboard does.
func (p *page) press(key string, code int, text string) {
	p.t.Helper()
	p.call("Input.dispatchKeyEvent", map[string]any{"type": "keyDown", "key": key, "windowsVirtualKeyCode": code, "text": text})
	p.call("Input.dispatchKeyEvent", map[string]any{"type": "keyUp", "key": key, "windowsVirtualKeyCode": code})
}

// tap clicks the middle of an element with the mouse.
func (p *page) tap(element string) {
	p.t.Helper()
	p.until(element)
	var at struct{ X, Y float64 }
	p.eval(fmt.Sprintf(`(() => { const e = %s; e.scrollIntoView({ block: "center" }); const r = e.getBoundingClientRect(); return { x: r.x + r.width / 2, y: r.y + r.height / 2 } })()`, element), &at)
	for _, kind := range []string{"mousePressed", "mouseReleased"} {
		p.call("Input.dispatchMouseEvent", map[string]any{"type": kind, "x": at.X, "y": at.Y, "button": "left", "clickCount": 1})
	}
}

func TestMessageImages(t *testing.T) {
	e := newEnv(t)
	dark := image.NewRGBA(image.Rect(0, 0, 2400, 1600))
	draw.Draw(dark, dark.Bounds(), image.NewUniform(color.RGBA{20, 20, 20, 255}), image.Point{}, draw.Src)
	var data bytes.Buffer
	png.Encode(&data, dark)
	attached, inline, linked := e.upload("attached.png", data.Bytes()), e.upload("inline.png", data.Bytes()), e.upload("linked.png", data.Bytes())
	prompt := fmt.Sprintf("![inline](%s)\n\n[![linked](%s)](https://example.com/)\n\n%s", inline.URL, linked.URL, attached.Path)
	id := e.mustKK("new", "claude", "-C", e.work, prompt)
	e.waitFor("answer", id, func(c convView) bool { return c.Status == statusIdle })

	p := e.openPage(1600, 900)
	p.eval(fmt.Sprintf(`location.hash = %q`, "~"+id), nil)
	// The user's message and the echo of it hold the two markdown images, and the user's message the attachment.
	p.until(`[...document.querySelectorAll(".feed img")].filter((i) => i.naturalWidth).length >= 5`)
	var frames []struct {
		Alt, Width, Style string
		Brightness        float64
	}
	p.eval(`[...document.querySelectorAll(".feed img")].map((img) => {
		const s = getComputedStyle(img.closest(".attachments li") ?? img)
		const [r, g, b] = s.borderTopColor.match(/[\d.]+/g).map(Number)
		const doubled = img.closest(".attachments li") && parseFloat(getComputedStyle(img).borderTopWidth)
		return { alt: img.alt, width: doubled ? "doubled" : s.borderTopWidth + " " + s.borderLeftWidth, style: s.borderTopStyle, brightness: (r + g + b) / 3 }
	})`, &frames)
	for _, f := range frames {
		// The line colors of the page are too close to a dark picture to separate them.
		if f.Width != "1px 1px" || f.Style != "solid" || f.Brightness < 80 {
			t.Errorf("frame of %q: %+v", f.Alt, f)
		}
	}

	type viewer struct {
		Open, OnClose, Inside bool
		Src, Focus, Conductor string
		Width, Height         float64
	}
	read := func() (got viewer) {
		t.Helper()
		p.eval(`(() => {
			const d = document.querySelector("dialog.viewer"), a = document.activeElement
			const img = d?.querySelector("img"), box = img?.getBoundingClientRect()
			return { open: !!d?.open, onClose: a?.getAttribute("aria-label") === "Close" && !!d?.contains(a), inside: a === document.body || !!d?.contains(a),
				src: img?.getAttribute("src") ?? "", focus: a?.querySelector?.("img")?.alt ?? "", width: box?.width ?? 0, height: box?.height ?? 0,
				conductor: document.querySelector(".conductor-toggle").getAttribute("aria-pressed") }
		})()`, &got)
		return got
	}
	closed := func(how, focus string) {
		t.Helper()
		p.until(`!document.querySelector("dialog.viewer")`)
		if got := read(); got.Focus != focus || got.Conductor != "true" {
			t.Errorf("closed with %s: %+v, want the focus on %q", how, got, focus)
		}
	}
	picture := func(alt string) string {
		// The echo names the attachment's path too, and shows it as a thumbnail of its own.
		return fmt.Sprintf(`document.querySelector('.msg.assistant .md > p img[alt="%s"]') ?? document.querySelector('.feed .attachments img[alt="%s"]')`, alt, alt)
	}
	for alt, file := range map[string]Attachment{"inline": inline, "linked": linked, "attached.png": attached} {
		p.eval(fmt.Sprintf(`(%s).closest("a").focus()`, picture(alt)), nil)
		p.press("Enter", 13, "\r")
		p.until(`document.querySelector("dialog.viewer img")?.naturalWidth`)
		got := read()
		if !got.Open || !got.OnClose || !strings.HasSuffix(got.Src, file.URL) {
			t.Errorf("%s opened from the keyboard: %+v", alt, got)
		}
		// Large: as big as the screen allows, with its shape kept.
		if got.Height < 700 || math.Abs(got.Width/got.Height-1.5) > 0.01 {
			t.Errorf("%s is not shown large: %+v", alt, got)
		}
		for range 3 {
			p.press("Tab", 9, "")
			if got := read(); !got.Inside {
				t.Errorf("%s: Tab left the open image: %+v", alt, got)
			}
		}
		p.press("Escape", 27, "")
		closed("Escape", alt)

		p.tap(picture(alt))
		p.until(`document.querySelector("dialog.viewer[open] img")?.naturalWidth`)
		p.tap(`document.querySelector("dialog.viewer img")`)
		closed("a click on the image", alt)

		p.tap(picture(alt))
		p.until(`document.querySelector("dialog.viewer[open]")`)
		p.tap(`document.querySelector('dialog.viewer button[aria-label="Close"]')`)
		closed("the close button", alt)

		p.tap(picture(alt))
		p.until(`document.querySelector("dialog.viewer[open]")`)
		p.press("c", 67, "c")
		closed("the mnemonic of the close button", alt)
	}
	var url string
	p.eval(`location.href`, &url)
	if !strings.Contains(url, e.addr) {
		t.Errorf("a click on a linked image left the page: %s", url)
	}
}

func TestLocalImagesInChat(t *testing.T) {
	e := newEnv(t)
	dir := t.TempDir()
	var shots []string
	for i, size := range []image.Point{{1600, 900}, {800, 1200}, {300, 200}} {
		img := image.NewRGBA(image.Rectangle{Max: size})
		draw.Draw(img, img.Bounds(), image.NewUniform(color.RGBA{uint8(40 * i), 30, 30, 255}), image.Point{}, draw.Src)
		var data bytes.Buffer
		png.Encode(&data, img)
		shots = append(shots, filepath.Join(dir, fmt.Sprintf("shot-%d.png", i+1)))
		os.WriteFile(shots[i], data.Bytes(), 0o644)
	}
	gone := filepath.Join(dir, "gone.png")
	prompt := fmt.Sprintf("## Screenshots\n\n```\n%s\n```\n\nThe first is `%s`, and %s is gone.\n\n![second](%s)", strings.Join(shots, "\n"), shots[0], gone, shots[1])
	id := e.mustKK("new", "claude", "-C", e.work, prompt)
	e.waitFor("answer", id, func(c convView) bool { return c.Status == statusIdle })

	p := e.openPage(1600, 900)
	p.eval(fmt.Sprintf(`location.hash = %q`, "~"+id), nil)
	answer := `document.querySelector(".msg.assistant .md")`
	p.until(answer + `?.querySelectorAll(".images img").length === 4 && [...` + answer + `.querySelectorAll("img")].every((i) => i.naturalWidth)`)
	var got struct {
		Pre, InlineImage           bool
		Strips                     [][]string
		Titles                     []string
		Code, Plain                string
		StripHeight, Border, Width float64
	}
	p.eval(`(() => {
		const md = `+answer+`, strips = [...md.querySelectorAll(".images")]
		return {
			pre: !!md.querySelector("pre"),
			inlineImage: md.querySelector(":scope > p > a > img")?.getAttribute("src").startsWith("/api/conversations/"),
			strips: strips.map((s) => [...s.querySelectorAll("span")].map((c) => c.textContent)),
			titles: [...strips[0].querySelectorAll("a")].map((a) => a.title),
			code: md.querySelector("code > a.image")?.textContent ?? "",
			plain: [...md.querySelectorAll(":scope > p")].map((p) => p.textContent).join("\n"),
			stripHeight: strips[0].getBoundingClientRect().height,
			border: parseFloat(getComputedStyle(strips[0].querySelector("img")).borderTopWidth),
			width: strips[0].querySelector("img").getBoundingClientRect().width,
		}
	})()`, &got)
	names := []string{"shot-1.png", "shot-2.png", "shot-3.png"}
	if got.Pre || len(got.Strips) != 2 || !slices.Equal(got.Strips[0], names) || !slices.Equal(got.Strips[1], names[:1]) || !slices.Equal(got.Titles, shots) {
		t.Fatalf("a code block of paths did not become thumbnails named by file: %+v", got)
	}
	if got.Code != shots[0] || !got.InlineImage || !strings.Contains(got.Plain, gone+" is gone") {
		t.Fatalf("paths in the text: %+v", got)
	}
	// Three screenshots sit side by side in one row, framed like the other pictures.
	if got.StripHeight > 200 || got.Width > 300 || got.Border != 1 {
		t.Fatalf("thumbnails are not compact: %+v", got)
	}

	caption := func() string {
		var s string
		p.eval(`document.querySelector("dialog.viewer[open] .caption")?.textContent ?? ""`, &s)
		return s
	}
	p.tap(answer + `.querySelectorAll(".images img")[1]`)
	p.until(`document.querySelector("dialog.viewer[open] img")?.naturalWidth`)
	if c := caption(); c != shots[1] {
		t.Fatalf("the viewer shows %q, want %q", c, shots[1])
	}
	for _, want := range []string{shots[2], shots[0]} {
		p.press("ArrowRight", 39, "")
		if c := caption(); c != want {
			t.Fatalf("after the right arrow the viewer shows %q, want %q", c, want)
		}
	}
	p.press("Escape", 27, "")
	p.until(`!document.querySelector("dialog.viewer")`)
	p.tap(answer + `.querySelector("code > a.image")`)
	p.until(`document.querySelector("dialog.viewer[open]")`)
	if c := caption(); c != shots[0] {
		t.Fatalf("a path in code opens %q, want %q", c, shots[0])
	}
}

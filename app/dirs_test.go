package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// shot saves a screenshot of the page to KK_SHOTS, when set, for a person to look at.
func (p *page) shot(name string) {
	p.t.Helper()
	dir := os.Getenv("KK_SHOTS")
	if dir == "" {
		return
	}
	var r struct{ Data string }
	if err := json.Unmarshal(p.call("Page.captureScreenshot", map[string]any{"format": "png"}), &r); err != nil {
		p.t.Fatal(err)
	}
	data, _ := base64.StdEncoding.DecodeString(r.Data)
	os.WriteFile(filepath.Join(dir, name+".png"), data, 0o644)
}

// strings returns the text of every element a selector finds.
func (p *page) texts(selector string) []string {
	p.t.Helper()
	var out []string
	p.eval(fmt.Sprintf(`[...document.querySelectorAll(%q)].map((e) => e.textContent.trim())`, selector), &out)
	return out
}

func (p *page) hash() string {
	p.t.Helper()
	var h string
	p.eval(`location.hash`, &h)
	return h
}

// view clicks a tab of the second level.
func (p *page) view(name string) {
	p.t.Helper()
	p.click(fmt.Sprintf(`[...document.querySelectorAll(".views button")].find((b) => b.textContent === %q)`, name))
}

// dirTab clicks the tab of a directory, or All.
func (p *page) dirTab(name string) {
	p.t.Helper()
	p.click(fmt.Sprintf(`[...document.querySelectorAll("nav.tabs .dirs .tab")].find((a) => a.textContent.trim().endsWith(%q))`, name))
}

// dirSessions starts sessions in three directories: alpha has two features, beta one session, and gamma only a closed one.
func (e *env) dirSessions() map[string]string {
	ids := map[string]string{}
	for _, s := range []struct{ dir, group, title string }{
		{"beta", "Search index", "Rebuild the index"},
		{"alpha", "Billing export", "Export invoices"},
		{"alpha", "Search index", "Tune the ranking"},
		{"gamma", "Old work", "Finished long ago"},
	} {
		dir := filepath.Join(e.work, s.dir)
		os.MkdirAll(dir, 0o755)
		ids[s.title] = e.mustKK("new", "claude", "-C", dir, "-g", s.group, s.title)
		e.waitFor(s.title, ids[s.title], turnEnded)
	}
	e.mustKK("close", ids["Finished long ago"])
	return ids
}

func TestDirectoryTabs(t *testing.T) {
	e := newEnv(t)
	ids := e.dirSessions()
	alpha, gamma := filepath.Join(e.work, "alpha"), filepath.Join(e.work, "gamma")
	p := e.openPage(1600, 900)
	p.until(`document.querySelectorAll("nav.tabs .dirs .tab").length === 3`)
	if got := p.texts("nav.tabs .dirs .tab"); !slices.Equal(got, []string{"All", "alpha", "beta"}) {
		t.Fatalf("directory tabs: %q", got)
	}
	if got := p.texts("nav.tabs .tab.global"); !slices.Equal(got, []string{"Inbox", "Stats", "Logs", "Tmux", "Settings"}) {
		t.Fatalf("global tabs: %q", got)
	}
	// All shows every open session, grouped by directory, and the directory filter.
	p.until(`document.querySelectorAll(".sidebar .tile").length === 4`)
	if got := p.texts(".sidebar .group .name"); len(got) != 3 || !slices.Contains(got, "alpha") || !slices.Contains(got, "beta") || !slices.Contains(got, "gamma") {
		t.Fatalf("groups in All: %q", got)
	}
	if got := p.texts(".sidebar .filters .menu > button"); !slices.Contains(got, "Directory") {
		t.Fatalf("filters in All: %q", got)
	}
	p.shot("all-agents")

	p.dirTab("alpha")
	p.until(`document.querySelectorAll(".sidebar .tile").length === 2`)
	if got := p.texts(".sidebar .group .name"); !slices.Equal(got, []string{"Billing export", "Search index"}) && !slices.Equal(got, []string{"Search index", "Billing export"}) {
		t.Fatalf("groups in alpha: %q", got)
	}
	if got := p.texts(".sidebar .filters .menu > button"); slices.Contains(got, "Directory") {
		t.Fatalf("filters in alpha: %q", got)
	}
	if want := "#agents/" + url.PathEscape(alpha) + "/"; !strings.HasPrefix(p.hash(), want) {
		t.Fatalf("hash in alpha: %s, want %s", p.hash(), want)
	}
	p.shot("alpha-agents")

	p.view("Sessions")
	p.until(`document.querySelectorAll(".board .tile").length === 2`)
	if n := len(p.texts(".board .column .project")); n != 0 {
		t.Fatalf("columns in alpha name their directory %d times", n)
	}
	p.shot("alpha-sessions")
	p.dirTab("All")
	p.until(`document.querySelectorAll(".board .tile").length === 3 && document.querySelectorAll(".board .column .project").length > 0`)

	// gamma has only closed sessions: More lists it, and picking it shows its tab until another directory is chosen.
	p.click(`document.querySelector("nav.tabs .more-tab")`)
	p.until(`document.querySelector(".dir-menu")`)
	if got := p.texts(".dir-menu > *"); !slices.Equal(got, []string{"alpha2", "beta1", "Only closed sessions", "gamma1"}) {
		t.Fatalf("More: %q", got)
	}
	p.shot("more")
	p.click(`[...document.querySelectorAll(".dir-menu .dir-item")].find((b) => b.title === ` + fmt.Sprintf("%q", gamma) + `)`)
	p.until(`!document.querySelector(".dir-menu") && document.querySelector("nav.tabs .dirs .tab.on")?.textContent === "gamma"`)
	p.until(`document.querySelectorAll(".board .column.folded").length === 1`)
	p.dirTab("beta")
	p.until(`document.querySelectorAll("nav.tabs .dirs .tab").length === 3`)

	// Alt with a digit picks a tab, also while typing.
	p.view("Agents")
	p.click(`document.querySelector(".sidebar .tile")`)
	p.until(`document.querySelector(".panels .composer textarea")`)
	p.eval(`document.querySelector(".panels .composer textarea").focus()`, nil)
	p.call("Input.dispatchKeyEvent", map[string]any{"type": "keyDown", "key": "™", "code": "Digit2", "modifiers": 1})
	p.until(`document.querySelector("nav.tabs .dirs .tab.on")?.textContent === "alpha"`)
	if got := p.texts(".panels textarea"); len(got) != 0 && got[0] != "" {
		t.Fatalf("the key typed into the composer: %q", got)
	}

	// Each directory keeps its own panels, also across a reload.
	p.click(fmt.Sprintf(`document.querySelector('.sidebar .tile[data-chain=%q]')`, ids["Export invoices"]))
	p.until(fmt.Sprintf(`document.querySelector('#panel-%s')`, ids["Export invoices"]))
	p.dirTab("beta")
	p.until(fmt.Sprintf(`document.querySelector('#panel-%s') && !document.querySelector('#panel-%s')`, ids["Rebuild the index"], ids["Export invoices"]))
	p.dirTab("alpha")
	p.until(fmt.Sprintf(`document.querySelector('#panel-%s') && !document.querySelector('#panel-%s')`, ids["Export invoices"], ids["Rebuild the index"]))
	p.view("Sessions")
	p.call("Page.reload", nil)
	p.until(`document.querySelector("nav.tabs .dirs .tab.on")?.textContent === "alpha" && document.querySelector(".board .tile")`)
	p.view("Agents")
	p.until(fmt.Sprintf(`document.querySelector('#panel-%s')`, ids["Export invoices"]))
	p.shot("alpha-panel")

	// A hash from before the directories opens its panels in the directory of their session.
	p.eval(fmt.Sprintf(`location.hash = %q`, "~"+ids["Rebuild the index"]), nil)
	p.until(fmt.Sprintf(`document.querySelector("nav.tabs .dirs .tab.on")?.textContent === "beta" && document.querySelector('#panel-%s')`, ids["Rebuild the index"]))
	if !strings.HasPrefix(p.hash(), "#agents/") {
		t.Fatalf("hash after an old one: %s", p.hash())
	}
	// In a narrow window the directory tabs scroll, and More and the global tabs stay in reach.
	p.resize(760, 900)
	var right float64
	p.eval(`document.querySelector("nav.tabs .konduktor-toggle").getBoundingClientRect().right`, &right)
	if right > 761 {
		t.Fatalf("the konduktor toggle ends at %.0f px in a 760 px window", right)
	}
	p.until(`(() => { const on = document.querySelector("nav.tabs .dirs .tab.on").getBoundingClientRect(), box = document.querySelector("nav.tabs .dirs").getBoundingClientRect(); return on.left >= box.left - 0.5 && on.right <= box.right + 0.5 })()`)
	p.shot("narrow")
}

func TestNewSessionInDirectory(t *testing.T) {
	e := newEnv(t)
	ids := e.dirSessions()
	alpha, beta := filepath.Join(e.work, "alpha"), filepath.Join(e.work, "beta")
	p := e.openPage(1600, 900)
	p.dirTab("alpha")
	p.view("Sessions")
	p.click(`document.querySelector(".sessions-head > button")`)
	p.until(`document.querySelector(".new-session .dir-input input")`)
	var dir string
	p.eval(`document.querySelector(".new-session .dir-input input").value`, &dir)
	if dir != alpha {
		t.Fatalf("New session starts in %q, want %q", dir, alpha)
	}
	p.shot("new-session")
	// A session started in another directory takes the page there.
	p.eval(fmt.Sprintf(`(() => {
		const set = (el, v) => { Object.getOwnPropertyDescriptor(el.constructor.prototype, "value").set.call(el, v); el.dispatchEvent(new Event("input", { bubbles: true })) }
		set(document.querySelector(".new-session .dir-input input"), %q)
		set(document.querySelector(".new-session textarea"), "Started from alpha")
	})()`, beta), nil)
	p.click(`document.querySelector(".new-session button[type=submit]")`)
	p.until(`!document.querySelector(".new-session") && document.querySelector("nav.tabs .dirs .tab.on")?.textContent === "beta"`)
	p.until(`[...document.querySelectorAll(".panels .conversation h1")].some((h) => h.textContent === "Started from alpha")`)
	p.until(`document.querySelector(".views button.on")?.textContent === "Agents"`)

	// A link to a session of another directory takes the page there.
	queued := e.mustKK("new", "claude", "-C", beta, "-after", ids["Export invoices"], "Follow up in beta")
	p.eval(fmt.Sprintf(`location.hash = %q`, "agents/"+url.PathEscape(beta)+"/~"+queued), nil)
	p.click(fmt.Sprintf(`document.querySelector('#panel-%s .queued-card button.link')`, queued))
	p.until(fmt.Sprintf(`document.querySelector("nav.tabs .dirs .tab.on")?.textContent === "alpha" && document.querySelector('#panel-%s')`, ids["Export invoices"]))
}

// hover moves the mouse over the middle of an element.
func (p *page) hover(element string) {
	p.t.Helper()
	p.until(element)
	var at struct{ X, Y float64 }
	p.eval(fmt.Sprintf(`(() => { const r = %s.getBoundingClientRect(); return { x: r.x + r.width / 2, y: r.y + r.height / 2 } })()`, element), &at)
	p.call("Input.dispatchMouseEvent", map[string]any{"type": "mouseMoved", "x": at.X, "y": at.Y})
}

func (p *page) width(element string) float64 {
	p.t.Helper()
	var w float64
	p.eval(element+`.getBoundingClientRect().width`, &w)
	return w
}

func TestHideDirectoryTab(t *testing.T) {
	e := newEnv(t)
	ids := e.dirSessions()
	alpha, beta := filepath.Join(e.work, "alpha"), filepath.Join(e.work, "beta")
	p := e.openPage(1600, 900)
	tabOf := func(name string) string {
		return fmt.Sprintf(`[...document.querySelectorAll("nav.tabs .dirs .tab")].find((a) => a.textContent === %q)`, name)
	}
	p.until(`document.querySelectorAll("nav.tabs .dirs .tab").length === 3`)
	if n := len(p.texts("nav.tabs .dirs .tab .dot")); n != 0 {
		t.Fatalf("%d tabs carry a state icon", n)
	}
	// Every directory tab has a close button, All has none.
	if n := len(p.texts("nav.tabs .dirs .tab .tab-close")); n != 2 {
		t.Fatalf("%d close buttons, want 2", n)
	}
	p.until(tabOf("All") + ` && !` + tabOf("All") + `.querySelector(".tab-close")`)
	// Neither the tab nor its close button changes the width of the tab on hover.
	before := p.width(tabOf("alpha"))
	p.hover(tabOf("alpha"))
	p.shot("tab-hover")
	p.hover(tabOf("alpha") + `.querySelector(".tab-close")`)
	if after := p.width(tabOf("alpha")); after != before {
		t.Fatalf("the tab is %.1f px wide on hover, %.1f px at rest", after, before)
	}
	p.shot("tab-close-hover")

	// Closing the selected tab by keyboard goes to All and leaves the sessions running.
	p.dirTab("alpha")
	p.until(`document.querySelector("nav.tabs .dirs .tab.on")?.textContent === "alpha"`)
	p.eval(tabOf("alpha")+`.querySelector(".tab-close").focus()`, nil)
	p.shot("tab-close-focus")
	p.press("Enter", 13, "\r")
	p.until(`document.querySelector("nav.tabs .dirs .tab.on")?.textContent === "All" && !(` + tabOf("alpha") + `)`)
	if !strings.HasPrefix(p.hash(), "#agents//") {
		t.Fatalf("hash after hiding the selected tab: %s", p.hash())
	}
	if c := e.conv(ids["Export invoices"]); c.Closed {
		t.Fatal("hiding the tab closed a session")
	}
	// Closing another tab stays where the user is; the tab itself is not picked.
	p.click(tabOf("beta") + `.querySelector(".tab-close")`)
	p.until(`!(` + tabOf("beta") + `)`)
	// The inbox sessions may bring a tab of their own, so only alpha and beta are checked.
	shown := func() bool {
		got := p.texts("nav.tabs .dirs .tab")
		return slices.Contains(got, "alpha") || slices.Contains(got, "beta")
	}
	if shown() {
		t.Fatalf("tabs after hiding: %q", p.texts("nav.tabs .dirs .tab"))
	}

	// The tabs stay hidden across a reload, and More lists them.
	p.call("Page.reload", nil)
	p.until(`document.querySelector(".sidebar .tile")`)
	if shown() {
		t.Fatalf("tabs after a reload: %q", p.texts("nav.tabs .dirs .tab"))
	}
	var stored []string
	p.eval(`JSON.parse(localStorage.getItem("kk.hiddenTabs"))`, &stored)
	if !slices.Equal(stored, []string{alpha, beta}) {
		t.Fatalf("kk.hiddenTabs: %q", stored)
	}

	// A hidden directory that needs the user marks its row and More, and keeps its tab hidden.
	e.mustKK("send", ids["Rebuild the index"], "ask for permission")
	e.waitFor("attention", ids["Rebuild the index"], func(c convView) bool { return c.Attention != "" })
	p.until(`document.querySelector("nav.tabs .more-tab.attention")`)
	p.click(`document.querySelector("nav.tabs .more-tab")`)
	p.until(`document.querySelector(".dir-menu")`)
	if got := p.texts(".dir-menu > *"); !strings.Contains(strings.Join(got, "|"), "Hidden tabs|alpha2|beta1|Only closed sessions|gamma1") {
		t.Fatalf("More: %q", got)
	}
	p.until(fmt.Sprintf(`document.querySelector('.dir-menu .dir-item.attention[title=%q]')`, beta))
	p.shot("more-hidden")
	if shown() {
		t.Fatalf("tabs while beta needs the user: %q", p.texts("nav.tabs .dirs .tab"))
	}

	// Picking a hidden directory in More brings its tab back.
	p.click(fmt.Sprintf(`document.querySelector('.dir-menu .dir-item[title=%q]')`, alpha))
	p.until(`document.querySelector("nav.tabs .dirs .tab.on")?.textContent === "alpha"`)
	p.eval(`JSON.parse(localStorage.getItem("kk.hiddenTabs"))`, &stored)
	if !slices.Equal(stored, []string{beta}) {
		t.Fatalf("kk.hiddenTabs after bringing alpha back: %q", stored)
	}
	p.shot("tab-back")
}

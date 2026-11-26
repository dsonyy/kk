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
		e.waitFor(s.title, ids[s.title], func(c convView) bool { return c.Status == statusIdle })
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

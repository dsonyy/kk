package main

import (
	"bytes"
	"fmt"
	"image"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// subagent finds a subagent of a conversation by its title.
func subagent(c convView, title string) subagentView {
	for _, a := range c.Subagents {
		if a.Title == title {
			return a
		}
	}
	return subagentView{}
}

func states(c convView) map[string]string {
	out := map[string]string{}
	for _, a := range c.Subagents {
		out[a.Title] = a.State
	}
	return out
}

func (e *env) finishAgent(n int, failed bool) {
	e.t.Helper()
	name := "finish-agent-%d"
	if failed {
		name = "fail-agent-%d"
	}
	if err := os.WriteFile(filepath.Join(e.fake, fmt.Sprintf(name, n)), nil, 0o644); err != nil {
		e.t.Fatal(err)
	}
}

// The fake Claude Code starts "Agent task 1" in a worktree, which starts "Agent task 2", and "Agent task 3" beside it.
func TestClaudeSubagentsTracked(t *testing.T) {
	e := newEnv(t)
	id := e.mustKK("new", "claude", "-m", "opus", "-C", e.work, "start 2 agents nested worktree")
	c := e.waitFor("three running subagents", id, func(c convView) bool {
		s := states(c)
		return len(s) == 3 && s["Agent task 1"] == "running" && s["Agent task 2"] == "running" && s["Agent task 3"] == "running"
	})
	first, nested, second := subagent(c, "Agent task 1"), subagent(c, "Agent task 2"), subagent(c, "Agent task 3")
	if first.Type != "general-purpose" || first.Model != "claude-sonnet-5-5" || !strings.HasPrefix(first.ID, id+":a") || first.Of != id || first.Parent != "" {
		t.Errorf("first subagent: %+v", first)
	}
	if first.Worktree == "" || !strings.HasPrefix(first.Branch, "worktree-agent-") || second.Worktree != "" {
		t.Errorf("worktrees: %+v, %+v", first, second)
	}
	if nested.Parent != first.ID || nested.Type != "Explore" || second.Parent != "" {
		t.Errorf("nesting: %+v, %+v", nested, second)
	}
	if first.Started.IsZero() || first.Last.Before(first.Started) {
		t.Errorf("times: %+v", first)
	}
	// Each subagent used 1000 tokens of input and 100 of output, which the session's usage holds once.
	for _, a := range c.Subagents {
		if a.Usage == nil || tokens(*a.Usage) != 1100 {
			t.Errorf("usage of %s: %+v", a.Title, a.Usage)
		}
	}
	if u := usageOf(c); tokens(u) != answerTokens+3*1100 {
		t.Errorf("session usage: %d tokens, want %d", tokens(u), answerTokens+3*1100)
	}

	ls := e.mustKK("ls")
	lines := strings.Split(ls, "\n")
	at := slices.IndexFunc(lines, func(l string) bool { return strings.HasPrefix(l, id+" ") })
	if at < 0 || at+3 >= len(lines) {
		t.Fatalf("kk ls:\n%s", ls)
	}
	for i, want := range []string{"  " + first.ID + " ", "    " + nested.ID + " ", "  " + second.ID + " "} {
		if l := lines[at+1+i]; !strings.HasPrefix(l, want) || !strings.Contains(l, "running") {
			t.Errorf("kk ls line %d: %q, want it to start with %q\n%s", i+1, l, want, ls)
		}
	}

	e.finishAgent(1, false)
	e.finishAgent(3, true)
	c = e.waitFor("finished subagents", id, func(c convView) bool {
		s := states(c)
		return s["Agent task 1"] == "done" && s["Agent task 3"] == "failed" && s["Agent task 2"] == "running"
	})
	// A finished subagent stays listed while one it started still runs, and a failed one stays while its session is open.
	ls = e.mustKK("ls")
	if !strings.Contains(ls, "  "+first.ID+" ") || !strings.Contains(ls, "    "+nested.ID+" ") || !strings.Contains(ls, "  "+second.ID+"  failed") || strings.Contains(ls, " subagent") {
		t.Errorf("kk ls after two subagents ended:\n%s", ls)
	}
	e.finishAgent(2, false)
	e.waitFor("the nested subagent done", id, func(c convView) bool { return states(c)["Agent task 2"] == "done" })
	ls = e.mustKK("ls")
	if strings.Contains(ls, first.ID) || strings.Contains(ls, nested.ID) || !strings.Contains(ls, "  "+second.ID+"  failed") || !strings.Contains(ls+"\n", "\n  2 done subagents\n") {
		t.Errorf("kk ls with only a failed subagent left:\n%s", ls)
	}
	show := e.mustKK("show", id)
	for _, want := range []string{"Subagents:", first.ID, "done", nested.ID, second.ID, "failed", first.Branch} {
		if !strings.Contains(show, want) {
			t.Errorf("kk show lacks %q:\n%s", want, show)
		}
	}
	if out := e.mustKK("show", first.ID); !strings.Contains(out, "Agent task 1") || !strings.Contains(out, "Work on Agent task 1") || !strings.Contains(out, "done with Agent task 1") {
		t.Errorf("kk show of a subagent:\n%s", out)
	}

	// A subagent is not a conversation of its own: nothing can be sent to it.
	if code, _ := e.post("/api/conversations/"+first.ID+"/send", map[string]any{"text": "hi"}); code != 404 {
		t.Errorf("send to a subagent: %d, want 404", code)
	}

	e.mustKK("close", id)
	c = e.waitFor("a closed session", id, func(c convView) bool { return c.Closed })
	if s := states(c); s["Agent task 1"] != "done" || s["Agent task 2"] != "done" || s["Agent task 3"] != "failed" {
		t.Errorf("states after close: %v", s)
	}
}

// Closing a session ends the subagents that still ran.
func TestClosingStopsSubagents(t *testing.T) {
	e := newEnv(t)
	id := e.mustKK("new", "claude", "-C", e.work, "start 1 agent")
	e.waitFor("a running subagent", id, func(c convView) bool { return states(c)["Agent task 1"] == "running" })
	e.mustKK("close", id)
	e.waitFor("a stopped subagent", id, func(c convView) bool { return c.Closed && states(c)["Agent task 1"] == "stopped" })
}

func TestCodexSubagentsTracked(t *testing.T) {
	e := newEnv(t)
	id := e.mustKK("new", "codex", "-m", "gpt-5.6-terra", "-C", e.work, "start 1 agent")
	c := e.waitFor("a running subagent", id, func(c convView) bool { return states(c)["Agent task 1"] == "running" })
	a := c.Subagents[0]
	if a.Type != "worker" || a.Model != "gpt-5.6-terra" || a.Usage == nil || tokens(*a.Usage) != 1100 {
		t.Errorf("codex subagent: %+v", a)
	}
	e.finishAgent(1, false)
	e.waitFor("a finished subagent", id, func(c convView) bool { return states(c)["Agent task 1"] == "done" })
	if out := e.mustKK("show", a.ID); !strings.Contains(out, "Work on Agent task 1") || !strings.Contains(out, "done with Agent task 1") {
		t.Errorf("kk show of a codex subagent:\n%s", out)
	}
}

// tileOf is the card of a session or subagent in a part of the page.
func tileOf(part, id string) string {
	return fmt.Sprintf(`document.querySelector(%q)`, fmt.Sprintf(`%s .tile[data-chain="%s"]`, part, id))
}

func TestSubagentCards(t *testing.T) {
	e := newEnv(t)
	writeImage(t, filepath.Join(e.fake, "shot.png"), func(b *bytes.Buffer, i image.Image) error { return png.Encode(b, i) })
	id := e.mustKK("new", "claude", "-m", "opus", "-C", e.work, "start 3 agents nested image worktree")
	c := e.waitFor("four running subagents", id, func(c convView) bool { return len(c.Subagents) == 4 && (turnEnded(c) || c.Status == statusBackground) })
	next := e.mustKK("new", "claude", "-C", e.work, "--after", id, "then this")
	first, nested, second, third := subagent(c, "Agent task 1"), subagent(c, "Agent task 2"), subagent(c, "Agent task 3"), subagent(c, "Agent task 4")

	p := e.openPage(1600, 900)
	for _, a := range []subagentView{first, nested, second, third} {
		p.until(tileOf(".sidebar", a.ID))
	}
	// Each subagent hangs below its parent, its nested one below it, and the queued successor after them.
	var order []string
	p.eval(`[...document.querySelectorAll(".sidebar .tile")].map((t) => t.dataset.chain)`, &order)
	if want := []string{id, first.ID, nested.ID, second.ID, third.ID, next}; !slices.Equal(order, want) {
		t.Errorf("sidebar order %v, want %v", order, want)
	}
	var edges struct{ Agent, Chain int }
	edgeCount := `(() => { const legs = [...document.querySelectorAll(".sidebar .chain-links .leg")]; return { agent: legs.filter((l) => l.classList.contains("agent")).length, chain: legs.filter((l) => !l.classList.contains("agent")).length } })()`
	p.until(`document.querySelectorAll(".sidebar .chain-links .leg").length === 5`)
	p.eval(edgeCount, &edges)
	if edges.Agent != 4 || edges.Chain != 1 {
		t.Errorf("edges: %+v, want 4 to subagents and 1 to the successor", edges)
	}
	if text := p.texts(fmt.Sprintf(`.sidebar .tile[data-chain="%s"] .title`, first.ID)); len(text) != 1 || text[0] != "Agent task 1" {
		t.Errorf("subagent title: %v", text)
	}
	p.shot("subagents-running")
	p.resize(1000, 900)
	p.shot("subagents-narrow")
	p.resize(1600, 900)

	// Finished subagents fold behind a count on the parent, unless one they started still runs. Failed ones stay.
	e.finishAgent(1, false)
	e.finishAgent(3, true)
	e.finishAgent(4, false)
	p.until(fmt.Sprintf(`!%s`, tileOf(".sidebar", third.ID)))
	p.until(fmt.Sprintf(`%s?.classList.contains("failed")`, tileOf(".sidebar", second.ID)))
	p.until(tileOf(".sidebar", first.ID))
	fold := fmt.Sprintf(`%s.querySelector(".agents-fold")`, tileOf(".sidebar", id))
	p.until(fmt.Sprintf(`%s?.textContent === "1 done subagent"`, fold))
	p.shot("subagents-folded")
	p.click(fold)
	p.until(tileOf(".sidebar", third.ID))
	p.shot("subagents-unfolded")

	// A subagent opens read-only: its transcript without a reply box or controls, and a way back to its parent.
	p.click(tileOf(".sidebar", first.ID))
	panel := fmt.Sprintf(`document.getElementById("panel-%s")`, first.ID)
	p.until(fmt.Sprintf(`%s?.textContent.includes("done with Agent task 1")`, panel))
	p.until(fmt.Sprintf(`[...%s.querySelectorAll(".msg.assistant img")].some((i) => i.complete && i.naturalWidth === 4)`, panel))
	var controls []string
	p.eval(fmt.Sprintf(`[...%s.querySelectorAll("textarea, .conv-head button")].map((b) => b.textContent.trim() || b.tagName)`, panel), &controls)
	for _, c := range controls {
		if c == "TEXTAREA" || strings.Contains(c, "Close") || strings.Contains(c, "Schedule") || strings.Contains(c, "Terminal") || strings.Contains(c, "Kill") || strings.Contains(c, "Reset") {
			t.Errorf("a subagent's panel offers %q: %v", c, controls)
		}
	}
	// The branch sits as far from its icon as the directory does.
	var gaps []string
	p.eval(fmt.Sprintf(`[".branch", ".dir"].map((s) => { const e = %s.querySelector(".conv-head " + s); return e && getComputedStyle(e).display + " " + getComputedStyle(e).columnGap })`, panel), &gaps)
	if len(gaps) != 2 || gaps[0] != "flex 5px" || gaps[1] != gaps[0] {
		t.Errorf("branch and directory in the header: %v", gaps)
	}
	p.shot("subagent-transcript")
	p.click(fmt.Sprintf(`%s.querySelector(".subagent-of")`, panel))
	p.until(fmt.Sprintf(`document.getElementById("panel-%s")`, id))

	p.view("Sessions")
	p.until(tileOf(".board", nested.ID))
	p.until(fmt.Sprintf(`%s.querySelector(".agents-fold")`, tileOf(".board", id)))
	// The finished subagents stay unfolded as they were in the sidebar.
	p.until(`document.querySelectorAll(".board .chain-links .leg.agent").length === 4`)
	p.shot("subagents-board")

	// A closed parent takes its subagents with it.
	e.mustKK("close", next)
	e.mustKK("close", id)
	p.view("Agents")
	p.until(fmt.Sprintf(`%s?.classList.contains("closed") && document.querySelectorAll(".sidebar .tile.closed").length === 2`, tileOf(".sidebar", id)))
	p.eval(`document.querySelector(".sidebar .group").open || document.querySelector(".sidebar .group > summary").click()`, nil)
	p.until(`document.querySelector(".sidebar .group").open`)
	p.until(fmt.Sprintf(`%s?.querySelector(".agents-fold")?.textContent === "2 done, 1 failed, 1 stopped subagents"`, tileOf(".sidebar", id)))
	if p.until(tileOf(".sidebar", id)); len(p.texts(".sidebar .tile.subagent")) != 0 {
		t.Error("a closed session shows its subagents unfolded")
	}
	p.shot("subagents-closed")
	p.click(fmt.Sprintf(`%s.querySelector(".agents-fold")`, tileOf(".sidebar", id)))
	p.until(tileOf(".sidebar", nested.ID))
	p.shot("subagents-closed-unfolded")
}

// chainEnds is where the line from a session to its queued successor leaves and enters, beside the cards of both, in
// the coordinates of the lines.
type chainEnds struct {
	Out, In    float64
	From, To   struct{ Top, Bottom, Right float64 }
	Port, Head float64
}

func (p *page) chainEnds(part, from, to string) chainEnds {
	p.t.Helper()
	var got chainEnds
	p.eval(fmt.Sprintf(`(() => {
		const tile = (id) => document.querySelector(%q + ' .tile[data-chain="' + id + '"]');
		const a = tile(%q), b = tile(%q), box = a.closest(".chained").getBoundingClientRect();
		const rel = (r) => ({ top: r.top - box.top, bottom: r.bottom - box.top, right: r.right - box.left });
		const svg = a.closest(".chained").querySelector(".chain-links");
		const trunk = [...svg.querySelectorAll(".trunk:not(.agent)")].map((t) => t.getAttribute("d").match(/^M([\d.]+) ([\d.]+)H[\d.]+V([\d.]+)$/))[0];
		const head = [...svg.querySelectorAll(".head:not(.agent)")].map((h) => +h.getAttribute("d").match(/^M[\d.]+ [\d.]+L([\d.]+)/)[1])[0];
		return { out: +trunk[2], in: +trunk[3], port: +trunk[1], head, from: rel(a.getBoundingClientRect()), to: rel(b.getBoundingClientRect()) };
	})()`, part, from, to), &got)
	return got
}

// The line from a session to its queued successor leaves the session near its bottom and enters the successor level
// with its title, wherever the two cards go in the list.
func (e *env) checkChain(p *page, part, from, to, when string) {
	e.t.Helper()
	var got chainEnds
	for end := time.Now().Add(3 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		got = p.chainEnds(part, from, to)
		ok := got.Out > (got.From.Top+got.From.Bottom)/2 && got.Out < got.From.Bottom && math.Abs(got.In-(got.To.Top+17.5)) <= 1 &&
			math.Abs(got.Port-got.From.Right) <= 1 && math.Abs(got.Head-(got.To.Right+1)) <= 1
		if ok {
			return
		}
		if time.Now().After(end) {
			break
		}
	}
	e.t.Errorf("%s, %s: the line leaves at y %.1f and enters at y %.1f; the predecessor spans %.1f-%.1f, the successor %.1f-%.1f",
		part, when, got.Out, got.In, got.From.Top, got.From.Bottom, got.To.Top, got.To.Bottom)
}

// The sidebar of a directory sorted by state moves a session with finished subagents and its queued successor below an
// unrelated session of another group and back, and the line between them moves with their cards.
func TestChainFollowsSortedCards(t *testing.T) {
	e := newEnv(t)
	other := e.mustKK("new", "claude", "-C", e.work, "-g", "Refactor kk tasks", "Tasky refactor")
	e.waitFor("the other session idle", other, turnEnded)
	pred := e.mustKK("new", "claude", "-C", e.work, "-g", "Kk bundle configuration", "start 2 agents")
	e.waitFor("two subagents", pred, func(c convView) bool { return len(c.Subagents) == 2 })
	e.finishAgent(1, false)
	e.finishAgent(2, false)
	e.waitFor("the subagents done", pred, func(c convView) bool {
		s := states(c)
		return s["Agent task 1"] == "done" && s["Agent task 2"] == "done" && turnEnded(c)
	})
	next := e.mustKK("new", "claude", "-C", e.work, "-g", "Kk bundle configuration", "-after", pred, "Explain the ticket")

	p := e.openPage(1600, 900)
	p.eval(`localStorage.setItem("kk.sort", "state")`, nil)
	p.call("Page.reload", map[string]any{})
	p.until(`document.querySelector("nav.tabs .dirs .tab")`)
	p.dirTab(filepath.Base(e.work))
	order := func(want ...string) {
		t.Helper()
		p.until(fmt.Sprintf(`[...document.querySelectorAll(".sidebar .tile")].map((t) => t.dataset.chain).join() === %q`, strings.Join(want, ",")))
	}
	fold := fmt.Sprintf(`%s?.querySelector(".agents-fold")?.textContent === "2 done subagents"`, tileOf(".sidebar", pred))
	p.until(fold)
	order(pred, next, other)
	e.checkChain(p, ".sidebar", pred, next, "first drawn")
	p.shot("chain-sorted")

	// Working sorts the session below the idle one, and turning idle again, as the most recent, back above it.
	e.mustKK("send", pred, "slow")
	e.waitFor("the session working", pred, func(c convView) bool { return c.Status == statusWorking })
	order(other, pred, next)
	e.checkChain(p, ".sidebar", pred, next, "the session working")
	p.shot("chain-sorted-working")
	e.mustKK("interrupt", pred)
	e.waitFor("the session idle", pred, func(c convView) bool { return c.Status == statusIdle })
	order(pred, next, other)
	p.until(fold)
	e.checkChain(p, ".sidebar", pred, next, "the session idle again")
	p.shot("chain-sorted-idle")

	p.view("Sessions")
	p.until(tileOf(".board", next))
	e.checkChain(p, ".board", pred, next, "on the Sessions page")
	p.shot("chain-sorted-board")
}

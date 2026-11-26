package main

import (
	"bytes"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
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
	// A finished subagent stays listed while one it started still runs.
	ls = e.mustKK("ls")
	if !strings.Contains(ls, "  "+first.ID+" ") || !strings.Contains(ls, "    "+nested.ID+" ") || strings.Contains(ls, second.ID) || !strings.Contains(ls+"\n", "\n  1 finished subagent\n") {
		t.Errorf("kk ls after two subagents ended:\n%s", ls)
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

	// Closing the session ends the subagent that still ran.
	e.mustKK("close", id)
	c = e.waitFor("subagents of a closed session", id, func(c convView) bool { return c.Closed && states(c)["Agent task 2"] == "stopped" })
	if s := states(c); s["Agent task 1"] != "done" || s["Agent task 3"] != "failed" {
		t.Errorf("states after close: %v", s)
	}
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
	id := e.mustKK("new", "claude", "-m", "opus", "-C", e.work, "start 2 agents nested image worktree")
	c := e.waitFor("three running subagents", id, func(c convView) bool { return len(c.Subagents) == 3 && (turnEnded(c) || c.Status == statusBackground) })
	next := e.mustKK("new", "claude", "-C", e.work, "--after", id, "then this")
	first, nested, second := subagent(c, "Agent task 1"), subagent(c, "Agent task 2"), subagent(c, "Agent task 3")

	p := e.openPage(1600, 900)
	for _, a := range []subagentView{first, nested, second} {
		p.until(tileOf(".sidebar", a.ID))
	}
	// Each subagent hangs below its parent, its nested one below it, and the queued successor after them.
	var order []string
	p.eval(`[...document.querySelectorAll(".sidebar .tile")].map((t) => t.dataset.chain)`, &order)
	if want := []string{id, first.ID, nested.ID, second.ID, next}; !slices.Equal(order, want) {
		t.Errorf("sidebar order %v, want %v", order, want)
	}
	var edges struct{ Agent, Chain int }
	edgeCount := `(() => { const legs = [...document.querySelectorAll(".sidebar .chain-links .leg")]; return { agent: legs.filter((l) => l.classList.contains("agent")).length, chain: legs.filter((l) => !l.classList.contains("agent")).length } })()`
	p.until(`document.querySelectorAll(".sidebar .chain-links .leg").length === 4`)
	p.eval(edgeCount, &edges)
	if edges.Agent != 3 || edges.Chain != 1 {
		t.Errorf("edges: %+v, want 3 to subagents and 1 to the successor", edges)
	}
	if text := p.texts(fmt.Sprintf(`.sidebar .tile[data-chain="%s"] .title`, first.ID)); len(text) != 1 || text[0] != "Agent task 1" {
		t.Errorf("subagent title: %v", text)
	}
	p.shot("subagents-running")
	p.resize(1000, 900)
	p.shot("subagents-narrow")
	p.resize(1600, 900)

	// Finished subagents fold behind a count on the parent, unless one they started still runs.
	e.finishAgent(1, false)
	e.finishAgent(3, true)
	p.until(fmt.Sprintf(`!%s`, tileOf(".sidebar", second.ID)))
	p.until(tileOf(".sidebar", first.ID))
	fold := fmt.Sprintf(`%s.querySelector(".agents-fold")`, tileOf(".sidebar", id))
	p.until(fmt.Sprintf(`%s?.textContent.includes("1 finished subagent")`, fold))
	p.shot("subagents-folded")
	p.click(fold)
	p.until(tileOf(".sidebar", second.ID))
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
	p.shot("subagent-transcript")
	p.click(fmt.Sprintf(`%s.querySelector(".subagent-of")`, panel))
	p.until(fmt.Sprintf(`document.getElementById("panel-%s")`, id))

	p.view("Sessions")
	p.until(tileOf(".board", nested.ID))
	p.until(fmt.Sprintf(`%s.querySelector(".agents-fold")`, tileOf(".board", id)))
	// The finished subagents stay unfolded as they were in the sidebar.
	p.until(`document.querySelectorAll(".board .chain-links .leg.agent").length === 3`)
	p.shot("subagents-board")

	// A closed parent takes its subagents with it.
	e.mustKK("close", next)
	e.mustKK("close", id)
	p.view("Agents")
	p.until(fmt.Sprintf(`%s?.classList.contains("closed") && document.querySelectorAll(".sidebar .tile.closed").length === 2`, tileOf(".sidebar", id)))
	p.eval(`document.querySelector(".sidebar .group").open || document.querySelector(".sidebar .group > summary").click()`, nil)
	p.until(`document.querySelector(".sidebar .group").open`)
	p.until(fmt.Sprintf(`%s?.querySelector(".agents-fold")?.textContent.includes("3 finished subagents")`, tileOf(".sidebar", id)))
	if p.until(tileOf(".sidebar", id)); len(p.texts(".sidebar .tile.subagent")) != 0 {
		t.Error("a closed session shows its subagents unfolded")
	}
	p.shot("subagents-closed")
	p.click(fmt.Sprintf(`%s.querySelector(".agents-fold")`, tileOf(".sidebar", id)))
	p.until(tileOf(".sidebar", nested.ID))
	p.shot("subagents-closed-unfolded")
}

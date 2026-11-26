package main

import (
	"fmt"
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

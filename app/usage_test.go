package main

import (
	"math"
	"slices"
	"testing"
)

// The fake agents answer with 1000 tokens of input, 990 of them from the cache, and 50 of output. A subagent adds
// 1000 of input and 100 of output on claude-haiku-5-5, or on the thread's model for Codex.
const (
	opusAnswer   = (10*4 + 990*0.20 + 50*20) / 1e6
	haikuAgent   = (1000*0.10 + 100*0.50) / 1e6
	terraAnswer  = (10*2 + 990*0.20 + 50*12) / 1e6
	terraAgent   = (1000*2 + 100*12) / 1e6
	answerTokens = 1050
)

func usageOf(c convView) Usage {
	if c.Usage == nil {
		return Usage{Input: -1}
	}
	return *c.Usage
}

func tokens(u Usage) int { return u.Input + u.CacheRead + u.CacheWrite + u.Output }

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func waitUsage(e *env, id string, n int, cost float64) convView {
	e.t.Helper()
	return e.waitFor("usage", id, func(c convView) bool {
		u := usageOf(c)
		return tokens(u) == n && near(u.Cost, cost) && c.Status == statusIdle
	})
}

func TestClaudeUsage(t *testing.T) {
	e := newEnv(t)
	id := e.mustKK("new", "claude", "-m", "opus", "-C", e.work, "ask a subagent")
	c := waitUsage(e, id, answerTokens+1100, opusAnswer+haikuAgent)
	if u := usageOf(c); u.Input != 1010 || u.CacheRead != 990 || u.Output != 150 || len(u.Unpriced) != 0 {
		t.Fatalf("usage: %+v", u)
	}
	want := opusAnswer + haikuAgent

	// The rewound branch was paid for, so it stays counted.
	e.mustKK("send", id, "two")
	want += opusAnswer
	c = waitUsage(e, id, 2*answerTokens+1100, want)
	target := slices.IndexFunc(c.Events, func(ev Event) bool { return ev.Kind == kindUser && ev.Text == "two" })
	if code, body := e.post("/api/conversations/"+id+"/rewind", map[string]any{"event": target, "was": "two", "text": "three"}); code != 200 {
		t.Fatalf("rewind: %d %s", code, body)
	}
	want += opusAnswer
	waitUsage(e, id, 3*answerTokens+1100, want)

	e.mustKK("reset", id)
	e.waitFor("ready after reset", id, func(c convView) bool { return c.Status == statusIdle })
	e.mustKK("send", id, "four")
	want += opusAnswer
	waitUsage(e, id, 4*answerTokens+1100, want)

	e.mustKK("send", id, "exit")
	e.waitFor("exit", id, func(c convView) bool { return c.Status == statusExited })
	e.mustKK("resume", id)
	e.waitFor("resumed", id, func(c convView) bool { return c.Status == statusIdle })
	e.mustKK("send", id, "five")
	want += opusAnswer
	waitUsage(e, id, 5*answerTokens+1100, want)

	e.stop()
	e.start()
	if u := usageOf(e.conv(id)); tokens(u) != 5*answerTokens+1100 || !near(u.Cost, want) {
		t.Fatalf("usage after a restart: %+v", u)
	}
	e.mustKK("close", id)
	if u := usageOf(e.conv(id)); tokens(u) != 5*answerTokens+1100 || !near(u.Cost, want) {
		t.Fatalf("usage after close: %+v", u)
	}
}

func TestCodexUsage(t *testing.T) {
	e := newEnv(t)
	id := e.mustKK("new", "codex", "-m", "gpt-5.6-terra", "-C", e.work, "ask a subagent")
	c := waitUsage(e, id, answerTokens+1100, terraAnswer+terraAgent)
	want := terraAnswer + terraAgent

	// Codex carries the running total over a resume.
	e.mustKK("send", id, "exit")
	e.waitFor("exit", id, func(c convView) bool { return c.Status == statusExited })
	e.mustKK("resume", id)
	e.waitFor("resumed", id, func(c convView) bool { return c.Status == statusIdle })
	e.mustKK("send", id, "two")
	want += terraAnswer
	c = waitUsage(e, id, 2*answerTokens+1100, want)

	// A reverted thread goes on in a new rollout from the total where the kept part ends.
	target := slices.IndexFunc(c.Events, func(ev Event) bool { return ev.Kind == kindUser && ev.Text == "two" })
	if code, body := e.post("/api/conversations/"+id+"/rewind", map[string]any{"event": target, "was": "two", "text": "three"}); code != 200 {
		t.Fatalf("rewind: %d %s", code, body)
	}
	want += terraAnswer
	waitUsage(e, id, 3*answerTokens+1100, want)
}

func TestUsageWithoutPrice(t *testing.T) {
	e := newEnv(t)
	id := e.mustKK("new", "claude", "-m", "claude-unreleased-9", "-C", e.work, "hi")
	c := e.waitFor("usage", id, func(c convView) bool { return tokens(usageOf(c)) == answerTokens })
	if u := usageOf(c); u.Cost != 0 || !slices.Equal(u.Unpriced, []string{"claude-unreleased-9"}) {
		t.Fatalf("usage: %+v", u)
	}
}

func TestCursorHasNoUsage(t *testing.T) {
	e := newEnv(t)
	id := e.mustKK("new", "cursor", "-C", e.work, "hi cursor")
	c := e.waitFor("answer", id, func(c convView) bool { return hasEvent(c, kindAssistant, "echo: hi cursor") })
	if c.Usage != nil {
		t.Fatalf("usage: %+v", *c.Usage)
	}
}

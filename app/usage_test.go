package main

import (
	"encoding/json"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
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

// An answer that consulted the advisor is one message of three calls: the main model's with 1000 and 1202 tokens of
// prompt, and the advisor's on Claude Fable 5.2. The subagent consults it too.
const (
	mainCalls      = (2*4+900*0.20+100*8+40*20)/1e6 + (2*4+1000*0.20+200*5+60*20)/1e6
	advisorCall    = (5000*8 + 300*40) / 1e6
	subagentAdvice = (2000*8 + 100*40) / 1e6
	advisorTokens  = 1042 + 5300 + 1262 + 1100 + 2100
)

func TestClaudeAdvisorUsage(t *testing.T) {
	e := newEnv(t)
	id := e.mustKK("new", "claude", "-m", "opus", "-C", e.work, "ask the advisor and a subagent")
	want := mainCalls + advisorCall + haikuAgent + subagentAdvice
	c := waitUsage(e, id, advisorTokens, want)
	if c.Context != 1202 {
		t.Fatalf("context should be the prompt of the last call of the main model: %d", c.Context)
	}
	models := func() map[string]Usage {
		for _, r := range activity(e) {
			if r.ID == id {
				return r.Models
			}
		}
		return nil
	}
	if m := models(); m["claude-fable-5-2"].Input != 7000 || m["claude-fable-5-2"].Output != 400 || m["claude-opus-5-5"].Output != 100 || m["claude-haiku-5-5"].Output != 100 {
		t.Fatalf("models: %+v", m)
	}

	// Usage stored by an earlier version is counted again from the transcripts that are still there.
	e.mustKK("close", id)
	e.stop()
	path := filepath.Join(e.home, "conversations", id+".yaml")
	data, _ := os.ReadFile(path)
	var stored Conversation
	if err := yaml.Unmarshal(data, &stored); err != nil {
		t.Fatal(err)
	}
	turns := 0
	for p, a := range stored.Transcripts {
		turns += a.Turns
		a.Usage, a.Models, a.Days = Usage{Output: 1}, map[string]Usage{"claude-opus-5-5": {Output: 1}}, nil
		stored.Transcripts[p] = a
	}
	stored.Transcripts["gone.jsonl"] = Activity{Usage: Usage{Output: 7, Cost: 1}, Turns: 1}
	data, _ = yaml.Marshal(stored)
	os.WriteFile(path, data, 0o600)
	e.start()
	c = e.waitFor("usage counted again", id, func(c convView) bool {
		u := usageOf(c)
		return tokens(u) == advisorTokens+7 && near(u.Cost, want+1)
	})
	if m := models(); m["claude-fable-5-2"].Input != 7000 {
		t.Fatalf("models after a restart: %+v", m)
	}
	for _, r := range activity(e) {
		if r.ID == id && r.Turns != turns+1 {
			t.Fatalf("turns after a restart: %d, want %d", r.Turns, turns+1)
		}
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

func activity(e *env) []activityRow {
	e.t.Helper()
	out, err := exec.Command("curl", "-s", "--unix-socket", filepath.Join(e.home, "kk.sock"), "http://kk/api/activity").Output()
	if err != nil {
		e.t.Fatal(err)
	}
	var rows []activityRow
	if err := json.Unmarshal(out, &rows); err != nil {
		e.t.Fatalf("%v: %s", err, out)
	}
	return rows
}

func TestActivity(t *testing.T) {
	e := newEnv(t)
	claude := e.mustKK("new", "claude", "-m", "opus", "-C", e.work, "ask a subagent")
	waitUsage(e, claude, answerTokens+1100, opusAnswer+haikuAgent)
	e.mustKK("reset", claude)
	e.waitFor("ready after reset", claude, func(c convView) bool { return c.Status == statusIdle })
	e.mustKK("send", claude, "two")
	waitUsage(e, claude, 2*answerTokens+1100, 2*opusAnswer+haikuAgent)
	e.mustKK("close", claude)
	cursor := e.mustKK("new", "cursor", "-C", e.work, "hi cursor")
	e.waitFor("cursor answer", cursor, func(c convView) bool { return hasEvent(c, kindAssistant, "echo: hi cursor") && c.Status == statusIdle })

	var rows map[string]activityRow
	deadline := time.Now().Add(10 * time.Second)
	for {
		rows = map[string]activityRow{}
		for _, r := range activity(e) {
			rows[r.ID] = r
		}
		if rows[cursor].Turns == 1 || time.Now().After(deadline) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	today := time.Now().Format(time.DateOnly)
	c := rows[claude]
	if !c.Closed || !c.Metered || c.Harness != "claude" || c.Project != e.work || c.Turns != 2 || c.Tools["Bash"] != 2 {
		t.Fatalf("claude row: %+v", c)
	}
	if !near(c.Days[today].Cost, 2*opusAnswer+haikuAgent) || !near(c.Models["claude-opus-5-5"].Cost, 2*opusAnswer) || !near(c.Models["claude-haiku-5-5"].Cost, haikuAgent) {
		t.Fatalf("claude usage by day and model: %+v %+v", c.Days, c.Models)
	}
	k := rows[cursor]
	if k.Metered || k.Turns != 1 || k.Tools["Shell"] != 1 || len(k.Days) != 0 {
		t.Fatalf("cursor row: %+v", k)
	}
}

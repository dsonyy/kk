package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func (e *env) list() []convView {
	e.t.Helper()
	out, err := exec.Command("curl", "-s", "--unix-socket", filepath.Join(e.home, "kk.sock"), "http://kk/api/conversations?all=1").Output()
	if err != nil {
		e.t.Fatal(err)
	}
	var list []convView
	json.Unmarshal(out, &list)
	return list
}

func (e *env) waitGroup(id, group string) convView {
	e.t.Helper()
	return e.waitFor("group "+group, id, func(c convView) bool { return c.Group == group })
}

func TestFeatureGroups(t *testing.T) {
	e := newEnv(t)
	other := filepath.Join(filepath.Dir(e.work), "other")
	os.MkdirAll(other, 0o755)

	first := e.mustKK("new", "claude", "-C", e.work, "Billing export as a CSV file")
	e.waitGroup(first, "Billing export")
	same := e.mustKK("new", "codex", "-C", e.work, "Add tests for the billing export")
	e.waitGroup(same, "Billing export")
	another := e.mustKK("new", "claude", "-C", e.work, "Login page redesign")
	e.waitGroup(another, "Login page")
	elsewhere := e.mustKK("new", "claude", "-C", other, "Polish the billing export")
	if c := e.waitGroup(elsewhere, "Polish the"); c.Project != other {
		t.Fatalf("project of a session in %s: %s", other, c.Project)
	}

	failed := e.mustKK("new", "claude", "-C", e.work, "GROUPFAIL and answer")
	e.waitFor("answer", failed, func(c convView) bool { return hasEvent(c, kindAssistant, "echo: GROUPFAIL and answer") })
	if c := e.conv(failed); c.Group != "" || c.Status != statusIdle {
		t.Fatalf("a session whose grouping failed: group %q, status %s", c.Group, c.Status)
	}

	byHand := e.mustKK("new", "claude", "-C", e.work, "-g", "login PAGE", "Something about the billing export")
	if c := e.conv(byHand); c.Group != "Login page" {
		t.Fatalf("group given with -g: %q", c.Group)
	}
	e.mustKK("group", failed, "Release  notes")
	e.mustKK("group", same, "release notes")
	if a, b := e.conv(failed), e.conv(same); a.Group != "Release notes" || b.Group != "Release notes" {
		t.Fatalf("groups set by hand: %q and %q", a.Group, b.Group)
	}
	for _, bad := range []string{strings.Repeat("x", 41), "ungrouped", " "} {
		if out, err := e.kk("group", first, bad); err == nil {
			t.Fatalf("group %q was accepted: %s", bad, out)
		}
		if out, err := e.kk("new", "claude", "-C", e.work, "-g", bad, "hello"); err == nil && bad != " " {
			t.Fatalf("new -g %q was accepted: %s", bad, out)
		}
	}
	if c := e.conv(byHand); c.Group != "Login page" {
		t.Fatalf("a group set by hand was decided again: %q", c.Group)
	}

	e.mustKK("close", another)
	want := map[string]string{first: "Billing export", same: "Release notes", another: "Login page", elsewhere: "Polish the", failed: "Release notes", byHand: "Login page"}
	seen := 0
	for _, c := range e.list() {
		if g, ok := want[c.ID]; ok {
			seen++
			if c.Group != g || (c.Project != e.work) != (c.ID == elsewhere) {
				t.Fatalf("%s: group %q in project %s, want %q", c.ID, c.Group, c.Project, g)
			}
		}
	}
	if seen != len(want) {
		t.Fatalf("the list with closed sessions has %d of %d sessions", seen, len(want))
	}
	groups := e.mustKK("group")
	for _, line := range []string{e.work + "  Billing export", e.work + "  Login page", e.work + "  Release notes", other + "  Polish the"} {
		if !strings.Contains(groups, line) {
			t.Fatalf("kk group misses %q:\n%s", line, groups)
		}
	}
}

func TestGroupWaitsForTheTask(t *testing.T) {
	e := newEnv(t)
	id := e.mustKK("new", "claude", "-C", e.work, "Read the rules and wait for the task")
	e.waitFor("answer", id, func(c convView) bool {
		return c.Status == statusIdle && hasEvent(c, kindAssistant, "echo: Read the rules and wait for the task")
	})
	time.Sleep(1500 * time.Millisecond)
	if c := e.conv(id); c.Group != "" {
		t.Fatalf("a prompt without a task got the group %q", c.Group)
	}
	e.mustKK("send", id, "Dark mode for the settings page")
	e.waitGroup(id, "Dark mode")
}

func TestGroupBackfill(t *testing.T) {
	e := newEnv(t)
	id := e.mustKK("new", "claude", "-C", e.work, "GROUPFAIL Search index rebuild")
	e.waitFor("answer", id, func(c convView) bool { return c.Status == statusIdle && len(c.Events) > 1 })
	e.stop()
	path := filepath.Join(e.home, "conversations", id+".yaml")
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "group:") {
		t.Fatalf("a failed grouping was stored:\n%s", data)
	}
	// Without the stored prompt the group comes from the first message of the transcript.
	os.WriteFile(path, []byte(strings.Replace(string(data), "prompt: GROUPFAIL Search index rebuild", "prompt: \"\"", 1)), 0o600)
	transcript := e.transcriptOf(path)
	raw, _ := os.ReadFile(transcript)
	os.WriteFile(transcript, []byte(strings.ReplaceAll(string(raw), "GROUPFAIL ", "")), 0o600)
	e.start()
	e.waitGroup(id, "Search index")
}

// transcriptOf reads the transcript path from a conversation file while the server is down.
func (e *env) transcriptOf(path string) string {
	data, _ := os.ReadFile(path)
	for _, line := range strings.Split(string(data), "\n") {
		if rest, ok := strings.CutPrefix(line, "transcript: "); ok {
			return rest
		}
	}
	e.t.Fatalf("no transcript in %s", path)
	return ""
}

func TestWorktreeSessionProject(t *testing.T) {
	c := Conversation{Dir: "/r/app/.claude/worktrees/fix-1/sub", Repo: "/r/app/sub"}
	if got := project(c); got != "/r/app/sub" {
		t.Fatalf("project of a kk worktree: %s", got)
	}
	if got := project(Conversation{Dir: "/r/app/.claude/worktrees/fix-1"}); got != "/r/app" {
		t.Fatalf("project of a session started inside a worktree: %s", got)
	}
}

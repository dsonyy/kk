package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func isQueued(c convView) bool { return c.Status == statusQueued }

func TestChainQueuesAndStarts(t *testing.T) {
	e := newEnv(t)
	pred := e.mustKK("new", "claude", "-C", e.work, "-g", "Billing export", "hello")
	e.waitFor("predecessor idle", pred, func(c convView) bool { return c.Status == statusIdle })

	next := e.mustKK("new", "claude", "-C", e.work, "-after", pred, "Second step")
	branch := e.mustKK("new", "codex", "-C", e.work, "-after", pred, "-g", "Release notes", "Other branch")
	deeper := e.mustKK("new", "claude", "-C", e.work, "-after", next, "Third step")
	c := e.conv(next)
	if !isQueued(c) || c.After != pred || c.Window != "" || c.Prompt != "Second step" || c.Title != "Second step" || len(c.Actions) != 0 || c.WaitingFor != "" {
		t.Fatalf("queued conversation: %+v actions %v", c.Conversation, c.Actions)
	}
	if g := [3]string{c.Group, e.conv(branch).Group, e.conv(deeper).Group}; g != [3]string{"Billing export", "Release notes", "Billing export"} {
		t.Fatalf("groups of the successors: %q", g)
	}
	time.Sleep(2500 * time.Millisecond)
	if c := e.conv(next); !isQueued(c) || c.Attention != "" {
		t.Fatalf("a queued conversation changed on its own: %+v", c.Conversation)
	}
	if ls := e.mustKK("ls"); !strings.Contains(ls, next+"  queued") || !strings.Contains(ls, "Second step  (after "+pred+")") {
		t.Fatalf("kk ls:\n%s", ls)
	}
	for _, args := range [][]string{{"send", next, "hi"}, {"resume", next}, {"new", "claude", "-C", e.work, "-after", "nope", "x"}, {"new", "claude", "-C", e.work, "-after", coordinatorID, "x"}} {
		if out, err := e.kk(args...); err == nil {
			t.Fatalf("kk %v was accepted: %s", args, out)
		}
	}

	if code, body := e.post("/api/conversations/"+next+"/prompt", map[string]string{"prompt": "Second step, edited\nwith details"}); code != 200 {
		t.Fatalf("edit the prompt: %d %s", code, body)
	}
	if c := e.conv(next); c.Prompt != "Second step, edited\nwith details" || c.Title != "Second step, edited" {
		t.Fatalf("edited prompt: %q title %q", c.Prompt, c.Title)
	}

	// A restart keeps the queue, and a closed queued conversation reopens queued.
	e.stop()
	e.start()
	e.mustKK("close", branch)
	if c := e.conv(branch); !c.Closed || !isQueued(c) {
		t.Fatalf("closed queued conversation: %+v", c.Conversation)
	}
	if out, err := e.kk("start", branch); err == nil {
		t.Fatalf("a closed conversation started: %s", out)
	}
	e.mustKK("resume", branch)
	if c := e.conv(branch); c.Closed || !isQueued(c) || c.Window != "" {
		t.Fatalf("reopened queued conversation: %+v", c.Conversation)
	}

	cmd := exec.Command(kkBin, "start", next, "-")
	cmd.Env = e.environ()
	cmd.Stdin = strings.NewReader("The billing export is on branch billing.\n")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("kk start: %v %s", err, out)
	}
	prompt := "Second step, edited\nwith details\n\nHandoff from the session before this one:\n\nThe billing export is on branch billing."
	c = e.waitFor("successor answer", next, func(c convView) bool { return hasEvent(c, kindAssistant, "echo: "+prompt) && c.Status == statusIdle })
	if c.WaitingFor != waitingUser || c.After != pred {
		t.Fatalf("started successor: %+v", c.Conversation)
	}
	if out, err := e.kk("start", next); err == nil || !strings.Contains(out, "only a queued session starts") {
		t.Fatalf("second start: %v %s", err, out)
	}
	if c := e.conv(deeper); !isQueued(c) {
		t.Fatalf("the successor's successor started: %+v", c.Conversation)
	}
}

func TestChainWorktreeAtStart(t *testing.T) {
	e := newEnv(t)
	pred := e.mustKK("new", "claude", "-C", e.work, "hello")
	if out, err := e.kk("new", "claude", "-w", "-C", e.work, "-after", pred, "no repo"); err == nil || !strings.Contains(out, "not a git repository") {
		t.Fatalf("queued worktree outside git: %v %s", err, out)
	}
	for _, args := range [][]string{{"init", "-q", "-b", "trunk"}, {"-c", "user.name=kk", "-c", "user.email=kk@example.com", "commit", "-q", "--allow-empty", "-m", "first"}} {
		if out, err := exec.Command("git", append([]string{"-C", e.work}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	next := e.mustKK("new", "claude", "-w", "-C", e.work, "-after", pred, "Build on it")
	if c := e.conv(next); c.Dir != e.work || c.Repo != "" || !c.Worktree {
		t.Fatalf("queued worktree conversation: %+v", c.Conversation)
	}
	if _, err := os.Stat(filepath.Join(e.work, worktreesDir)); err == nil {
		t.Fatal("a worktree was made before the start")
	}
	e.mustKK("start", next)
	c := e.waitFor("answer", next, func(c convView) bool { return hasEvent(c, kindAssistant, "echo: Build on it") })
	if c.Repo != e.work || !strings.Contains(c.Dir, worktreesDir) || c.Worktree || c.Project != e.work {
		t.Fatalf("started worktree conversation: %+v", c.Conversation)
	}
}

func TestChainNotifiesKonduktor(t *testing.T) {
	e := newCoordinatorEnv(t)
	// The user's own agent: its turns go to the user, its handoff to the konduktor.
	mine := e.mustKK("new", "claude", "-C", e.work, "hello")
	e.waitFor("idle", mine, func(c convView) bool { return c.Status == statusIdle })
	a := e.asKonduktor("new", "claude", "-C", e.work, "-after", mine, "Step A")
	b := e.asKonduktor("new", "claude", "-C", e.work, "-after", mine, "Step B")
	e.mustKK("send", mine, "again")
	e.waitNotified(mine, fmt.Sprintf(`(idle): finished its turn; queued after it: %s "Step A", %s "Step B". Look: kk screen %s`, a, b, mine))
	if c := e.conv(mine); c.WaitingFor != waitingUser {
		t.Fatalf("the user's predecessor went to the konduktor: %+v", c.Conversation)
	}
	if !isQueued(e.conv(a)) || !isQueued(e.conv(b)) {
		t.Fatal("a successor started without kk start")
	}
	e.asKonduktor("start", a, "Done in commit abc.")
	c := e.waitFor("successor answer", a, func(c convView) bool { return c.Status == statusIdle && c.WaitingFor == waitingKonduktor })
	if !hasEvent(c, kindUser, "Step A\n\nHandoff from the session before this one:\n\nDone in commit abc.") {
		t.Fatalf("successor prompt: %+v", c.Events)
	}

	// A predecessor the user needs to answer is not the konduktor's, and its close is news.
	asks := e.mustKK("new", "claude", "-C", e.work, "hello")
	e.waitFor("idle", asks, func(c convView) bool { return c.Status == statusIdle })
	waits := e.asKonduktor("new", "claude", "-C", e.work, "-after", asks, "Waits")
	e.mustKK("send", asks, "ask for permission")
	e.waitFor("dialog", asks, func(c convView) bool { return c.Attention != "" })
	e.mustKK("close", asks)
	e.waitNotified(asks, fmt.Sprintf(`was closed; queued after it: %s "Waits"`, waits))
	if n := countLines(e.notifications(), asks, "needs action"); n != 0 {
		t.Fatalf("the user's dialog went to the konduktor: %q", e.notifications())
	}
	if !isQueued(e.conv(waits)) {
		t.Fatal("the successor of a closed conversation started")
	}
}

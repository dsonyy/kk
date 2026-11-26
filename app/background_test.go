package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// finishTask ends the fake agent's background task, which makes Claude Code start a turn of its own.
func (e *env) finishTask() {
	e.t.Helper()
	if err := os.WriteFile(filepath.Join(e.fake, "finish"), nil, 0o644); err != nil {
		e.t.Fatal(err)
	}
}

func inBackground(c convView) bool { return c.Status == statusBackground }

func TestBackgroundTask(t *testing.T) {
	e := newEnv(t)
	id := e.mustKK("new", "claude", "-C", e.work, "run the tests in the background")
	c := e.waitFor("a turn that ends with a task running", id, inBackground)
	if c.WaitingFor != "" || c.Unread || !c.TurnEnded.IsZero() || slices.Contains(c.Actions, "interrupt") {
		t.Fatalf("background: waiting for %q, unread %v, turn ended %v, actions %v", c.WaitingFor, c.Unread, c.TurnEnded, c.Actions)
	}
	if ls := e.mustKK("ls"); !strings.Contains(ls, id+"  background") {
		t.Fatalf("ls: %s", ls)
	}
	time.Sleep(1500 * time.Millisecond)
	if c = e.conv(id); !inBackground(c) {
		t.Fatalf("left the background state with the task running: %s", c.Status)
	}
	e.finishTask()
	c = e.waitFor("the turn after the task", id, func(c convView) bool {
		return hasEvent(c, kindAssistant, "the background task finished") && c.Status == statusIdle
	})
	if c.WaitingFor != waitingUser || !c.Unread {
		t.Fatalf("after the task: waiting for %q, unread %v", c.WaitingFor, c.Unread)
	}
}

func TestBackgroundTaskStopped(t *testing.T) {
	e := newEnv(t)
	id := e.mustKK("new", "claude", "-C", e.work, "start a background build")
	e.waitFor("background", id, inBackground)
	e.mustKK("send", id, "stop task")
	e.waitFor("the turn that stopped the task", id, func(c convView) bool {
		return hasEvent(c, kindAssistant, "stopped the task") && c.Status == statusIdle
	})
	time.Sleep(1500 * time.Millisecond)
	if c := e.conv(id); c.Status != statusIdle {
		t.Fatalf("after the task was stopped: %s", c.Status)
	}
}

func TestBackgroundAgentExits(t *testing.T) {
	e := newEnv(t)
	id := e.mustKK("new", "claude", "-C", e.work, "start a background build")
	e.waitFor("background", id, inBackground)
	e.killAgent(id)
	e.waitFor("exit", id, func(c convView) bool { return c.Status == statusExited })
}

func TestBackgroundOutlivesRestart(t *testing.T) {
	e := newEnv(t)
	id := e.mustKK("new", "claude", "-C", e.work, "start a background build")
	e.waitFor("background", id, inBackground)
	e.stop()
	e.start()
	time.Sleep(1500 * time.Millisecond)
	if c := e.conv(id); !inBackground(c) {
		t.Fatalf("after a restart: %s", c.Status)
	}
	e.finishTask()
	e.waitFor("the turn after the task", id, func(c convView) bool {
		return hasEvent(c, kindAssistant, "the background task finished") && c.Status == statusIdle
	})
}

// A task whose end the transcript never records gives the turn back after the timeout.
func TestBackgroundTimeout(t *testing.T) {
	e := newEnv(t, "KK_BACKGROUND_TIMEOUT=3s")
	id := e.mustKK("new", "claude", "-C", e.work, "start a background build")
	e.waitFor("background", id, inBackground)
	c := e.waitFor("the timeout", id, func(c convView) bool { return c.Status == statusIdle })
	if c.TurnEnded.IsZero() || !c.Unread || c.WaitingFor != waitingUser {
		t.Fatalf("after the timeout: turn ended %v, unread %v, waiting for %q", c.TurnEnded, c.Unread, c.WaitingFor)
	}
}

func TestBackgroundNotice(t *testing.T) {
	e := newCoordinatorEnv(t)
	id := e.asKonduktor("new", "claude", "-C", e.work, "run the tests in the background")
	e.waitFor("background", id, inBackground)
	time.Sleep(4 * time.Second)
	if n := countLines(e.notifications(), id, ""); n != 0 {
		t.Fatalf("notified while the task runs: %q", e.notifications())
	}
	e.finishTask()
	e.waitNotified(id, "(idle): finished its turn")
	time.Sleep(3 * time.Second)
	if n := countLines(e.notifications(), id, ""); n != 1 {
		t.Fatalf("%d notifications: %q", n, e.notifications())
	}
}

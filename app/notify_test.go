package main

import (
	"os/exec"
	"strings"
	"testing"
	"time"
)

// newAs starts an agent the way an agent running kk new does.
func (e *env) newAs(by string, args ...string) string {
	e.t.Helper()
	cmd := exec.Command(kkBin, append([]string{"new"}, args...)...)
	cmd.Env = append(e.environ(), "KK_CONVERSATION="+by)
	out, err := cmd.CombinedOutput()
	if err != nil {
		e.t.Fatalf("kk new %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// notifications returns the kk notifications the coordinator received, and those it holds in its queue.
func (e *env) notifications() []string {
	c := e.conv(coordinatorID)
	var out []string
	for _, ev := range append(c.Events, c.Queued...) {
		if ev.Kind == kindUser && strings.HasPrefix(ev.Text, notifyMarker) {
			out = append(out, ev.Text)
		}
	}
	return out
}

// waitNotified waits until the coordinator received a line about id that has want in it.
func (e *env) waitNotified(id, want string) {
	e.t.Helper()
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if countLines(e.notifications(), id, want) > 0 {
			return
		}
	}
	e.t.Fatalf("no notification %q for %s, got:\n%s", want, id, strings.Join(e.notifications(), "\n---\n"))
}

func countLines(messages []string, id, want string) int {
	n := 0
	for _, m := range messages {
		for _, line := range strings.Split(m, "\n") {
			if strings.HasPrefix(line, "- "+id+" ") && strings.Contains(line, want) {
				n++
			}
		}
	}
	return n
}

func newCoordinatorEnv(t *testing.T, extra ...string) *env {
	e := newEnv(t, append([]string{"KK_COORDINATOR=1"}, extra...)...)
	e.waitFor("coordinator start", coordinatorID, func(c convView) bool { return c.Status == statusIdle })
	return e
}

func TestNotifyCoordinator(t *testing.T) {
	e := newCoordinatorEnv(t)
	started := e.newAs(coordinatorID, "claude", "-C", e.work, "hello")
	byUser := e.mustKK("new", "claude", "-C", e.work, "first")
	permission := e.mustKK("new", "claude", "-C", e.work, "ask for permission")
	trust := e.mustKK("new", "claude", "-C", e.work, "TRUST me")
	apiError := e.newAs(coordinatorID, "claude", "-C", e.work, "apierror now")
	stopped := e.mustKK("new", "claude", "-C", e.work, "stop me")

	e.waitNotified(started, "(idle): finished its turn. Look: kk screen "+started)
	e.waitNotified(permission, "needs action: Claude needs your permission to use Bash. Look: kk screen "+permission)
	e.waitNotified(trust, "needs action: The agent has not started its session")
	e.waitNotified(apiError, "needs action: The turn ended on an API error")
	e.waitFor("user's agent idle", byUser, func(c convView) bool { return c.Status == statusIdle })
	e.waitFor("stopped agent idle", stopped, func(c convView) bool { return c.Status == statusIdle })
	e.mustKK("stop", stopped)
	e.mustKK("send", byUser, "exit")
	e.waitNotified(byUser, "(exited): exited without being stopped. Look: kk screen "+byUser)

	// Nothing more comes while the states stay, and a restart is no news either.
	time.Sleep(4 * time.Second)
	e.stop()
	e.start()
	time.Sleep(5 * time.Second)
	got := e.notifications()
	for _, tc := range []struct{ id, want string }{
		{started, "finished its turn"}, {permission, "needs action"}, {trust, "needs action"}, {apiError, "needs action"}, {byUser, "exited"},
	} {
		if n := countLines(got, tc.id, ""); n != 1 {
			t.Errorf("%s: %d lines, want one %q", tc.id, n, tc.want)
		}
	}
	for _, id := range []string{stopped} {
		if n := countLines(got, id, ""); n != 0 {
			t.Errorf("%s: %d lines, want none", id, n)
		}
	}
	if t.Failed() {
		t.Fatalf("notifications:\n%s", strings.Join(got, "\n---\n"))
	}
	if c := e.conv(coordinatorID); !hasEvent(c, kindAssistant, "noted") {
		t.Fatalf("the coordinator did not take the notification as a message: %+v", c.Events)
	}
}

func TestNotifyGathers(t *testing.T) {
	e := newCoordinatorEnv(t)
	// The coordinator's own dialog must not be answered by a notification's Enter.
	e.mustKK("send", coordinatorID, "ask for permission")
	e.waitFor("coordinator dialog", coordinatorID, func(c convView) bool { return c.Attention != "" })
	a := e.mustKK("new", "claude", "-C", e.work, "permission one")
	b := e.mustKK("new", "claude", "-C", e.work, "permission two")
	e.waitFor("attention", b, func(c convView) bool { return c.Attention != "" })
	time.Sleep(4 * time.Second)
	if got := e.notifications(); len(got) != 0 {
		t.Fatalf("notified during the coordinator's dialog: %q", got)
	}
	if c := e.conv(coordinatorID); c.Attention == "" {
		t.Fatalf("the coordinator's dialog was answered: %+v", c.Conversation)
	}
	e.mustKK("keys", coordinatorID, "1", "Enter")
	e.waitNotified(b, "needs action")
	got := e.notifications()
	if len(got) != 1 || countLines(got, a, "needs action") != 1 {
		t.Fatalf("two events close together, want one message with both: %q", got)
	}

	// Mid-turn the notification waits in the coordinator's queue, and the turn goes on.
	e.mustKK("send", coordinatorID, "slow work")
	e.waitFor("coordinator working", coordinatorID, func(c convView) bool { return c.Status == statusWorking })
	exits := e.mustKK("new", "claude", "-C", e.work, "first")
	e.waitFor("answer", exits, func(c convView) bool { return c.Status == statusIdle })
	e.mustKK("send", exits, "exit")
	e.waitNotified(exits, "exited")
	if c := e.conv(coordinatorID); c.Status != statusWorking || len(c.Queued) != 1 {
		t.Fatalf("coordinator turn: %+v queued %+v", c.Conversation, c.Queued)
	}
}

func TestNotifyStall(t *testing.T) {
	e := newCoordinatorEnv(t, "KK_STALL_AFTER=3s")
	id := e.mustKK("new", "claude", "-C", e.work, "slow work")
	e.waitFor("working", id, func(c convView) bool { return c.Status == statusWorking })
	e.waitNotified(id, "(working): working with no hook, transcript or screen change for 3s")
	time.Sleep(6 * time.Second)
	if n := countLines(e.notifications(), id, ""); n != 1 {
		t.Fatalf("%d stall notifications: %q", n, e.notifications())
	}
}

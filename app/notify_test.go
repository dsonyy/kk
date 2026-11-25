package main

import (
	"encoding/json"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// killAgent ends the agent's process the way a crash does.
func (e *env) killAgent(id string) {
	e.t.Helper()
	sock := strings.TrimPrefix(e.vars[2], "KK_TMUX=")
	out, err := exec.Command("tmux", "-L", sock, "display-message", "-p", "-t", e.conv(id).Window, "#{pane_pid}").Output()
	pid, _ := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil || pid == 0 {
		e.t.Fatalf("pane pid of %s: %v %q", id, err, out)
	}
	syscall.Kill(pid, syscall.SIGTERM)
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
	started := e.asConductor("new", "claude", "-C", e.work, "hello")
	exits1 := e.asConductor("new", "claude", "-C", e.work, "first")
	permission := e.asConductor("new", "claude", "-C", e.work, "ask for permission")
	trust := e.asConductor("new", "claude", "-C", e.work, "TRUST me")
	apiError := e.asConductor("new", "claude", "-C", e.work, "apierror now")
	stopped := e.asConductor("new", "claude", "-C", e.work, "stop me")
	mine := e.mustKK("new", "claude", "-C", e.work, "ask my permission")
	escalated := e.asConductor("new", "claude", "-C", e.work, "escalate me")

	e.waitNotified(started, "(idle): finished its turn. Look: kk screen "+started)
	e.waitNotified(permission, "needs action: Claude needs your permission to use Bash. Look: kk screen "+permission)
	e.waitNotified(trust, "needs action: The agent has not started its session")
	e.waitNotified(apiError, "needs action: The turn ended on an API error")
	e.waitFor("user's agent idle", exits1, func(c convView) bool { return c.Status == statusIdle })
	e.waitFor("stopped agent idle", stopped, func(c convView) bool { return c.Status == statusIdle })
	e.asConductor("stop", stopped)
	e.waitFor("user's own dialog", mine, func(c convView) bool { return c.Attention != "" && c.WaitingFor == waitingUser })
	e.waitNotified(escalated, "finished its turn")
	e.asConductor("escalate", escalated, "needs a product decision")
	e.waitFor("escalation", escalated, func(c convView) bool { return c.WaitingFor == waitingUser })
	e.killAgent(escalated)
	e.waitFor("escalated agent exit", escalated, func(c convView) bool { return c.Status == statusExited })
	e.asConductor("send", exits1, "exit")
	e.waitNotified(exits1, "(exited): exited without being stopped. Look: kk screen "+exits1)

	// Nothing more comes while the states stay, and a restart is no news either.
	time.Sleep(4 * time.Second)
	e.stop()
	e.start()
	time.Sleep(5 * time.Second)
	got := e.notifications()
	for _, tc := range []struct{ id, want string }{
		{started, "finished its turn"}, {permission, "needs action"}, {trust, "needs action"}, {apiError, "needs action"},
		{exits1, "finished its turn"}, {exits1, "exited"}, {stopped, "finished its turn"}, {escalated, "finished its turn"},
	} {
		if n := countLines(got, tc.id, tc.want); n != 1 {
			t.Errorf("%s: %d lines %q, want one", tc.id, n, tc.want)
		}
	}
	// No exit for an agent stopped on purpose or escalated to the user, nothing for the user's own agent.
	for _, tc := range []struct{ id, want string }{{stopped, "exited"}, {escalated, "exited"}, {mine, ""}} {
		if n := countLines(got, tc.id, tc.want); n != 0 {
			t.Errorf("%s: %d lines %q, want none", tc.id, n, tc.want)
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
	a := e.asConductor("new", "claude", "-C", e.work, "permission one")
	b := e.asConductor("new", "claude", "-C", e.work, "permission two")
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
	exits := e.asConductor("new", "claude", "-C", e.work, "first")
	e.waitFor("answer", exits, func(c convView) bool { return c.Status == statusIdle })
	e.asConductor("send", exits, "exit")
	e.waitNotified(exits, "exited")
	if c := e.conv(coordinatorID); c.Status != statusWorking || len(c.Queued) != 1 {
		t.Fatalf("coordinator turn: %+v queued %+v", c.Conversation, c.Queued)
	}
}

func TestNotifyStall(t *testing.T) {
	e := newCoordinatorEnv(t, "KK_STALL_AFTER=3s")
	id := e.asConductor("new", "claude", "-C", e.work, "slow work")
	e.waitFor("working", id, func(c convView) bool { return c.Status == statusWorking })
	e.waitNotified(id, "(working): working with no hook, transcript or screen change for 3s")
	time.Sleep(6 * time.Second)
	if n := countLines(e.notifications(), id, ""); n != 1 {
		t.Fatalf("%d stall notifications: %q", n, e.notifications())
	}
}

func TestNotifyEveryRaise(t *testing.T) {
	e := newCoordinatorEnv(t)
	twice := e.asConductor("new", "claude", "-C", e.work, "permission twice")
	e.waitNotified(twice, "needs action")
	e.asConductor("keys", twice, "1", "Enter")
	e.waitFor("second dialog", twice, func(c convView) bool {
		return c.Attention != "" && strings.Count(e.mustKK("screen", twice), "Allow Bash?") == 2
	})
	for deadline := time.Now().Add(10 * time.Second); countLines(e.notifications(), twice, "needs action") < 2; time.Sleep(100 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("second dialog not reported: %q", e.notifications())
		}
	}

	// An agent that dies again right after a resume is news.
	id := e.asConductor("new", "claude", "-C", e.work, "first")
	e.waitFor("answer", id, func(c convView) bool { return c.Status == statusIdle })
	e.asConductor("send", id, "exit")
	e.waitNotified(id, "exited")
	e.asConductor("resume", id)
	e.waitFor("resumed", id, func(c convView) bool { return c.Status == statusIdle })
	e.asConductor("send", id, "exit")
	for deadline := time.Now().Add(10 * time.Second); countLines(e.notifications(), id, "exited") < 2; time.Sleep(100 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("second exit not reported: %q", e.notifications())
		}
	}
}

func TestHandOverToConductor(t *testing.T) {
	e := newCoordinatorEnv(t)
	create := func(prompt string, conductor bool) string {
		code, body := e.post("/api/conversations", map[string]any{"harness": "claude", "dir": e.work, "prompt": prompt, "conductor": conductor})
		var c convView
		if err := json.Unmarshal([]byte(body), &c); code != 200 || err != nil {
			t.Fatalf("create: %d %s", code, body)
		}
		return c.ID
	}
	handed := create("hello\nfrom the user "+strings.Repeat("x", 400), true)
	mine := create("hello mine", false)
	e.waitNotified(handed, "the user started it in "+e.work+" and handed it over to you, first prompt \"hello from the user xxx")
	e.waitNotified(handed, "\"; finished its turn. Look: kk screen "+handed)
	e.waitFor("conductor's turn", handed, func(c convView) bool { return c.Status == statusIdle && c.WaitingFor == waitingConductor })
	e.waitFor("user's turn", mine, func(c convView) bool { return c.Status == statusIdle && c.WaitingFor == waitingUser })

	e.mustKK("send", handed, "from the user")
	e.waitFor("taken back by the user", handed, func(c convView) bool {
		return hasEvent(c, kindAssistant, "echo: from the user") && c.Status == statusIdle && c.WaitingFor == waitingUser
	})
	time.Sleep(3 * time.Second)
	got := e.notifications()
	if n := countLines(got, handed, "handed it over"); n != 1 {
		t.Errorf("%d handover lines, want one", n)
	}
	if n := countLines(got, mine, ""); n != 0 {
		t.Errorf("%d lines for the user's own session, want none", n)
	}
	if t.Failed() {
		t.Fatalf("notifications:\n%s", strings.Join(got, "\n---\n"))
	}
}

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSleepingSession(t *testing.T) {
	e := newCoordinatorEnv(t, "KK_SLEEP_AFTER=2s")
	quiet := e.asConductor("new", "claude", "-C", e.work, "hello")
	busy := e.mustKK("new", "claude", "-C", e.work, "slow task")
	e.waitFor("the quiet agent's turn", quiet, func(c convView) bool { return hasEvent(c, kindAssistant, "echo: hello") })
	c := e.waitFor("sleep", quiet, func(c convView) bool { return c.Sleeping })
	if c.Status != statusExited || c.WaitingFor != "" || c.Closed || len(c.Actions) != 0 {
		t.Fatalf("asleep: status %s, waiting for %q, closed %v, actions %v", c.Status, c.WaitingFor, c.Closed, c.Actions)
	}
	if ls := e.mustKK("ls"); !strings.Contains(ls, quiet+"  sleeping") {
		t.Fatalf("ls: %s", ls)
	}
	time.Sleep(3 * time.Second)
	if c := e.conv(busy); c.Status != statusWorking || c.Sleeping {
		t.Fatalf("a working agent: %s, sleeping %v", c.Status, c.Sleeping)
	}
	if c := e.conv(coordinatorID); c.Status != statusIdle || c.Sleeping {
		t.Fatalf("the conductor: %s, sleeping %v", c.Status, c.Sleeping)
	}
	if n := countLines(e.notifications(), quiet, "exited without"); n != 0 {
		t.Fatalf("the conductor heard of the sleep: %s", strings.Join(e.notifications(), "\n---\n"))
	}

	e.mustKK("resume", quiet)
	e.waitFor("awake", quiet, func(c convView) bool { return c.Status == statusIdle && !c.Sleeping })
	e.mustKK("send", quiet, "again")
	c = e.waitFor("an answer after waking", quiet, func(c convView) bool { return hasEvent(c, kindAssistant, "echo: again") })
	if !hasEvent(c, kindAssistant, "echo: hello") {
		t.Fatalf("history lost: %+v", c.Events)
	}
}

func (e *env) schedule(id string, s Schedule) (int, convView) {
	e.t.Helper()
	data, _ := json.Marshal(s)
	resp, out := e.request("PUT", "/api/conversations/"+id+"/schedule", bytes.NewReader(data))
	var c convView
	json.Unmarshal(out, &c)
	return resp.StatusCode, c
}

// soon schedules id to run with prompt in d, every day.
func (e *env) soon(id, prompt string, d time.Duration) {
	e.t.Helper()
	at := time.Now().Add(d)
	if at.Day() != time.Now().Day() {
		e.t.Skip("a run planned across midnight")
	}
	if code, c := e.schedule(id, Schedule{Days: []int{0, 1, 2, 3, 4, 5, 6}, Times: []string{at.Format("15:04:05")}, Prompt: prompt}); code != 200 || c.Schedule == nil {
		e.t.Fatalf("schedule: %d %+v", code, c.Conversation)
	}
}

func count(c convView, kind, text string) int {
	n := 0
	for _, ev := range c.Events {
		if ev.Kind == kind && ev.Text == text {
			n++
		}
	}
	return n
}

func TestScheduledSession(t *testing.T) {
	e := newCoordinatorEnv(t)
	id := e.asConductor("new", "claude", "-C", e.work, "first")
	e.waitFor("the first turn", id, func(c convView) bool { return c.Status == statusIdle && c.WaitingFor == waitingConductor })

	for _, bad := range []Schedule{
		{Days: []int{7}, Times: []string{"09:00"}, Prompt: "x"},
		{Days: []int{1}, Times: []string{"9am"}, Prompt: "x"},
		{Days: []int{1}, Times: []string{"09:00"}, Prompt: " "},
		{Times: []string{"09:00"}, Prompt: "x"},
		{Days: []int{1}, Prompt: "x"},
	} {
		if code, _ := e.schedule(id, bad); code != 400 {
			t.Fatalf("schedule %+v: %d", bad, code)
		}
	}
	if code, _ := e.schedule(coordinatorID, Schedule{Days: []int{1}, Times: []string{"09:00"}, Prompt: "x"}); code != 400 {
		t.Fatalf("scheduled the conductor: %d", code)
	}
	code, c := e.schedule(id, Schedule{Days: []int{5, 1, 1}, Times: []string{"18:00", "09:30"}, Prompt: " check "})
	if code != 200 || c.Schedule == nil || c.Schedule.Prompt != "check" || len(c.Schedule.Days) != 2 || c.Schedule.Times[0] != "09:30" || c.NextRun.IsZero() {
		t.Fatalf("schedule: %d %+v next %v", code, c.Schedule, c.NextRun)
	}
	if wd := c.NextRun.Weekday(); wd != time.Monday && wd != time.Friday {
		t.Fatalf("next run on %s", wd)
	}

	// An idle agent gets the prompt typed in, and what it brings waits for the user.
	e.soon(id, "scheduled check", 2*time.Second)
	c = e.waitFor("a run into the idle agent", id, func(c convView) bool {
		return hasEvent(c, kindAssistant, "echo: scheduled check") && c.Status == statusIdle
	})
	if !c.RunDone || c.WaitingFor != waitingUser || c.LastRun.IsZero() {
		t.Fatalf("after the run: done %v, waiting for %q, last run %v", c.RunDone, c.WaitingFor, c.LastRun)
	}
	e.post("/api/conversations/"+id+"/seen", struct{}{})
	if c = e.conv(id); c.RunDone {
		t.Fatal("the run stays marked after it was seen")
	}

	// A stopped agent is resumed with the prompt.
	e.mustKK("stop", id)
	e.waitFor("stop", id, func(c convView) bool { return c.Status == statusExited })
	e.soon(id, "scheduled resume", 2*time.Second)
	c = e.waitFor("a run that resumes", id, func(c convView) bool {
		return hasEvent(c, kindAssistant, "echo: scheduled resume") && c.Status == statusIdle
	})
	if !hasEvent(c, kindAssistant, "echo: first") || !c.RunDone {
		t.Fatalf("after the resumed run: done %v, %+v", c.RunDone, c.Events)
	}
	argv, _ := os.ReadFile(filepath.Join(e.fake, "argv.log"))
	if !strings.Contains(string(argv), `"--resume","`+c.Session+`","--","scheduled resume"`) {
		t.Fatalf("argv: %s", argv)
	}

	// A working agent is not interrupted; the run waits until its turn ends.
	e.mustKK("send", id, "slow task")
	e.waitFor("work", id, func(c convView) bool { return c.Status == statusWorking })
	e.soon(id, "scheduled later", time.Second)
	time.Sleep(2500 * time.Millisecond)
	if c = e.conv(id); c.Status != statusWorking || hasEvent(c, kindUser, "scheduled later") {
		t.Fatalf("the run interrupted the work: %s", c.Status)
	}
	e.mustKK("interrupt", id)
	e.waitFor("the run after the work", id, func(c convView) bool { return hasEvent(c, kindAssistant, "echo: scheduled later") })

	// A run missed while kk was off is made up once.
	e.soon(id, "made up", 2*time.Second)
	e.stop()
	time.Sleep(3 * time.Second)
	e.start()
	e.waitFor("the run made up", id, func(c convView) bool { return hasEvent(c, kindAssistant, "echo: made up") })
	time.Sleep(2 * time.Second)
	if n := count(e.conv(id), kindUser, "made up"); n != 1 {
		t.Fatalf("made up %d times", n)
	}

	resp, _ := e.request("DELETE", "/api/conversations/"+id+"/schedule", nil)
	if c = e.conv(id); resp.StatusCode != 200 || c.Schedule != nil {
		t.Fatalf("remove: %d %+v", resp.StatusCode, c.Schedule)
	}
	e.soon(id, "closed", 2*time.Second)
	e.mustKK("close", id)
	if c = e.conv(id); c.Schedule != nil {
		t.Fatalf("closing kept the schedule: %+v", c.Schedule)
	}
}

// A queued session never sleeps and cannot be scheduled; a scheduled run of its predecessor leaves it queued for the conductor's handoff.
func TestScheduleAndSleepInChain(t *testing.T) {
	e := newCoordinatorEnv(t, "KK_SLEEP_AFTER=2s")
	pred := e.asConductor("new", "claude", "-C", e.work, "hello")
	e.waitFor("predecessor idle", pred, func(c convView) bool { return c.Status == statusIdle })
	next := e.asConductor("new", "claude", "-C", e.work, "-after", pred, "Second step")
	if code, _ := e.schedule(next, Schedule{Days: []int{0, 1, 2, 3, 4, 5, 6}, Times: []string{"09:00"}, Prompt: "x"}); code != 409 {
		t.Fatalf("scheduled a queued session: %d", code)
	}

	e.soon(pred, "nightly check", 2*time.Second)
	e.waitFor("the scheduled run", pred, func(c convView) bool { return hasEvent(c, kindAssistant, "echo: nightly check") })
	e.waitNotified(pred, "queued after it: "+next)
	e.waitFor("the predecessor asleep", pred, func(c convView) bool { return c.Sleeping })
	time.Sleep(3 * time.Second)
	if c := e.conv(next); c.Status != statusQueued || c.Sleeping {
		t.Fatalf("the queued session: %s, sleeping %v", c.Status, c.Sleeping)
	}
	if n := countLines(e.notifications(), pred, "exited without"); n != 0 {
		t.Fatalf("the conductor heard of the sleep: %s", strings.Join(e.notifications(), "\n---\n"))
	}
	e.asConductor("start", next, "the nightly build is green")
	e.waitFor("the successor's answer", next, func(c convView) bool {
		return c.Status == statusIdle && hasEvent(c, kindUser, "Second step\n\nHandoff from the session before this one:\n\nthe nightly build is green")
	})
}

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

// as runs the kk CLI the way the agent of a conversation does, from its own window.
func (e *env) as(id string, args ...string) (string, error) {
	cmd := exec.Command(kkBin, args...)
	cmd.Env = append(e.environ(), "KK_CONVERSATION="+id)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func (e *env) mustAs(id string, args ...string) string {
	e.t.Helper()
	out, err := e.as(id, args...)
	if err != nil {
		e.t.Fatalf("kk %v as %s: %v\n%s", args, id, err, out)
	}
	return out
}

// startKonduktor gives the konduktor of dir its first message, which starts it, and waits until it answered.
func (e *env) startKonduktor(dir, prompt string) string {
	e.t.Helper()
	os.MkdirAll(dir, 0o755)
	code, body := e.post("/api/konduktors", map[string]string{"dir": dir, "prompt": prompt})
	var k convView
	if err := json.Unmarshal([]byte(body), &k); code != 200 || err != nil {
		e.t.Fatalf("start the konduktor of %s: %d %s", dir, code, body)
	}
	e.waitFor("konduktor answer", k.ID, func(c convView) bool { return hasEvent(c, kindAssistant, "echo: "+prompt) && c.Status == statusIdle })
	return k.ID
}

func (e *env) konduktors() []convView {
	e.t.Helper()
	out, err := exec.Command("curl", "-s", "--unix-socket", filepath.Join(e.home, "kk.sock"), "http://kk/api/konduktors").Output()
	if err != nil {
		e.t.Fatal(err)
	}
	var ks []convView
	json.Unmarshal(out, &ks)
	return ks
}

func TestKonduktorStartsOnFirstMessage(t *testing.T) {
	e := newCoordinatorEnv(t)
	alpha := filepath.Join(e.work, "alpha")
	k := e.startKonduktor(alpha, "hello alpha")
	c := e.conv(k)
	if c.Role != coordinatorID || c.Scope != alpha || c.Title != "alpha" || c.Dir != e.home {
		t.Fatalf("konduktor: %+v", c.Conversation)
	}
	// It keeps the name of its directory, whatever title its transcript gets.
	time.Sleep(time.Second)
	if c := e.conv(k); c.Title != "alpha" {
		t.Fatalf("renamed to %q", c.Title)
	}
	ks := e.konduktors()
	if len(ks) != 2 || ks[0].ID != coordinatorID || ks[1].ID != k {
		t.Fatalf("konduktors: %+v", ks)
	}
	// Each prompt lists the other konduktor, and the main one hears of the new one.
	for id, want := range map[string]string{
		coordinatorID: "- " + k + " \"alpha\": " + alpha,
		k:             "- coordinator \"main\": every directory without its own konduktor",
	} {
		md, err := os.ReadFile(filepath.Join(e.home, "konduktors", id+".md"))
		if err != nil || !strings.Contains(string(md), want) {
			t.Errorf("prompt of %s lacks %q: %v\n%s", id, want, err, md)
		}
	}
	e.waitNotified("konduktor", k+" \"alpha\" now runs for "+alpha)
	// A konduktor that runs gets the message, and none starts twice.
	code, body := e.post("/api/konduktors", map[string]string{"dir": alpha, "prompt": "again"})
	if code != 200 || !strings.Contains(body, `"id":"`+k+`"`) {
		t.Fatalf("second message: %d %s", code, body)
	}
	e.waitFor("second answer", k, func(c convView) bool { return hasEvent(c, kindAssistant, "echo: again") })
	if ls := strings.Split(e.mustKK("ls"), "\n"); len(ls) < 2 || !strings.HasPrefix(ls[1], k+" ") || !strings.Contains(ls[1], "konduktor") || !strings.Contains(ls[1], "alpha ("+alpha+")") {
		t.Fatalf("kk ls: %q", ls)
	}
}

func TestKonduktorsOff(t *testing.T) {
	e := newEnv(t)
	if code, body := e.post("/api/konduktors", map[string]string{"dir": e.work, "prompt": "hello"}); code != 409 {
		t.Fatalf("start with konduktors off: %d %s", code, body)
	}
	if ks := e.konduktors(); len(ks) != 0 {
		t.Fatalf("konduktors: %+v", ks)
	}
}

func TestNotifyGoesToItsKonduktor(t *testing.T) {
	e := newCoordinatorEnv(t)
	alpha := filepath.Join(e.work, "alpha")
	k := e.startKonduktor(alpha, "hello alpha")
	// The main konduktor is in a dialog; the other one still hears of its sessions.
	e.mustKK("send", coordinatorID, "ask for permission")
	e.waitFor("dialog", coordinatorID, func(c convView) bool { return c.Attention != "" })
	theirs := e.mustAs(k, "new", "claude", "-C", alpha, "work in alpha")
	if c := e.conv(theirs); c.Konduktor != k {
		t.Fatalf("session of %q, want %s", c.Konduktor, k)
	}
	e.waitNotifiedTo(k, theirs, "finished its turn")
	// A session an agent of that konduktor starts belongs to it too.
	sub := e.mustAs(theirs, "new", "claude", "-C", e.work, "started by an agent")
	if c := e.conv(sub); c.Konduktor != k {
		t.Fatalf("session started by its agent belongs to %q, want %s", c.Konduktor, k)
	}
	if ls := e.mustKK("ls"); !strings.Contains(ls, theirs+"  idle             "+k) {
		t.Fatalf("kk ls does not name the konduktor: %s", ls)
	}
	if out := e.mustKK("show", theirs); !strings.Contains(out, "Waiting for konduktor alpha ("+k+")") {
		t.Fatalf("kk show: %s", out)
	}
	mine := e.asKonduktor("new", "claude", "-C", alpha, "the main one's work")
	time.Sleep(3 * time.Second)
	if n := countLines(e.notifications(), theirs, ""); n != 0 {
		t.Fatalf("the main konduktor heard of another's session")
	}
	if n := countLines(e.notificationsOf(k), mine, ""); n != 0 {
		t.Fatalf("konduktor %s heard of the main one's session", k)
	}
}

func TestKonduktorMessages(t *testing.T) {
	e := newCoordinatorEnv(t)
	k := e.startKonduktor(filepath.Join(e.work, "alpha"), "hello alpha")
	e.mustAs(k, "send", coordinatorID, "I merge into trunk")
	c := e.waitFor("peer message", coordinatorID, func(c convView) bool {
		return hasEvent(c, kindUser, "[from konduktor \"alpha\" ("+k+") via kk send - not from the user]\nI merge into trunk") && c.Status == statusIdle
	})
	if !c.Unread {
		t.Fatal("a message from another konduktor marked the main one read")
	}
	e.mustKK("send", coordinatorID, "ask for permission")
	e.waitFor("dialog", coordinatorID, func(c convView) bool { return c.Attention != "" })
	if out, err := e.as(k, "send", coordinatorID, "are you there"); err == nil || !strings.Contains(out, "in a dialog") {
		t.Fatalf("send into a dialog: %v %s", err, out)
	}
}

func TestKonduktorHandover(t *testing.T) {
	e := newCoordinatorEnv(t)
	k := e.startKonduktor(filepath.Join(e.work, "alpha"), "hello alpha")
	id := e.asKonduktor("new", "claude", "-C", e.work, "hand me over")
	e.waitNotified(id, "finished its turn")
	e.asKonduktor("handover", id, k)
	if c := e.conv(id); c.Konduktor != k || c.WaitingFor != waitingKonduktor {
		t.Fatalf("after the handover: %q waits for %q", c.Konduktor, c.WaitingFor)
	}
	e.waitNotifiedTo(k, id, "konduktor coordinator \"main\" handed it over to you")
	if out, err := e.as(k, "handover", id, "nobody"); err == nil || !strings.Contains(out, "not a running konduktor") {
		t.Fatalf("handover to nobody: %v %s", err, out)
	}
}

func TestCloseKonduktor(t *testing.T) {
	e := newCoordinatorEnv(t)
	alpha := filepath.Join(e.work, "alpha")
	k := e.startKonduktor(alpha, "hello alpha")
	id := e.mustAs(k, "new", "claude", "-C", alpha, "work in alpha")
	if out, err := e.kk("close", coordinatorID); err == nil || !strings.Contains(out, "cannot be closed") {
		t.Fatalf("close the main konduktor: %v %s", err, out)
	}
	e.mustKK("close", k)
	e.waitFor("closed", k, func(c convView) bool { return c.Closed && c.Status == statusExited })
	if c := e.conv(id); c.Konduktor != coordinatorID {
		t.Fatalf("the session of a closed konduktor goes to %q", c.Konduktor)
	}
	e.waitNotified("konduktor", k+" \"alpha\" for "+alpha+" was closed")
	md, _ := os.ReadFile(filepath.Join(e.home, "konduktors", coordinatorID+".md"))
	if strings.Contains(string(md), k) {
		t.Fatalf("the main prompt still lists %s", k)
	}
	// The next message reopens it in its own session.
	code, body := e.post("/api/konduktors", map[string]string{"dir": alpha, "prompt": "back again"})
	if code != 200 || !strings.Contains(body, `"id":"`+k+`"`) {
		t.Fatalf("reopen: %d %s", code, body)
	}
	e.waitFor("reopened", k, func(c convView) bool {
		return !c.Closed && hasEvent(c, kindAssistant, "echo: hello alpha") && hasEvent(c, kindAssistant, "echo: back again")
	})
	if c := e.conv(id); c.Konduktor != k {
		t.Fatalf("the session went to %q after the reopen", c.Konduktor)
	}
}

func TestSecondKonduktorCompacts(t *testing.T) {
	e := newCoordinatorEnv(t, "KK_COMPACT_AFTER=2s")
	k := e.startKonduktor(filepath.Join(e.work, "alpha"), "hello alpha")
	e.mustKK("send", k, "big context")
	e.waitFor("compaction", k, func(c convView) bool { return hasEvent(c, kindNote, "Conversation compacted") })
}

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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

func TestOwnerlessSessionStaysWithTheMainKonduktor(t *testing.T) {
	e := newCoordinatorEnv(t)
	alpha := filepath.Join(e.work, "alpha")
	os.MkdirAll(alpha, 0o755)
	id := e.asKonduktor("new", "claude", "-C", alpha, "from before the owners")
	e.waitFor("idle", id, turnEnded)
	// A session the main konduktor started before kk recorded owners.
	e.stop()
	path := filepath.Join(e.home, "conversations", id+".yaml")
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "owner: coordinator\n") {
		t.Fatalf("no owner recorded:\n%s", data)
	}
	os.WriteFile(path, []byte(strings.Replace(string(data), "owner: coordinator\n", "", 1)), 0o600)
	e.start()
	e.startKonduktor(alpha, "hello alpha")
	if c := e.conv(id); c.Konduktor != coordinatorID {
		t.Fatalf("the main konduktor's session went to %q", c.Konduktor)
	}
	// A session the user started goes to the konduktor of its directory once an agent of that konduktor takes it over.
	mine := e.mustKK("new", "claude", "-C", alpha, "the user's own")
	k := e.konduktors()[1].ID
	e.waitFor("idle", mine, turnEnded)
	e.mustAs(k, "send", mine, "taken over")
	e.waitFor("taken over", mine, func(c convView) bool { return c.Konduktor == k && c.WaitingFor == waitingKonduktor })
}

func TestKonduktorPanel(t *testing.T) {
	e := newCoordinatorEnv(t)
	ids := e.dirSessions()
	alpha := filepath.Join(e.work, "alpha")
	// Read sessions leave the tab of alpha to its konduktor.
	for _, title := range []string{"Export invoices", "Tune the ranking"} {
		e.post("/api/conversations/"+ids[title]+"/seen", map[string]string{})
	}
	p := e.openPage(1600, 900)
	head := `document.querySelector(".side-panel .konduktor-name")?.textContent`
	away := `document.querySelector(".side-panel .konduktor-away")`
	notStarted := `(document.querySelector(".side-panel .konduktor-pick h1 .muted")?.textContent === "not started")`
	pickRow := func(name string) {
		p.click(`document.querySelector(".side-panel .konduktor-name")`)
		p.click(fmt.Sprintf(`[...document.querySelectorAll(".dir-menu .dir-item")].find((b) => b.querySelector(".mono").textContent === %q)`, name))
	}
	// All shows the main konduktor.
	p.until(head + ` === "All"`)
	if n := len(p.texts(".side-panel .pin")); n != 0 {
		t.Fatal("the konduktor panel has a pin")
	}
	p.shot("konduktor-all")

	// A directory without a konduktor offers to start one with the first message.
	p.dirTab("alpha")
	p.until(`document.querySelector(".side-panel .konduktor-hint")?.textContent === "The konduktor of alpha starts with your first message."`)
	p.until(head + ` === "alpha" && ` + notStarted)
	p.eval(`(() => {
		const area = document.querySelector(".side-panel textarea")
		Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype, "value").set.call(area, "hello alpha")
		area.dispatchEvent(new Event("input", { bubbles: true }))
		area.focus()
	})()`, nil)
	p.shot("konduktor-start")
	p.press("Enter", 13, "\r")
	p.until(`[...document.querySelectorAll(".side-panel .msg.assistant")].some((m) => m.textContent.includes("echo: hello alpha"))`)
	ks := e.konduktors()
	if len(ks) != 2 || ks[1].Scope != alpha {
		t.Fatalf("konduktors: %+v", ks)
	}
	k := ks[1].ID
	p.until(head + ` === "alpha" && !` + notStarted)
	p.shot("konduktor-alpha")

	// It follows the tab.
	p.dirTab("beta")
	p.until(head + ` === "beta" && ` + notStarted + ` && document.querySelector(".side-panel .konduktor-hint")`)
	p.dirTab("All")
	p.until(head + ` === "All" && !` + away)

	// The menu lists main and every directory, also those whose konduktor has not started: kk is the one of the inbox sessions.
	// The answer shows before the konduktor's turn ends.
	e.waitFor("the alpha konduktor's turn", k, turnEnded)
	p.click(`document.querySelector(".side-panel .konduktor-name")`)
	p.until(`[...document.querySelectorAll(".dir-menu .dir-item")].some((b) => b.textContent === "alphaYour turn")`)
	if got := p.texts(".dir-menu .dir-item"); !slices.Equal(got, []string{"AllYour turn", "alphaYour turn", "betanot started", "gammanot started", "kknot started"}) {
		t.Fatalf("konduktor menu: %q", got)
	}
	p.shot("konduktor-menu")
	p.click(`document.querySelector(".side-panel .konduktor-name")`)
	p.until(`!document.querySelector(".dir-menu")`)

	// A konduktor picked in the menu holds until the tab changes, and the head offers the way back.
	pickRow("alpha")
	p.until(head + ` === "alpha" && !` + notStarted + ` && ` + away + `?.textContent === "Showing alpha, not All.Back to All"`)
	p.shot("konduktor-away")
	p.dirTab("beta")
	p.until(head + ` === "beta" && ` + notStarted + ` && !` + away)
	pickRow("alpha")
	p.until(head + ` === "alpha" && !` + notStarted)
	p.click(`[...document.querySelectorAll(".side-panel .konduktor-away button")].find((b) => b.textContent === "Back to beta")`)
	p.until(head + ` === "beta" && ` + notStarted + ` && !` + away)
	// Picking the tab again also ends it, and a konduktor picked before it starts is the one a message starts.
	pickRow("gamma")
	p.until(head + ` === "gamma" && ` + notStarted + ` && document.querySelector(".side-panel .konduktor-hint")?.textContent.includes("gamma")`)
	p.shot("konduktor-away-start")
	p.dirTab("beta")
	p.until(head + ` === "beta" && ` + notStarted + ` && !` + away)

	// A konduktor that wrote while another is shown marks its directory.
	p.until(`![...document.querySelectorAll("nav.tabs .dirs .tab.unread")].some((a) => a.textContent === "alpha")`)
	e.mustKK("send", k, "ping")
	p.until(`[...document.querySelectorAll("nav.tabs .dirs .tab.unread")].some((a) => a.textContent === "alpha")`)
	p.shot("konduktor-unread")
	p.dirTab("alpha")
	p.until(`![...document.querySelectorAll("nav.tabs .dirs .tab.unread")].some((a) => a.textContent === "alpha")`)
	// The page clears the mark before the server hears that the konduktor was seen.
	e.waitFor("the konduktor shown read", k, func(c convView) bool { return !c.Unread })

	// Closing it from the panel brings back the start.
	p.click(`[...document.querySelectorAll(".side-panel .conv-head button")].find((b) => b.textContent === "Close")`)
	p.click(`[...document.querySelectorAll(".side-panel .conv-head button")].find((b) => b.textContent === "Confirm close")`)
	p.until(`document.querySelector(".side-panel .konduktor-hint")`)
}

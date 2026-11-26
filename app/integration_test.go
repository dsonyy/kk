package main

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
)

// lastLaunch is the argv of the last start of a fake claude that is not a headless run.
func (e *env) lastLaunch() []string {
	e.t.Helper()
	data, _ := os.ReadFile(filepath.Join(e.fake, "argv.log"))
	var last []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var argv []string
		if json.Unmarshal([]byte(line), &argv) == nil && argv[0] == "claude" {
			last = argv
		}
	}
	return last
}

func argOf(argv []string, name string) (string, bool) {
	i := slices.Index(argv, name)
	if i < 0 || i+1 >= len(argv) {
		return "", false
	}
	return argv[i+1], true
}

// checkLimits fails unless argv starts Claude Code with exactly the limits of the Gmail check.
func checkLimits(t *testing.T, argv []string) {
	t.Helper()
	want := map[string]string{
		"--setting-sources": "", "--tools": "", "--permission-mode": "dontAsk", "--model": "sonnet",
		"--allowedTools":    "mcp__claude_ai_Gmail__search_threads,mcp__claude_ai_Gmail__get_thread,mcp__claude_ai_Gmail__get_message,mcp__claude_ai_Gmail__list_labels",
		"--disallowedTools": gmailRun.tools(gmailRun.write),
		"--system-prompt":   gmailRun.system,
	}
	for name, v := range want {
		if got, ok := argOf(argv, name); !ok || got != v {
			t.Fatalf("%s is %q, want %q: %v", name, got, v, argv)
		}
	}
	if !slices.Contains(argv, "--disable-slash-commands") {
		t.Fatalf("slash commands are on: %v", argv)
	}
	settings, _ := argOf(argv, "--settings")
	var s struct {
		Hooks map[string][]struct {
			Hooks []struct{ Command string } `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal([]byte(settings), &s); err != nil || len(s.Hooks["PreToolUse"]) != 2 ||
		!strings.Contains(s.Hooks["PreToolUse"][0].Hooks[0].Command, "'guard' '"+want["--allowedTools"]+"'") {
		t.Fatalf("the guard hook is missing: %s", settings)
	}
}

func (e *env) toolsRun() []string {
	data, _ := os.ReadFile(filepath.Join(e.fake, "gmail-tools.log"))
	return strings.Fields(string(data))
}

func TestIntegrationSession(t *testing.T) {
	e := newEnv(t, "KK_COORDINATOR=1")
	e.waitFor("conductor start", coordinatorID, func(c convView) bool { return c.Status == statusIdle })
	id := integrationID("gmail")
	writeFile(t, filepath.Join(e.fake, "gmail.out"), gmailAnswer)
	e.refreshInbox("gmail")
	e.post("/api/inbox/tick", struct{}{})

	c := e.waitFor("the first check", id, func(c convView) bool { return c.Status == statusIdle })
	if c.Integration != "gmail" || c.Title != "Gmail inbox" || c.Attention != "" || c.WaitingFor != "" || c.Unread || c.Group != inboxGroup {
		t.Fatalf("a normal run should leave the session quiet: %+v", c.Conversation)
	}
	if !hasEvent(c, kindAssistant, gmailAnswer) || c.Usage == nil || c.Usage.Output != 50 || slices.Contains(c.Actions, "reset") {
		t.Fatalf("the session should show the run with its usage: %+v %+v %v", c.Events, c.Usage, c.Actions)
	}
	var listed []convView
	_, out := e.request("GET", "/api/conversations", nil)
	json.Unmarshal(out, &listed)
	if !slices.ContainsFunc(listed, func(l convView) bool { return l.ID == id }) {
		t.Fatalf("the session is not in the list: %s", out)
	}
	argv := e.lastLaunch()
	checkLimits(t, argv)
	first, _ := argOf(argv, "--session-id")
	if p, _ := argOf(argv, "--"); !strings.HasPrefix(p, "Collect my Gmail inbox. Now is ") || slices.Contains(argv, "--resume") {
		t.Fatalf("a run starts a fresh agent with its prompt: %v", argv)
	}
	if got := e.toolsRun(); !slices.Equal(got, []string{"mcp__claude_ai_Gmail__search_threads"}) {
		t.Fatalf("tools run: %v", got)
	}

	// Typing into the session neither widens its tools nor makes it the conductor's.
	for i, tool := range []string{"mcp__claude_ai_Gmail__send_message", "Bash", "mcp__claude_ai_Google_Drive__create_file", "mcp__claude_ai_Gmail__get_thread"} {
		e.asConductor("send", id, "use "+tool)
		c = e.waitFor("the typed turn", id, func(c convView) bool { return c.Status == statusIdle && userTurns(c) == 2+i && c.Events[len(c.Events)-1].Text == "done" })
	}
	if got := e.toolsRun(); !slices.Equal(got, []string{"mcp__claude_ai_Gmail__search_threads", "mcp__claude_ai_Gmail__get_thread"}) {
		t.Fatalf("only the read tools may run, whatever is typed: %v", got)
	}
	refused := 0
	for _, ev := range c.Events {
		if ev.Kind == kindTool && ev.Failed && strings.Contains(ev.Output, "kk lets this session use only") {
			refused++
		}
	}
	if refused != 3 || c.WaitingFor != "" {
		t.Fatalf("the guard should refuse three tools and the session wait for nobody: %d %q %+v", refused, c.WaitingFor, c.Events)
	}

	// A stopped session resumes with the same limits.
	e.mustKK("stop", id)
	e.waitFor("stop", id, func(c convView) bool { return c.Status == statusExited })
	if c = e.conv(id); c.WaitingFor != "" {
		t.Fatalf("a stopped session waits for %q", c.WaitingFor)
	}
	e.mustKK("resume", id)
	e.waitFor("resume", id, func(c convView) bool { return c.Status == statusIdle })
	argv = e.lastLaunch()
	checkLimits(t, argv)
	if s, _ := argOf(argv, "--resume"); s != first {
		t.Fatalf("resume: %v", argv)
	}
	e.mustKK("send", id, "use Bash")
	e.waitFor("the guard after a resume", id, func(c convView) bool { return c.Status == statusIdle && userTurns(c) == 6 && c.Events[len(c.Events)-1].Text == "done" })
	if got := e.toolsRun(); slices.Contains(got, "Bash") {
		t.Fatalf("Bash ran after a resume: %v", got)
	}

	// The next run clears the context: a fresh agent in a new transcript, the last one removed, its usage kept.
	transcript := e.conv(id).Transcript
	e.refreshInbox("gmail")
	c = e.waitFor("the second check", id, func(c convView) bool { return c.Status == statusIdle && c.Transcript != transcript })
	argv = e.lastLaunch()
	checkLimits(t, argv)
	if s, _ := argOf(argv, "--session-id"); s == first || userTurns(c) != 1 {
		t.Fatalf("the second run should start over: %v %+v", argv, c.Events)
	}
	if _, err := os.Stat(transcript); !os.IsNotExist(err) {
		t.Fatalf("the transcript of the last run should be gone: %v", err)
	}
	var saved Conversation
	data, _ := os.ReadFile(filepath.Join(e.home, "conversations", id+".yaml"))
	yaml.Unmarshal(data, &saved)
	if len(saved.Transcripts) != 2 || saved.Transcripts[earlierRuns].Output != 300 || c.Usage.Output != 350 {
		t.Fatalf("usage should fold the earlier runs into one entry: %+v %+v", saved.Transcripts, c.Usage)
	}

	// A failed run asks for the user, the next good one clears it.
	writeFile(t, filepath.Join(e.fake, "gmail-error"), "")
	e.refreshInbox("gmail")
	c = e.conv(id)
	if c.Attention != "The Gmail run ended on an API error." || c.WaitingFor != waitingUser {
		t.Fatalf("a failed run should need the user: %+v", c.Conversation)
	}
	os.Remove(filepath.Join(e.fake, "gmail-error"))
	e.refreshInbox("gmail")
	if c = e.conv(id); c.Attention != "" || c.WaitingFor != "" {
		t.Fatalf("a good run should clear the attention: %+v", c.Conversation)
	}

	if c := e.conv(coordinatorID); slices.ContainsFunc(c.Events, func(ev Event) bool { return strings.Contains(ev.Text, id) }) {
		t.Fatalf("the conductor heard of the session: %+v", c.Events)
	}

	// Chains, schedules, closing and resuming from history keep off the session.
	if out, err := e.kk("new", "claude", "-C", e.work, "-after", id, "next"); err == nil {
		t.Fatalf("a session followed the integration: %s", out)
	}
	if resp, _ := e.request("PUT", "/api/conversations/"+id+"/schedule", strings.NewReader(`{"days":[1],"times":["09:00"],"prompt":"x"}`)); resp.StatusCode != 400 {
		t.Fatalf("schedule: %d", resp.StatusCode)
	}
	if out, err := e.kk("close", id); err == nil {
		t.Fatalf("closed: %s", out)
	}
	if code, out := e.post("/api/conversations", map[string]string{"harness": "claude", "session": e.conv(id).Session, "dir": e.work}); code != 400 {
		t.Fatalf("resumed the session from history without its limits: %d %s", code, out)
	}

	// Mail text stays out of kk's state, except for the excerpts of the inbox.
	filepath.WalkDir(e.home, func(path string, d fs.DirEntry, err error) error {
		if d.IsDir() || filepath.Base(path) == "inbox.yaml" {
			return nil
		}
		data, _ := os.ReadFile(path)
		for _, text := range []string{"Contract review", "Please review the attached contract", "Gus"} {
			if strings.Contains(string(data), text) {
				t.Errorf("%s holds mail text %q", path, text)
			}
		}
		return nil
	})

	// Switching the source off stops the session.
	_, out = e.request("GET", "/api/settings", nil)
	var view settingsView
	json.Unmarshal(out, &view)
	view.Settings.Inbox["gmail"] = InboxSettings{IntervalMinutes: 15, Methods: map[string]bool{"connector": false}}
	data, _ = json.Marshal(view.Settings)
	if resp, out := e.request("PUT", "/api/settings", strings.NewReader(string(data))); resp.StatusCode != 200 {
		t.Fatalf("save: %s", out)
	}
	c = e.waitFor("the session closed", id, func(c convView) bool { return c.Closed && c.Status == statusExited })
	if alive, _ := e.kk("screen", id); strings.TrimSpace(alive) != "" {
		t.Fatalf("the window is still there: %s", alive)
	}
	if out, err := e.kk("resume", id); err == nil {
		t.Fatalf("a switched off source resumed: %s", out)
	}
	e.post("/api/inbox/tick", struct{}{})
	time.Sleep(500 * time.Millisecond)
	if c = e.conv(id); !c.Closed {
		t.Fatalf("a switched off source ran: %+v", c.Conversation)
	}
}

func userTurns(c convView) int {
	n := 0
	for _, ev := range c.Events {
		if ev.Kind == kindUser {
			n++
		}
	}
	return n
}


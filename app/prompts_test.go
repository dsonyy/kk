package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// putSettings saves a partial settings body and returns the server's view of the result.
func (e *env) putSettings(body map[string]any) settingsView {
	e.t.Helper()
	data, _ := json.Marshal(body)
	out, err := exec.Command("curl", "-s", "-f", "-X", "PUT", "--unix-socket", filepath.Join(e.home, "kk.sock"), "-d", string(data), "http://kk/api/settings").Output()
	if err != nil {
		e.t.Fatalf("saving settings: %v %s", err, out)
	}
	var v settingsView
	json.Unmarshal(out, &v)
	return v
}

// starts returns the argv of every start of the fake program, in order.
func (e *env) starts(program string) [][]string {
	data, _ := os.ReadFile(filepath.Join(e.fake, "argv.log"))
	var out [][]string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var argv []string
		if json.Unmarshal([]byte(line), &argv) == nil && argv[0] == program {
			out = append(out, argv[1:])
		}
	}
	return out
}

func TestKonduktorPrompt(t *testing.T) {
	e := newEnv(t, "KK_COORDINATOR=1")
	e.waitFor("konduktor start", coordinatorID, func(c convView) bool { return c.Status == statusIdle })
	file := filepath.Join(e.home, "konduktors", coordinatorID+".md")
	// The section about the konduktors follows the editable base and overrides it where they differ.
	read := func() string {
		data, _ := os.ReadFile(file)
		base, section, ok := strings.Cut(string(data), "\n## Konduktors\n")
		if !ok || !strings.Contains(section, "This section overrides the text above") {
			t.Fatalf("prompt file without the section about the konduktors:\n%s", data)
		}
		return base
	}
	root := filepath.Dir(e.home)
	filled := func(text string) string {
		return strings.NewReplacer("{{BIN}}", kkBin, "{{HOME}}", e.home, "{{SOURCE}}", root).Replace(text)
	}

	v := e.putSettings(map[string]any{"coordinator_prompt": "# Mine\n\nRun {{BIN}}, state in {{HOME}}, source in {{SOURCE}}.", "worker_prompt": "Worker rules."})
	if v.Defaults["coordinator_prompt"] != coordinatorPrompt || v.Placeholders["{{HOME}}"] != e.home || v.Settings.WorkerPrompt != "Worker rules." {
		t.Fatalf("settings view: %+v", v)
	}
	want := "# Mine\n\nRun " + kkBin + ", state in " + e.home + ", source in " + root + "."
	if got := read(); got != want {
		t.Fatalf("prompt file:\n%s\nwant:\n%s", got, want)
	}
	if n := strings.Count(e.mustKK("screen", coordinatorID), "fake claude started"); n != 1 {
		t.Fatalf("saving restarted the konduktor: %d starts", n)
	}

	// The next start reads the edited prompt, and the worker prompt is not the konduktor's.
	e.mustKK("stop", coordinatorID)
	e.waitFor("konduktor stop", coordinatorID, func(c convView) bool { return c.Status == statusExited })
	e.stop()
	e.start()
	e.waitFor("konduktor restart", coordinatorID, func(c convView) bool { return c.Status == statusIdle })
	starts := e.starts("claude")
	last := starts[len(starts)-1]
	if p, _ := argOf(last, "--append-system-prompt-file"); p != file || strings.Contains(strings.Join(last, " "), "Worker rules.") || read() != want {
		t.Fatalf("konduktor start: %v\n%s", last, read())
	}

	// Empty, blank and the default text itself all mean the default, so it keeps following the built-in one.
	for _, text := range []string{"", "  \n", coordinatorPrompt + "\n"} {
		e.putSettings(map[string]any{"coordinator_prompt": "edited"})
		if v := e.putSettings(map[string]any{"coordinator_prompt": text}); v.Settings.CoordinatorPrompt != "" || read() != filled(coordinatorPrompt) {
			t.Fatalf("%q did not reset to the default: %q\n%s", text, v.Settings.CoordinatorPrompt, read())
		}
	}
	if data, _ := os.ReadFile(settingsPath(e.home)); strings.Contains(string(data), "coordinator_prompt") {
		t.Fatalf("settings.yaml keeps the default:\n%s", data)
	}
}

func TestWorkerPrompt(t *testing.T) {
	e := newEnv(t)
	worker := "Work in small commits.\nSay \"done\" at the end."
	e.putSettings(map[string]any{"worker_prompt": worker})

	claude := e.mustKK("new", "claude", "-C", e.work, "hi claude")
	codex := e.mustKK("new", "codex", "-C", e.work, "hi codex")
	cursor := e.mustKK("new", "cursor", "-C", e.work, "hi cursor")
	for _, id := range []string{claude, codex, cursor} {
		e.waitFor("answer", id, func(c convView) bool { return c.Status == statusIdle })
	}
	if p, _ := argOf(e.starts("claude")[0], "--append-system-prompt"); p != worker {
		t.Fatalf("claude: %v", e.starts("claude")[0])
	}
	quoted, _ := json.Marshal(worker)
	if !slices.Contains(e.starts("codex")[0], "developer_instructions="+string(quoted)) {
		t.Fatalf("codex: %v", e.starts("codex")[0])
	}
	// kk also runs cursor-agent to list its models.
	starts := e.starts("cursor-agent")
	if p, _ := argOf(starts[len(starts)-1], "--"); p != worker+"\n\nhi cursor" {
		t.Fatalf("cursor: %q", p)
	}
	if c := e.conv(cursor); c.Title != "hi cursor" || c.Prompt != "hi cursor" {
		t.Fatalf("the worker prompt went into the session's own prompt: %q %q", c.Title, c.Prompt)
	}

	// A resumed Claude Code session gets the system prompt again, since Claude Code does not keep it.
	e.mustKK("stop", claude)
	e.waitFor("stop", claude, func(c convView) bool { return c.Status == statusExited })
	e.mustKK("resume", claude)
	e.waitFor("resume", claude, func(c convView) bool { return c.Status == statusIdle })
	if p, _ := argOf(e.starts("claude")[1], "--append-system-prompt"); p != worker || !strings.Contains(strings.Join(e.starts("claude")[1], " "), "--resume") {
		t.Fatalf("claude resume: %v", e.starts("claude")[1])
	}

	// The worker prompt rides on every launch's command line, which tmux limits.
	big, _ := json.Marshal(map[string]string{"worker_prompt": strings.Repeat("x", workerPromptMax+1)})
	if resp, out := e.request("PUT", "/api/settings", bytes.NewReader(big)); resp.StatusCode != 400 || !strings.Contains(string(out), "at most") {
		t.Fatalf("an oversize worker prompt was saved: %d %s", resp.StatusCode, out)
	}

	writeFile(t, filepath.Join(e.fake, "gmail.out"), gmailAnswer)
	e.refreshInbox("gmail")
	argv := e.lastLaunch()
	checkLimits(t, argv)
	if strings.Contains(strings.Join(argv, " "), "small commits") {
		t.Fatalf("the inbox check got the worker prompt: %v", argv)
	}
}

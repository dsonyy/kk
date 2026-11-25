package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

type Harness struct {
	Name   string   `json:"name"`
	Label  string   `json:"label"`
	Models []string `json:"models"`
	// Hooks tells whether the harness reports its state through kk hooks.
	Hooks bool `json:"hooks"`
	// Rewind is nil when the harness cannot go back to an earlier message.
	Rewind *Rewind `json:"rewind,omitempty"`
}

var hookEvents = []string{"SessionStart", "UserPromptSubmit", "PreToolUse", "PostToolUse", "Stop", "Notification", "PermissionRequest"}

func harnesses() []Harness {
	return []Harness{
		{Name: "claude", Label: "Claude Code", Models: []string{"", "opus", "sonnet", "haiku", "fable"}, Hooks: true, Rewind: rewinds["claude"]},
		{Name: "codex", Label: "Codex", Models: codexModels(), Hooks: true, Rewind: rewinds["codex"]},
		{Name: "cursor", Label: "Cursor", Models: cursorModels(), Hooks: true, Rewind: rewinds["cursor"]},
	}
}

// controls holds, per harness, how kk acts on a running agent. A harness that is not listed gets only stop.
var controls = map[string]struct {
	// interrupt is the key that stops the current turn.
	interrupt string
	// reset is the command that starts a new conversation. Codex also has /new, which asks where to start the chat.
	// Without a command kk starts a fresh agent: after Cursor's /clear its hooks keep reporting the old chat.
	reset string
	// blind marks a harness whose turns kk can miss: Cursor reports no prompt and writes its transcript late,
	// so a turn without tool calls looks idle and must stay interruptible. It also reports no session start when it
	// resumes a chat, so a resumed agent counts as started.
	blind bool
}{
	"claude": {interrupt: "Escape", reset: "/clear"},
	"codex":  {interrupt: "Escape", reset: "/clear"},
	"cursor": {interrupt: "C-c", blind: true},
}

// actions lists the controls that apply to c in its current state.
func actions(c Conversation) []string {
	out := []string{}
	ct, ok := controls[c.Harness]
	if ok && (c.Status == statusWorking || ct.blind && c.Status == statusIdle) {
		out = append(out, "interrupt")
	}
	if ok && (c.Status == statusIdle || c.Status == statusBackground) && c.Attention == "" {
		out = append(out, "reset")
	}
	if c.Status != statusExited {
		out = append(out, "stop")
	}
	return out
}

func findHarness(name string) (Harness, bool) {
	for _, h := range harnesses() {
		if h.Name == name {
			return h, true
		}
	}
	return Harness{}, false
}

func codexHome() string {
	if h := os.Getenv("CODEX_HOME"); h != "" {
		return h
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".codex")
}

func codexModels() []string {
	var cache struct {
		Models []struct {
			Slug       string `json:"slug"`
			Visibility string `json:"visibility"`
		} `json:"models"`
	}
	data, _ := os.ReadFile(filepath.Join(codexHome(), "models_cache.json"))
	json.Unmarshal(data, &cache)
	var out []string
	for _, m := range cache.Models {
		if m.Visibility == "list" {
			out = append(out, m.Slug)
		}
	}
	if len(out) == 0 {
		out = []string{""}
	}
	return out
}

var cursorCache struct {
	sync.Mutex
	models []string
	at     time.Time
}

func cursorModels() []string {
	cursorCache.Lock()
	defer cursorCache.Unlock()
	if time.Since(cursorCache.at) < 10*time.Minute {
		return cursorCache.models
	}
	cursorCache.at = time.Now()
	cursorCache.models = []string{""}
	var listed []string
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "cursor-agent", "--list-models").Output()
	if err != nil {
		return cursorCache.models
	}
	for _, line := range strings.Split(string(out), "\n") {
		id, _, ok := strings.Cut(strings.TrimSpace(line), " - ")
		if ok && id != "" && !strings.Contains(id, " ") {
			listed = append(listed, id)
		}
	}
	if len(listed) > 0 {
		cursorCache.models = listed
	}
	return cursorCache.models
}

func newUUID() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

func (s *Server) claudeSettings() (string, error) {
	hooks := map[string]any{}
	for _, e := range hookEvents {
		hooks[e] = []any{map[string]any{"hooks": []any{map[string]any{"type": "command", "command": shellJoin([]string{s.bin, "hook", e})}}}}
	}
	data, _ := json.MarshalIndent(map[string]any{"hooks": hooks}, "", "  ")
	path := filepath.Join(s.store.dir, "hooks", "claude.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	return path, os.WriteFile(path, data, 0o600)
}

// command builds the argv that starts or resumes the agent of c.
func (s *Server) command(c *Conversation, resume bool) ([]string, error) {
	switch c.Harness {
	case "claude":
		settings, err := s.claudeSettings()
		if err != nil {
			return nil, err
		}
		argv := []string{"claude", "--settings", settings}
		if c.Model != "" {
			argv = append(argv, "--model", c.Model)
		}
		if c.Role == coordinatorID {
			argv = append(argv, "--append-system-prompt-file", filepath.Join(s.store.dir, "coordinator.md"), "--name", "kk conductor")
		}
		if resume {
			return append(argv, "--resume", c.Session), nil
		}
		if c.Session == "" {
			c.Session = newUUID()
		}
		argv = append(argv, "--session-id", c.Session)
		if c.Prompt != "" {
			argv = append(argv, "--", c.Prompt)
		}
		return argv, nil
	case "codex":
		argv := []string{"codex", "--dangerously-bypass-hook-trust"}
		for _, e := range hookEvents {
			argv = append(argv, "-c", fmt.Sprintf(`hooks.%s=[{hooks=[{type="command",command=%q}]}]`, e, shellJoin([]string{s.bin, "hook", e})))
		}
		if c.Model != "" {
			argv = append(argv, "-m", c.Model)
		}
		if resume {
			return append(argv, "resume", c.Session), nil
		}
		if c.Prompt != "" {
			argv = append(argv, "--", c.Prompt)
		}
		return argv, nil
	case "cursor":
		plugin, err := s.cursorPlugin()
		if err != nil {
			return nil, err
		}
		argv := []string{"cursor-agent", "--plugin-dir", plugin}
		if c.Model != "" {
			argv = append(argv, "--model", c.Model)
		}
		if resume {
			if c.Session == "" {
				return append(argv, "--continue"), nil
			}
			return append(argv, "--resume", c.Session), nil
		}
		if c.Prompt != "" {
			argv = append(argv, "--", c.Prompt)
		}
		return argv, nil
	}
	return nil, fmt.Errorf("unknown harness %q", c.Harness)
}

// cursorPlugin writes a Cursor plugin whose hooks report to kk. Cursor CLI 2026.11 fires only
// sessionStart and the tool hooks; turns end in the transcript.
func (s *Server) cursorPlugin() (string, error) {
	dir := filepath.Join(s.store.dir, "hooks", "cursor")
	if err := os.MkdirAll(filepath.Join(dir, ".cursor-plugin"), 0o700); err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Join(dir, "hooks"), 0o700); err != nil {
		return "", err
	}
	manifest := `{"name":"kk","version":"1.0.0","description":"Reports agent state to kk"}`
	if err := os.WriteFile(filepath.Join(dir, ".cursor-plugin", "plugin.json"), []byte(manifest), 0o600); err != nil {
		return "", err
	}
	events := map[string]string{"sessionStart": "SessionStart", "beforeSubmitPrompt": "UserPromptSubmit", "preToolUse": "PreToolUse", "postToolUse": "PostToolUse", "stop": "Stop"}
	hooks := map[string]any{}
	for cursor, kk := range events {
		hooks[cursor] = []any{map[string]string{"command": shellJoin([]string{s.bin, "hook", kk})}}
	}
	data, _ := json.MarshalIndent(map[string]any{"version": 1, "hooks": hooks}, "", "  ")
	return dir, os.WriteFile(filepath.Join(dir, "hooks", "hooks.json"), data, 0o600)
}

var nonSlug = regexp.MustCompile(`[^A-Za-z0-9]+`)

func cursorTranscript(dir, session string) string {
	h, _ := os.UserHomeDir()
	slug := strings.Trim(nonSlug.ReplaceAllString(dir, "-"), "-")
	return filepath.Join(h, ".cursor", "projects", slug, "agent-transcripts", session, session+".jsonl")
}

func parserFor(harness string) func(*Tail, []byte) {
	switch harness {
	case "cursor":
		return parseCursor
	case "claude":
		return parseClaude
	case "codex":
		return parseCodex
	}
	return nil
}

func codexThreadName(session string) string {
	data, err := os.ReadFile(filepath.Join(codexHome(), "session_index.jsonl"))
	if err != nil {
		return ""
	}
	name := ""
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.Contains(line, session) {
			continue
		}
		var e struct {
			ID   string `json:"id"`
			Name string `json:"thread_name"`
		}
		if json.Unmarshal([]byte(line), &e) == nil && e.ID == session {
			name = e.Name
		}
	}
	return name
}

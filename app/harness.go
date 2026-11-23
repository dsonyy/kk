package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
}

var hookEvents = []string{"SessionStart", "UserPromptSubmit", "PreToolUse", "PostToolUse", "Stop", "Notification", "PermissionRequest"}

func harnesses() []Harness {
	return []Harness{
		{Name: "claude", Label: "Claude Code", Models: []string{"", "opus", "sonnet", "haiku", "fable"}, Hooks: true},
		{Name: "codex", Label: "Codex", Models: codexModels(), Hooks: true},
		{Name: "cursor", Label: "Cursor", Models: cursorModels(), Hooks: false},
	}
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
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "cursor-agent", "--list-models").Output()
	if err != nil {
		return cursorCache.models
	}
	for _, line := range strings.Split(string(out), "\n") {
		id, _, ok := strings.Cut(strings.TrimSpace(line), " - ")
		if ok && id != "" && !strings.Contains(id, " ") {
			cursorCache.models = append(cursorCache.models, id)
		}
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
			argv = append(argv, "--append-system-prompt-file", filepath.Join(s.store.dir, "coordinator.md"), "--name", "kk coordinator")
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
		argv := []string{"cursor-agent"}
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

func parserFor(harness string) func(*Tail, []byte) {
	switch harness {
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

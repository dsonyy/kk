// fakeagent imitates claude, codex and cursor-agent for kk's end-to-end tests. It reads the hook
// configuration kk passes, calls the hooks and writes transcripts in the harness's real format.
package main

import (
	"bufio"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var (
	mode       string
	hooks      = map[string]string{}
	session    string
	transcript string
	in         = bufio.NewReader(os.Stdin)
)

func main() {
	mode = filepath.Base(os.Args[0])
	args := os.Args[1:]
	logArgs(args)
	prompt, resume := parseArgs(args)
	fmt.Print("\x1b[?2004h")
	fmt.Printf("fake %s started\n", mode)
	if strings.Contains(prompt, "TRUST") {
		fmt.Println("Do you trust this folder?\n 1. Yes, I trust this folder\n 2. No, exit")
		if strings.TrimSpace(readMessage()) != "1" {
			os.Exit(1)
		}
	}
	if session == "" {
		session = uuid()
	}
	dir := filepath.Join(os.Getenv("FAKE_HOME"), mode)
	transcript = filepath.Join(dir, session+".jsonl")
	if mode == "cursor-agent" {
		wd, _ := os.Getwd()
		slug := strings.Trim(regexp.MustCompile(`[^A-Za-z0-9]+`).ReplaceAllString(wd, "-"), "-")
		dir = filepath.Join(os.Getenv("HOME"), ".cursor", "projects", slug, "agent-transcripts", session)
		transcript = filepath.Join(dir, session+".jsonl")
	}
	os.MkdirAll(dir, 0o755)
	if mode == "codex" && !resume {
		write(map[string]any{"type": "session_meta", "payload": map[string]any{"session_id": session}})
	}
	hook("SessionStart", nil)
	if prompt != "" {
		handle(prompt)
	}
	for {
		fmt.Print("> ")
		msg := readMessage()
		if msg == "" {
			continue
		}
		handle(msg)
	}
}

func logArgs(args []string) {
	if dir := os.Getenv("FAKE_HOME"); dir != "" {
		f, _ := os.OpenFile(filepath.Join(dir, "argv.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		json.NewEncoder(f).Encode(append([]string{mode}, args...))
		f.Close()
		wd, _ := os.Getwd()
		os.WriteFile(filepath.Join(dir, "cwd.log"), []byte(wd+"\n"), 0o644)
	}
}

var codexHook = regexp.MustCompile(`^hooks\.(\w+)=.*command=("(?:[^"\\]|\\.)*")`)

func parseArgs(args []string) (prompt string, resume bool) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		next := func() string { i++; return args[i] }
		switch {
		case a == "--":
			return strings.Join(args[i+1:], " "), resume
		case a == "--settings":
			var s struct {
				Hooks map[string][]struct {
					Hooks []struct{ Command string } `json:"hooks"`
				} `json:"hooks"`
			}
			data, _ := os.ReadFile(next())
			json.Unmarshal(data, &s)
			for e, v := range s.Hooks {
				hooks[e] = v[0].Hooks[0].Command
			}
		case a == "--plugin-dir":
			var p struct {
				Hooks map[string][]struct{ Command string } `json:"hooks"`
			}
			data, _ := os.ReadFile(filepath.Join(next(), "hooks", "hooks.json"))
			json.Unmarshal(data, &p)
			// Cursor CLI 2026.11 fires only these plugin hooks.
			for _, e := range []string{"sessionStart", "preToolUse", "postToolUse"} {
				if v := p.Hooks[e]; len(v) > 0 {
					hooks[strings.ToUpper(e[:1])+e[1:]] = v[0].Command
				}
			}
		case a == "-c":
			if m := codexHook.FindStringSubmatch(next()); m != nil {
				hooks[m[1]], _ = strconv.Unquote(m[2])
			}
		case a == "--session-id":
			session = next()
		case a == "--resume" || (mode == "codex" && a == "resume"):
			session, resume = next(), true
		case a == "--model" || a == "-m" || a == "--append-system-prompt-file" || a == "--name":
			next()
		}
	}
	return "", resume
}

// readMessage reads one submitted message, joining the lines of a bracketed paste.
func readMessage() string {
	line, err := in.ReadString('\n')
	if err != nil {
		os.Exit(0)
	}
	if !strings.Contains(line, "\x1b[200~") {
		return strings.TrimRight(line, "\r\n")
	}
	text := line
	for !strings.Contains(text, "\x1b[201~") {
		more, err := in.ReadString('\n')
		if err != nil {
			os.Exit(0)
		}
		text += more
	}
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\x1b[200~", ""), "\x1b[201~", "")
	return strings.TrimRight(strings.ReplaceAll(text, "\r", "\n"), "\n")
}

func hook(event string, extra map[string]any) {
	cmd, ok := hooks[event]
	if !ok {
		return
	}
	payload := map[string]any{"session_id": session, "transcript_path": transcript, "hook_event_name": event}
	if mode == "cursor-agent" && event == "SessionStart" {
		payload["transcript_path"] = nil
	}
	for k, v := range extra {
		payload[k] = v
	}
	data, _ := json.Marshal(payload)
	c := exec.Command("sh", "-c", cmd)
	c.Stdin = strings.NewReader(string(data))
	if out, err := c.CombinedOutput(); err != nil || len(out) > 0 {
		fmt.Printf("hook %s: %v %s\n", event, err, out)
	}
}

func write(v map[string]any) {
	v["timestamp"] = time.Now().UTC().Format(time.RFC3339Nano)
	data, _ := json.Marshal(v)
	f, _ := os.OpenFile(transcript, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	f.Write(append(data, '\n'))
	f.Close()
}

func handle(prompt string) {
	fmt.Printf("you: %s\n", prompt)
	if mode == "cursor-agent" {
		if prompt == "exit" {
			os.Exit(0)
		}
		cursorLine(map[string]any{"role": "user", "message": map[string]any{"content": []any{map[string]any{"type": "text", "text": "<timestamp>Monday</timestamp>\n<user_query>\n" + prompt + "\n</user_query>"}}}})
		hook("PreToolUse", map[string]any{"tool_name": "Shell"})
		cursorLine(map[string]any{"role": "assistant", "message": map[string]any{"content": []any{map[string]any{"type": "tool_use", "name": "Shell", "input": map[string]any{"command": "ls", "description": "List"}}}}})
		hook("PostToolUse", map[string]any{"tool_name": "Shell"})
		cursorLine(map[string]any{"role": "assistant", "message": map[string]any{"content": []any{map[string]any{"type": "text", "text": "echo: " + prompt}}}})
		cursorLine(map[string]any{"type": "turn_ended", "status": "success"})
		fmt.Printf("cursor answer: %s\n", prompt)
		return
	}
	hook("UserPromptSubmit", map[string]any{"prompt": prompt})
	if prompt == "exit" {
		os.Exit(0)
	}
	if prompt == "/compact" {
		write(map[string]any{"type": "user", "isCompactSummary": true, "message": map[string]any{"role": "user", "content": "summary"}})
		write(map[string]any{"type": "assistant", "message": map[string]any{"role": "assistant", "content": []any{}, "usage": map[string]any{"input_tokens": 3000}}})
		hook("Stop", nil)
		return
	}
	userLine(prompt)
	if strings.Contains(prompt, "permission") {
		hook("PreToolUse", map[string]any{"tool_name": "Bash"})
		hook("Notification", map[string]any{"message": "Claude needs your permission to use Bash", "notification_type": "permission_prompt"})
		fmt.Println("Allow Bash? 1. Yes 2. No")
		if strings.TrimSpace(readMessage()) != "1" {
			hook("Stop", nil)
			return
		}
	}
	tokens := 1000
	if strings.Contains(prompt, "big") {
		tokens = 60000
	}
	toolCall("ls", "a.txt\nb.txt\n")
	hook("PostToolUse", map[string]any{"tool_name": "Bash"})
	answer("echo: "+prompt, tokens)
	if mode == "claude" {
		words := strings.Fields(prompt)
		if len(words) > 3 {
			words = words[:3]
		}
		write(map[string]any{"type": "ai-title", "aiTitle": "Title: " + strings.Join(words, " ")})
	}
	fmt.Printf("agent: echo: %s\n", prompt)
	hook("Stop", map[string]any{"last_assistant_message": "echo: " + prompt})
}

func cursorLine(v map[string]any) {
	data, _ := json.Marshal(v)
	f, _ := os.OpenFile(transcript, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	f.Write(append(data, '\n'))
	f.Close()
}

func userLine(prompt string) {
	if mode == "codex" {
		write(map[string]any{"type": "event_msg", "payload": map[string]any{"type": "item_completed", "item": map[string]any{"type": "UserMessage", "content": []any{map[string]any{"type": "text", "text": prompt}}}}})
		return
	}
	write(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": prompt}})
}

func toolCall(cmd, output string) {
	if mode == "codex" {
		write(map[string]any{"type": "event_msg", "payload": map[string]any{"type": "item_completed", "item": map[string]any{"type": "Reasoning", "summary_text": []string{"Listing files"}}}})
		write(map[string]any{"type": "event_msg", "payload": map[string]any{"type": "item_completed", "item": map[string]any{"type": "CommandExecution", "command": []string{"/usr/bin/zsh", "-lc", cmd}, "aggregated_output": output, "exit_code": 0}}})
		return
	}
	id := "toolu_" + uuid()[:8]
	write(map[string]any{"type": "assistant", "message": map[string]any{"role": "assistant", "content": []any{
		map[string]any{"type": "thinking", "thinking": "Listing files"},
		map[string]any{"type": "tool_use", "id": id, "name": "Bash", "input": map[string]any{"command": cmd}},
	}}})
	write(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": id, "content": output}}}})
}

func answer(text string, tokens int) {
	if mode == "codex" {
		write(map[string]any{"type": "event_msg", "payload": map[string]any{"type": "item_completed", "item": map[string]any{"type": "AgentMessage", "content": []any{map[string]any{"type": "Text", "text": text}}}}})
		write(map[string]any{"type": "event_msg", "payload": map[string]any{"type": "token_count", "info": map[string]any{"last_token_usage": map[string]any{"input_tokens": tokens}}}})
		return
	}
	write(map[string]any{"type": "assistant", "message": map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": text}}, "usage": map[string]any{"input_tokens": 10, "cache_read_input_tokens": tokens - 10}}})
}

func uuid() string {
	b := make([]byte, 16)
	rand.Read(b)
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

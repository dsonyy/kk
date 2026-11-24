// fakeagent imitates claude, codex and cursor-agent for kk's end-to-end tests. It reads the hook
// configuration kk passes, calls the hooks and writes transcripts in the harness's real format.
package main

import (
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
	keys       = make(chan byte, 1<<16)
	// prefill is the prompt a rewind puts back in the input box.
	prefill string
	// turns are the prompts of this run, each with what a rewind needs to go back before it: the
	// parent of the prompt line for claude, the transcript size for codex and cursor-agent.
	turns []turn
	// last is the uuid of the last line of the claude transcript.
	last string
	// rollout is the id of the codex rollout file, which differs from the session id after a revert.
	rollout  string
	reverted bool
	// Codex reports a session cleared with /clear only when its first turn starts.
	cleared bool
)

type turn struct {
	prompt string
	parent string
	offset int64
}

func main() {
	mode = filepath.Base(os.Args[0])
	args := os.Args[1:]
	logArgs(args)
	prompt, resume := parseArgs(args)
	// Real agents read the terminal raw, so Escape and Ctrl-C arrive as bytes instead of being held or turned into a signal.
	raw := exec.Command("stty", "-icanon", "-isig", "-echo", "min", "1")
	raw.Stdin = os.Stdin
	raw.Run()
	go func() {
		buf := make([]byte, 1)
		for {
			if n, err := os.Stdin.Read(buf); err != nil || n == 0 {
				os.Exit(0)
			}
			keys <- buf[0]
		}
	}()
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
	openSession(!resume, map[bool]string{false: "startup", true: "resume"}[resume])
	if prompt != "" {
		handle(prompt)
	}
	for {
		fmt.Print("> ")
		msg := readMessage()
		if msg == "" {
			continue
		}
		if msg == "/rewind" && mode != "codex" {
			rewind()
			continue
		}
		handle(msg)
	}
}

// openSession points the transcript at the current session and reports its start.
func openSession(fresh bool, source string) {
	dir := filepath.Join(os.Getenv("FAKE_HOME"), mode)
	if mode == "cursor-agent" {
		wd, _ := os.Getwd()
		slug := strings.Trim(regexp.MustCompile(`[^A-Za-z0-9]+`).ReplaceAllString(wd, "-"), "-")
		dir = filepath.Join(os.Getenv("HOME"), ".cursor", "projects", slug, "agent-transcripts", session)
	}
	transcript = filepath.Join(dir, session+".jsonl")
	os.MkdirAll(dir, 0o755)
	rollout, turns, last = session, nil, ""
	if !fresh {
		last = "before-" + uuid()
	}
	if mode == "codex" && fresh {
		write(map[string]any{"type": "session_meta", "payload": map[string]any{"session_id": session}})
	}
	if mode == "claude" && fresh {
		write(map[string]any{"type": "attachment"})
	}
	// Cursor CLI 2026.11 reports no session start when it resumes a chat.
	if mode != "cursor-agent" || source != "resume" {
		hook("SessionStart", map[string]any{"source": source})
	}
}

// Each harness stops a turn with its own key. Cursor's /clear is left out: kk restarts Cursor instead.
var (
	resetCommand = map[string]string{"claude": "/clear", "codex": "/clear"}
	interruptKey = map[string]byte{"claude": 0x1b, "codex": 0x1b, "cursor-agent": 0x03}
)

// slowTurn works until the user interrupts it.
func slowTurn() {
	fmt.Println("working, waiting for an interrupt")
	for <-keys != interruptKey[mode] {
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

// readKey returns Enter, Escape, Up, Down, Left, Right, C-u, a paste as "paste:" and its text, or one byte of typed text.
func readKey() string {
	switch b := <-keys; b {
	case '\r', '\n':
		return "Enter"
	case 0x15:
		return "C-u"
	case 0x1b:
		select {
		case <-keys:
		case <-time.After(30 * time.Millisecond):
			return "Escape"
		}
		seq := ""
		for seq == "" || seq[len(seq)-1] < '@' {
			seq += string(<-keys)
		}
		if seq != "200~" {
			return map[string]string{"A": "Up", "B": "Down", "C": "Right", "D": "Left"}[seq]
		}
		text := ""
		for !strings.HasSuffix(text, "\x1b[201~") {
			text += string(<-keys)
		}
		return "paste:" + strings.ReplaceAll(strings.TrimSuffix(text, "\x1b[201~"), "\r", "\n")
	default:
		return string(b)
	}
}

// readMessage reads one submitted message. Like the real input boxes it keeps what a rewind put
// there until Ctrl+U clears it.
func readMessage() string {
	text := ""
	for {
		switch k := readKey(); {
		case k == "Enter":
			text, prefill = prefill+text, ""
			fmt.Println()
			return text
		case k == "C-u":
			text, prefill = "", ""
		case k == "Escape" && mode == "codex" && text == "" && prefill == "":
			backtrack()
		case strings.HasPrefix(k, "paste:"):
			text += k[6:]
		case len(k) == 1:
			text += k
			fmt.Print(k)
		}
	}
}

// choose shows a list with the cursor on one line and returns the line chosen with Enter, or -1 after Escape.
func choose(mark string, head []string, items []string, at int, foot string, digits bool) int {
	for {
		fmt.Print("\x1b[2J\x1b[H")
		fmt.Println(strings.Join(head, "\n"))
		for i, item := range items {
			cursor := strings.Repeat(" ", len([]rune(mark)))
			if i == at {
				cursor = mark
			}
			fmt.Printf(" %s %s\n", cursor, item)
		}
		fmt.Println(foot)
		switch k := readKey(); {
		case k == "Up" && at > 0:
			at--
		case k == "Down" && at < len(items)-1:
			at++
		case k == "Enter":
			return at
		case k == "Escape":
			return -1
		case digits && len(k) == 1 && k[0] >= '1' && int(k[0]-'0') <= len(items):
			return int(k[0] - '1')
		}
	}
}

func size() int64 {
	st, err := os.Stat(transcript)
	if err != nil {
		return 0
	}
	return st.Size()
}

// rewind imitates /rewind of Claude Code 2.1 and Cursor CLI 2026.11. A prompt with "edit" in it counts as one that changed files.
func rewind() {
	defer fmt.Print("\x1b[2J\x1b[H")
	mark, title, foot := "❯", []string{"  Rewind", "  Restore the code and/or conversation to the point before…"}, "  Enter to continue · Esc to cancel"
	if mode == "cursor-agent" {
		mark, title, foot = "→", []string{" Rewind", " Pick a past turn to rewind to. Your files and conversation revert to the state right before that turn - the selected turn and everything after are permanently", " discarded."}, " ↑/↓ to navigate • Enter for details • Shift+Enter to restore • Esc to close"
	}
	var items []string
	for _, t := range turns {
		items = append(items, strings.Split(t.prompt, "\n")[0])
	}
	for {
		at := choose(mark, title, append(items, "(current)"), len(items), foot, false)
		if at < 0 || at == len(items) {
			return
		}
		t := turns[at]
		edited := strings.Contains(t.prompt, "edit")
		var options []string
		var detail string
		if mode == "cursor-agent" {
			options = []string{"1. Restore files and conversation", "2. Restore conversation", "3. Cancel"}
			detail = "   no file changes\n ↑/↓ to navigate • Enter to select • Esc to go back"
			if edited {
				detail = "   Files (1):\n   a.txt +1 -0\n ↑/↓ to navigate • Enter to select • Esc to go back"
			}
		} else if edited {
			options = []string{"1. Restore code and conversation", "2. Restore conversation", "3. Restore code", "4. Summarize from here", "5. Summarize up to here", "6. Never mind"}
			detail = "  ⚠ Restored files go back whole, losing later changes by anyone. Bash changes to other files are not undone."
		} else {
			options = []string{"1. Restore conversation", "2. Summarize from here", "3. Summarize up to here", "4. Never mind"}
		}
		head := append(title[:len(title):len(title)], "   "+items[at])
		if mode == "claude" {
			head = []string{title[0], "  Restore to the point before you sent this message:", "  │ " + items[at], "  The conversation goes back to before this message."}
		}
		picked := choose(mark, head, options, 0, detail, mode == "claude")
		if picked < 0 {
			continue
		}
		option := options[picked][3:]
		if !strings.HasPrefix(option, "Restore") || !strings.HasSuffix(option, "conversation") {
			return
		}
		f, _ := os.OpenFile(filepath.Join(os.Getenv("FAKE_HOME"), "rewind.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		fmt.Fprintf(f, "%s: %s: %s\n", mode, option, items[at])
		f.Close()
		if mode == "cursor-agent" {
			// Cursor rewrites its transcript, here already at the restore instead of at the next message.
			os.Truncate(transcript, t.offset)
		}
		turns, last, prefill = turns[:at], t.parent, t.prompt
		return
	}
}

// backtrack imitates "edit previous message" of Codex 0.166 after the first Esc: a second Esc
// opens the transcript on the last prompt, Left and Right move between prompts, Enter reverts.
// The thread then continues in a new rollout that points at the part of the old one it keeps.
// Written from the Codex source at rust-v0.166.0, not from a live run.
func backtrack() {
	if len(turns) == 0 {
		return
	}
	fmt.Println("  esc again to edit previous message")
	if readKey() != "Escape" {
		return
	}
	at := len(turns) - 1
	for {
		fmt.Print("\x1b[2J\x1b[H")
		fmt.Printf("› %s\n\n  Browsing · ↑↓/jk scroll · ←→/hl prompts · ⌃t details · ↵ rewind · esc back\n", turns[at].prompt)
		switch readKey() {
		case "Left":
			at = max(at-1, 0)
		case "Right":
			at = min(at+1, len(turns)-1)
		case "Escape":
			fmt.Print("\x1b[2J\x1b[H")
			return
		case "Enter":
			t := turns[at]
			base := map[string]any{"thread_id": rollout, "end_ordinal_exclusive": at, "end_byte_offset": t.offset}
			rollout = uuid()
			transcript = filepath.Join(filepath.Dir(transcript), "rollout-"+time.Now().Format("2006-01-02T15-04-05")+"-"+session+"_"+rollout+".jsonl")
			write(map[string]any{"type": "session_meta", "payload": map[string]any{"id": session, "history_mode": "paginated", "history_base": base}})
			turns, prefill, reverted = nil, t.prompt, true
			fmt.Print("\x1b[2J\x1b[H")
			fmt.Printf("• Conversation reverted to this point. File changes are unchanged.\n\n› %s\n", t.prompt)
			return
		}
	}
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
	if mode == "claude" && v["message"] != nil || v["type"] == "attachment" {
		v["parentUuid"], last = last, uuid()
		v["uuid"] = last
	}
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
		turns = append(turns, turn{prompt, "", size()})
		cursorLine(map[string]any{"role": "user", "message": map[string]any{"content": []any{map[string]any{"type": "text", "text": "<timestamp>Monday</timestamp>\n<user_query>\n" + prompt + "\n</user_query>"}}}})
		hook("PreToolUse", map[string]any{"tool_name": "Shell"})
		if strings.Contains(prompt, "slow") {
			slowTurn()
			cursorLine(map[string]any{"type": "turn_ended", "status": "aborted"})
			return
		}
		cursorLine(map[string]any{"role": "assistant", "message": map[string]any{"content": []any{map[string]any{"type": "tool_use", "name": "Shell", "input": map[string]any{"command": "ls", "description": "List"}}}}})
		hook("PostToolUse", map[string]any{"tool_name": "Shell"})
		cursorLine(map[string]any{"role": "assistant", "message": map[string]any{"content": []any{map[string]any{"type": "text", "text": "echo: " + prompt}}}})
		cursorLine(map[string]any{"type": "turn_ended", "status": "success"})
		fmt.Printf("cursor answer: %s\n", prompt)
		return
	}
	if prompt == resetCommand[mode] {
		session = uuid()
		if cleared = mode == "codex"; !cleared {
			openSession(true, "clear")
		}
		return
	}
	if cleared {
		cleared = false
		openSession(true, "clear")
	}
	if reverted {
		// Codex reloads a reverted thread and reports it at the start of the next turn.
		reverted = false
		hook("SessionStart", map[string]any{"source": "resume"})
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
	turns = append(turns, turn{prompt, last, size()})
	if mode == "codex" {
		write(map[string]any{"type": "event_msg", "payload": map[string]any{"type": "task_started"}})
	}
	userLine(prompt)
	if mode == "claude" && strings.Contains(prompt, "apierror") {
		// Claude Code 2.1 ends a turn that died on an API error with this line, fires no Stop hook and waits at its prompt.
		write(map[string]any{"type": "assistant", "isApiErrorMessage": true, "apiError": "stream_idle_timeout", "error": "server_error", "message": map[string]any{"role": "assistant", "model": "<synthetic>", "content": []any{map[string]any{"type": "text", "text": "API Error: The response stopped arriving. The response above may be incomplete."}}}})
		fmt.Println("Resend your message to try again")
		return
	}
	if strings.Contains(prompt, "slow") {
		hook("PreToolUse", map[string]any{"tool_name": "Bash"})
		slowTurn()
		// An interrupted turn leaves a marker in the transcript and fires no Stop hook.
		if mode == "codex" {
			write(map[string]any{"type": "event_msg", "payload": map[string]any{"type": "turn_aborted", "reason": "interrupted"}})
		} else {
			write(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "[Request interrupted by user]"}}}})
		}
		return
	}
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
	if mode == "codex" {
		write(map[string]any{"type": "event_msg", "payload": map[string]any{"type": "task_complete"}})
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

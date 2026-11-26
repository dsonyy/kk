// fakeagent imitates claude, codex and cursor-agent for kk's end-to-end tests. It reads the hook
// configuration kk passes, calls the hooks and writes transcripts in the harness's real format.
package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	mode string
	// hooks holds the commands of each hook event, in order.
	hooks      = map[string][]string{}
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
	// turn keeps a turn the harness starts by itself, when a background task ends, apart from the user's turns.
	turnMu sync.Mutex
	// tasks are the background tasks of claude that are still running.
	tasks []string
	// model is the model the harness reports, from --model or -m.
	model string
	// total is the running token count of a codex thread, which codex carries over a resume and a revert.
	total counts
	// connector is the inbox source of kk's integration session, from the connector tools it may use.
	connector string
	// allowed are the tools of --allowedTools, and builtins those of --tools.
	allowed  []string
	builtins []string
	// reads are the connector's tools among the allowed ones.
	reads []string
	argv    []string
)

type counts struct {
	Input  int `json:"input_tokens"`
	Cached int `json:"cached_input_tokens"`
	Output int `json:"output_tokens"`
	Total  int `json:"total_tokens"`
}

// claudeModels are the model ids Claude Code 2.1 writes for its aliases.
var claudeModels = map[string]string{"": "claude-opus-5-5", "opus": "claude-opus-5-5", "sonnet": "claude-sonnet-5-5", "haiku": "claude-haiku-5-5", "fable": "claude-fable-5-2"}

type turn struct {
	prompt string
	parent string
	offset int64
	total  counts
}

func main() {
	mode = filepath.Base(os.Args[0])
	args := os.Args[1:]
	if mode == "gh" {
		fakeGH(args)
		return
	}
	if mode == "claude" && len(args) > 0 && args[0] == "-p" {
		printGroup(args[len(args)-1])
		return
	}
	argv = args
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
		turnMu.Lock()
		handle(prompt)
		turnMu.Unlock()
	}
	for {
		fmt.Print("> ")
		msg := readMessage()
		if msg == "" {
			continue
		}
		turnMu.Lock()
		if msg == "/rewind" && mode != "codex" {
			rewind()
		} else {
			handle(msg)
		}
		turnMu.Unlock()
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
	rollout, turns, last, total = session, nil, "", counts{}
	if !fresh {
		last = "before-" + uuid()
		total = lastTotal()
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
	interruptKey = map[string]string{"claude": "Escape", "codex": "Escape", "cursor-agent": "\x03"}
)

// slowTurn works until the user interrupts it. Claude Code 2.1 queues the messages sent meanwhile and
// hands them over one by one at its next tool call, which here comes with a message that has "boundary" in it.
func slowTurn() {
	fmt.Println("working, waiting for an interrupt")
	var queue []string
	for text := ""; ; {
		switch k := readKey(); {
		case k == interruptKey[mode]:
			return
		case mode != "claude":
		case strings.HasPrefix(k, "paste:"):
			text += pasted(k[6:])
		case k == "Enter" && text != "":
			write(map[string]any{"type": "queue-operation", "operation": "enqueue", "content": text})
			queue = append(queue, text)
			if strings.Contains(text, "boundary") {
				for _, q := range queue {
					write(map[string]any{"type": "queue-operation", "operation": "remove", "content": q})
				}
				for _, q := range queue {
					write(map[string]any{"type": "attachment", "attachment": map[string]any{"type": "queued_command", "prompt": q, "commandMode": "prompt", "origin": map[string]any{"kind": "human"}}})
				}
				queue = nil
			}
			text = ""
		case len(k) == 1:
			text += k
		}
	}
}

// pasted puts a paste in the input box. Claude Code 2.1 first looks at a paste that has text and the
// path of an image, for about 150 ms on an idle machine, and loses an Enter that arrives meanwhile.
func pasted(text string) string {
	if mode == "claude" && strings.Contains(text, ".png") && strings.Contains(text, "\n") {
		var late []byte
		for end := time.After(250 * time.Millisecond); end != nil; {
			select {
			case b := <-keys:
				if b != '\r' && b != '\n' {
					late = append(late, b)
				}
			case <-end:
				end = nil
			}
		}
		for _, b := range late {
			keys <- b
		}
	}
	fmt.Print(text)
	return text
}

// printGroup answers kk's headless question about the feature group of a task, the way claude -p prints an answer:
// an existing group that the task mentions, otherwise the first two words of its last line, and None for a prompt that only
// tells the agent to wait.
func printGroup(prompt string) {
	head, task, _ := strings.Cut(prompt, "\n\nTask:\n")
	if strings.Contains(task, "GROUPFAIL") {
		fmt.Fprintln(os.Stderr, "API Error: 529 Overloaded")
		os.Exit(1)
	}
	for _, line := range strings.Split(head, "\n") {
		if name, ok := strings.CutPrefix(line, "- "); ok && strings.Contains(strings.ToLower(task), strings.ToLower(name)) {
			fmt.Println(name)
			return
		}
	}
	if strings.HasSuffix(task, "wait for the task") {
		fmt.Println("None")
		return
	}
	lines := strings.Split(task, "\n")
	words := strings.Fields(lines[len(lines)-1])
	fmt.Println(strings.Join(words[:min(2, len(words))], " "))
}

// fakeGH answers gh with the files in FAKE_HOME/gh, named after the query, and an empty list when there is none.
func fakeGH(args []string) {
	dir := filepath.Join(os.Getenv("FAKE_HOME"), "gh")
	if _, err := os.Stat(filepath.Join(dir, "logged-out")); err == nil {
		fmt.Fprintln(os.Stderr, "To get started with GitHub CLI, please run:  gh auth login")
		os.Exit(4)
	}
	if _, err := os.Stat(filepath.Join(dir, "fail")); err == nil {
		fmt.Fprintln(os.Stderr, "HTTP 502: Bad Gateway")
		os.Exit(1)
	}
	line := strings.Join(args, " ")
	name := "notifications"
	switch {
	case strings.HasPrefix(line, "search prs"):
		name = "review"
	case strings.Contains(line, "--assignee"):
		name = "assigned"
	case strings.Contains(line, "--mentions"):
		name = "mentions"
	}
	data, err := os.ReadFile(filepath.Join(dir, name+".json"))
	if err != nil {
		data = []byte("[]")
	}
	os.Stdout.Write(data)
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
			// The value is a file or the settings themselves.
			data := []byte(next())
			if !strings.HasPrefix(string(data), "{") {
				data, _ = os.ReadFile(string(data))
			}
			json.Unmarshal(data, &s)
			for e, groups := range s.Hooks {
				for _, g := range groups {
					for _, h := range g.Hooks {
						hooks[e] = append(hooks[e], h.Command)
					}
				}
			}
		case a == "--allowedTools":
			allowed = strings.Split(next(), ",")
			for source, prefix := range map[string]string{"gmail": "mcp__claude_ai_Gmail__", "slack": "mcp__claude_ai_Slack__"} {
				for _, t := range allowed {
					if strings.HasPrefix(t, prefix) {
						connector, reads = source, append(reads, t)
					}
				}
			}
		case a == "--tools":
			builtins = strings.Split(next(), ",")
		case a == "--plugin-dir":
			var p struct {
				Hooks map[string][]struct{ Command string } `json:"hooks"`
			}
			data, _ := os.ReadFile(filepath.Join(next(), "hooks", "hooks.json"))
			json.Unmarshal(data, &p)
			// Cursor CLI 2026.11 fires only these plugin hooks.
			for _, e := range []string{"sessionStart", "preToolUse", "postToolUse"} {
				if v := p.Hooks[e]; len(v) > 0 {
					hooks[strings.ToUpper(e[:1])+e[1:]] = []string{v[0].Command}
				}
			}
		case a == "-c":
			if m := codexHook.FindStringSubmatch(next()); m != nil {
				cmd, _ := strconv.Unquote(m[2])
				hooks[m[1]] = []string{cmd}
			}
		case a == "--session-id":
			session = next()
		case a == "--resume" || (mode == "codex" && a == "resume"):
			session, resume = next(), true
		case a == "--model" || a == "-m":
			model = next()
		case a == "--append-system-prompt-file" || a == "--name" || a == "--system-prompt" || a == "--setting-sources" ||
			a == "--permission-mode" || a == "--disallowedTools":
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
			text += pasted(k[6:])
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
			turns, prefill, reverted, total = nil, t.prompt, true, t.total
			fmt.Print("\x1b[2J\x1b[H")
			fmt.Printf("• Conversation reverted to this point. File changes are unchanged.\n\n› %s\n", t.prompt)
			return
		}
	}
}

// hook runs the commands of a hook event and tells whether one refused it with exit code 2, and why.
func hook(event string, extra map[string]any) (bool, string) {
	payload := map[string]any{"session_id": session, "transcript_path": transcript, "hook_event_name": event}
	if mode == "cursor-agent" && event == "SessionStart" {
		payload["transcript_path"] = nil
	}
	for k, v := range extra {
		payload[k] = v
	}
	data, _ := json.Marshal(payload)
	for _, cmd := range hooks[event] {
		c := exec.Command("sh", "-c", cmd)
		c.Stdin = strings.NewReader(string(data))
		out, err := c.CombinedOutput()
		if c.ProcessState != nil && c.ProcessState.ExitCode() == 2 {
			return true, strings.TrimSpace(string(out))
		}
		if err != nil || len(out) > 0 {
			fmt.Printf("hook %s: %v %s\n", event, err, out)
		}
	}
	return false, ""
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
	if connector != "" {
		integrationTurn(prompt)
		return
	}
	if mode == "cursor-agent" {
		if prompt == "exit" {
			os.Exit(0)
		}
		turns = append(turns, turn{prompt, "", size(), total})
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
	// Claude Code 2.1 runs /rename locally: no hooks, no turn, a custom title and an agent name.
	if name, ok := strings.CutPrefix(prompt, "/rename "); ok && mode == "claude" {
		write(map[string]any{"type": "system", "subtype": "local_command", "level": "info", "content": "<command-name>/rename</command-name>\n<command-message>rename</command-message>\n<command-args>" + name + "</command-args>"})
		write(map[string]any{"type": "custom-title", "customTitle": name})
		write(map[string]any{"type": "agent-name", "agentName": name})
		write(map[string]any{"type": "system", "subtype": "local_command", "level": "info", "content": "<local-command-stdout>Session renamed to: " + name + "</local-command-stdout>"})
		return
	}
	hook("UserPromptSubmit", map[string]any{"prompt": prompt})
	// A kk notification quotes other agents, whose words must not trigger the behaviours below.
	if strings.HasPrefix(prompt, "[kk notification") {
		userLine(prompt)
		answer("noted", 1000)
		hook("Stop", nil)
		return
	}
	if prompt == "exit" {
		os.Exit(0)
	}
	if prompt == "/compact" {
		write(map[string]any{"type": "user", "isCompactSummary": true, "message": map[string]any{"role": "user", "content": "summary"}})
		write(map[string]any{"type": "assistant", "message": map[string]any{"role": "assistant", "content": []any{}, "usage": map[string]any{"input_tokens": 3000}}})
		hook("Stop", nil)
		return
	}
	turns = append(turns, turn{prompt, last, size(), total})
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
	if mode == "claude" && strings.Contains(prompt, "background") {
		startTask()
		answer("waiting for the background task", 1000)
		hook("Stop", nil)
		return
	}
	if mode == "claude" && strings.Contains(prompt, "stop task") && len(tasks) > 0 {
		stopTask()
		answer("stopped the task", 1000)
		hook("Stop", nil)
		return
	}
	asks := 0
	if strings.Contains(prompt, "permission") {
		asks = 1
	}
	if strings.Contains(prompt, "twice") {
		// Two tool calls in one message: the second dialog follows the first within milliseconds.
		asks = 2
	}
	for range asks {
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
	advisor := strings.Contains(prompt, "advisor") && mode == "claude"
	if strings.Contains(prompt, "subagent") {
		subagent(advisor)
	}
	if advisor {
		advisorAnswer("echo: " + prompt)
	} else {
		answer("echo: "+prompt, tokens)
	}
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

// startTask runs a command in the background the way Claude Code 2.1 does: the tool result carries the task id, and the
// task ends when FAKE_HOME/finish appears. Claude Code then queues a task notification and takes it as a prompt of a turn
// of its own, with the same hooks as a user's turn.
func startTask() {
	id, ref := "b"+uuid()[:8], "toolu_"+uuid()[:8]
	write(map[string]any{"type": "assistant", "message": map[string]any{"role": "assistant", "content": []any{
		map[string]any{"type": "tool_use", "id": ref, "name": "Bash", "input": map[string]any{"command": "go test ./...", "run_in_background": true}},
	}}})
	write(map[string]any{"type": "user", "toolUseResult": map[string]any{"stdout": "", "stderr": "", "interrupted": false, "backgroundTaskId": id},
		"message": map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": ref, "content": "Command running in background with ID: " + id}}}})
	hook("PostToolUse", map[string]any{"tool_name": "Bash"})
	tasks = append(tasks, id)
	go func() {
		finish := filepath.Join(os.Getenv("FAKE_HOME"), "finish")
		for {
			time.Sleep(100 * time.Millisecond)
			if os.Remove(finish) == nil {
				break
			}
		}
		turnMu.Lock()
		defer turnMu.Unlock()
		if !slices.Contains(tasks, id) {
			return
		}
		tasks = slices.DeleteFunc(tasks, func(t string) bool { return t == id })
		note := "<task-notification>\n<task-id>" + id + "</task-id>\n<tool-use-id>" + ref + "</tool-use-id>\n<status>completed</status>\n<summary>Background command \"go test\" completed (exit code 0)</summary>\n</task-notification>"
		write(map[string]any{"type": "queue-operation", "operation": "enqueue", "content": note})
		write(map[string]any{"type": "queue-operation", "operation": "dequeue"})
		hook("UserPromptSubmit", map[string]any{"prompt": note})
		write(map[string]any{"type": "user", "origin": map[string]any{"kind": "task-notification"}, "message": map[string]any{"role": "user", "content": note}})
		answer("the background task finished", 1000)
		fmt.Println("agent: the background task finished")
		hook("Stop", nil)
	}()
}

// stopTask stops the last background task with TaskStop. Claude Code 2.1 writes no task notification for it.
func stopTask() {
	id, ref := tasks[len(tasks)-1], "toolu_"+uuid()[:8]
	tasks = tasks[:len(tasks)-1]
	hook("PreToolUse", map[string]any{"tool_name": "TaskStop"})
	write(map[string]any{"type": "assistant", "message": map[string]any{"role": "assistant", "content": []any{
		map[string]any{"type": "tool_use", "id": ref, "name": "TaskStop", "input": map[string]any{"task_id": id}},
	}}})
	write(map[string]any{"type": "user", "toolUseResult": map[string]any{"message": "Successfully stopped task: " + id, "task_id": id, "task_type": "local_bash", "command": "go test ./..."},
		"message": map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": ref, "content": `{"message":"Successfully stopped task"}`}}}})
	hook("PostToolUse", map[string]any{"tool_name": "TaskStop"})
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
	line := map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": prompt}}
	if blocks, ids := pastedImages(prompt); len(ids) > 0 {
		line["message"] = map[string]any{"role": "user", "content": blocks}
		line["imagePasteIds"] = ids
	}
	write(line)
}

// pastedImages is how Claude Code records the images pasted into its prompt: the text with an [Image #N] placeholder
// for each, then an image block per placeholder in their order. The image of placeholder N is FAKE_HOME/paste-N.png.
func pastedImages(prompt string) ([]any, []int) {
	blocks := []any{map[string]any{"type": "text", "text": prompt}}
	var ids []int
	for _, m := range regexp.MustCompile(`\[Image #(\d+)\]`).FindAllStringSubmatch(prompt, -1) {
		data, err := os.ReadFile(filepath.Join(os.Getenv("FAKE_HOME"), "paste-"+m[1]+".png"))
		if err != nil {
			continue
		}
		n, _ := strconv.Atoi(m[1])
		ids = append(ids, n)
		blocks = append(blocks, map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": base64.StdEncoding.EncodeToString(data)}})
	}
	return blocks, ids
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

// answer writes the agent's answer with the usage of its request: tokens of input, of which all but 10 come from the
// cache, and 50 of output.
func answer(text string, tokens int) {
	if mode == "codex" {
		write(map[string]any{"type": "turn_context", "payload": map[string]any{"model": model}})
		write(map[string]any{"type": "event_msg", "payload": map[string]any{"type": "item_completed", "item": map[string]any{"type": "AgentMessage", "content": []any{map[string]any{"type": "Text", "text": text}}}}})
		last := counts{tokens, tokens - 10, 50, tokens + 50}
		total = counts{total.Input + last.Input, total.Cached + last.Cached, total.Output + last.Output, total.Total + last.Total}
		write(map[string]any{"type": "event_msg", "payload": map[string]any{"type": "token_count", "info": map[string]any{"total_token_usage": total, "last_token_usage": last}}})
		return
	}
	// Claude Code writes a line per content block, each with the usage of the whole message.
	id := "msg_" + uuid()[:8]
	name := model
	if m, ok := claudeModels[model]; ok {
		name = m
	}
	usage := map[string]any{"input_tokens": 10, "cache_read_input_tokens": tokens - 10, "cache_creation_input_tokens": 0, "output_tokens": 50, "cache_creation": map[string]any{"ephemeral_1h_input_tokens": 0}}
	for _, block := range []map[string]any{{"type": "thinking", "thinking": "", "signature": "x"}, {"type": "text", "text": text}} {
		write(map[string]any{"type": "assistant", "message": map[string]any{"id": id, "model": name, "role": "assistant", "content": []any{block}, "usage": usage}})
	}
}

// call is the usage of one model call within a Claude Code message, as an entry of usage.iterations.
func call(kind string, input, cacheRead, write1h, write5m, output int) map[string]any {
	return map[string]any{"input_tokens": input, "output_tokens": output, "cache_read_input_tokens": cacheRead, "cache_creation_input_tokens": write1h + write5m,
		"cache_creation": map[string]any{"ephemeral_5m_input_tokens": write5m, "ephemeral_1h_input_tokens": write1h}, "type": kind}
}

// advisorCall is the advisor's call, which Claude Code records under the model that made it.
func advisorCall(input, output int) map[string]any {
	c := call("advisor_message", input, 0, 0, 0, output)
	c["model"] = "claude-fable-5-2"
	return c
}

// advisorAnswer writes an answer that consulted the advisor, as Claude Code 2.1 records it: a message of three calls,
// the main model's before and after the advisor's. The top-level usage adds up only the main model's calls and splits
// the cache writes as the first call did.
func advisorAnswer(text string) {
	id := "msg_" + uuid()[:8]
	usage := map[string]any{"input_tokens": 4, "cache_read_input_tokens": 1900, "cache_creation_input_tokens": 300, "output_tokens": 100,
		"cache_creation":  map[string]any{"ephemeral_1h_input_tokens": 100, "ephemeral_5m_input_tokens": 0},
		"server_tool_use": map[string]any{"web_search_requests": 0, "web_fetch_requests": 0}, "service_tier": "standard", "inference_geo": "global", "speed": "standard",
		"iterations": []any{call("message", 2, 900, 100, 0, 40), advisorCall(5000, 300), call("message", 2, 1000, 0, 200, 60)}}
	tool := "srvtoolu_" + uuid()[:8]
	for _, block := range []map[string]any{
		{"type": "thinking", "thinking": "", "signature": "x"},
		{"type": "server_tool_use", "id": tool, "name": "advisor", "input": map[string]any{}},
		{"type": "advisor_tool_result", "tool_use_id": tool, "content": map[string]any{"type": "advisor_result", "text": "advice"}},
		{"type": "text", "text": text},
	} {
		write(map[string]any{"type": "assistant", "advisorModel": "claude-fable-5-2", "message": map[string]any{"id": id, "model": claudeModels[model], "role": "assistant", "content": []any{block}, "usage": usage}})
	}
}

// subagent writes the transcript of a subagent that used 1000 tokens of input and 100 of output, where the harness
// keeps it. A Claude Code subagent that consults the advisor adds 2000 tokens of input and 100 of output on its model.
func subagent(advisor bool) {
	if mode == "codex" {
		child := uuid()
		path := filepath.Join(filepath.Dir(transcript), "rollout-"+time.Now().Format("2006-01-02T15-04-05")+"-"+child+".jsonl")
		spawn := map[string]any{"thread_spawn": map[string]any{"parent_thread_id": session, "depth": 1}}
		usage := counts{1000, 0, 100, 1100}
		lines := []map[string]any{
			{"type": "session_meta", "payload": map[string]any{"id": child, "source": map[string]any{"subagent": spawn}}},
			{"type": "turn_context", "payload": map[string]any{"model": model}},
			{"type": "event_msg", "payload": map[string]any{"type": "token_count", "info": map[string]any{"total_token_usage": usage, "last_token_usage": usage}}},
		}
		appendLines(path, lines)
		return
	}
	dir := filepath.Join(strings.TrimSuffix(transcript, ".jsonl"), "subagents")
	os.MkdirAll(dir, 0o755)
	usage := map[string]any{"input_tokens": 1000, "cache_read_input_tokens": 0, "cache_creation_input_tokens": 0, "output_tokens": 100}
	if advisor {
		usage["iterations"] = []any{call("message", 1000, 0, 0, 0, 100), advisorCall(2000, 100)}
	}
	appendLines(filepath.Join(dir, "agent-a"+uuid()[:8]+".jsonl"), []map[string]any{{"type": "assistant", "isSidechain": true, "message": map[string]any{
		"id": "msg_" + uuid()[:8], "model": "claude-haiku-5-5", "role": "assistant", "content": []any{map[string]any{"type": "text", "text": "found it"}},
		"usage": usage,
	}}})
}

func appendLines(path string, lines []map[string]any) {
	f, _ := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	defer f.Close()
	for _, l := range lines {
		l["timestamp"] = time.Now().UTC().Format(time.RFC3339Nano)
		data, _ := json.Marshal(l)
		f.Write(append(data, '\n'))
	}
}

// lastTotal reads the running token count a resumed codex thread goes on from.
func lastTotal() counts {
	var t counts
	data, _ := os.ReadFile(transcript)
	for _, line := range strings.Split(string(data), "\n") {
		var l struct {
			Payload struct {
				Info *struct {
					Total *counts `json:"total_token_usage"`
				} `json:"info"`
			} `json:"payload"`
		}
		if json.Unmarshal([]byte(line), &l) == nil && l.Payload.Info != nil && l.Payload.Info.Total != nil {
			t = *l.Payload.Info.Total
		}
	}
	return t
}

func uuid() string {
	b := make([]byte, 16)
	rand.Read(b)
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// integrationTurn answers a turn of kk's inbox session like a model that has only its connector's tools. A prompt
// "use <tool>" calls that tool; any other prompt collects the inbox: it keeps the arguments in FAKE_HOME/<source>-args.json,
// reads with the first allowed connector tool and answers with FAKE_HOME/<source>.once, which it then removes, or else
// with FAKE_HOME/<source>.out. FAKE_HOME/<source>-connector "none"
// leaves the connector out, and FAKE_HOME/<source>-error ends the turn on an API error. Tools that ran are listed in
// FAKE_HOME/<source>-tools.log.
//
// Like Claude Code 2.1, the agent takes its first prompt while the claude.ai connectors are still connecting: it has
// no connector tools then, unless it may wait for them with ToolSearch. Claude Code sends ToolSearch to the model only
// while some tool is deferred, so with no deferred built-in in --tools the model writes the call as text. And it fires
// the Stop hook before the answer reaches the transcript.
func integrationTurn(prompt string) {
	home := os.Getenv("FAKE_HOME")
	hook("UserPromptSubmit", map[string]any{"prompt": prompt})
	userLine(prompt)
	// Claude Code names the session after what it read, which kk must not take over.
	write(map[string]any{"type": "ai-title", "aiTitle": "Mail from Gus about a contract"})
	if tool, ok := strings.CutPrefix(prompt, "use "); ok {
		useTool(tool)
		reply("done")
		return
	}
	data, _ := json.Marshal(argv)
	os.WriteFile(filepath.Join(home, connector+"-args.json"), data, 0o644)
	if _, err := os.Stat(filepath.Join(home, connector+"-error")); err == nil {
		write(map[string]any{"type": "assistant", "isApiErrorMessage": true, "apiError": "overloaded", "error": "server_error", "message": map[string]any{"role": "assistant", "model": "<synthetic>", "content": []any{map[string]any{"type": "text", "text": "API Error: 529 Overloaded"}}}})
		return
	}
	if !slices.Contains(builtins, "ToolSearch") {
		reply(`{"connector":"missing"}`)
		return
	}
	if !slices.ContainsFunc(builtins, func(t string) bool { return deferredBuiltins[t] }) {
		reply(fmt.Sprintf(`<invoke name="ToolSearch"><parameter name="query">select:%s</parameter></invoke>`, strings.Join(reads, ",")))
		return
	}
	useTool("ToolSearch")
	if b, _ := os.ReadFile(filepath.Join(home, connector+"-connector")); strings.TrimSpace(string(b)) == "none" {
		reply(`{"connector":"missing"}`)
		return
	}
	useTool(reads[0])
	out, err := os.ReadFile(filepath.Join(home, connector+".once"))
	if err == nil {
		os.Remove(filepath.Join(home, connector+".once"))
	} else {
		out, err = os.ReadFile(filepath.Join(home, connector+".out"))
	}
	if err != nil {
		out = []byte(`{"items":[]}`)
	}
	fmt.Println("agent: answered")
	reply(string(out))
}

// deferredBuiltins are built-in tools that Claude Code 2.1.332 defers behind ToolSearch.
var deferredBuiltins = map[string]bool{"CronList": true, "TaskStop": true, "NotebookEdit": true}

// reply ends an integration turn the way Claude Code 2.1 does: the Stop hook reports the answer before the transcript
// holds it.
func reply(text string) {
	hook("Stop", map[string]any{"last_assistant_message": text})
	time.Sleep(300 * time.Millisecond)
	answer(text, 1000)
}

// useTool calls a tool the way Claude Code does: a PreToolUse hook that exits with 2 refuses it, and then it does not run.
func useTool(name string) {
	id := "toolu_" + uuid()[:8]
	write(map[string]any{"type": "assistant", "message": map[string]any{"role": "assistant", "content": []any{
		map[string]any{"type": "tool_use", "id": id, "name": name, "input": map[string]any{}},
	}}})
	if refused, why := hook("PreToolUse", map[string]any{"tool_name": name}); refused {
		write(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": id, "is_error": true, "content": "PreToolUse hook error: " + why}}}})
		return
	}
	f, _ := os.OpenFile(filepath.Join(os.Getenv("FAKE_HOME"), connector+"-tools.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	fmt.Fprintln(f, name)
	f.Close()
	write(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": id, "content": "ok"}}}})
	hook("PostToolUse", map[string]any{"tool_name": name})
}

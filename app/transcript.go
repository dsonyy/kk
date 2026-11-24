package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"regexp"
	"strings"
	"time"
)

type Event struct {
	// ID is the message's id in the agent's own transcript, when the harness records one.
	ID     string `json:"id,omitempty"`
	Kind   string `json:"kind"`
	Text   string `json:"text,omitempty"`
	Tool   string `json:"tool,omitempty"`
	Input  string `json:"input,omitempty"`
	Detail string `json:"detail,omitempty"`
	Output string `json:"output,omitempty"`
	Failed bool   `json:"failed,omitempty"`
	// Attachments are the files of a user message, filled in when the conversation is served.
	Attachments []Attachment `json:"attachments,omitempty"`
	Time        time.Time    `json:"time"`
}

const (
	kindUser      = "user"
	kindAssistant = "assistant"
	kindThinking  = "thinking"
	kindTool      = "tool"
	kindError     = "error"
	kindNote      = "note"
)

// Tail reads a growing JSONL transcript incrementally.
type Tail struct {
	path    string
	offset  int64
	partial []byte
	Events  []Event
	tools   map[string]int
	Title   string
	Context int
	Last    time.Time
	// Ended is when the last turn ended, for harnesses whose transcript records it (Cursor).
	Ended time.Time
	// Waiting is true when the last message is the user's and no turn end follows.
	Waiting bool
	stamp   time.Time
}

func newTail(path string) *Tail {
	return &Tail{path: path, tools: map[string]int{}}
}

func (t *Tail) read(parse func(*Tail, []byte)) (bool, error) {
	f, err := os.Open(t.path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.Size() == t.offset {
		return false, err
	}
	if st.Size() < t.offset {
		*t = *newTail(t.path)
	}
	if _, err := f.Seek(t.offset, io.SeekStart); err != nil {
		return false, err
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return false, err
	}
	t.offset += int64(len(data))
	t.stamp = st.ModTime()
	data = append(t.partial, data...)
	end := bytes.LastIndexByte(data, '\n')
	if end < 0 {
		t.partial = data
		return false, nil
	}
	t.partial = append([]byte(nil), data[end+1:]...)
	for _, line := range bytes.Split(data[:end], []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) > 0 {
			parse(t, line)
		}
	}
	return true, nil
}

func (t *Tail) add(e Event) {
	t.Events = append(t.Events, e)
	if (e.Kind == kindUser || e.Kind == kindAssistant) && e.Time.After(t.Last) {
		t.Last = e.Time
	}
}

func (t *Tail) addTool(ref string, e Event) {
	e.Kind = kindTool
	t.add(e)
	if ref != "" {
		t.tools[ref] = len(t.Events) - 1
	}
}

func (t *Tail) toolResult(ref, output string, failed bool) {
	if i, ok := t.tools[ref]; ok {
		t.Events[i].Output = output
		t.Events[i].Failed = failed
		delete(t.tools, ref)
	}
}

// Claude Code transcript: ~/.claude/projects/<slug>/<session>.jsonl

type claudeBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

type claudeLine struct {
	Type              string    `json:"type"`
	UUID              string    `json:"uuid"`
	Timestamp         time.Time `json:"timestamp"`
	IsMeta            bool      `json:"isMeta"`
	IsCompactSummary  bool      `json:"isCompactSummary"`
	IsApiErrorMessage bool      `json:"isApiErrorMessage"`
	AiTitle           string    `json:"aiTitle"`
	Level             string    `json:"level"`
	Content           string    `json:"content"`
	Message           struct {
		Content json.RawMessage `json:"content"`
		Usage   *struct {
			Input       int `json:"input_tokens"`
			CacheRead   int `json:"cache_read_input_tokens"`
			CacheCreate int `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	} `json:"message"`
}

var commandName = regexp.MustCompile(`<command-name>(.*?)</command-name>`)

func parseClaude(t *Tail, raw []byte) {
	var l claudeLine
	if json.Unmarshal(raw, &l) != nil {
		return
	}
	switch l.Type {
	case "ai-title":
		if l.AiTitle != "" {
			t.Title = l.AiTitle
		}
	case "system":
		if l.Level == "error" && l.Content != "" {
			t.add(Event{Kind: kindError, Text: l.Content, Time: l.Timestamp})
		}
	case "user":
		if l.IsCompactSummary {
			t.add(Event{Kind: kindNote, Text: "Conversation compacted", Time: l.Timestamp})
			return
		}
		if l.IsMeta {
			return
		}
		var text string
		if json.Unmarshal(l.Message.Content, &text) == nil {
			addClaudeUserText(t, text, l.Timestamp)
			return
		}
		var blocks []claudeBlock
		json.Unmarshal(l.Message.Content, &blocks)
		for _, b := range blocks {
			switch b.Type {
			case "text":
				addClaudeUserText(t, b.Text, l.Timestamp)
			case "tool_result":
				t.toolResult(b.ToolUseID, blockText(b.Content), b.IsError)
			}
		}
	case "assistant":
		if u := l.Message.Usage; u != nil {
			t.Context = u.Input + u.CacheRead + u.CacheCreate
		}
		var blocks []claudeBlock
		json.Unmarshal(l.Message.Content, &blocks)
		for _, b := range blocks {
			switch b.Type {
			case "text":
				kind := kindAssistant
				if l.IsApiErrorMessage {
					kind = kindError
				}
				if strings.TrimSpace(b.Text) != "" {
					t.add(Event{ID: l.UUID, Kind: kind, Text: b.Text, Time: l.Timestamp})
				}
			case "thinking":
				if strings.TrimSpace(b.Thinking) != "" {
					t.add(Event{Kind: kindThinking, Text: b.Thinking, Time: l.Timestamp})
				}
			case "tool_use":
				input, detail := toolSummary(b.Input)
				t.addTool(b.ID, Event{Tool: b.Name, Input: input, Detail: detail, Time: l.Timestamp})
			}
		}
	}
}

// Claude Code wraps a pasted multi-line message in these tags.
var pastedContent = regexp.MustCompile(`</?pasted_content[^>]*>`)

func addClaudeUserText(t *Tail, text string, at time.Time) {
	text = strings.TrimSpace(pastedContent.ReplaceAllString(text, ""))
	if m := commandName.FindStringSubmatch(text); m != nil {
		t.add(Event{Kind: kindUser, Text: m[1], Time: at})
		return
	}
	if strings.HasPrefix(text, "<local-command") || strings.HasPrefix(text, "[Request interrupted") || strings.TrimSpace(text) == "" {
		return
	}
	t.add(Event{Kind: kindUser, Text: text, Time: at})
}

func blockText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []claudeBlock
	json.Unmarshal(raw, &parts)
	var out []string
	for _, p := range parts {
		if p.Type == "text" {
			out = append(out, p.Text)
		}
	}
	return strings.Join(out, "\n")
}

var summaryKeys = []string{"command", "file_path", "path", "pattern", "url", "query", "description", "prompt", "skill"}

// toolSummary returns a one-line summary of a tool input and the detail worth showing when it is expanded.
func toolSummary(raw json.RawMessage) (string, string) {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return "", pretty(raw)
	}
	for _, k := range summaryKeys {
		s, ok := m[k].(string)
		if !ok || s == "" {
			continue
		}
		summary := firstLine(s, 160)
		delete(m, k)
		delete(m, "description")
		if len(m) > 0 {
			return summary, pretty(raw)
		}
		if summary != s {
			return summary, s
		}
		return summary, ""
	}
	return "", pretty(raw)
}

func pretty(raw json.RawMessage) string {
	var buf bytes.Buffer
	if json.Indent(&buf, raw, "", "  ") != nil {
		return string(raw)
	}
	return buf.String()
}

// Codex rollout: ~/.codex/sessions/YYYY/MM/DD/rollout-*.jsonl

type codexLine struct {
	Timestamp time.Time `json:"timestamp"`
	Type      string    `json:"type"`
	Payload   struct {
		Type    string          `json:"type"`
		Message string          `json:"message"`
		Item    json.RawMessage `json:"item"`
		Info    *struct {
			Last struct {
				Input int `json:"input_tokens"`
			} `json:"last_token_usage"`
		} `json:"info"`
	} `json:"payload"`
}

type codexItem struct {
	Type    string `json:"type"`
	Content []struct {
		Text string `json:"text"`
	} `json:"content"`
	SummaryText []string `json:"summary_text"`
	Command     []string `json:"command"`
	Output      string   `json:"aggregated_output"`
	ExitCode    *int     `json:"exit_code"`
	Status      string   `json:"status"`
	Query       string   `json:"query"`
	Changes     map[string]struct {
		Type string `json:"type"`
		Diff string `json:"unified_diff"`
	} `json:"changes"`
	Stdout string `json:"stdout"`
}

func parseCodex(t *Tail, raw []byte) {
	var l codexLine
	if json.Unmarshal(raw, &l) != nil || l.Type != "event_msg" {
		return
	}
	switch l.Payload.Type {
	case "token_count":
		if l.Payload.Info != nil {
			t.Context = l.Payload.Info.Last.Input
		}
	case "error":
		t.add(Event{Kind: kindError, Text: l.Payload.Message, Time: l.Timestamp})
	case "item_completed":
		var it codexItem
		if json.Unmarshal(l.Payload.Item, &it) != nil {
			return
		}
		at := l.Timestamp
		switch it.Type {
		case "UserMessage", "AgentMessage":
			var parts []string
			for _, c := range it.Content {
				parts = append(parts, c.Text)
			}
			kind := kindUser
			if it.Type == "AgentMessage" {
				kind = kindAssistant
			}
			if text := strings.Join(parts, "\n"); strings.TrimSpace(text) != "" {
				t.add(Event{Kind: kind, Text: text, Time: at})
			}
		case "Reasoning":
			if text := strings.Join(it.SummaryText, "\n\n"); strings.TrimSpace(text) != "" {
				t.add(Event{Kind: kindThinking, Text: text, Time: at})
			}
		case "CommandExecution":
			cmd := strings.Join(it.Command, " ")
			if n := len(it.Command); n == 3 && strings.HasSuffix(it.Command[1], "c") {
				cmd = it.Command[2]
			}
			failed := it.ExitCode != nil && *it.ExitCode != 0
			t.addTool("", Event{Tool: "Shell", Input: firstLine(cmd, 160), Detail: cmd, Output: it.Output, Failed: failed, Time: at})
		case "FileChange":
			var paths, diffs []string
			for p, ch := range it.Changes {
				paths = append(paths, p)
				diffs = append(diffs, p+"\n"+ch.Diff)
			}
			t.addTool("", Event{Tool: "Edit", Input: strings.Join(paths, ", "), Detail: strings.Join(diffs, "\n"), Output: it.Stdout, Failed: it.Status == "failed", Time: at})
		case "WebSearch":
			t.addTool("", Event{Tool: "WebSearch", Input: it.Query, Time: at})
		case "ContextCompaction":
			t.add(Event{Kind: kindNote, Text: "Conversation compacted", Time: at})
		default:
			t.addTool("", Event{Tool: it.Type, Detail: pretty(l.Payload.Item), Time: at})
		}
	}
}

// Cursor transcript: ~/.cursor/projects/<slug>/agent-transcripts/<id>/<id>.jsonl, without timestamps.

var userQuery = regexp.MustCompile(`(?s)<user_query>\n?(.*?)\n?</user_query>`)

func parseCursor(t *Tail, raw []byte) {
	var l struct {
		Type    string `json:"type"`
		Role    string `json:"role"`
		Message struct {
			Content []claudeBlock `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(raw, &l) != nil {
		return
	}
	at := t.stamp
	if l.Type == "turn_ended" {
		t.Ended, t.Waiting = at, false
		return
	}
	for _, b := range l.Message.Content {
		switch {
		case l.Role == "user" && b.Type == "text":
			text := b.Text
			if m := userQuery.FindStringSubmatch(text); m != nil {
				text = m[1]
			}
			t.add(Event{Kind: kindUser, Text: text, Time: at})
			t.Waiting = true
		case b.Type == "text" && strings.TrimSpace(b.Text) != "":
			t.add(Event{Kind: kindAssistant, Text: b.Text, Time: at})
		case b.Type == "thinking" && strings.TrimSpace(b.Thinking) != "":
			t.add(Event{Kind: kindThinking, Text: b.Thinking, Time: at})
		case b.Type == "tool_use":
			input, detail := toolSummary(b.Input)
			t.addTool("", Event{Tool: b.Name, Input: input, Detail: detail, Time: at})
		}
	}
}

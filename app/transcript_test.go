package main

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func readFixture(t *testing.T, path string, parse func(*Tail, []byte)) *Tail {
	t.Helper()
	tail := newTail(path)
	if _, err := tail.read(parse); err != nil {
		t.Fatal(err)
	}
	return tail
}

func TestParseRealClaudeTranscript(t *testing.T) {
	tail := readFixture(t, "testdata/claude.jsonl", parseClaude)
	want := []Event{
		{Kind: kindUser, Text: "Run ls, read a.txt, then reply with just: hi"},
		{Kind: kindTool, Tool: "Bash", Input: "ls", Output: "a.txt"},
		{Kind: kindTool, Tool: "Read", Input: "/home/dsonyy/repos/kk-spike/a.txt"},
		{Kind: kindAssistant, Text: "hi"},
		{Kind: kindUser, Text: "Line one of reply.\nLine two: reply with just: ok"},
	}
	if len(tail.Events) != len(want) {
		t.Fatalf("events: %+v", tail.Events)
	}
	for i, w := range want {
		got := tail.Events[i]
		if got.Kind != w.Kind || got.Text != w.Text || got.Tool != w.Tool || got.Input != w.Input || (w.Output != "" && got.Output != w.Output) {
			t.Fatalf("event %d: got %+v, want %+v", i, got, w)
		}
	}
	if tail.Title != "Read a.txt and ls" || tail.Context == 0 || tail.Last.IsZero() {
		t.Fatalf("title %q, context %d, last %v", tail.Title, tail.Context, tail.Last)
	}
}

func TestClaudeAPIErrorEndsTurn(t *testing.T) {
	lines, _ := os.ReadFile("testdata/claude-api-error.jsonl")
	path := filepath.Join(t.TempDir(), "session.jsonl")
	tail := newTail(path)
	failed := map[int]string{
		2: "API Error: The response stopped arriving. The response above may be incomplete.",
		3: "API Error: The response stopped arriving. The response above may be incomplete.",
		7: "API Error: Your computer went to sleep mid-response. The response above may be incomplete.",
		8: "API Error: Your computer went to sleep mid-response. The response above may be incomplete.",
	}
	f, _ := os.Create(path)
	defer f.Close()
	n := 0
	for line := range bytes.Lines(lines) {
		f.Write(line)
		tail.read(parseClaude)
		if n++; tail.Failed != failed[n] || tail.Ended.IsZero() != (failed[n] == "") {
			t.Fatalf("after line %d: failed %q, ended %v", n, tail.Failed, tail.Ended)
		}
	}
	if last := tail.Events[len(tail.Events)-1]; last.Kind != kindError || last.Text != failed[8] {
		t.Fatalf("last event: %+v", last)
	}
}

func TestParseRealCodexTranscript(t *testing.T) {
	tail := readFixture(t, "testdata/codex.jsonl", parseCodex)
	want := []Event{
		{Kind: kindUser, Text: "Run ls, then reply with just: hi"},
		{Kind: kindTool, Tool: "Shell", Input: "ls", Output: "a.txt\n"},
		{Kind: kindAssistant, Text: "hi"},
	}
	if len(tail.Events) != len(want) {
		t.Fatalf("events: %+v", tail.Events)
	}
	for i, w := range want {
		got := tail.Events[i]
		if got.Kind != w.Kind || got.Text != w.Text || got.Tool != w.Tool || got.Input != w.Input || got.Output != w.Output {
			t.Fatalf("event %d: got %+v, want %+v", i, got, w)
		}
	}
	if tail.Context != 17806 {
		t.Fatalf("context %d", tail.Context)
	}
}

func TestParseRealCursorTranscript(t *testing.T) {
	tail := readFixture(t, "testdata/cursor.jsonl", parseCursor)
	want := []Event{
		{Kind: kindUser, Text: "Run ls, then reply with just: hi"},
		{Kind: kindTool, Tool: "Shell", Input: "ls"},
		{Kind: kindAssistant, Text: "hi"},
		{Kind: kindUser, Text: "First line.\nSecond line: reply with just: ok"},
		{Kind: kindAssistant, Text: "ok"},
	}
	if len(tail.Events) != len(want) {
		t.Fatalf("events: %+v", tail.Events)
	}
	for i, w := range want {
		got := tail.Events[i]
		if got.Kind != w.Kind || got.Text != w.Text || got.Tool != w.Tool || got.Input != w.Input {
			t.Fatalf("event %d: got %+v, want %+v", i, got, w)
		}
	}
	if tail.Ended.IsZero() || tail.Waiting {
		t.Fatalf("ended %v, waiting %v", tail.Ended, tail.Waiting)
	}
}

func TestClaudePastedMessageShowsWithoutItsWrapper(t *testing.T) {
	tail := newTail("")
	parseClaude(tail, []byte(`{"type":"user","message":{"content":"\n\n<pasted_content id=\"757e\">\n> quoted\n\nReply\n</pasted_content id=\"757e\">\n"}}`))
	if len(tail.Events) != 1 || tail.Events[0].Text != "> quoted\n\nReply" {
		t.Fatalf("events: %+v", tail.Events)
	}
}

func TestClaudeRewindDropsTheAbandonedBranch(t *testing.T) {
	tail := readFixture(t, "testdata/claude-rewind.jsonl", parseClaude)
	want := []Event{
		{Kind: kindUser, Text: "Append the line two to a.txt, nothing else."},
		{Kind: kindTool, Tool: "Bash"},
		{Kind: kindTool, Tool: "Bash"},
		{Kind: kindAssistant, Text: "I appended `two` to `a.txt` as a new line after `one`. The file now reads `one` then `two`."},
		{Kind: kindUser, Text: "Use the Edit tool to append the line THREE-EDITED to a.txt\nand then say ok"},
		{Kind: kindTool, Tool: "Read"},
		{Kind: kindTool, Tool: "Edit"},
		{Kind: kindAssistant, Text: "ok"},
	}
	if len(tail.Events) != len(want) {
		t.Fatalf("events: %+v", tail.Events)
	}
	for i, w := range want {
		got := tail.Events[i]
		if got.Kind != w.Kind || got.Text != w.Text || got.Tool != w.Tool || (got.Kind == kindUser && got.ID == "") || (got.Kind == kindTool && got.Output == "") {
			t.Fatalf("event %d: got %+v, want %+v", i, got, w)
		}
	}
}

func TestRewrittenTranscriptIsReadAgain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cursor.jsonl")
	line := func(role, text string) string {
		return `{"role":"` + role + `","message":{"content":[{"type":"text","text":"` + text + `"}]}}` + "\n"
	}
	os.WriteFile(path, []byte(line("user", "one")+line("assistant", "1")+line("user", "two")+line("assistant", "2")), 0o644)
	tail := newTail(path)
	tail.read(parseCursor)
	os.WriteFile(path, []byte(line("user", "one")+line("assistant", "1")+line("user", "a longer second message")+line("assistant", "a longer second answer")), 0o644)
	tail.read(parseCursor)
	if len(tail.Events) != 4 || tail.Events[2].Text != "a longer second message" {
		t.Fatalf("events: %+v", tail.Events)
	}
}

func TestParseClaudeQueue(t *testing.T) {
	data, err := os.ReadFile("testdata/claude-queue.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	tail := newTail("")
	var queued [][]string
	for _, line := range bytes.Split(bytes.TrimSpace(data), []byte{'\n'}) {
		parseClaude(tail, line)
		var texts []string
		for _, e := range tail.Queue {
			texts = append(texts, firstLine(e.Text, 40))
		}
		if len(queued) == 0 || !slices.Equal(queued[len(queued)-1], texts) {
			queued = append(queued, texts)
		}
	}
	// The first three messages reach the agent at a tool call, the last three at the end of a turn.
	wantQueued := [][]string{nil, {"test one"}, {"test one", "are u here?"}, {"test one", "are u here?", "third with image"}, {"are u here?", "third with image"}, {"third with image"}, nil,
		{"q-one"}, {"q-one", "q-two, reply ok"}, {"q-one", "q-two, reply ok", "q-three"}, nil}
	if !slices.EqualFunc(queued, wantQueued, slices.Equal) {
		t.Fatalf("queue over time: %q", queued)
	}
	var users []string
	for _, e := range tail.Events {
		if e.Kind == kindUser {
			users = append(users, firstLine(e.Text, 40))
		}
	}
	var notes []string
	for _, e := range tail.Events {
		if e.Kind == kindNote {
			notes = append(notes, e.Text)
		}
	}
	// A task notification is not something the user typed, whichever way it reaches the model.
	if want := []string{`Background command "Start Codex and Cursor conversations in the private instance" completed (exit code 0)`, `Background command "Run the test suite" completed (exit code 0)`, `Background agent "Research task chunking" was stopped by the user.`}; !slices.Equal(notes, want) {
		t.Fatalf("notes: %q", notes)
	}
	want := []string{"Run /tmp/scratch/build.sh with the Bash…", "test one", "are u here?", "third with image", "reply only ok Areply only ok Breply onl…", "Without using any tools, write a 900 wo…", "q-one", "q-two, reply ok", "q-three"}
	if !slices.Equal(users, want) {
		t.Fatalf("messages: %q", users)
	}
}

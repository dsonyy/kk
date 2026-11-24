package main

import "testing"

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

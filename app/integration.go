package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// An integration is an inbox source that kk reads through an agent: one lasting Claude Code session per source, in its
// own tmux window. Every run starts a fresh agent with the run's prompt, so no earlier run stays in its context, and
// every launch carries the same limits, which nothing typed into the session can widen.

const (
	integrationModel = "sonnet"
	inboxGroup       = "Inbox"
	// earlierRuns keys the activity of an integration's earlier runs, whose transcripts kk removes.
	earlierRuns = "earlier runs"
)

func integrationID(source string) string { return "inbox-" + source }

// toolSearch is the only built-in tool of an integration's agent. Claude Code takes the run's prompt before the claude.ai
// connectors have connected, and ToolSearch waits for them; without it the agent finds no connector tools.
const toolSearch = "ToolSearch"

// integrationCommand builds the argv of an integration's agent. Without the user's settings it gets no tools but
// ToolSearch and the connector's read tools. The guard hook refuses every other tool whatever the permission mode and
// rules say by then.
func (s *Server) integrationCommand(c *Conversation, resume bool) ([]string, error) {
	r, ok := connectorRuns[c.Integration]
	if !ok {
		return nil, fmt.Errorf("unknown integration %q", c.Integration)
	}
	allowed := toolSearch + "," + r.tools(r.read)
	hooks := s.claudeHooks()
	guard := map[string]any{"hooks": []any{map[string]any{"type": "command", "command": shellJoin([]string{s.bin, "guard", allowed})}}}
	hooks["PreToolUse"] = append([]any{guard}, hooks["PreToolUse"]...)
	settings, _ := json.Marshal(map[string]any{"hooks": hooks})
	argv := []string{"claude", "--settings", string(settings), "--model", integrationModel, "--setting-sources", "", "--tools", toolSearch,
		"--permission-mode", "dontAsk", "--allowedTools", allowed, "--disallowedTools", r.tools(r.write), "--disable-slash-commands",
		"--system-prompt", r.system}
	if resume {
		return append(argv, "--resume", c.Session), nil
	}
	return append(argv, "--session-id", c.Session, "--", c.Prompt), nil
}

// guard is a PreToolUse hook that lets the agent use only the tools in its argument. Exit code 2 makes Claude Code
// refuse the call and tell the agent why. A payload it cannot read is refused as well.
func guard(args []string) {
	var p struct {
		Tool string `json:"tool_name"`
	}
	data, _ := io.ReadAll(io.LimitReader(os.Stdin, 16<<20))
	json.Unmarshal(data, &p)
	if len(args) == 1 && p.Tool != "" && slices.Contains(strings.Split(args[0], ","), p.Tool) {
		return
	}
	fmt.Fprintf(os.Stderr, "kk lets this session use only %s.\n", strings.Join(args, " "))
	os.Exit(2)
}

// runIntegration runs r in its source's session and returns the items of the answer. A run that fails raises
// attention on the session; one that finds no connector does not, the inbox shows it as not set up.
func (s *Server) runIntegration(ctx context.Context, r connectorRun) ([]connectorItem, error) {
	answer, err := s.integrationAnswer(ctx, r)
	var items []connectorItem
	if err == nil {
		items, err = r.parse(answer)
	}
	var missing notSetUp
	if err != nil && !errors.As(err, &missing) && !errors.Is(err, context.Canceled) {
		id := integrationID(r.source)
		s.store.update(id, func(c *Conversation) bool {
			msg := err.Error()
			c.Attention = strings.ToUpper(msg[:1]) + msg[1:] + "."
			return !c.Closed
		})
		s.publish(id)
	}
	return items, err
}

// integrationAnswer starts a run and waits for the agent's answer to it.
func (s *Server) integrationAnswer(ctx context.Context, r connectorRun) (string, error) {
	id := integrationID(r.source)
	// A tick between reading a transcript and saving its activity would count the run that is folded away again.
	s.ticking.Lock()
	c, ok := s.store.get(id)
	if !ok {
		c = Conversation{ID: id, Integration: r.source, Harness: "claude", Model: integrationModel, Dir: s.store.dir, Title: r.label + " inbox", Group: inboxGroup, Created: time.Now()}
	}
	forget(c)
	c.Session, c.Transcript, c.Context, c.ByAgent = newUUID(), "", 0, false
	c.Prompt = fmt.Sprintf("%s Now is %s.", r.prompt, time.Now().Format(time.RFC3339))
	c.Transcripts = foldRuns(c.Transcripts)
	err := s.launch(&c, false, "")
	s.ticking.Unlock()
	if err != nil {
		return "", err
	}
	for {
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return "", fmt.Errorf("the %s run did not answer within %s", r.label, inboxFetchLimit)
			}
			return "", ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
		cur, ok := s.store.get(id)
		switch {
		case !ok || cur.Closed || !cur.Launched.Equal(c.Launched):
			return "", fmt.Errorf("the %s run was stopped", r.label)
		case cur.Status == statusExited:
			return "", fmt.Errorf("Claude Code exited before the %s run answered", r.label)
		case cur.Status == statusIdle && cur.TurnEnded.After(cur.Launched):
			if cur.Attention != "" {
				return "", fmt.Errorf("the %s run ended on an API error", r.label)
			}
			s.mu.Lock()
			a := s.answers[id]
			delete(s.answers, id)
			s.mu.Unlock()
			if !a.launched.Equal(cur.Launched) {
				return "", nil
			}
			return a.text, nil
		}
	}
}

// runAnswer is the last message of the first turn of an integration's run, which answers the run's prompt.
type runAnswer struct {
	launched time.Time
	text     string
}

// keepAnswer keeps the answer a Stop hook of c's agent reports, unless the run already has one: later turns answer
// what is typed into the session. The answer is taken from the hook because Claude Code fires it before it writes
// the answer to the transcript. It stays in memory, since it holds message text.
func (s *Server) keepAnswer(c Conversation, session, text string) {
	if c.Integration == "" || session != c.Session {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.answers[c.ID].launched.Equal(c.Launched) {
		s.answers[c.ID] = runAnswer{c.Launched, text}
	}
}

// foldRuns adds up the activity of every earlier run into one entry, so the conversation does not grow with each run.
func foldRuns(m map[string]Activity) map[string]Activity {
	if len(m) == 0 {
		return m
	}
	var a Activity
	for _, t := range m {
		a.add(t)
	}
	slices.Sort(a.Unpriced)
	return map[string]Activity{earlierRuns: a}
}

// forget removes the transcript of an integration's last run, which holds the mail or messages it read. Only the run's
// own file goes, named after its session.
func forget(c Conversation) {
	if c.Session == "" || filepath.Base(c.Transcript) != c.Session+".jsonl" {
		return
	}
	os.Remove(c.Transcript)
	os.RemoveAll(strings.TrimSuffix(c.Transcript, ".jsonl"))
}

// syncIntegrations ends the run and closes the session of every integration whose source is switched off.
func (s *Server) syncIntegrations() {
	cfg := s.cfg().Inbox
	for source := range connectorRuns {
		c, ok := s.store.get(integrationID(source))
		if !ok || c.Closed || cfg[source].enabled() {
			continue
		}
		s.inbox.cancelRun(source)
		s.controlled(c.ID)
		if c.Window != "" {
			killWindow(c.Window)
		}
		s.ticking.Lock()
		s.store.update(c.ID, func(c *Conversation) bool {
			forget(*c)
			c.Closed, c.ClosedAt, c.Status, c.Attention, c.Sleeping, c.Transcript = true, time.Now(), statusExited, "", false, ""
			return true
		})
		s.ticking.Unlock()
		s.publish(c.ID)
	}
}

// integrationSessions are the agent sessions of the integrations, which must not be resumed without their limits.
func (s *Server) integrationSessions() map[string]bool {
	out := map[string]bool{}
	for _, c := range s.store.list() {
		if c.Integration != "" && c.Session != "" {
			out[c.Session] = true
		}
	}
	return out
}

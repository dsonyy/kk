package main

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Rewind says how a harness goes back to an earlier message. The texts are shown before the user confirms.
type Rewind struct {
	// Conversation tells what going back does to the conversation.
	Conversation string `json:"conversation"`
	// Files tells what restoring files does. Empty when the harness cannot restore files.
	Files string `json:"files,omitempty"`
}

var rewinds = map[string]*Rewind{
	"claude": {
		Conversation: "Claude Code takes this message and everything after it out of the conversation. Files stay as they are now.",
		Files:        "Claude Code puts the files it edited since this message back to how they were before it. Restored files go back whole, losing later changes by anyone, including you and other agents. Changes made by shell commands are not undone.",
	},
	"codex": {
		Conversation: "Codex reverts the conversation to the point before this message. File changes are unchanged. Codex does not show which message it picked until it has reverted: after a resume it loads only the last few turns, and a message sent while it was working cannot be picked. kk checks afterwards and sends nothing when Codex went back to a different message.",
	},
	"cursor": {
		Conversation: "Cursor permanently discards this message and everything after it from the conversation. Files stay as they are now.",
		Files:        "Cursor reverts the files it changed since this message to their state right before it.",
	},
}

// rewindMenu is the /rewind dialog of Claude Code 2.1 and Cursor CLI 2026.11: a list of the user's
// messages with the cursor below the last one, then a list of what to restore.
type rewindMenu struct {
	// detail is on the screen that asks what to restore.
	detail              string
	files, conversation string
}

var rewindMenus = map[string]rewindMenu{
	"claude": {"Restore to the point before you sent this message", "Restore code and conversation", "Restore conversation"},
	"cursor": {"Enter to select", "Restore files and conversation", "Restore conversation"},
}

// rewinding holds the ids of the conversations whose terminal a rewind is driving. Anything else
// typed there meanwhile would land in the rewind dialog.
var rewinding sync.Map

func midRewind(id string) error {
	if _, busy := rewinding.Load(id); busy {
		return fail(409, "the agent is going back to an earlier message, try again in a moment")
	}
	return nil
}

func (s *Server) handleRewind(r *http.Request) (any, error) {
	c, err := s.conv(r)
	if err != nil {
		return nil, err
	}
	var in struct {
		// Event is the index of the user's message in the conversation's events.
		Event int `json:"event"`
		// Was is the message as the user saw it when they started editing, without its attachment paths.
		Was   string `json:"was"`
		Text  string `json:"text"`
		Files bool   `json:"files"`
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	h, _ := findHarness(c.Harness)
	if h.Rewind == nil {
		return nil, fail(400, "%s cannot go back to an earlier message", h.Label)
	}
	if in.Files && h.Rewind.Files == "" {
		return nil, fail(400, "%s cannot restore files", h.Label)
	}
	if strings.TrimSpace(in.Text) == "" {
		return nil, fail(400, "text is empty")
	}
	if _, busy := rewinding.LoadOrStore(c.ID, true); busy {
		return nil, fail(409, "the agent is already going back to an earlier message")
	}
	defer rewinding.Delete(c.ID)
	if !s.alive(c) {
		return nil, fail(409, "the agent is not running, resume it first")
	}
	if c, _ = s.store.get(c.ID); c.Status != statusIdle || c.Attention != "" {
		return nil, fail(409, "the agent is busy, wait until it is idle")
	}
	s.mu.Lock()
	var events []Event
	if t := s.tails[c.ID]; t != nil {
		events = t.Events
	}
	s.mu.Unlock()
	if in.Event < 0 || in.Event >= len(events) || events[in.Event].Kind != kindUser || (c.Harness == "claude" && events[in.Event].ID == "") {
		return nil, fail(400, "event %d is not a message of yours that can be edited", in.Event)
	}
	if s.withAttachments(events[in.Event]).Text != in.Was {
		return nil, fail(409, "the conversation changed since you started editing, open the message again")
	}
	back := 0
	for _, e := range events[in.Event:] {
		if e.Kind == kindUser {
			back++
		}
	}
	if err := clearPrompt(c.Window); err != nil {
		return nil, fail(409, "%v", err)
	}
	message := firstLine(events[in.Event].Text, 40)
	if c.Harness == "codex" {
		err = backtrackCodex(c.Window, message, back)
	} else {
		err = rewindMenus[c.Harness].run(c.Window, message, back, in.Files)
	}
	if err == nil {
		err = clearPrompt(c.Window)
	}
	if err != nil {
		return nil, fail(409, "%v", err)
	}
	if err := pasteText(c.Window, in.Text); err != nil {
		return nil, err
	}
	s.markSeen(c.ID)
	return map[string]bool{"ok": true}, nil
}

func press(window string, keys ...string) error {
	for _, k := range keys {
		if _, err := tmux("", "send-keys", "-t", window, k); err != nil {
			return err
		}
		time.Sleep(80 * time.Millisecond)
	}
	return nil
}

var spaces = regexp.MustCompile(`\s+`)

// shows tells whether the screen has text, however the terminal wrapped it.
func shows(screen, text string) bool {
	text = strings.TrimSuffix(text, "…")
	return strings.Contains(spaces.ReplaceAllString(screen, " "), spaces.ReplaceAllString(text, " "))
}

// await polls the screen of a window until ok accepts it.
func await(window, what string, ok func(screen string) bool) (string, error) {
	var screen string
	for end := time.Now().Add(8 * time.Second); time.Now().Before(end); time.Sleep(100 * time.Millisecond) {
		var err error
		if screen, err = capture(window); err != nil {
			return "", err
		}
		if ok(screen) {
			return screen, nil
		}
	}
	return screen, fmt.Errorf("the agent's terminal did not show %s, nothing was sent", what)
}

// clearPrompt empties the agent's input box. In all three TUIs Ctrl+U deletes one line or one line
// break, so it is pressed until the screen stops changing.
func clearPrompt(window string) error {
	before, err := capture(window)
	for still, i := 0, 0; err == nil && i < 100; i++ {
		keys := []string{"send-keys", "-t", window}
		for range 8 {
			keys = append(keys, "C-u")
		}
		if _, err = tmux("", keys...); err != nil {
			break
		}
		time.Sleep(150 * time.Millisecond)
		var now string
		if now, err = capture(window); now != before {
			still = 0
		} else if still++; still == 2 {
			return nil
		}
		before = now
	}
	return errors.Join(errors.New("could not clear the agent's input box, nothing was sent"), err)
}

var menuOption = regexp.MustCompile(`^\s*(\S+\s+)?\d+\. (.+?)\s*$`)

// rewindDialog returns the part of the screen below the title of the rewind dialog.
func rewindDialog(screen string) (string, bool) {
	lines := strings.Split(screen, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) == "Rewind" {
			return strings.Join(lines[i+1:], "\n"), true
		}
	}
	return "", false
}

// run goes back to the message `back` user messages from the end, whose text starts with message.
func (m rewindMenu) run(window, message string, back int, files bool) error {
	if _, err := tmux("", "send-keys", "-t", window, "-l", "/rewind"); err != nil {
		return err
	}
	time.Sleep(400 * time.Millisecond)
	press(window, "Enter")
	if _, err := await(window, "its rewind menu", func(s string) bool {
		d, open := rewindDialog(s)
		return open && strings.Contains(d, "(current)")
	}); err != nil {
		return err
	}
	for range back {
		press(window, "Up")
	}
	press(window, "Enter")
	screen, err := await(window, "what to restore", func(s string) bool {
		d, _ := rewindDialog(s)
		return strings.Contains(d, m.detail) && strings.Contains(d, m.conversation)
	})
	dialog, _ := rewindDialog(screen)
	if err == nil && !shows(dialog, message) {
		err = errors.New("the agent's rewind menu does not list this message, nothing was changed")
	}
	if err != nil {
		press(window, "Escape", "Escape")
		return err
	}
	want, at, n := -1, 0, 0
	for _, line := range strings.Split(dialog, "\n") {
		o := menuOption.FindStringSubmatch(line)
		if o == nil {
			continue
		}
		if o[1] != "" {
			at = n
		}
		// Without file changes to restore the harness offers only the conversation.
		if o[2] == m.conversation && (!files || want < 0) || o[2] == m.files && files {
			want = n
		}
		n++
	}
	for range want - at {
		press(window, "Down")
	}
	for range at - want {
		press(window, "Up")
	}
	press(window, "Enter")
	_, err = await(window, "the conversation after going back", func(s string) bool {
		_, open := rewindDialog(s)
		return !open
	})
	return err
}

// backtrackCodex drives "edit previous message" of Codex 0.166: Esc Esc highlights the last prompt,
// Left moves to older ones and Enter reverts the thread and puts the prompt in the input box.
// Codex does not show which prompt is chosen before it reverts, so the check comes after.
func backtrackCodex(window, message string, back int) error {
	press(window, "Escape")
	if _, err := await(window, "the hint to edit a previous message", func(s string) bool { return strings.Contains(s, "again to edit previous message") }); err != nil {
		return err
	}
	press(window, "Escape")
	if _, err := await(window, "its transcript browser", func(s string) bool { return strings.Contains(s, "Browsing") }); err != nil {
		return err
	}
	for range back - 1 {
		press(window, "Left")
	}
	press(window, "Enter")
	screen, err := await(window, "that the conversation was reverted", func(s string) bool {
		return strings.Contains(s, "Conversation reverted to this point") || strings.Contains(s, "Failed to edit the selected prompt")
	})
	if err != nil {
		press(window, "Escape")
		return err
	}
	if strings.Contains(screen, "Failed to edit the selected prompt") {
		return errors.New("Codex could not edit this message, see the terminal")
	}
	if !shows(screen, message) {
		return errors.New("Codex went back to a different message than the one you picked. Nothing was sent, see the terminal")
	}
	return nil
}

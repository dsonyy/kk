package main

import (
	"fmt"
	"log"
	"regexp"
	"slices"
	"strings"
	"time"
)

const (
	notifyMarker = "[kk notification - automatic, not from the user]"
	// notifyGather is how long the first event waits for others to arrive in the same message.
	notifyGather = 2 * time.Second
	// controlQuiet is how long after a stop, close, interrupt, reset or resume the conversation's events are not reported.
	controlQuiet = 5 * time.Second
)

// watch is what the notifier last knew about a conversation.
type watch struct {
	last      Conversation
	attention bool
	stirred   time.Time
	screen    string
	checked   time.Time
	stalled   bool
}

type notice struct {
	reason string
	at     time.Time
}

// stallAfter is how long a working agent may go without a hook, a transcript line or a change on its screen.
func stallAfter() time.Duration { return envDuration("KK_STALL_AFTER", 10*time.Minute) }

// controlled records a deliberate control of id, whose effects are no news to anyone.
func (s *Server) controlled(id string) {
	s.mu.Lock()
	s.controls[id] = time.Now()
	s.mu.Unlock()
}

var nonLetters = regexp.MustCompile(`[^\pL]+`)

// observe compares c with what the notifier last knew and queues the events the coordinator should hear about.
// The first tick only takes note: a restart is no news.
func (s *Server) observe(c Conversation, transcript bool, now time.Time) {
	if c.Role == coordinatorID {
		return
	}
	w := s.watches[c.ID]
	if w == nil {
		w = &watch{stirred: now}
		s.watches[c.ID] = w
		if s.ticks == 1 {
			w.last, w.attention = c, c.Attention != ""
		}
	}
	prev := w.last
	w.last = c
	if transcript || c.Activity != prev.Activity || c.Status != prev.Status {
		w.stirred, w.stalled = now, false
	}
	stall := stallAfter()
	if c.Status == statusWorking && now.Sub(w.checked) >= stall/5 {
		w.checked = now
		if screen, err := capture(c.Window); err == nil {
			if screen = nonLetters.ReplaceAllString(screen, ""); screen != w.screen {
				w.screen, w.stirred, w.stalled = screen, now, false
			}
		}
	}
	attention := c.Attention != ""
	var reason string
	switch {
	case c.Closed:
	case attention && !w.attention:
		reason = "needs action"
	case c.Status == statusExited && prev.Status != statusExited && prev.Status != "":
		reason = "exited without being stopped"
	case c.TurnEnded.After(prev.TurnEnded) && c.Status == statusIdle && !attention && c.StartedBy == coordinatorID:
		reason = "finished its turn"
	case c.Status == statusWorking && !attention && !w.stalled && now.Sub(w.stirred) >= stall:
		w.stalled = true
		reason = fmt.Sprintf("working with no hook, transcript or screen change for %s", stall.Round(time.Second))
	}
	w.attention = attention
	s.mu.Lock()
	quiet := now.Sub(s.controls[c.ID]) < controlQuiet
	s.mu.Unlock()
	if reason != "" && !quiet {
		s.notices[c.ID] = notice{reason, now}
	}
}

// flushNotices types the gathered events into the coordinator as one message, like kk send does. Claude Code
// queues a message that arrives mid-turn. Nothing is typed while the coordinator is not ready for a message,
// because the Enter would answer its own dialog.
func (s *Server) flushNotices(now time.Time) {
	if len(s.notices) == 0 {
		return
	}
	if !s.cfg().coordinator() {
		clear(s.notices)
		return
	}
	for _, n := range s.notices {
		if now.Sub(n.at) < notifyGather {
			return
		}
	}
	co, ok := s.store.get(coordinatorID)
	if !ok || co.Attention != "" || (co.Status != statusIdle && co.Status != statusWorking) || !s.alive(co) || midRewind(co.ID) != nil {
		return
	}
	ids := make([]string, 0, len(s.notices))
	for id := range s.notices {
		ids = append(ids, id)
	}
	slices.SortFunc(ids, func(a, b string) int { return s.notices[a].at.Compare(s.notices[b].at) })
	lines := []string{notifyMarker}
	for _, id := range ids {
		c, ok := s.store.get(id)
		n := s.notices[id]
		// An attention that someone has dealt with meanwhile is no longer news.
		if !ok || c.Closed || n.reason == "needs action" && c.Attention == "" {
			continue
		}
		line := fmt.Sprintf("- %s %q (%s): %s", id, firstLine(c.Title, 80), c.Status, n.reason)
		if c.Attention != "" {
			line += ": " + firstLine(c.Attention, 300)
		}
		lines = append(lines, line+". Look: kk screen "+id)
	}
	clear(s.notices)
	if len(lines) == 1 {
		return
	}
	text := strings.Join(lines, "\n")
	go func() {
		if err := pasteText(co.Window, text); err != nil {
			log.Printf("notify the coordinator: %v", err)
		}
	}()
}

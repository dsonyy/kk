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
	// controlQuiet is how long after a stop, close or interrupt the conversation's events are not reported.
	controlQuiet = 5 * time.Second
)

// watch is what the notifier last knew about a conversation.
type watch struct {
	last      Conversation
	attention bool
	raises    int
	stirred   time.Time
	screen    string
	checked   time.Time
	stalled   bool
}

type notice struct {
	reason   string
	handover bool
	at       time.Time
}

// stallAfter is how long a working agent may go without a hook, a transcript line or a change on its screen.
func stallAfter() time.Duration { return envDuration("KK_STALL_AFTER", 10*time.Minute) }

// controlled records a deliberate control of id, whose effects are no news to anyone.
func (s *Server) controlled(id string) {
	s.mu.Lock()
	s.controls[id] = time.Now()
	s.mu.Unlock()
}

// conductorsTurn tells whether the conductor, not the user, has to act when c stops: an agent started or last
// driven through the kk CLI and not escalated. The user sees their own agents in the web app.
func conductorsTurn(c Conversation) bool { return c.ByAgent && c.Escalation == "" }

// handOver tells the conductor about a conversation the user started and handed to it. With no conductor running, nobody hears of it.
func (s *Server) handOver(id string) {
	if !s.cfg().coordinator() {
		return
	}
	s.mu.Lock()
	s.handovers[id] = true
	s.mu.Unlock()
}

var nonLetters = regexp.MustCompile(`[^\pL]+`)

// shorten puts s on one line of at most max characters.
func shorten(s string, max int) string { return firstLine(strings.Join(strings.Fields(s), " "), max) }

// observe compares c with what the notifier last knew and queues the events the conductor should hear about.
// The first tick only takes note: a restart is no news.
func (s *Server) observe(c Conversation, transcript bool, chain bool, now time.Time) {
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
	s.mu.Lock()
	raises := s.raises[c.ID]
	quiet := now.Sub(s.controls[c.ID]) < controlQuiet
	handover := s.handovers[c.ID]
	delete(s.handovers, c.ID)
	s.mu.Unlock()
	if s.ticks == 1 {
		w.raises = raises
	}
	// The conductor owns the handoff to queued successors, also of an agent that waits for the user.
	mine := conductorsTurn(c)
	var reason string
	switch {
	case c.Closed || !mine && !chain:
	case mine && attention && (!w.attention || raises > w.raises):
		reason = "needs action"
	case c.Status == statusExited && prev.Status != statusExited && prev.Status != "":
		reason = "exited without being stopped"
	case c.TurnEnded.After(prev.TurnEnded) && c.Status == statusIdle && !attention:
		reason = "finished its turn"
	case mine && c.Status == statusWorking && !attention && !w.stalled && now.Sub(w.stirred) >= stall:
		w.stalled = true
		reason = fmt.Sprintf("working with no hook, transcript or screen change for %s", stall.Round(time.Second))
	}
	w.attention, w.raises = attention, raises
	n := s.notices[c.ID]
	if reason != "" && !quiet {
		n.reason, n.at = reason, now
	}
	if handover {
		n.handover, n.at = true, now
	}
	if n.reason != "" || n.handover {
		s.notices[c.ID] = n
	}
}

// flushNotices types the gathered events into the conductor as one message, like kk send does. Claude Code
// queues a message that arrives mid-turn. Nothing is typed while the conductor is not ready for a message,
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
	if !ok || co.Attention != "" || !slices.Contains([]string{statusIdle, statusWorking, statusBackground}, co.Status) || !s.alive(co) || midRewind(co.ID) != nil {
		return
	}
	ids := make([]string, 0, len(s.notices))
	for id := range s.notices {
		ids = append(ids, id)
	}
	slices.SortFunc(ids, func(a, b string) int { return s.notices[a].at.Compare(s.notices[b].at) })
	lines := []string{notifyMarker}
	var told []string
	queued := s.queued()
	for _, id := range ids {
		c, ok := s.store.get(id)
		n := s.notices[id]
		// An attention that someone has dealt with meanwhile, or an agent the user took over or got escalated, is no longer
		// news, unless queued successors wait for it.
		next := queued[id]
		if !ok || c.Closed && n.reason != closedReason || !conductorsTurn(c) && len(next) == 0 {
			continue
		}
		if n.reason == "needs action" && c.Attention == "" {
			n.reason = ""
		}
		var parts []string
		if n.handover {
			handover := "the user started it in " + c.Dir + " and handed it over to you"
			if c.Prompt != "" {
				handover += fmt.Sprintf(", first prompt %q", shorten(c.Prompt, 300))
			}
			parts = append(parts, handover)
		}
		if n.reason != "" {
			reason := n.reason
			if c.Attention != "" {
				reason += ": " + strings.TrimSuffix(firstLine(c.Attention, 300), ".")
			}
			parts = append(parts, reason)
		}
		if len(parts) == 0 {
			continue
		}
		if len(next) > 0 {
			parts = append(parts, chainNote(next))
		}
		line := fmt.Sprintf("- %s %q (%s): %s", id, firstLine(c.Title, 80), c.Status, strings.Join(parts, "; "))
		lines = append(lines, line+". Look: kk screen "+id)
		told = append(told, id)
	}
	clear(s.notices)
	if len(lines) == 1 {
		return
	}
	text := strings.Join(lines, "\n")
	log.Printf("notify the conductor about %s", strings.Join(told, ", "))
	go func() {
		if err := pasteText(co.Window, text); err != nil {
			log.Printf("notify the conductor: %v", err)
		}
	}()
}

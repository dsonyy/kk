package main

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// closedReason is the notice the conductor gets when a conversation with queued successors is closed.
const closedReason = "was closed"

// queued maps each conversation to the queued conversations that follow it, oldest first.
func (s *Server) queued() map[string][]Conversation {
	out := map[string][]Conversation{}
	for _, c := range s.store.list() {
		if c.After != "" && c.Status == statusQueued && !c.Closed {
			out[c.After] = append(out[c.After], c)
		}
	}
	for _, next := range out {
		sort.Slice(next, func(i, j int) bool { return next[i].Created.Before(next[j].Created) })
	}
	return out
}

func (s *Server) successors(id string) []Conversation { return s.queued()[id] }

// queue saves c as a conversation that waits for kk start after its predecessor.
func (s *Server) queue(c Conversation, after string, worktree bool) (Conversation, error) {
	pred, ok := s.store.get(after)
	switch {
	case !ok:
		return c, fail(400, "no conversation %s to follow", after)
	case pred.Role != "":
		return c, fail(400, "a session cannot follow the conductor")
	case pred.Integration != "":
		return c, fail(400, "a session cannot follow the %s session", pred.Integration)
	case pred.Closed:
		return c, fail(400, "conversation %s is closed", after)
	}
	if worktree {
		if _, err := git(c.Dir, "rev-parse", "--git-dir"); err != nil {
			return c, fail(400, "%s is not a git repository", c.Dir)
		}
	}
	c.After, c.Worktree, c.Status = pred.ID, worktree, statusQueued
	if c.Group == "" {
		c.Group = pred.Group
	}
	if err := s.store.put(c); err != nil {
		return c, err
	}
	s.publish(c.ID)
	return c, nil
}

// inheritGroup gives the queued successors of id that have no group yet the group id just got.
func (s *Server) inheritGroup(id, group string) {
	for _, next := range s.successors(id) {
		if _, changed := s.store.update(next.ID, func(c *Conversation) bool {
			if c.Group != "" {
				return false
			}
			c.Group = group
			return true
		}); changed {
			s.publish(next.ID)
			s.inheritGroup(next.ID, group)
		}
	}
}

// handlePrompt changes the prepared prompt of a queued conversation.
func (s *Server) handlePrompt(r *http.Request) (any, error) {
	var in struct {
		Prompt string `json:"prompt"`
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	if strings.TrimSpace(in.Prompt) == "" {
		return nil, fail(400, "prompt is empty")
	}
	c, err := s.conv(r)
	if err != nil {
		return nil, err
	}
	c, ok := s.store.update(c.ID, func(c *Conversation) bool {
		if c.Status != statusQueued || c.Closed {
			return false
		}
		c.Prompt, c.Title = in.Prompt, firstLine(in.Prompt, 80)
		return true
	})
	if !ok {
		return nil, fail(409, "only the prompt of a queued session can change, this one is %s", c.Status)
	}
	s.publish(c.ID)
	return s.view(c, false), nil
}

// handleStart starts a queued conversation, with the handoff from its predecessor after the prepared prompt.
func (s *Server) handleStart(r *http.Request) (any, error) {
	var in struct {
		Handoff string `json:"handoff"`
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	c, err := s.conv(r)
	if err != nil {
		return nil, err
	}
	// The status changes before the worktree is made, so a second start finds the conversation taken.
	c, ok := s.store.update(c.ID, func(c *Conversation) bool {
		if c.Status != statusQueued || c.Closed {
			return false
		}
		c.Status, c.Launched = statusStarting, time.Now()
		return true
	})
	if !ok {
		if c.Closed {
			return nil, fail(409, "the session is closed, reopen it first")
		}
		return nil, fail(409, "the session is %s, only a queued session starts", c.Status)
	}
	if handoff := strings.TrimSpace(in.Handoff); handoff != "" {
		c.Prompt += "\n\nHandoff from the session before this one:\n\n" + handoff
	}
	if c.Worktree {
		dir, err := addWorktree(c.Dir, worktreeBranch(c.Title, c.ID))
		if err != nil {
			s.store.update(c.ID, func(c *Conversation) bool {
				c.Status, c.Launched = statusQueued, time.Time{}
				return true
			})
			s.publish(c.ID)
			return nil, err
		}
		c.Repo, c.Dir, c.Worktree = c.Dir, dir, false
	}
	if pred, ok := s.store.get(c.After); ok && c.Group == "" {
		c.Group = pred.Group
	}
	c.ByAgent, c.Seen = !byUser(r), time.Now()
	if err := s.launch(&c, false, ""); err != nil {
		return nil, err
	}
	if c.Group == "" {
		s.queueGroup(c.ID)
	}
	return s.view(c, false), nil
}

// chainNote names the queued successors of a conversation for the conductor.
func chainNote(next []Conversation) string {
	names := make([]string, len(next))
	for i, c := range next {
		names[i] = fmt.Sprintf("%s %q", c.ID, firstLine(c.Title, 80))
	}
	return "queued after it: " + strings.Join(names, ", ")
}

// closeNotice tells the conductor that a conversation with queued successors was closed. The tick no longer
// looks at a closed conversation, so the notice is queued here.
func (s *Server) closeNotice(id string) {
	if !s.cfg().coordinator() || len(s.successors(id)) == 0 {
		return
	}
	s.ticking.Lock()
	s.notices[id] = notice{reason: closedReason, at: time.Now()}
	s.ticking.Unlock()
}

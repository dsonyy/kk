package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Subagent is an agent that a conversation's agent started, such as with Claude Code's Agent tool. kk keeps what it
// is, never what it said: its transcript is the source, and kk reads it only while someone looks at it.
type Subagent struct {
	// ID is the harness's id of the subagent, unique within the conversation.
	ID string `yaml:"id"`
	// Parent is the ID of the subagent that started this one, empty when the conversation's agent did.
	Parent string `yaml:"parent,omitempty"`
	Title  string `yaml:"title,omitempty"`
	Type   string `yaml:"type,omitempty"`
	Model  string `yaml:"model,omitempty"`
	// State is running, done, failed or stopped, as far as the harness recorded it. A subagent recorded as running
	// stopped when its parent's process did.
	State      string    `yaml:"state"`
	Started    time.Time `yaml:"started,omitempty"`
	Last       time.Time `yaml:"last,omitempty"`
	Worktree   string    `yaml:"worktree,omitempty"`
	Branch     string    `yaml:"branch,omitempty"`
	Transcript string    `yaml:"transcript"`
}

const (
	agentRunning = "running"
	agentDone    = "done"
	agentFailed  = "failed"
	agentStopped = "stopped"
)

// subagentView is a subagent as the API shows it. Its id is the conversation's id and the subagent's, joined by a colon.
type subagentView struct {
	ID string `json:"id"`
	// Of is the conversation the subagent belongs to.
	Of string `json:"of"`
	// Parent is the id of the subagent that started this one, empty when the conversation's agent did.
	Parent   string    `json:"parent,omitempty"`
	Title    string    `json:"title"`
	Type     string    `json:"type,omitempty"`
	Model    string    `json:"model,omitempty"`
	State    string    `json:"state"`
	Started  time.Time `json:"started"`
	Last     time.Time `json:"last"`
	Worktree string    `json:"worktree,omitempty"`
	Branch   string    `json:"branch,omitempty"`
	// Usage is what the subagent's models used, already part of the conversation's usage. Nil for a harness without usage.
	Usage *Usage `json:"usage"`
}

func subagentID(conv, agent string) string { return conv + ":" + agent }

// live reports whether the agent of c runs, so that the subagents it started can.
func live(c Conversation) bool {
	return !c.Closed && slices.Contains([]string{statusStarting, statusWorking, statusBackground, statusIdle}, c.Status)
}

// subagentViews lists the subagents of c, oldest first.
func subagentViews(c Conversation) []subagentView {
	out := []subagentView{}
	for _, a := range c.Subagents {
		v := subagentView{ID: subagentID(c.ID, a.ID), Of: c.ID, Title: a.Title, Type: a.Type, Model: a.Model, State: a.State, Started: a.Started, Last: a.Last, Worktree: a.Worktree, Branch: a.Branch}
		if a.Parent != "" {
			v.Parent = subagentID(c.ID, a.Parent)
		}
		// Subagents die with the process that started them.
		if a.State == agentRunning && (!live(c) || a.Started.Before(c.Launched)) {
			v.State = agentStopped
		}
		if usageParser[c.Harness] != nil {
			u := c.Transcripts[a.Transcript].Usage
			v.Usage = &u
		}
		out = append(out, v)
	}
	return out
}

// mergeSubagents puts what the tick read about the subagents of c's current transcript into the ones c keeps, which
// include those of its earlier transcripts. It reports whether anything changed.
func mergeSubagents(c *Conversation, read []Subagent) bool {
	changed := false
	for _, a := range read {
		i := slices.IndexFunc(c.Subagents, func(b Subagent) bool { return b.ID == a.ID })
		switch {
		case i < 0:
			c.Subagents = append(slices.Clone(c.Subagents), a)
		case c.Subagents[i] != a:
			c.Subagents = slices.Clone(c.Subagents)
			c.Subagents[i] = a
		default:
			continue
		}
		changed = true
	}
	if changed {
		slices.SortStableFunc(c.Subagents, func(a, b Subagent) int { return a.Started.Compare(b.Started) })
	}
	return changed
}

// describe tells what the subagent with the transcript at path is, from what its tail read and from the parent's
// transcript tail. The caller holds s.mu.
func (s *Server) describe(c Conversation, path string, sub, parent *Tail) (Subagent, bool) {
	a := Subagent{Transcript: path, Model: sub.model, Started: sub.first, Last: sub.Last, State: agentRunning}
	switch c.Harness {
	case "claude":
		a.ID = strings.TrimPrefix(strings.TrimSuffix(filepath.Base(path), ".jsonl"), "agent-")
		m, ok := s.claudeMeta(strings.TrimSuffix(path, ".jsonl") + ".meta.json")
		if !ok {
			return a, false
		}
		a.Parent, a.Title, a.Type, a.Worktree, a.Branch = m.ParentAgentID, m.Description, m.AgentType, m.WorktreePath, m.WorktreeBranch
		if a.Model == "" {
			a.Model = m.Model
		}
		switch {
		case parent != nil && parent.agents[a.ID] == "failed":
			a.State = agentFailed
		case m.StoppedByUser || parent != nil && slices.Contains([]string{"killed", "stopped"}, parent.agents[a.ID]):
			a.State = agentStopped
		case m.Finished || parent != nil && parent.agents[a.ID] == "completed":
			a.State = agentDone
		}
	case "codex":
		a.ID = codexThread(path)
		spawn := codexSpawnOf(path)
		if spawn.Parent != c.Session {
			a.Parent = spawn.Parent
		}
		a.Title, a.Type = spawn.Nickname, spawn.Role
		if a.Title == "" {
			a.Title = spawn.Role
		}
		if !sub.busy {
			a.State = agentDone
		}
	default:
		return a, false
	}
	return a, a.ID != ""
}

// claudeMeta is what Claude Code keeps about a subagent next to its transcript.
type claudeMeta struct {
	AgentType      string `json:"agentType"`
	Description    string `json:"description"`
	ParentAgentID  string `json:"parentAgentId"`
	Model          string `json:"model"`
	WorktreePath   string `json:"worktreePath"`
	WorktreeBranch string `json:"worktreeBranch"`
	Finished       bool   `json:"finished"`
	StoppedByUser  bool   `json:"stoppedByUser"`
}

type metaFile struct {
	mod  time.Time
	meta claudeMeta
}

// claudeMeta reads a subagent's meta file when it changed since the last read. The caller holds s.mu.
func (s *Server) claudeMeta(path string) (claudeMeta, bool) {
	st, err := os.Stat(path)
	if err != nil {
		return claudeMeta{}, false
	}
	if f, ok := s.metas[path]; ok && f.mod.Equal(st.ModTime()) {
		return f.meta, true
	}
	data, err := os.ReadFile(path)
	var m claudeMeta
	if err != nil || json.Unmarshal(data, &m) != nil {
		return m, false
	}
	s.metas[path] = metaFile{st.ModTime(), m}
	return m, true
}

// taskStatus is how a task ended, from the notification Claude Code gives the model.
var taskStatus = regexp.MustCompile(`<task-notification>\s*<task-id>([^<]+)</task-id>[\s\S]*?<status>([^<]+)</status>`)

// peekIdle is how long kk keeps the events of a subagent nobody asked for.
const peekIdle = 2 * time.Minute

// peek is the full tail of a subagent's transcript, kept while someone looks at the subagent.
type peek struct {
	tail *Tail
	used time.Time
}

// subagentTail reads the transcript of a subagent of a conversation with its harness's full parser and returns a copy
// of its tail, or false when there is no such subagent.
func (s *Server) subagentTail(id string) (Conversation, Subagent, *Tail, bool) {
	conv, agent, _ := strings.Cut(id, ":")
	c, ok := s.store.get(conv)
	if !ok {
		return c, Subagent{}, nil, false
	}
	i := slices.IndexFunc(c.Subagents, func(a Subagent) bool { return a.ID == agent })
	parse := parserFor(c.Harness)
	if i < 0 || parse == nil {
		return c, Subagent{}, nil, false
	}
	a := c.Subagents[i]
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.peeks[a.Transcript]
	if p == nil {
		p = &peek{tail: newTail(a.Transcript)}
		s.peeks[a.Transcript] = p
	}
	p.used = time.Now()
	p.tail.read(parse)
	cp := *p.tail
	cp.Events = slices.Clone(p.tail.Events)
	return c, a, &cp, true
}

// forget drops the events of the subagents nobody looked at for a while.
func (s *Server) forget(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for path, p := range s.peeks {
		if now.Sub(p.used) > peekIdle {
			delete(s.peeks, path)
		}
	}
}

// showSubagent shows a subagent the way a conversation is shown, read only: no actions, and nobody to wait for.
func (s *Server) showSubagent(id string) (convView, error) {
	c, a, t, ok := s.subagentTail(id)
	if !ok {
		return convView{}, fail(404, "no subagent %s", id)
	}
	var sv subagentView
	for _, v := range subagentViews(c) {
		if v.ID == id {
			sv = v
		}
	}
	dir := c.Dir
	if a.Worktree != "" {
		dir = a.Worktree
	}
	status := statusIdle
	if sv.State == agentRunning {
		status = statusWorking
	}
	conv := Conversation{ID: id, Harness: c.Harness, Model: a.Model, Dir: dir, Title: a.Title, Created: a.Started, LastMessage: a.Last, Activity: a.Last, Status: status, Group: c.Group}
	v := convView{Conversation: conv, Project: c.Project(), Actions: []string{}, Events: []Event{}, Queued: []Event{}, Usage: sv.Usage, Subagent: &sv, Subagents: []subagentView{}}
	for _, e := range t.Events {
		v.Events = append(v.Events, withImages(id, s.withAttachments(e)))
	}
	return v, nil
}

// subagentNames reports whether a message of the subagent with this id names the image path.
func (s *Server) subagentNames(id, path string) bool {
	_, _, t, ok := s.subagentTail(id)
	if !ok {
		return false
	}
	for _, e := range t.Events {
		if (e.Kind == kindUser || e.Kind == kindAssistant) && slices.Contains(imagePaths(e.Text), path) {
			return true
		}
	}
	return false
}

// agentRow is a subagent in a tree of them, at its depth below the conversation.
type agentRow struct {
	subagentView
	depth int
}

// agentTree orders subagents as a tree, each below the one that started it. Unless all is set, it keeps only the
// running ones, the failed ones while their session is open, and those that started one of these, and returns the others.
func agentTree(list []subagentView, all, closed bool) ([]agentRow, []subagentView) {
	kids := map[string][]subagentView{}
	for _, a := range list {
		parent := a.Parent
		if !slices.ContainsFunc(list, func(b subagentView) bool { return b.ID == parent }) {
			parent = ""
		}
		kids[parent] = append(kids[parent], a)
	}
	var shown func(a subagentView) bool
	shown = func(a subagentView) bool {
		return all || a.State == agentRunning || a.State == agentFailed && !closed || slices.ContainsFunc(kids[a.ID], shown)
	}
	var out []agentRow
	var walk func(parent string, depth int)
	walk = func(parent string, depth int) {
		for _, a := range kids[parent] {
			if shown(a) {
				out = append(out, agentRow{a, depth})
				walk(a.ID, depth+1)
			}
		}
	}
	walk("", 0)
	var folded []subagentView
	for _, a := range list {
		if !slices.ContainsFunc(out, func(r agentRow) bool { return r.ID == a.ID }) {
			folded = append(folded, a)
		}
	}
	return out, folded
}

// foldLabel says what a fold of subagents hides, by state: "2 done, 1 stopped subagents".
func foldLabel(folded []subagentView) string {
	var parts []string
	for _, state := range []string{agentDone, agentFailed, agentStopped} {
		if n := len(slices.DeleteFunc(slices.Clone(folded), func(a subagentView) bool { return a.State != state })); n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, state))
		}
	}
	return strings.Join(parts, ", ") + " " + plural(len(folded), "subagent", "subagents")
}

// printAgents prints the subagents of a conversation, indented under it.
func printAgents(list []subagentView, all, closed bool) {
	rows, folded := agentTree(list, all, closed)
	for _, a := range rows {
		model := a.Model
		if model == "" {
			model = "default"
		}
		line := fmt.Sprintf("%s%s  %-8s  %-16s  %-17s  %s  %s", strings.Repeat("  ", a.depth+1), a.ID, a.State, a.Type, model, a.Last.Local().Format("01-02 15:04"), a.Title)
		if a.Worktree != "" {
			line += fmt.Sprintf("  (worktree %s on %s)", a.Worktree, a.Branch)
		}
		fmt.Println(line)
	}
	if len(folded) > 0 {
		fmt.Printf("  %s\n", foldLabel(folded))
	}
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

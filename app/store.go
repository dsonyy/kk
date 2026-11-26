package main

import (
	"crypto/rand"
	"encoding/hex"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"go.yaml.in/yaml/v3"
)

// coordinatorID names the conductor's conversation and tmux window. It keeps its old value so a running conductor and its state carry over.
const coordinatorID = "coordinator"

type Conversation struct {
	ID      string `yaml:"id" json:"id"`
	Role    string `yaml:"role,omitempty" json:"role,omitempty"`
	Harness string `yaml:"harness" json:"harness"`
	Model   string `yaml:"model,omitempty" json:"model,omitempty"`
	Dir     string `yaml:"dir" json:"dir"`
	// Repo is the directory the user chose when the agent runs in a worktree made for it.
	Repo        string    `yaml:"repo,omitempty" json:"repo,omitempty"`
	Prompt      string    `yaml:"prompt,omitempty" json:"prompt,omitempty"`
	Title       string    `yaml:"title,omitempty" json:"title"`
	Created     time.Time `yaml:"created" json:"created"`
	LastMessage time.Time `yaml:"last_message,omitempty" json:"last_message"`
	Session     string    `yaml:"session,omitempty" json:"session,omitempty"`
	Transcript  string    `yaml:"transcript,omitempty" json:"transcript,omitempty"`
	Window      string    `yaml:"window,omitempty" json:"window,omitempty"`
	Status      string    `yaml:"status" json:"status"`
	Attention   string    `yaml:"attention,omitempty" json:"attention,omitempty"`
	Launched    time.Time `yaml:"launched" json:"launched"`
	Activity    time.Time `yaml:"activity,omitempty" json:"activity"`
	TurnEnded   time.Time `yaml:"turn_ended,omitempty" json:"turn_ended"`
	Seen        time.Time `yaml:"seen,omitempty" json:"seen"`
	Context     int       `yaml:"context_tokens,omitempty" json:"context_tokens"`
	Compacted   bool      `yaml:"compacted,omitempty" json:"compacted,omitempty"`
	Closed      bool      `yaml:"closed,omitempty" json:"closed,omitempty"`
	ClosedAt    time.Time `yaml:"closed_at,omitempty" json:"closed_at,omitzero"`
	// ByAgent is set when an agent through the kk CLI, not the user, started the conversation or acted on it last.
	// Conversations from before it existed count as the user's.
	ByAgent bool `yaml:"by_agent,omitempty" json:"-"`
	// Escalation is why the conductor handed a waiting conversation to the user. It lasts while the agent waits.
	Escalation string `yaml:"escalation,omitempty" json:"escalation,omitempty"`
	// Group names the feature the agent works on, shared by the agents of one project that work on the same thing.
	Group string `yaml:"group,omitempty" json:"group,omitempty"`
	// After is the conversation this one follows in a chain. A queued conversation starts only once it has finished.
	After string `yaml:"after,omitempty" json:"after,omitempty"`
	// Worktree asks for a worktree of Dir when a queued conversation starts.
	Worktree bool `yaml:"worktree,omitempty" json:"worktree,omitempty"`
}

const (
	statusStarting = "starting"
	statusWorking  = "working"
	statusIdle     = "idle"
	// statusBackground is an agent whose turn ended while a background task it started still runs. The harness starts
	// the next turn by itself when the task ends, so nobody has to act.
	statusBackground = "background"
	statusExited     = "exited"
	// statusQueued is a conversation prepared with its prompt that waits for kk start. No agent runs yet.
	statusQueued = "queued"
)

const (
	waitingUser      = "user"
	waitingConductor = "conductor"
)

// Waiting reports whether the agent has stopped and someone has to act: its turn ended, it needs input, or it exited.
func (c *Conversation) Waiting() bool {
	return !c.Closed && (c.Attention != "" || c.Status == statusIdle || c.Status == statusExited)
}

// WaitingFor says who has to act on a waiting agent. Every agent that stops waits for the conductor, unless the conductor
// escalated it, the user acted on it last, or no conductor runs. The conductor itself always waits for the user.
func (c *Conversation) WaitingFor(conductor bool) string {
	switch {
	case !c.Waiting():
		return ""
	case c.Role != "" || c.Escalation != "" || !c.ByAgent || !conductor:
		return waitingUser
	}
	return waitingConductor
}

func (c *Conversation) Unread() bool {
	return !c.Closed && !c.TurnEnded.IsZero() && c.TurnEnded.After(c.Seen)
}

// Project is the directory the conversation belongs to: the one the user chose, also when the agent works in a worktree of it.
// A session resumed from history has no Repo, so a worktree made by kk is recognised by its path.
func (c *Conversation) Project() string {
	if c.Repo != "" {
		return c.Repo
	}
	if checkout, rest, ok := strings.Cut(c.Dir, "/"+worktreesDir+"/"); ok {
		_, sub, _ := strings.Cut(rest, "/")
		return filepath.Join(checkout, sub)
	}
	return c.Dir
}

type Store struct {
	dir   string
	mu    sync.Mutex
	convs map[string]*Conversation
}

func home() string {
	if h := os.Getenv("KK_HOME"); h != "" {
		return h
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".kk")
}

func openStore(dir string) (*Store, error) {
	s := &Store{dir: dir, convs: map[string]*Conversation{}}
	if err := os.MkdirAll(filepath.Join(dir, "conversations"), 0o700); err != nil {
		return nil, err
	}
	files, _ := filepath.Glob(filepath.Join(dir, "conversations", "*.yaml"))
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var c Conversation
		if yaml.Unmarshal(data, &c) == nil && c.ID != "" {
			s.convs[c.ID] = &c
		}
	}
	return s, nil
}

func newID() string {
	b := make([]byte, 4)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *Store) get(id string) (Conversation, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.convs[id]
	if !ok {
		return Conversation{}, false
	}
	return *c, true
}

func (s *Store) list() []Conversation {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Conversation, 0, len(s.convs))
	for _, c := range s.convs {
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool { return lastActive(out[i]).After(lastActive(out[j])) })
	return out
}

func lastActive(c Conversation) time.Time {
	t := c.Created
	for _, x := range []time.Time{c.LastMessage, c.Activity} {
		if x.After(t) {
			t = x
		}
	}
	return t
}

func (s *Store) put(c Conversation) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.save(&c)
}

// update applies fn under the lock and saves only when fn reports a change.
func (s *Store) update(id string, fn func(c *Conversation) bool) (Conversation, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.convs[id]
	if !ok {
		return Conversation{}, false
	}
	c := *cur
	if !fn(&c) {
		return c, false
	}
	s.save(&c)
	return c, true
}

func (s *Store) save(c *Conversation) error {
	if !c.Waiting() {
		c.Escalation = ""
	}
	data, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	path := filepath.Join(s.dir, "conversations", c.ID+".yaml")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	logChange(s.convs[c.ID], c)
	s.convs[c.ID] = c
	return nil
}

// logChange logs what changed about a conversation. Titles, groups, prompts and attention texts can repeat what was said, so they stay out.
func logChange(old, c *Conversation) {
	if old == nil {
		log.Printf("conversation %s created: %s in %s", c.ID, c.Harness, c.Dir)
		old = &Conversation{Status: c.Status}
	}
	if old.Status != c.Status {
		log.Printf("conversation %s: %s -> %s", c.ID, old.Status, c.Status)
	}
	if (old.Attention == "") != (c.Attention == "") {
		log.Printf("conversation %s: %s", c.ID, map[bool]string{true: "needs attention", false: "attention cleared"}[c.Attention != ""])
	}
	if old.Closed != c.Closed {
		log.Printf("conversation %s: %s", c.ID, map[bool]string{true: "closed", false: "reopened"}[c.Closed])
	}
	if old.Escalation == "" && c.Escalation != "" {
		log.Printf("conversation %s: escalated to the user", c.ID)
	}
	if old.Group != c.Group {
		log.Printf("conversation %s: group changed", c.ID)
	}
	if old.After != c.After {
		log.Printf("conversation %s: after %s", c.ID, c.After)
	}
	if old.Session != c.Session && c.Session != "" {
		log.Printf("conversation %s: session %s", c.ID, c.Session)
	}
}

func (s *Store) recentDirs() []string {
	seen := map[string]bool{}
	var dirs []string
	for _, c := range s.list() {
		dir := c.Project()
		if c.Role == "" && !seen[dir] {
			seen[dir] = true
			dirs = append(dirs, dir)
		}
	}
	if len(dirs) == 0 {
		h, _ := os.UserHomeDir()
		dirs = append(dirs, h)
	}
	return dirs
}

func firstLine(s string, max int) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if r := []rune(s); len(r) > max {
		s = string(r[:max-1]) + "…"
	}
	return s
}

package main

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"go.yaml.in/yaml/v3"
)

const coordinatorID = "coordinator"

type Conversation struct {
	ID          string    `yaml:"id" json:"id"`
	Role        string    `yaml:"role,omitempty" json:"role,omitempty"`
	Harness     string    `yaml:"harness" json:"harness"`
	Model       string    `yaml:"model,omitempty" json:"model,omitempty"`
	Dir         string    `yaml:"dir" json:"dir"`
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
}

const (
	statusStarting = "starting"
	statusWorking  = "working"
	statusIdle     = "idle"
	statusExited   = "exited"
)

func (c *Conversation) Unread() bool {
	return !c.TurnEnded.IsZero() && c.TurnEnded.After(c.Seen)
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
	s.convs[c.ID] = c
	return nil
}

func (s *Store) recentDirs() []string {
	seen := map[string]bool{}
	var dirs []string
	for _, c := range s.list() {
		if c.Role == "" && !seen[c.Dir] {
			seen[c.Dir] = true
			dirs = append(dirs, c.Dir)
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

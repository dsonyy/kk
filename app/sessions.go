package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// PastSession is a session found in a harness's own history, started from kk or not.
type PastSession struct {
	ID       string    `json:"id"`
	Title    string    `json:"title"`
	Dir      string    `json:"dir"`
	Modified time.Time `json:"modified"`
	// Messages counts what the user and the agent said, without tool calls.
	Messages int       `json:"messages"`
	Started  time.Time `json:"started"`
	Ended    time.Time `json:"ended"`
	path     string
}

const pastSessionLimit = 80

// pastEntry keeps a transcript's reader between listings, so a growing transcript is only read from where it ended.
type pastEntry struct {
	sync.Mutex
	tail  *Tail
	ps    PastSession
	first string
}

var pastCache = struct {
	sync.Mutex
	byPath map[string]*pastEntry
}{byPath: map[string]*pastEntry{}}

func (e *pastEntry) scan(parse func(*Tail, []byte)) PastSession {
	e.Lock()
	defer e.Unlock()
	e.tail.read(func(t *Tail, line []byte) {
		if e.ps.Dir == "" {
			var meta struct {
				Cwd     string `json:"cwd"`
				Payload struct {
					Cwd string `json:"cwd"`
				} `json:"payload"`
			}
			if json.Unmarshal(line, &meta) == nil {
				e.ps.Dir = meta.Cwd + meta.Payload.Cwd
			}
		}
		parse(t, line)
	})
	for _, ev := range e.tail.Events {
		if ev.Kind != kindUser && ev.Kind != kindAssistant {
			continue
		}
		e.ps.Messages++
		if e.first == "" && ev.Kind == kindUser && !ev.command {
			e.first = firstLine(ev.Text, 80)
		}
		if e.ps.Started.IsZero() {
			e.ps.Started = ev.Time
		}
		if ev.Time.After(e.ps.Ended) {
			e.ps.Ended = ev.Time
		}
	}
	// The events are counted and dropped; a whole history of transcripts does not fit in memory.
	e.tail.Events, e.tail.tools, e.tail.forks = nil, map[string]int{}, map[string]int{}
	ps := e.ps
	if ps.Title = e.tail.Title; ps.Title == "" {
		ps.Title = e.first
	}
	return ps
}

func pastSessions(harness, dir string) []PastSession {
	home, _ := os.UserHomeDir()
	var pattern string
	switch harness {
	case "claude":
		pattern = filepath.Join(home, ".claude", "projects", "*", "*.jsonl")
	case "codex":
		pattern = filepath.Join(codexHome(), "sessions", "*", "*", "*", "*.jsonl")
	case "cursor":
		pattern = filepath.Join(filepath.Dir(filepath.Dir(cursorTranscript(dir, "x"))), "*", "*.jsonl")
	}
	type file struct {
		path string
		mod  time.Time
	}
	var files []file
	paths, _ := filepath.Glob(pattern)
	for _, p := range paths {
		if st, err := os.Stat(p); err == nil {
			files = append(files, file{p, st.ModTime()})
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mod.After(files[j].mod) })
	if len(files) > pastSessionLimit {
		files = files[:pastSessionLimit]
	}
	found := make([]PastSession, len(files))
	var wg sync.WaitGroup
	slots := make(chan bool, 8)
	for i, f := range files {
		pastCache.Lock()
		e := pastCache.byPath[f.path]
		if e == nil {
			e = &pastEntry{tail: newTail(f.path)}
			e.ps.ID, e.ps.path = strings.TrimSuffix(filepath.Base(f.path), ".jsonl"), f.path
			pastCache.byPath[f.path] = e
		}
		pastCache.Unlock()
		wg.Add(1)
		go func() {
			defer wg.Done()
			slots <- true
			defer func() { <-slots }()
			ps := e.scan(parserFor(harness))
			ps.Modified = f.mod
			if harness == "cursor" {
				ps.Dir = dir
			}
			if harness == "codex" {
				// The file name ends with the session id after a timestamp. A thread that was reverted
				// continues in a file named <session id>_<rollout id>.
				if n := len(ps.ID); n > 73 && ps.ID[n-37] == '_' {
					ps.ID = ps.ID[n-73 : n-37]
				} else if n > 36 {
					ps.ID = ps.ID[n-36:]
				}
				if name := codexThreadName(ps.ID); name != "" {
					ps.Title = name
				}
			}
			found[i] = ps
		}()
	}
	wg.Wait()
	out := []PastSession{}
	seen := map[string]bool{}
	for _, ps := range found {
		if ps.Title != "" && ps.Dir != "" && !seen[ps.ID] {
			seen[ps.ID] = true
			out = append(out, ps)
		}
	}
	return out
}

func (s *Server) handleSessions(r *http.Request) (any, error) {
	harness := r.URL.Query().Get("harness")
	if _, ok := findHarness(harness); !ok {
		return nil, fail(400, "unknown harness %q", harness)
	}
	dir, _ := expandDir(r.URL.Query().Get("dir"))
	own := s.integrationSessions()
	return slices.DeleteFunc(pastSessions(harness, dir), func(p PastSession) bool { return own[p.ID] }), nil
}

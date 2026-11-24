package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
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
	path     string
}

const pastSessionLimit = 80

var pastCache = struct {
	sync.Mutex
	byPath map[string]PastSession
}{byPath: map[string]PastSession{}}

// readHead parses the start of a transcript, enough for its directory and what it is about.
func readHead(path string, parse func(*Tail, []byte)) (dir string, t *Tail) {
	t = newTail(path)
	f, err := os.Open(path)
	if err != nil {
		return "", t
	}
	defer f.Close()
	rd := bufio.NewReaderSize(io.LimitReader(f, 4<<20), 1<<20)
	for lines := 0; lines < 400; lines++ {
		line, err := rd.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			var meta struct {
				Cwd     string `json:"cwd"`
				Payload struct {
					Cwd string `json:"cwd"`
				} `json:"payload"`
			}
			if dir == "" && json.Unmarshal(line, &meta) == nil {
				dir = meta.Cwd + meta.Payload.Cwd
			}
			parse(t, line)
		}
		if err != nil || (dir != "" && t.Title != "") {
			break
		}
	}
	return dir, t
}

func pastSessions(harness, dir string) []PastSession {
	home, _ := os.UserHomeDir()
	var pattern string
	id := func(path string) string { return strings.TrimSuffix(filepath.Base(path), ".jsonl") }
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
	out := []PastSession{}
	for _, f := range files {
		pastCache.Lock()
		ps, ok := pastCache.byPath[f.path]
		pastCache.Unlock()
		if !ok || !ps.Modified.Equal(f.mod) {
			from, t := readHead(f.path, parserFor(harness))
			ps = PastSession{ID: id(f.path), Title: t.Title, Dir: from, Modified: f.mod, path: f.path}
			if harness == "cursor" {
				ps.Dir = dir
			}
			if harness == "codex" {
				// The file name ends with the session id after a timestamp.
				if n := len(ps.ID); n > 36 {
					ps.ID = ps.ID[n-36:]
				}
				if name := codexThreadName(ps.ID); name != "" {
					ps.Title = name
				}
			}
			for _, e := range t.Events {
				if ps.Title == "" && e.Kind == kindUser {
					ps.Title = firstLine(e.Text, 80)
				}
			}
			pastCache.Lock()
			pastCache.byPath[f.path] = ps
			pastCache.Unlock()
		}
		if ps.Title != "" && ps.Dir != "" {
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
	return pastSessions(harness, dir), nil
}

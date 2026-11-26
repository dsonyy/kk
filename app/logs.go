package main

import (
	"bytes"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	// logMax bounds kk.log; past it the file keeps its newer half.
	logMax = 1 << 20
	// logTail is how many lines a new viewer of the Logs tab gets before the live ones.
	logTail = 2000
)

// logFile is where log.Printf writes in the server: stderr, kk.log in the kk home, and the open Logs tabs.
// Every event is one line, and none carries message contents, payloads or secrets.
type logFile struct {
	mu   sync.Mutex
	path string
	f    *os.File
	size int64
	subs map[chan string]bool
}

var logs *logFile

func openLog(dir string) error {
	path := filepath.Join(dir, "kk.log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	st, _ := f.Stat()
	logs = &logFile{path: path, f: f, size: st.Size(), subs: map[chan string]bool{}}
	log.SetFlags(log.LstdFlags)
	log.SetOutput(logs)
	return nil
}

func (l *logFile) Write(p []byte) (int, error) {
	line := strings.ReplaceAll(strings.TrimRight(string(p), "\n"), "\n", " ") + "\n"
	os.Stderr.WriteString(line)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.size+int64(len(line)) > logMax {
		l.trim()
	}
	n, _ := l.f.WriteString(line)
	l.size += int64(n)
	for ch := range l.subs {
		select {
		case ch <- line[:len(line)-1]:
		default:
		}
	}
	return len(p), nil
}

// trim keeps the newer half of the file. The caller holds l.mu.
func (l *logFile) trim() {
	data, err := os.ReadFile(l.path)
	if err != nil {
		return
	}
	data = data[len(data)-min(len(data), logMax/2):]
	if i := bytes.IndexByte(data, '\n'); i >= 0 {
		data = data[i+1:]
	}
	tmp := l.path + ".tmp"
	if os.WriteFile(tmp, data, 0o600) != nil || os.Rename(tmp, l.path) != nil {
		return
	}
	f, err := os.OpenFile(l.path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	l.f.Close()
	l.f, l.size = f, int64(len(data))
}

// handleLogs streams the last lines of the log and then every new one.
func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	ch := make(chan string, 256)
	logs.mu.Lock()
	data, _ := os.ReadFile(logs.path)
	logs.subs[ch] = true
	logs.mu.Unlock()
	defer func() {
		logs.mu.Lock()
		delete(logs.subs, ch)
		logs.mu.Unlock()
	}()
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	for _, line := range lines[max(0, len(lines)-logTail):] {
		if line != "" {
			fmt.Fprintf(w, "data: %s\n\n", line)
		}
	}
	flusher, _ := w.(http.Flusher)
	flusher.Flush()
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case line := <-ch:
			fmt.Fprintf(w, "data: %s\n\n", line)
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
		}
		flusher.Flush()
	}
}

// handleClientError logs an error the web client ran into.
func (s *Server) handleClientError(r *http.Request) (any, error) {
	var in struct {
		Error string `json:"error"`
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	log.Printf("web client error: %s", clip(strings.TrimSpace(in.Error), 300))
	return map[string]bool{"ok": true}, nil
}

package main

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/dsonyy/kk/web"
)

type Server struct {
	store *Store
	bin   string
	token string

	mu       sync.Mutex
	settings Settings
	tails    map[string]*Tail
	subs     map[chan string]bool
	ticks    int
	// controls holds when a conversation was last stopped, closed, interrupted, reset or resumed.
	controls map[string]time.Time
	watches  map[string]*watch
	notices  map[string]notice
}

type convView struct {
	Conversation
	// Project is the directory the list groups the conversation under.
	Project string   `json:"project"`
	Unread  bool     `json:"unread"`
	Actions []string `json:"actions"`
	Events  []Event  `json:"events"`
	// Queued are the messages the agent holds until it gets to them, for a harness that records its queue.
	Queued []Event `json:"queued"`
}

type apiError struct {
	code int
	msg  string
}

func (e apiError) Error() string { return e.msg }

func fail(code int, format string, args ...any) error {
	return apiError{code, fmt.Sprintf(format, args...)}
}

func (s *Server) routes() *http.ServeMux {
	mux := http.NewServeMux()
	api := func(pattern string, h func(r *http.Request) (any, error)) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			v, err := h(r)
			w.Header().Set("Content-Type", "application/json")
			if err != nil {
				code := http.StatusInternalServerError
				var ae apiError
				if errors.As(err, &ae) {
					code = ae.code
				}
				w.WriteHeader(code)
				json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
				return
			}
			json.NewEncoder(w).Encode(v)
		})
	}
	api("GET /api/conversations", s.handleList)
	api("POST /api/conversations", s.handleCreate)
	api("GET /api/conversations/{id}", s.handleShow)
	api("POST /api/conversations/{id}/send", s.handleSend)
	api("POST /api/conversations/{id}/rewind", s.handleRewind)
	api("POST /api/conversations/{id}/keys", s.handleKeys)
	api("GET /api/conversations/{id}/screen", s.handleScreen)
	api("POST /api/conversations/{id}/seen", s.handleSeen)
	api("POST /api/conversations/{id}/resume", s.handleResume)
	api("POST /api/conversations/{id}/close", s.handleClose)
	api("POST /api/conversations/{id}/interrupt", s.handleInterrupt)
	api("POST /api/conversations/{id}/stop", s.handleStop)
	api("POST /api/conversations/{id}/reset", s.handleReset)
	api("POST /api/hook/{id}/{event}", s.handleHook)
	api("GET /api/harnesses", func(*http.Request) (any, error) { return harnesses(), nil })
	api("GET /api/skills", s.handleSkills)
	api("GET /api/sessions", s.handleSessions)
	api("GET /api/stats", s.handleStats)
	api("GET /api/repo", handleRepo)
	api("GET /api/browse", handleBrowse)
	api("GET /api/dirs", func(*http.Request) (any, error) {
		dirs := s.store.recentDirs()
		if d := s.cfg().DefaultDir; d != "" {
			dirs = append([]string{d}, slices.DeleteFunc(dirs, func(x string) bool { return x == d })...)
		}
		return dirs, nil
	})
	api("POST /api/attachments", s.handleUpload)
	mux.HandleFunc("GET /api/attachments/{id}/{name}", s.handleAttachment)
	api("GET /api/settings", s.handleSettings)
	api("PUT /api/settings", s.handleSaveSettings)
	mux.HandleFunc("GET /api/events", s.handleEvents)
	mux.HandleFunc("GET /api/conversations/{id}/terminal", s.handleTerminal)
	dist, _ := fs.Sub(web.Dist, "dist")
	mux.Handle("GET /", http.FileServerFS(dist))
	return mux
}

// guard protects the TCP listener: a token from the URL becomes a cookie, and writes must come from the same origin.
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if t := r.URL.Query().Get("token"); t != "" && r.URL.Path == "/" {
			if subtle.ConstantTimeCompare([]byte(t), []byte(s.token)) != 1 {
				http.Error(w, "wrong token", http.StatusUnauthorized)
				return
			}
			http.SetCookie(w, &http.Cookie{Name: "kk_token", Value: s.token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: 365 * 24 * 3600})
			http.Redirect(w, r, "/", http.StatusFound)
			return
		}
		c, err := r.Cookie("kk_token")
		if err != nil || subtle.ConstantTimeCompare([]byte(c.Value), []byte(s.token)) != 1 {
			http.Error(w, "open the address with ?token= printed by kk", http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodGet {
			if o := r.Header.Get("Origin"); o != "" {
				if u, err := url.Parse(o); err != nil || u.Host != r.Host {
					http.Error(w, "cross-origin request refused", http.StatusForbidden)
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

func decode(r *http.Request, v any) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, 16<<20))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fail(400, "invalid JSON body: %v", err)
	}
	return nil
}

func (s *Server) conv(r *http.Request) (Conversation, error) {
	c, ok := s.store.get(r.PathValue("id"))
	if !ok {
		return c, fail(404, "no conversation %s", r.PathValue("id"))
	}
	return c, nil
}

func (s *Server) view(c Conversation, events bool) convView {
	v := convView{Conversation: c, Project: c.Project(), Unread: c.Unread(), Actions: actions(c), Events: []Event{}, Queued: []Event{}}
	if events {
		s.mu.Lock()
		if t := s.tails[c.ID]; t != nil && t.path == c.Transcript {
			for _, e := range t.Events {
				v.Events = append(v.Events, s.withAttachments(e))
			}
			for _, e := range t.Queue {
				// A queue dies with the agent's process, and the transcript does not say so. Task notifications wait in it too.
				if c.Status == statusWorking && e.Time.After(c.Launched) && !strings.HasPrefix(e.Text, "<") && e.Text != "" {
					v.Queued = append(v.Queued, s.withAttachments(e))
				}
			}
		}
		s.mu.Unlock()
	}
	return v
}

func (s *Server) handleList(r *http.Request) (any, error) {
	all := r.URL.Query().Get("all") != ""
	out := []convView{}
	for _, c := range s.store.list() {
		if c.Role == "" && (all || !c.Closed) {
			out = append(out, s.view(c, false))
		}
	}
	return out, nil
}

func (s *Server) handleShow(r *http.Request) (any, error) {
	c, err := s.conv(r)
	if err != nil {
		return nil, err
	}
	return s.view(c, true), nil
}

func expandDir(dir string) (string, error) {
	h, _ := os.UserHomeDir()
	if dir == "" || dir == "~" {
		dir = h
	} else if strings.HasPrefix(dir, "~/") {
		dir = filepath.Join(h, dir[2:])
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return "", fail(400, "%s is not a directory", dir)
	}
	return dir, nil
}

func (s *Server) handleCreate(r *http.Request) (any, error) {
	var in struct {
		Harness string `json:"harness"`
		Model   string `json:"model"`
		Dir     string `json:"dir"`
		Prompt  string `json:"prompt"`
		// Session resumes a session from the harness's own history instead of starting a new one.
		Session string `json:"session"`
		// Worktree starts the agent in a new git worktree of dir, on its own branch.
		Worktree bool `json:"worktree"`
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	h, ok := findHarness(in.Harness)
	if !ok {
		return nil, fail(400, "unknown harness %q, use claude, codex or cursor", in.Harness)
	}
	if in.Model == "" {
		in.Model = h.Models[0]
	}
	dir, err := expandDir(in.Dir)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	by := r.Header.Get("X-KK-Conversation")
	if in.Session != "" {
		if in.Worktree {
			return nil, fail(400, "an earlier session resumes in its own directory, not in a new worktree")
		}
		for _, c := range s.store.list() {
			if c.Harness == in.Harness && c.Session == in.Session && !c.Closed {
				return s.view(c, false), nil
			}
		}
		for _, p := range pastSessions(in.Harness, dir) {
			if p.ID != in.Session {
				continue
			}
			c := Conversation{ID: newID(), Harness: in.Harness, Model: in.Model, Dir: p.Dir, Title: p.Title, Session: p.ID, Transcript: p.path, Created: now, Seen: now, StartedBy: by}
			if err := s.launch(&c, true, in.Prompt); err != nil {
				return nil, err
			}
			return s.view(c, false), nil
		}
		return nil, fail(400, "no %s session %s", in.Harness, in.Session)
	}
	if strings.TrimSpace(in.Prompt) == "" {
		return nil, fail(400, "prompt is empty")
	}
	c := Conversation{ID: newID(), Harness: in.Harness, Model: in.Model, Dir: dir, Prompt: in.Prompt, Title: firstLine(in.Prompt, 80), Created: now, Seen: now, StartedBy: by}
	if in.Worktree {
		c.Repo = dir
		if c.Dir, err = addWorktree(dir, worktreeBranch(c.Title, c.ID)); err != nil {
			return nil, err
		}
	}
	if err := s.launch(&c, false, ""); err != nil {
		return nil, err
	}
	return s.view(c, false), nil
}

// launch starts the agent of c. A prompt given with a resume is the first message in the resumed session.
func (s *Server) launch(c *Conversation, resume bool, prompt string) error {
	argv, err := s.command(c, resume)
	if resume && prompt != "" {
		argv = append(argv, "--", prompt)
	}
	if err != nil {
		return err
	}
	h, _ := findHarness(c.Harness)
	c.Status = statusStarting
	if !h.Hooks {
		c.Status = statusWorking
	}
	if resume && controls[c.Harness].blind {
		c.Status = statusIdle
	}
	c.Attention = ""
	c.Closed = false
	c.Launched = time.Now()
	// Saved before the agent starts, because its first hook can arrive within milliseconds.
	if err := s.store.put(*c); err != nil {
		return err
	}
	env := map[string]string{"KK_CONVERSATION": c.ID, "KK_HOME": s.store.dir}
	alive, _ := windows()
	if _, ok := alive[c.Window]; ok && c.Window != "" {
		err = respawnWindow(c.Window, c.Dir, env, argv)
	} else {
		var window string
		window, err = openWindow(c.ID, c.Dir, env, argv)
		*c, _ = s.store.update(c.ID, func(c *Conversation) bool {
			c.Window = window
			return true
		})
	}
	if err != nil {
		s.store.update(c.ID, func(c *Conversation) bool {
			c.Status = statusExited
			return true
		})
		return err
	}
	s.publish(c.ID)
	return nil
}

func (s *Server) alive(c Conversation) bool {
	alive, _ := windows()
	return c.Window != "" && alive[c.Window]
}

func (s *Server) handleSend(r *http.Request) (any, error) {
	c, err := s.conv(r)
	if err != nil {
		return nil, err
	}
	var in struct {
		Text string `json:"text"`
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	if strings.TrimSpace(in.Text) == "" {
		return nil, fail(400, "text is empty")
	}
	if !s.alive(c) {
		return nil, fail(409, "the agent is not running, resume it first")
	}
	if err := midRewind(c.ID); err != nil {
		return nil, err
	}
	if err := pasteText(c.Window, in.Text); err != nil {
		return nil, err
	}
	s.markSeen(c.ID)
	return map[string]bool{"ok": true}, nil
}

func (s *Server) handleKeys(r *http.Request) (any, error) {
	c, err := s.conv(r)
	if err != nil {
		return nil, err
	}
	var in struct {
		Keys []string `json:"keys"`
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	if !s.alive(c) {
		return nil, fail(409, "the agent is not running")
	}
	if err := midRewind(c.ID); err != nil {
		return nil, err
	}
	if err := sendKeys(c.Window, in.Keys); err != nil {
		return nil, fail(400, "%v", err)
	}
	return map[string]bool{"ok": true}, nil
}

func (s *Server) handleScreen(r *http.Request) (any, error) {
	c, err := s.conv(r)
	if err != nil {
		return nil, err
	}
	alive, _ := windows()
	if _, ok := alive[c.Window]; !ok || c.Window == "" {
		return map[string]string{"screen": ""}, nil
	}
	screen, err := capture(c.Window)
	if err != nil {
		return nil, err
	}
	return map[string]string{"screen": screen}, nil
}

func (s *Server) markSeen(id string) {
	if _, changed := s.store.update(id, func(c *Conversation) bool {
		if !c.Unread() {
			return false
		}
		c.Seen = time.Now()
		return true
	}); changed {
		s.publish(id)
	}
}

func (s *Server) handleSeen(r *http.Request) (any, error) {
	c, err := s.conv(r)
	if err != nil {
		return nil, err
	}
	s.markSeen(c.ID)
	return map[string]bool{"ok": true}, nil
}

func (s *Server) handleResume(r *http.Request) (any, error) {
	c, err := s.conv(r)
	if err != nil {
		return nil, err
	}
	if s.alive(c) {
		return nil, fail(409, "the agent is already running")
	}
	s.controlled(c.ID)
	if err := s.launch(&c, s.canResume(c), ""); err != nil {
		return nil, err
	}
	return s.view(c, false), nil
}

func (s *Server) canResume(c Conversation) bool {
	if c.Harness == "cursor" {
		return true
	}
	if c.Session == "" || c.Transcript == "" {
		return false
	}
	_, err := os.Stat(c.Transcript)
	return err == nil
}

func (s *Server) handleClose(r *http.Request) (any, error) {
	c, err := s.conv(r)
	if err != nil {
		return nil, err
	}
	if c.Role != "" {
		return nil, fail(400, "the coordinator cannot be closed")
	}
	s.controlled(c.ID)
	if c.Window != "" {
		killWindow(c.Window)
	}
	s.store.update(c.ID, func(c *Conversation) bool {
		c.Closed, c.Status, c.Attention = true, statusExited, ""
		return true
	})
	s.publish(c.ID)
	return map[string]bool{"ok": true}, nil
}

// control returns the conversation of r when the action applies to it now.
func (s *Server) control(r *http.Request, action string) (Conversation, error) {
	c, err := s.conv(r)
	if err != nil {
		return c, err
	}
	if !slices.Contains(actions(c), action) {
		return c, fail(409, "cannot %s a %s agent that is %s", action, c.Harness, c.Status)
	}
	s.controlled(c.ID)
	return c, midRewind(c.ID)
}

func (s *Server) handleInterrupt(r *http.Request) (any, error) {
	c, err := s.control(r, "interrupt")
	if err != nil {
		return nil, err
	}
	if err := sendKeys(c.Window, []string{controls[c.Harness].interrupt}); err != nil {
		return nil, err
	}
	// Claude Code leaves no trace of a turn that is interrupted before it answered, so the turn is ended here.
	// A hook from a turn that went on sets it back to working.
	s.store.update(c.ID, func(c *Conversation) bool {
		c.Status, c.Attention, c.TurnEnded = statusIdle, "", time.Now()
		return true
	})
	s.publish(c.ID)
	return map[string]bool{"ok": true}, nil
}

func (s *Server) handleStop(r *http.Request) (any, error) {
	c, err := s.control(r, "stop")
	if err != nil {
		return nil, err
	}
	if err := hangUp(c.Window); err != nil {
		return nil, err
	}
	s.store.update(c.ID, func(c *Conversation) bool {
		c.Status, c.Attention = statusExited, ""
		return true
	})
	s.publish(c.ID)
	return map[string]bool{"ok": true}, nil
}

func (s *Server) handleReset(r *http.Request) (any, error) {
	c, err := s.control(r, "reset")
	if err != nil {
		return nil, err
	}
	// Reset drives the terminal like a rewind does, so it takes the same turn at it.
	if _, busy := rewinding.LoadOrStore(c.ID, true); busy {
		return nil, midRewind(c.ID)
	}
	defer rewinding.Delete(c.ID)
	// The old session is let go before the command is typed: Codex reports the new one only with its first turn.
	c, _ = s.store.update(c.ID, func(c *Conversation) bool {
		c.Session, c.Transcript, c.Prompt, c.Context = "", "", "", 0
		return true
	})
	command := controls[c.Harness].reset
	if command == "" {
		return map[string]bool{"ok": true}, s.launch(&c, false, "")
	}
	// A draft left in the input, such as the prompt an interrupt puts back, would be sent together with the command.
	if _, err := tmux("", "send-keys", "-N", "50", "-t", c.Window, "C-u"); err != nil {
		return nil, err
	}
	if err := pasteText(c.Window, command); err != nil {
		return nil, err
	}
	s.publish(c.ID)
	return map[string]bool{"ok": true}, nil
}

func (s *Server) handleHook(r *http.Request) (any, error) {
	id, event := r.PathValue("id"), r.PathValue("event")
	var p struct {
		SessionID        string `json:"session_id"`
		TranscriptPath   string `json:"transcript_path"`
		Message          string `json:"message"`
		NotificationType string `json:"notification_type"`
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 16<<20))
	json.Unmarshal(body, &p)
	now := time.Now()
	_, changed := s.store.update(id, func(c *Conversation) bool {
		if c.Status == statusExited {
			return false
		}
		// The harness moves to a new session when the user starts the conversation over, for example with /clear.
		switched := p.SessionID != "" && c.Session != "" && p.SessionID != c.Session
		if p.SessionID != "" {
			c.Session = p.SessionID
		}
		if p.TranscriptPath != "" {
			c.Transcript = p.TranscriptPath
		} else if c.Harness == "cursor" && c.Session != "" {
			c.Transcript = cursorTranscript(c.Dir, c.Session)
		}
		if switched {
			// The first prompt belongs to the session that was left and must not be sent again on a restart.
			c.Prompt, c.Context = "", 0
		}
		c.Activity = now
		switch event {
		case "SessionStart":
			if c.Status == statusStarting || switched {
				c.Status = statusIdle
			}
			c.Attention = ""
		case "UserPromptSubmit":
			c.Status, c.Attention, c.Compacted = statusWorking, "", false
		case "PreToolUse", "PostToolUse":
			c.Status, c.Attention = statusWorking, ""
		case "Stop":
			c.Status, c.Attention, c.TurnEnded = statusIdle, "", now
		case "Notification":
			if p.NotificationType == "idle_prompt" {
				return true
			}
			c.Attention = p.Message
			if c.Attention == "" {
				c.Attention = "The agent needs your input"
			}
		case "PermissionRequest":
			c.Attention = "The agent asks for permission"
		}
		return true
	})
	if changed {
		s.publish(id)
	}
	return map[string]bool{"ok": true}, nil
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	ch := make(chan string, 64)
	s.mu.Lock()
	s.subs[ch] = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.subs, ch)
		s.mu.Unlock()
	}()
	flusher, _ := w.(http.Flusher)
	fmt.Fprint(w, ": hello\n\n")
	flusher.Flush()
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case id := <-ch:
			fmt.Fprintf(w, "data: %s\n\n", id)
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
		}
		flusher.Flush()
	}
}

func (s *Server) publish(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for ch := range s.subs {
		select {
		case ch <- id:
		default:
		}
	}
}

// tick reconciles conversations with tmux and their transcripts.
func (s *Server) tick() {
	alive, err := windows()
	if err != nil {
		log.Print(err)
		return
	}
	s.ticks++
	now := time.Now()
	startTimeout := s.cfg().startTimeout()
	notify := s.cfg().coordinator()
	for _, c := range s.store.list() {
		if c.Closed {
			continue
		}
		events := s.readTail(c)
		_, changed := s.store.update(c.ID, func(c *Conversation) bool {
			changed := false
			// The list of windows may predate a launch that has just revived this one.
			if c.Status != statusExited && c.Window != "" && !alive[c.Window] && now.Sub(c.Launched) > time.Second {
				c.Status, c.Attention, changed = statusExited, "", true
			}
			if c.Status == statusStarting && c.Attention == "" && now.Sub(c.Launched) > startTimeout {
				c.Attention, changed = fmt.Sprintf("The agent has not started its session after %s and may be waiting for input", startTimeout), true
			}
			if t := events; t != nil {
				if t.Title != "" && t.Title != c.Title {
					c.Title, changed = t.Title, true
					tmux("", "rename-window", "-t", c.Window, firstLine(t.Title, 40))
				}
				if t.Last.After(c.LastMessage) {
					c.LastMessage, changed = t.Last, true
				}
				if t.Context != c.Context {
					c.Context, changed = t.Context, true
				}
				// A turn end from before this launch is history, read again after a resume.
				if t.Ended.After(c.TurnEnded) && t.Ended.After(c.Launched) && c.Status != statusExited {
					c.Status, c.TurnEnded, changed = statusIdle, t.Ended, true
					if t.Failed != "" {
						c.Attention = "The turn ended on an API error. Retry it or send a message to continue."
					}
				}
				if t.Waiting && c.Status == statusIdle {
					c.Status, changed = statusWorking, true
				}
			}
			if c.Harness == "codex" && c.Session != "" && s.ticks%10 == 0 {
				if name := codexThreadName(c.Session); name != "" && name != c.Title {
					c.Title, changed = name, true
					tmux("", "rename-window", "-t", c.Window, firstLine(name, 40))
				}
			}
			return changed
		})
		if changed || events != nil {
			s.publish(c.ID)
		}
		if notify {
			cur, _ := s.store.get(c.ID)
			s.observe(cur, events != nil, now)
		}
		if c.Role == coordinatorID {
			s.maybeCompact(c, now)
		}
	}
	s.flushNotices(now)
}

// readTail returns the tail of c's transcript when it has new lines.
func (s *Server) readTail(c Conversation) *Tail {
	parse := parserFor(c.Harness)
	if c.Transcript == "" || parse == nil {
		return nil
	}
	s.mu.Lock()
	t := s.tails[c.ID]
	if t == nil || t.path != c.Transcript {
		t = newTail(c.Transcript)
		s.tails[c.ID] = t
	}
	s.mu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	changed, _ := t.read(parse)
	if !changed {
		return nil
	}
	cp := *t
	return &cp
}

func (s *Server) maybeCompact(c Conversation, now time.Time) {
	cfg := s.cfg()
	if midRewind(c.ID) != nil || c.Status != statusIdle || c.Compacted || c.Context < cfg.compactTokens() || now.Sub(lastActive(c)) < cfg.compactAfter() {
		return
	}
	if err := pasteText(c.Window, "/compact"); err != nil {
		log.Printf("compact coordinator: %v", err)
		return
	}
	log.Printf("coordinator compacted at %d tokens", c.Context)
	s.store.update(c.ID, func(c *Conversation) bool {
		c.Compacted = true
		return true
	})
	s.publish(c.ID)
}

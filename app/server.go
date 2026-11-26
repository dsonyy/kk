package main

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/dsonyy/kk/web"
)

type Server struct {
	store *Store
	inbox *Inbox
	bin   string
	token string

	mu       sync.Mutex
	settings Settings
	tails    map[string]*Tail
	// subagents holds the tails of the transcripts of agents' subagents, by path.
	subagents map[string]*Tail
	grouping  chan string
	subs      map[chan string]bool
	// asked counts, per conversation without a group, the user messages the last question about its group covered.
	asked map[string]int
	ticks int
	// ticking runs one tick at a time: the Stop hook runs one of its own.
	ticking sync.Mutex
	// controls holds when a conversation was last stopped, closed or interrupted.
	controls map[string]time.Time
	// raises counts per conversation how often a hook raised attention, which may clear and come back between two ticks.
	raises  map[string]int
	watches map[string]*watch
	notices map[string]notice
	// handovers holds the conversations handed to a konduktor, until it hears of them, with who handed them over: the
	// user, or the konduktor that gave it away.
	handovers map[string]string
	// opened holds when the latest launch of each conversation had its window, or the zero time while tmux still opens it,
	// which can take seconds on a busy machine.
	opened sync.Map
	// starting runs one start of a konduktor at a time.
	starting sync.Mutex
	// roster holds, per konduktor, the lines about other konduktors that started or closed, until it hears of them.
	roster map[string][]string
	// answers holds the answer of the current run of each integration, by conversation.
	answers map[string]runAnswer
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
	// WaitingFor is who has to act on an agent that stopped: "user" or "conductor". Empty while the agent works.
	WaitingFor string `json:"waiting_for"`
	// Konduktor is the konduktor the session belongs to. Empty for konduktors, kk's own inbox sessions and with konduktors off.
	Konduktor string `json:"konduktor,omitempty"`
	// InWorktree says the agent works in a git worktree, apart from the main checkout.
	InWorktree bool `json:"in_worktree"`
	// RunDone marks a scheduled run that finished and waits for the user to see it.
	RunDone bool      `json:"run_done"`
	NextRun time.Time `json:"next_run,omitzero"`
	// Usage covers every transcript of the conversation. It is nil for a harness that does not report usage.
	Usage *Usage `json:"usage"`
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
				// A read of something missing is not an event.
				if r.Method != http.MethodGet || code >= 500 {
					log.Printf("%s %s by %s failed (%d): %v", r.Method, r.URL.Path, who(r), code, err)
				}
				w.WriteHeader(code)
				json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
				return
			}
			if logged(r) {
				log.Printf("%s %s by %s", r.Method, r.URL.Path, who(r))
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
	api("POST /api/conversations/{id}/escalate", s.handleEscalate)
	api("POST /api/conversations/{id}/group", s.handleSetGroup)
	api("PUT /api/conversations/{id}/schedule", s.handleSchedule)
	api("DELETE /api/conversations/{id}/schedule", s.handleUnschedule)
	api("POST /api/conversations/{id}/start", s.handleStart)
	api("POST /api/conversations/{id}/prompt", s.handlePrompt)
	api("POST /api/conversations/{id}/handover", s.handleHandover)
	api("GET /api/konduktors", s.handleKonduktors)
	api("POST /api/konduktors", s.handleStartKonduktor)
	api("GET /api/groups", s.handleGroups)
	api("POST /api/hook/{id}/{event}", s.handleHook)
	api("GET /api/harnesses", func(*http.Request) (any, error) { return harnesses(), nil })
	api("GET /api/skills", s.handleSkills)
	api("GET /api/sessions", s.handleSessions)
	api("GET /api/stats", s.handleStats)
	api("GET /api/activity", s.handleActivity)
	api("GET /api/repo", handleRepo)
	api("GET /api/browse", handleBrowse)
	api("POST /api/pick", handlePick)
	api("GET /api/dirs", func(*http.Request) (any, error) {
		dirs := s.store.recentDirs()
		if d := s.cfg().DefaultDir; d != "" {
			dirs = append([]string{d}, slices.DeleteFunc(dirs, func(x string) bool { return x == d })...)
		}
		return dirs, nil
	})
	api("POST /api/attachments", s.handleUpload)
	mux.HandleFunc("GET /api/attachments/{id}/{name}", s.handleAttachment)
	mux.HandleFunc("GET /api/conversations/{id}/image", s.handleImage)
	api("GET /api/settings", s.handleSettings)
	api("PUT /api/settings", s.handleSaveSettings)
	api("GET /api/inbox", s.handleInbox)
	api("POST /api/inbox/refresh", s.handleInboxRefresh)
	api("POST /api/inbox/tick", func(*http.Request) (any, error) { s.inbox.tick(time.Now()); return s.inbox.view(), nil })
	api("POST /api/inbox/status", s.handleInboxStatus)
	api("GET /api/inbox/linear-key", func(*http.Request) (any, error) {
		return map[string]bool{"set": linearSource{s.store.dir}.key() != ""}, nil
	})
	api("PUT /api/inbox/linear-key", s.handleLinearKey)
	mux.HandleFunc("GET /api/events", s.handleEvents)
	mux.HandleFunc("GET /api/conversations/{id}/terminal", s.handleTerminal)
	mux.HandleFunc("GET /api/tmux", s.handleTmux)
	mux.HandleFunc("GET /api/logs", s.handleLogs)
	api("POST /api/logs", s.handleClientError)
	dist, _ := fs.Sub(web.Dist, "dist")
	files := http.FileServerFS(dist)
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		// The page names its scripts by content hash, so a page that is always fetched again never runs an old build.
		if r.URL.Path == "/" {
			w.Header().Set("Cache-Control", "no-cache")
		}
		files.ServeHTTP(w, r)
	})
	return mux
}

// logged reports whether a successful request is an action for the log: reads, hooks, client errors and the inbox
// polling log what happens on their own.
func logged(r *http.Request) bool {
	return r.Method != http.MethodGet && !strings.HasPrefix(r.URL.Path, "/api/hook/") && r.URL.Path != "/api/logs" && r.URL.Path != "/api/inbox/tick"
}

// who names who made a request for the log.
func who(r *http.Request) string {
	if byUser(r) {
		return "user"
	}
	return "agent " + clip(r.Header.Get(agentHeader), 40)
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
	v := convView{Conversation: c, Konduktor: s.konduktorOf(c, s.konduktors()), Project: c.Project(), InWorktree: inWorktree(c), Unread: c.Unread(), WaitingFor: c.WaitingFor(s.cfg().coordinator()), RunDone: c.RunDone(), Actions: actions(c), Events: []Event{}, Queued: []Event{}}
	if c.Schedule != nil {
		v.NextRun = c.Schedule.next(time.Now())
	}
	if usageParser[c.Harness] != nil {
		u := c.TotalActivity().Usage
		v.Usage = &u
	}
	if events {
		s.mu.Lock()
		if t := s.tails[c.ID]; t != nil && t.path == c.Transcript {
			for _, e := range t.Events {
				v.Events = append(v.Events, withImages(c.ID, s.withAttachments(e)))
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
	if c.Closed {
		// The tick leaves closed conversations alone, so their transcript is read when someone opens them.
		s.readTail(c)
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
		// Group puts the conversation in a feature group. Without it kk decides from the prompt.
		Group string `json:"group"`
		// Konduktor hands a conversation the user starts to the konduktor, as if the konduktor had started it.
		Konduktor bool `json:"conductor"`
		// After queues the conversation behind another one. It waits with its prompt until kk start.
		After string `json:"after"`
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
	if in.Group != "" && cleanGroup(in.Group) == "" {
		return nil, fail(400, "a group name is 1 to %d characters and not %q", groupMaxLen, ungrouped)
	}
	now := time.Now()
	byAgent := !byUser(r) || in.Konduktor
	if in.Session != "" {
		if in.After != "" {
			return nil, fail(400, "an earlier session resumes now and cannot wait for another")
		}
		if in.Worktree {
			return nil, fail(400, "an earlier session resumes in its own directory, not in a new worktree")
		}
		if s.integrationSessions()[in.Session] {
			return nil, fail(400, "session %s checks the inbox for kk and resumes only from its own panel", in.Session)
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
			c := Conversation{ID: newID(), Harness: in.Harness, Model: in.Model, Dir: p.Dir, Title: p.Title, Session: p.ID, Transcript: p.path, Created: now, Seen: now, ByAgent: byAgent}
			c.Group = s.groupName(c.Project(), in.Group)
			s.own(&c, r, in.Konduktor)
			if err := s.launch(&c, true, in.Prompt); err != nil {
				return nil, err
			}
			if byUser(r) && in.Konduktor {
				s.handOver(c.ID, waitingUser)
			}
			s.queueGroup(c.ID)
			return s.view(c, false), nil
		}
		return nil, fail(400, "no %s session %s", in.Harness, in.Session)
	}
	if strings.TrimSpace(in.Prompt) == "" {
		return nil, fail(400, "prompt is empty")
	}
	c := Conversation{ID: newID(), Harness: in.Harness, Model: in.Model, Dir: dir, Prompt: in.Prompt, Title: firstLine(in.Prompt, 80), Created: now, Seen: now, ByAgent: byAgent}
	if in.After != "" {
		c.Group = s.groupName(c.Project(), in.Group)
		s.own(&c, r, in.Konduktor)
		c, err := s.queue(c, in.After, in.Worktree)
		if err != nil {
			return nil, err
		}
		return s.view(c, false), nil
	}
	if in.Worktree {
		c.Repo = dir
		if c.Dir, err = addWorktree(dir, worktreeBranch(c.Title, c.ID)); err != nil {
			return nil, err
		}
	}
	c.Group = s.groupName(c.Project(), in.Group)
	s.own(&c, r, in.Konduktor)
	if err := s.launch(&c, false, ""); err != nil {
		return nil, err
	}
	if byUser(r) && in.Konduktor {
		s.handOver(c.ID, waitingUser)
	}
	s.queueGroup(c.ID)
	return s.view(c, false), nil
}

// own records the konduktor a new conversation belongs to: the one behind the agent that starts it, or for one the user
// hands over, the konduktor of its directory, which starts when it does not run.
func (s *Server) own(c *Conversation, r *http.Request, handover bool) {
	if !byUser(r) {
		c.Owner = s.callerKonduktor(r)
		return
	}
	if !handover || !s.cfg().coordinator() {
		return
	}
	k, err := s.konduktorFor(c.Project(), "")
	if err != nil {
		log.Printf("konduktor of %s: %v", c.ID, err)
		return
	}
	c.Owner = k.ID
}

// callerKonduktor names the konduktor behind an agent's request: the agent itself when it is one, else the one it belongs to.
func (s *Server) callerKonduktor(r *http.Request) string {
	caller, ok := s.store.get(r.Header.Get(agentHeader))
	if !ok {
		return ""
	}
	if caller.Role == coordinatorID {
		return caller.ID
	}
	return s.konduktorOf(caller, s.konduktors())
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
	c.Attention, c.Sleeping = "", false
	c.Closed, c.ClosedAt = false, time.Time{}
	c.Launched = time.Now()
	// Saved before the agent starts, because its first hook can arrive within milliseconds.
	if err := s.store.put(*c); err != nil {
		return err
	}
	env := map[string]string{"KK_CONVERSATION": c.ID, "KK_HOME": s.store.dir}
	if c.Integration != "" {
		// A claude.ai connector gets this long to connect; the agent waits for it with ToolSearch.
		env["MCP_CONNECT_TIMEOUT_MS"] = "30000"
	}
	s.opened.Store(c.ID, time.Time{})
	defer s.opened.Store(c.ID, time.Now())
	alive, _ := windows()
	if _, ok := alive[c.Window]; ok && c.Window != "" {
		err = respawnWindow(c.Window, c.Dir, env, argv)
	} else {
		var window string
		window, err = openWindow(c.ID, c.Dir, env, argv)
		title := c.Title
		*c, _ = s.store.update(c.ID, func(c *Conversation) bool {
			c.Window = window
			if c.Title != title {
				nameWindow(*c)
			}
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

// nameWindow gives the window of a conversation its title. The agent can write its title before tmux reports the id
// of a new window, and renaming no window would rename the one the user is in.
func nameWindow(c Conversation) {
	if c.Window != "" {
		tmux("", "rename-window", "-t", c.Window, firstLine(c.Title, 40))
	}
}

// windowSettled tells whether the window of the latest launch has been open long enough that the list of windows taken at
// now shows it.
func (s *Server) windowSettled(id string, now time.Time) bool {
	at, ok := s.opened.Load(id)
	return !ok || !at.(time.Time).IsZero() && now.Sub(at.(time.Time)) > time.Second
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
	if c.Status == statusQueued {
		return nil, fail(409, "the session is queued, start it first")
	}
	if !s.alive(c) {
		return nil, fail(409, "the agent is not running, resume it first")
	}
	if err := midRewind(c.ID); err != nil {
		return nil, err
	}
	// A message from an agent to a konduktor says where it comes from, and leaves the user's unread mark alone.
	peer := c.Role != "" && !byUser(r)
	if peer {
		if c.Attention != "" {
			return nil, fail(409, "konduktor %s is in a dialog, try again in a moment", c.ID)
		}
		from := "agent " + r.Header.Get(agentHeader)
		if k, ok := s.store.get(r.Header.Get(agentHeader)); ok && k.Role != "" {
			from = fmt.Sprintf("konduktor %q (%s)", konduktorName(k), k.ID)
		}
		in.Text = fmt.Sprintf("[from %s via kk send - not from the user]\n%s", from, in.Text)
	}
	if err := pasteText(c.Window, in.Text); err != nil {
		return nil, err
	}
	if !peer {
		s.driven(c.ID, r)
		s.markSeen(c.ID)
	}
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
	s.driven(c.ID, r)
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

// byUser reports whether the user made the request, from the web app or their own shell, rather than an agent through the kk CLI.
func byUser(r *http.Request) bool {
	return r.Header.Get(agentHeader) == ""
}

// driven records who acted on the conversation last, so its next stop waits for them. A session without a konduktor
// becomes the one of the agent that takes it over.
func (s *Server) driven(id string, r *http.Request) {
	agent := !byUser(r)
	owner := ""
	if agent {
		owner = s.callerKonduktor(r)
	}
	if _, changed := s.store.update(id, func(c *Conversation) bool {
		if c.ByAgent == agent && (c.Owner != "" || owner == "") {
			return false
		}
		c.ByAgent = agent
		if c.Owner == "" {
			c.Owner = owner
		}
		return true
	}); changed {
		s.publish(id)
	}
}

// handleEscalate hands a waiting agent from the konduktor to the user, with the reason the user sees.
func (s *Server) handleEscalate(r *http.Request) (any, error) {
	c, err := s.conv(r)
	if err != nil {
		return nil, err
	}
	var in struct {
		Reason string `json:"reason"`
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	reason := firstLine(in.Reason, 200)
	if reason == "" {
		return nil, fail(400, "reason is empty")
	}
	if c.Role != "" {
		return nil, fail(400, "the konduktor waits for the user already")
	}
	if !c.Waiting() {
		return nil, fail(409, "the agent is %s, escalate it once it stops", c.Status)
	}
	s.store.update(c.ID, func(c *Conversation) bool {
		c.Escalation = reason
		return true
	})
	s.publish(c.ID)
	return map[string]bool{"ok": true}, nil
}

// handleHandover hands a session to a konduktor, which hears of it and drives it from then on.
func (s *Server) handleHandover(r *http.Request) (any, error) {
	c, err := s.conv(r)
	if err != nil {
		return nil, err
	}
	var in struct {
		Konduktor string `json:"konduktor"`
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	if c.Role != "" || c.Integration != "" {
		return nil, fail(400, "only a session can be handed over")
	}
	k, ok := s.store.get(in.Konduktor)
	if !ok || k.Role != coordinatorID || k.Closed {
		return nil, fail(400, "%q is not a running konduktor, kk ls lists them", in.Konduktor)
	}
	c, _ = s.store.update(c.ID, func(c *Conversation) bool {
		c.Owner, c.ByAgent, c.Escalation = k.ID, true, ""
		return true
	})
	from := s.callerKonduktor(r)
	if byUser(r) {
		from = waitingUser
	}
	s.handOver(c.ID, from)
	s.publish(c.ID)
	return s.view(c, false), nil
}

// handleKonduktors lists the running konduktors, the main one first.
func (s *Server) handleKonduktors(r *http.Request) (any, error) {
	out := []convView{}
	for _, k := range s.konduktors() {
		out = append(out, s.view(k, false))
	}
	return out, nil
}

// handleStartKonduktor finds the konduktor of a directory and gives it the prompt, starting or reopening it when it does not run.
func (s *Server) handleStartKonduktor(r *http.Request) (any, error) {
	var in struct {
		Dir    string `json:"dir"`
		Prompt string `json:"prompt"`
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	dir := ""
	if in.Dir != "" {
		var err error
		if dir, err = expandDir(in.Dir); err != nil {
			return nil, err
		}
	}
	k, err := s.konduktorFor(dir, in.Prompt)
	if err != nil {
		return nil, err
	}
	return s.view(k, false), nil
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
	if c.Integration != "" && c.Closed {
		return nil, fail(409, "switch %s on in Settings, its next check opens the session", c.Integration)
	}
	if c.Status == statusQueued {
		if !c.Closed {
			return nil, fail(409, "the session is queued, start it instead")
		}
		c, _ = s.store.update(c.ID, func(c *Conversation) bool {
			c.Closed, c.ClosedAt = false, time.Time{}
			return true
		})
		s.publish(c.ID)
		return s.view(c, false), nil
	}
	if c.Role != "" {
		k, err := s.konduktorFor(c.Scope, "")
		if err != nil {
			return nil, err
		}
		return s.view(k, false), nil
	}
	s.driven(c.ID, r)
	c, _ = s.store.get(c.ID)
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
	if c.ID == coordinatorID {
		return nil, fail(400, "the main konduktor cannot be closed")
	}
	if c.Integration != "" {
		return nil, fail(400, "switch %s off in Settings to stop its checks", c.Integration)
	}
	s.controlled(c.ID)
	if c.Window != "" {
		killWindow(c.Window)
	}
	s.store.update(c.ID, func(c *Conversation) bool {
		c.Closed, c.ClosedAt, c.Attention = true, time.Now(), ""
		if c.Status != statusQueued {
			c.Status = statusExited
		}
		c.Sleeping, c.Schedule = false, nil
		return true
	})
	s.closeNotice(c.ID)
	if c.Role != "" {
		s.writePrompts()
		s.tellKonduktors(c, fmt.Sprintf("konduktor %s %q for %s was closed; its sessions go to the konduktor of their directory or to the main konduktor.", c.ID, konduktorName(c), c.Scope))
		s.mu.Lock()
		delete(s.roster, c.ID)
		s.mu.Unlock()
	}
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
	s.driven(c.ID, r)
	return c, midRewind(c.ID)
}

func (s *Server) handleInterrupt(r *http.Request) (any, error) {
	c, err := s.control(r, "interrupt")
	if err != nil {
		return nil, err
	}
	s.controlled(c.ID)
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
	s.controlled(c.ID)
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
		LastMessage      string `json:"last_assistant_message"`
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 16<<20))
	json.Unmarshal(body, &p)
	log.Printf("hook %s %s", event, id)
	now := time.Now()
	open := 0
	if event == "Stop" {
		// The tasks the turn leaves running are in the transcript lines the last tick may not have read yet.
		s.tick()
		if cur, ok := s.store.get(id); ok && (p.TranscriptPath == "" || p.TranscriptPath == cur.Transcript) {
			open, _ = s.background(cur)
			s.keepAnswer(cur, p.SessionID, p.LastMessage)
		}
	}
	raised := false
	_, changed := s.store.update(id, func(c *Conversation) bool {
		defer func(was string) { raised = was == "" && c.Attention != "" }(c.Attention)
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
			c.Status, c.Attention = statusIdle, ""
			if open > 0 {
				c.Status = statusBackground
			} else {
				c.TurnEnded = now
			}
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
	if raised {
		s.mu.Lock()
		s.raises[id]++
		s.mu.Unlock()
	}
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
	s.ticking.Lock()
	defer s.ticking.Unlock()
	alive, err := windows()
	if err != nil {
		log.Print(err)
		return
	}
	s.ticks++
	now := time.Now()
	startTimeout := s.cfg().startTimeout()
	notify := s.cfg().coordinator()
	queued := s.queued()
	for _, c := range s.store.list() {
		if c.Closed {
			continue
		}
		events := s.readTail(c)
		activity := s.activity(c)
		open, settled := s.background(c)
		_, changed := s.store.update(c.ID, func(c *Conversation) bool {
			changed := false
			// The list of windows may predate a launch that has just revived this one.
			if c.Status != statusExited && c.Window != "" && !alive[c.Window] && now.Sub(c.Launched) > time.Second && s.windowSettled(c.ID, now) {
				c.Status, c.Attention, changed = statusExited, "", true
			}
			// When the last task ends, the harness starts a turn of its own, which its hooks report. The wait ends without
			// that turn too, and after a timeout from the last hook when the transcript never records the end of a task.
			if c.Status == statusBackground && (open == 0 && now.Sub(settled) > backgroundGrace || now.Sub(c.Activity) > backgroundTimeout()) {
				c.Status, c.TurnEnded, changed = statusIdle, now, true
			}
			if c.Status == statusStarting && c.Attention == "" && now.Sub(c.Launched) > startTimeout {
				c.Attention, changed = fmt.Sprintf("The agent has not started its session after %s and may be waiting for input", startTimeout), true
			}
			if t := events; t != nil {
				// An integration keeps its name: a title made up from its transcript can repeat the mail it read. A konduktor
				// keeps the name of its directory.
				if t.Title != "" && t.Title != c.Title && c.Integration == "" && c.Role == "" {
					c.Title, changed = t.Title, true
					nameWindow(*c)
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
			for path, a := range activity {
				if !reflect.DeepEqual(c.Transcripts[path], a) {
					// The stored conversation shares the map with this copy.
					c.Transcripts = maps.Clone(c.Transcripts)
					if c.Transcripts == nil {
						c.Transcripts = map[string]Activity{}
					}
					c.Transcripts[path], changed = a, true
				}
			}
			if c.Harness == "codex" && c.Session != "" && s.ticks%10 == 0 {
				if name := codexThreadName(c.Session); name != "" && name != c.Title {
					c.Title, changed = name, true
					nameWindow(*c)
				}
			}
			return changed
		})
		if changed || events != nil {
			s.publish(c.ID)
		}
		// A group that could not be decided is asked for again when the user has said more.
		if events != nil && c.Role == "" && c.Group == "" {
			if n := len(userTexts(events.Events)); n > s.asked[c.ID] {
				s.asked[c.ID] = n
				s.queueGroup(c.ID)
			}
		}
		if notify {
			cur, _ := s.store.get(c.ID)
			s.observe(cur, events != nil, len(queued[c.ID]) > 0, now)
		}
		if c.Role == coordinatorID {
			s.maybeCompact(c, now)
		}
	}
	s.lifecycle(now)
	s.flushNotices(now)
}

// readTail returns the tail of c's transcript when it has new lines.
func (s *Server) readTail(c Conversation) *Tail {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.tail(c)
	if t == nil {
		return nil
	}
	if changed, _ := t.read(parserFor(c.Harness)); !changed {
		return nil
	}
	cp := *t
	return &cp
}

// activity returns the activity of c's current transcript and of the transcripts of its subagents, by file. It counts
// the lines of the transcript that were read.
func (s *Server) activity(c Conversation) map[string]Activity {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.tail(c)
	if t == nil {
		return nil
	}
	out := map[string]Activity{c.Transcript: t.Activity.clone()}
	if list := subagents[c.Harness]; list != nil {
		for _, p := range list(c) {
			sub := s.subagents[p]
			if sub == nil {
				sub = newTail(p)
				s.subagents[p] = sub
			}
			sub.read(usageParser[c.Harness])
			out[p] = sub.Activity.clone()
		}
	}
	return out
}

// tail returns the tail of c's transcript, or nil when kk cannot read it. The caller holds s.mu.
func (s *Server) tail(c Conversation) *Tail {
	if c.Transcript == "" || parserFor(c.Harness) == nil {
		return nil
	}
	t := s.tails[c.ID]
	if t == nil || t.path != c.Transcript {
		t = newTail(c.Transcript)
		s.tails[c.ID] = t
	}
	return t
}

const (
	// backgroundGrace is how long after the end of its last background task an agent may take to start the turn its harness gives it.
	backgroundGrace = 5 * time.Second
)

// backgroundTimeout is how long an agent may wait for background tasks without a hook.
func backgroundTimeout() time.Duration { return envDuration("KK_BACKGROUND_TIMEOUT", 30*time.Minute) }

// background counts the background tasks the agent of c started since its launch that have not ended, as far as the
// transcript was read, and tells when the last one ended. Tasks of an earlier launch died with its process.
func (s *Server) background(c Conversation) (int, time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.tail(c)
	if t == nil {
		return 0, time.Time{}
	}
	return t.background(c.Launched), t.settled
}

func (s *Server) maybeCompact(c Conversation, now time.Time) {
	cfg := s.cfg()
	if midRewind(c.ID) != nil || c.Status != statusIdle || c.Compacted || c.Context < cfg.compactTokens() || now.Sub(lastActive(c)) < cfg.compactAfter() {
		return
	}
	if err := pasteText(c.Window, "/compact"); err != nil {
		log.Printf("compact konduktor: %v", err)
		return
	}
	log.Printf("konduktor compacted at %d tokens", c.Context)
	s.store.update(c.ID, func(c *Conversation) bool {
		c.Compacted = true
		return true
	})
	s.publish(c.ID)
}

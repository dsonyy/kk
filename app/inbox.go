package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"go.yaml.in/yaml/v3"
)

// InboxItem is one ticket, mention or ask from an outside service, in the same shape for every source.
type InboxItem struct {
	Source      string    `yaml:"source" json:"source"`
	ID          string    `yaml:"id" json:"id"`
	Kind        string    `yaml:"kind" json:"kind"`
	Title       string    `yaml:"title" json:"title"`
	Description string    `yaml:"description,omitempty" json:"description"`
	URL         string    `yaml:"url" json:"url"`
	Author      string    `yaml:"author,omitempty" json:"author"`
	Time        time.Time `yaml:"time" json:"time"`
}

// inboxSource is one outside service. ready says why the source cannot run, or nil when it can.
// fetch returns a notSetUp error when the service turns out to be missing only on a run.
type inboxSource interface {
	name() string
	ready() error
	fetch(ctx context.Context) ([]InboxItem, error)
}

type notSetUp string

func (e notSetUp) Error() string { return string(e) }

type sourceState struct {
	Checked time.Time `yaml:"checked,omitempty"`
	Error   string    `yaml:"error,omitempty"`
	// Missing is set when a run found the service is not set up, for sources that can only tell on a run.
	Missing string `yaml:"missing,omitempty"`
}

type inboxState struct {
	Items     []InboxItem             `yaml:"items"`
	Sources   map[string]*sourceState `yaml:"sources"`
	Dismissed []string                `yaml:"dismissed"`
}

type Inbox struct {
	dir      string
	sources  []inboxSource
	settings func() map[string]InboxSettings
	mu       sync.Mutex
	state    inboxState
	running  map[string]bool
}

const (
	inboxTick       = 30 * time.Second
	descriptionMax  = 600
	dismissedMax    = 2000
	inboxFetchLimit = 5 * time.Minute
)

func openInbox(dir string, settings func() map[string]InboxSettings) *Inbox {
	in := &Inbox{dir: dir, settings: settings, running: map[string]bool{}, state: inboxState{Sources: map[string]*sourceState{}}}
	in.sources = []inboxSource{githubSource{}, linearSource{dir: dir}, slackSource{dir: dir}}
	if data, err := os.ReadFile(in.path()); err == nil {
		yaml.Unmarshal(data, &in.state)
	}
	if in.state.Sources == nil {
		in.state.Sources = map[string]*sourceState{}
	}
	for _, s := range in.sources {
		if in.state.Sources[s.name()] == nil {
			in.state.Sources[s.name()] = &sourceState{}
		}
	}
	return in
}

func (in *Inbox) path() string { return filepath.Join(in.dir, "inbox.yaml") }

// save writes the state; the caller holds the lock.
func (in *Inbox) save() {
	data, err := yaml.Marshal(in.state)
	if err != nil {
		return
	}
	tmp := in.path() + ".tmp"
	if os.WriteFile(tmp, data, 0o600) == nil {
		os.Rename(tmp, in.path())
	}
}

func (in *Inbox) source(name string) inboxSource {
	for _, s := range in.sources {
		if s.name() == name {
			return s
		}
	}
	return nil
}

// loop refreshes every enabled source when its interval has passed since its last check, also across restarts.
func (in *Inbox) loop() {
	for {
		cfg := in.settings()
		for _, s := range in.sources {
			c := cfg[s.name()]
			in.mu.Lock()
			due := time.Since(in.state.Sources[s.name()].Checked) >= time.Duration(c.IntervalMinutes)*time.Minute
			in.mu.Unlock()
			if c.Enabled && due && s.ready() == nil {
				in.refresh(s)
			}
		}
		time.Sleep(inboxTick)
	}
}

// refresh fetches one source in the background unless it already runs.
func (in *Inbox) refresh(s inboxSource) {
	in.mu.Lock()
	if in.running[s.name()] {
		in.mu.Unlock()
		return
	}
	in.running[s.name()] = true
	in.mu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), inboxFetchLimit)
		defer cancel()
		items, err := s.fetch(ctx)
		in.mu.Lock()
		defer in.mu.Unlock()
		delete(in.running, s.name())
		st := in.state.Sources[s.name()]
		st.Checked = time.Now()
		st.Error, st.Missing = "", ""
		var missing notSetUp
		switch {
		case errors.As(err, &missing):
			st.Missing = string(missing)
			in.setItems(s.name(), nil)
		case err != nil:
			// Only the error, never the output, which may hold message text.
			st.Error = err.Error()
			log.Printf("inbox %s: %v", s.name(), err)
		default:
			in.setItems(s.name(), items)
		}
		in.save()
	}()
}

func (in *Inbox) setItems(source string, items []InboxItem) {
	kept := slices.DeleteFunc(in.state.Items, func(it InboxItem) bool { return it.Source == source })
	seen := map[string]bool{}
	for _, it := range items {
		it.Source = source
		it.Description = excerpt(it.Description, descriptionMax)
		if it.ID == "" || seen[it.ID] || !safeURL(it.URL) {
			continue
		}
		seen[it.ID] = true
		kept = append(kept, it)
	}
	in.state.Items = kept
}

func safeURL(u string) bool { return strings.HasPrefix(u, "https://") }

var blankLines = regexp.MustCompile(`\n\s*\n\s*`)

func excerpt(s string, max int) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\r\n", "\n")
	return clip(blankLines.ReplaceAllString(s, "\n\n"), max)
}

type sourceView struct {
	Name     string    `json:"name"`
	Enabled  bool      `json:"enabled"`
	Setup    string    `json:"setup"`
	Checked  time.Time `json:"checked,omitzero"`
	Error    string    `json:"error"`
	Running  bool      `json:"running"`
	Interval int       `json:"interval_minutes"`
}

type inboxView struct {
	Items   []InboxItem  `json:"items"`
	Sources []sourceView `json:"sources"`
}

func (in *Inbox) view() inboxView {
	cfg := in.settings()
	in.mu.Lock()
	defer in.mu.Unlock()
	v := inboxView{Items: []InboxItem{}}
	setUp := map[string]bool{}
	for _, s := range in.sources {
		st := in.state.Sources[s.name()]
		sv := sourceView{Name: s.name(), Enabled: cfg[s.name()].Enabled, Checked: st.Checked, Error: st.Error, Running: in.running[s.name()], Interval: cfg[s.name()].IntervalMinutes}
		if err := s.ready(); err != nil {
			sv.Setup = err.Error()
		} else if !sv.Running {
			sv.Setup = st.Missing
		}
		setUp[s.name()] = sv.Setup == ""
		v.Sources = append(v.Sources, sv)
	}
	for _, it := range in.state.Items {
		if setUp[it.Source] && !slices.Contains(in.state.Dismissed, it.Source+":"+it.ID) {
			v.Items = append(v.Items, it)
		}
	}
	sort.SliceStable(v.Items, func(i, j int) bool { return v.Items[i].Time.After(v.Items[j].Time) })
	return v
}

func (s *Server) handleInbox(*http.Request) (any, error) { return s.inbox.view(), nil }

func (s *Server) handleInboxRefresh(r *http.Request) (any, error) {
	var in struct {
		Source string `json:"source"`
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	if in.Source != "" && s.inbox.source(in.Source) == nil {
		return nil, fail(404, "no inbox source %q", in.Source)
	}
	cfg := s.cfg().Inbox
	for _, src := range s.inbox.sources {
		// One source refreshes on demand even when it is switched off; everything means every enabled source.
		if (in.Source == src.name() || in.Source == "" && cfg[src.name()].Enabled) && src.ready() == nil {
			s.inbox.refresh(src)
		}
	}
	return s.inbox.view(), nil
}

func (s *Server) handleInboxDismiss(r *http.Request) (any, error) {
	var in struct {
		Source string `json:"source"`
		ID     string `json:"id"`
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	if in.Source == "" || in.ID == "" {
		return nil, fail(400, "source and id are required")
	}
	s.inbox.mu.Lock()
	key := in.Source + ":" + in.ID
	if !slices.Contains(s.inbox.state.Dismissed, key) {
		d := append(s.inbox.state.Dismissed, key)
		s.inbox.state.Dismissed = d[max(0, len(d)-dismissedMax):]
		s.inbox.save()
	}
	s.inbox.mu.Unlock()
	return s.inbox.view(), nil
}

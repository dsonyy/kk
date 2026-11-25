package main

import (
	"cmp"
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

// An item without a status record is in the backlog.
const (
	statusBacklog    = "backlog"
	statusInProgress = "in_progress"
	statusDismissed  = "dismissed"
	statusClosed     = "closed"
)

var statuses = []string{statusBacklog, statusInProgress, statusDismissed, statusClosed}

type itemStatus struct {
	Status  string    `yaml:"status"`
	Changed time.Time `yaml:"changed,omitempty"`
	// Auto is set when kk closed the item because the source stopped returning it, so it reopens when it comes back.
	Auto bool `yaml:"auto,omitempty"`
}

type inboxState struct {
	// Items holds what the sources returned on their last successful check, and the items out of the backlog they no longer return.
	Items   []InboxItem             `yaml:"items"`
	Sources map[string]*sourceState `yaml:"sources"`
	// Status is keyed by source:id and outlives the item, so an item that comes back keeps it.
	Status map[string]itemStatus `yaml:"status"`
	// Dismissed is the list of an earlier version, read once and turned into status records.
	Dismissed []string `yaml:"dismissed,omitempty"`
}

type Inbox struct {
	dir      string
	sources  []inboxSource
	settings func() map[string]InboxSettings
	mu       sync.Mutex
	state    inboxState
	running  map[string]bool
	// ctx ends the running fetches when the server stops, and wait waits for them.
	ctx    context.Context
	cancel context.CancelFunc
	wait   sync.WaitGroup
}

const (
	descriptionMax  = 600
	statusMax       = 2000
	keptMax         = 300
	inboxFetchLimit = 5 * time.Minute
)

func openInbox(dir string, settings func() map[string]InboxSettings) *Inbox {
	in := &Inbox{dir: dir, settings: settings, running: map[string]bool{}, state: inboxState{Sources: map[string]*sourceState{}}}
	in.ctx, in.cancel = context.WithCancel(context.Background())
	in.sources = []inboxSource{githubSource{}, linearSource{dir: dir}, slackSource{dir: dir}, gmailSource{dir: dir}}
	if data, err := os.ReadFile(in.path()); err == nil {
		yaml.Unmarshal(data, &in.state)
	}
	if in.state.Sources == nil {
		in.state.Sources = map[string]*sourceState{}
	}
	if in.state.Status == nil {
		in.state.Status = map[string]itemStatus{}
	}
	if in.migrate() {
		in.save()
	}
	for _, s := range in.sources {
		if in.state.Sources[s.name()] == nil {
			in.state.Sources[s.name()] = &sourceState{}
		}
	}
	return in
}

// migrate turns the records of earlier versions into the statuses of this one: the legacy dismissed list, which an earlier
// version turned into closed records without a time, becomes dismissed, and to do becomes the backlog.
func (in *Inbox) migrate() bool {
	changed := len(in.state.Dismissed) > 0
	for _, key := range in.state.Dismissed {
		in.state.Status[key] = itemStatus{Status: statusDismissed}
	}
	in.state.Dismissed = nil
	for key, st := range in.state.Status {
		switch {
		case st.Status == "todo":
			delete(in.state.Status, key)
		case st.Status == statusClosed && st.Changed.IsZero():
			in.state.Status[key] = itemStatus{Status: statusDismissed}
		default:
			continue
		}
		changed = true
	}
	return changed
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

// tick refreshes every enabled source whose interval has passed since its last check, also across restarts.
// The page calls it while it has focus, so sources are not checked while nobody looks.
func (in *Inbox) tick(now time.Time) {
	cfg := in.settings()
	for _, s := range in.sources {
		c := cfg[s.name()]
		in.mu.Lock()
		due := now.Sub(in.state.Sources[s.name()].Checked) >= time.Duration(c.IntervalMinutes)*time.Minute
		in.mu.Unlock()
		if c.enabled() && due && s.ready() == nil {
			in.refresh(s)
		}
	}
}

// stop ends the running fetches and waits for them, so no run outlives the server.
func (in *Inbox) stop() {
	in.mu.Lock()
	in.cancel()
	in.mu.Unlock()
	in.wait.Wait()
}

// refresh fetches one source in the background unless it already runs.
func (in *Inbox) refresh(s inboxSource) {
	in.mu.Lock()
	if in.running[s.name()] || in.ctx.Err() != nil {
		in.mu.Unlock()
		return
	}
	in.running[s.name()] = true
	in.wait.Add(1)
	in.mu.Unlock()
	go func() {
		defer in.wait.Done()
		ctx, cancel := context.WithTimeout(in.ctx, inboxFetchLimit)
		defer cancel()
		items, err := s.fetch(ctx)
		in.mu.Lock()
		defer in.mu.Unlock()
		delete(in.running, s.name())
		// A fetch cut short by the server stopping did not check the source.
		if in.ctx.Err() != nil {
			return
		}
		st := in.state.Sources[s.name()]
		st.Checked = time.Now()
		st.Error, st.Missing = "", ""
		var missing notSetUp
		switch {
		case errors.As(err, &missing):
			st.Missing = string(missing)
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

// setItems takes the items of a successful check. Backlog items the source no longer returns leave the inbox, the others
// stay, and those in progress close until they come back.
func (in *Inbox) setItems(source string, items []InboxItem) {
	now := time.Now()
	var next, gone []InboxItem
	seen := map[string]bool{}
	for _, it := range items {
		it.Source = source
		it.Description = excerpt(it.Description, descriptionMax)
		if it.ID == "" || seen[it.ID] || !safeURL(it.URL) {
			continue
		}
		seen[it.ID] = true
		key := source + ":" + it.ID
		if st := in.state.Status[key]; st.Status == statusClosed && st.Auto {
			in.state.Status[key] = itemStatus{Status: statusInProgress, Changed: now}
		}
		next = append(next, it)
	}
	for _, it := range in.state.Items {
		key := it.Source + ":" + it.ID
		st := in.state.Status[key]
		switch {
		case it.Source != source:
			next = append(next, it)
		case seen[it.ID] || st.Status == "":
		case st.Status == statusInProgress:
			in.state.Status[key] = itemStatus{Status: statusClosed, Changed: now, Auto: true}
			fallthrough
		default:
			gone = append(gone, it)
		}
	}
	sort.SliceStable(gone, func(i, j int) bool {
		return in.state.Status[gone[i].Source+":"+gone[i].ID].Changed.After(in.state.Status[gone[j].Source+":"+gone[j].ID].Changed)
	})
	in.state.Items = append(next, gone[:min(len(gone), keptMax)]...)
	in.prune()
}

func safeURL(u string) bool { return strings.HasPrefix(u, "https://") }

var blankLines = regexp.MustCompile(`\n\s*\n\s*`)

func excerpt(s string, max int) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\r\n", "\n")
	return clip(blankLines.ReplaceAllString(s, "\n\n"), max)
}

type sourceView struct {
	Name    string    `json:"name"`
	Enabled bool      `json:"enabled"`
	Setup   string    `json:"setup"`
	Checked time.Time `json:"checked,omitzero"`
	Error   string    `json:"error"`
	Running bool      `json:"running"`
	// Methods holds every way of integrating the source, switched on or off.
	Methods  map[string]bool `json:"methods"`
	Interval int             `json:"interval_minutes"`
}

type itemView struct {
	InboxItem
	Status  string    `json:"status"`
	Changed time.Time `json:"changed,omitzero"`
}

type inboxView struct {
	Items   []itemView   `json:"items"`
	Sources []sourceView `json:"sources"`
}

func (in *Inbox) view() inboxView {
	cfg := in.settings()
	in.mu.Lock()
	defer in.mu.Unlock()
	v := inboxView{Items: []itemView{}}
	setUp := map[string]bool{}
	for _, s := range in.sources {
		st := in.state.Sources[s.name()]
		c := cfg[s.name()]
		methods := map[string]bool{}
		for _, m := range inboxMethods[s.name()] {
			methods[m] = c.Methods[m]
		}
		sv := sourceView{Name: s.name(), Enabled: c.enabled(), Checked: st.Checked, Error: st.Error, Running: in.running[s.name()], Methods: methods, Interval: c.IntervalMinutes}
		if err := s.ready(); err != nil {
			sv.Setup = err.Error()
		} else if !sv.Running {
			sv.Setup = st.Missing
		}
		setUp[s.name()] = sv.Setup == ""
		v.Sources = append(v.Sources, sv)
	}
	// The backlog of a source that is not set up is out of date, but the items the user moved on stay in sight.
	for _, it := range in.state.Items {
		st := in.state.Status[it.Source+":"+it.ID]
		if setUp[it.Source] || st.Status != "" {
			v.Items = append(v.Items, itemView{it, cmp.Or(st.Status, statusBacklog), st.Changed})
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
		if (in.Source == src.name() || in.Source == "" && cfg[src.name()].enabled()) && src.ready() == nil {
			s.inbox.refresh(src)
		}
	}
	return s.inbox.view(), nil
}

func (s *Server) handleInboxStatus(r *http.Request) (any, error) {
	var in struct {
		Source string `json:"source"`
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	if in.Source == "" || in.ID == "" {
		return nil, fail(400, "source and id are required")
	}
	if !slices.Contains(statuses, in.Status) {
		return nil, fail(400, "unknown status %q", in.Status)
	}
	s.inbox.setStatus(in.Source+":"+in.ID, in.Status)
	return s.inbox.view(), nil
}

func (in *Inbox) setStatus(key, status string) {
	in.mu.Lock()
	defer in.mu.Unlock()
	if status == statusBacklog {
		delete(in.state.Status, key)
	} else if in.state.Status[key].Status != status {
		in.state.Status[key] = itemStatus{Status: status, Changed: time.Now()}
	}
	in.prune()
	in.save()
}

// prune forgets the oldest status records of items the inbox no longer holds, beyond statusMax.
func (in *Inbox) prune() {
	if len(in.state.Status) > statusMax {
		// Forget the oldest records of items no source returns any more.
		present := map[string]bool{}
		for _, it := range in.state.Items {
			present[it.Source+":"+it.ID] = true
		}
		var gone []string
		for k := range in.state.Status {
			if !present[k] {
				gone = append(gone, k)
			}
		}
		sort.Slice(gone, func(i, j int) bool { return in.state.Status[gone[i]].Changed.Before(in.state.Status[gone[j]].Changed) })
		for _, k := range gone[:min(len(gone), len(in.state.Status)-statusMax)] {
			delete(in.state.Status, k)
		}
	}
}

package main

import (
	"cmp"
	"log"
	"maps"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// Settings is everything a user can change about kk. It lives in settings.yaml in the kk home.
// An environment variable, when set, wins over the file for that run.
type Settings struct {
	Addr                string   `yaml:"addr" json:"addr"`
	DefaultDir          string   `yaml:"default_dir" json:"default_dir"`
	SkillDirs           []string `yaml:"skill_dirs" json:"skill_dirs"`
	Coordinator         bool     `yaml:"coordinator" json:"coordinator"`
	CoordinatorModel    string   `yaml:"coordinator_model" json:"coordinator_model"`
	Source              string   `yaml:"source" json:"source"`
	StartTimeoutSeconds int      `yaml:"start_timeout_seconds" json:"start_timeout_seconds"`
	CompactAfterMinutes int      `yaml:"compact_after_minutes" json:"compact_after_minutes"`
	CompactTokens       int      `yaml:"compact_tokens" json:"compact_tokens"`
	// Inbox holds, per inbox source, the ways kk reads it and how often it checks.
	Inbox map[string]InboxSettings `yaml:"inbox" json:"inbox"`
}

// InboxSettings switches each way of integrating a source on or off. The work_minutes and off_minutes of an earlier
// version are ignored, and its enabled key carries over to the methods once.
type InboxSettings struct {
	IntervalMinutes int             `yaml:"interval_minutes" json:"interval_minutes"`
	Methods         map[string]bool `yaml:"methods" json:"methods"`
	Enabled         *bool           `yaml:"enabled,omitempty" json:"-"`
}

func (c InboxSettings) enabled() bool {
	for _, on := range c.Methods {
		if on {
			return true
		}
	}
	return false
}

// inboxMethods lists the ways of integrating each source.
var inboxMethods = map[string][]string{"github": {"gh"}, "linear": {"api_key"}, "slack": {"connector"}, "gmail": {"connector"}}

// settingsEnv maps a setting to the environment variable that overrides it.
var settingsEnv = map[string]string{
	"addr": "KK_ADDR", "coordinator": "KK_NO_COORDINATOR", "source": "KK_SOURCE",
	"start_timeout_seconds": "KK_START_TIMEOUT", "compact_after_minutes": "KK_COMPACT_AFTER", "compact_tokens": "KK_COMPACT_TOKENS",
}

func settingsPath(dir string) string { return filepath.Join(dir, "settings.yaml") }

const inboxInterval = 15

func inboxDefaults() map[string]InboxSettings {
	out := map[string]InboxSettings{}
	for name, methods := range inboxMethods {
		on := map[string]bool{}
		for _, m := range methods {
			on[m] = true
		}
		out[name] = InboxSettings{IntervalMinutes: inboxInterval, Methods: on}
	}
	return out
}

func loadSettings(dir string) Settings {
	h, _ := os.UserHomeDir()
	st := Settings{
		Addr: "127.0.0.1:7420", Coordinator: true, Source: filepath.Join(h, "repos", "kk"), SkillDirs: []string{},
		StartTimeoutSeconds: 20, CompactAfterMinutes: 55, CompactTokens: 50_000,
		Inbox: inboxDefaults(),
	}
	if data, err := os.ReadFile(settingsPath(dir)); err == nil {
		yaml.Unmarshal(data, &st)
	}
	// A source saved without an interval loads with it at zero, which would check on every tick.
	for name, in := range st.Inbox {
		d, ok := inboxDefaults()[name]
		if !ok {
			continue
		}
		in.IntervalMinutes = cmp.Or(max(in.IntervalMinutes, 0), d.IntervalMinutes)
		if in.Methods == nil {
			in.Methods = d.Methods
			for m := range in.Methods {
				in.Methods[m] = in.Enabled == nil || *in.Enabled
			}
		}
		in.Enabled = nil
		st.Inbox[name] = in
	}
	return st
}

func (st Settings) addr() string {
	if a := os.Getenv("KK_ADDR"); a != "" {
		return a
	}
	return st.Addr
}

func (st Settings) coordinator() bool {
	return st.Coordinator && os.Getenv("KK_NO_COORDINATOR") == ""
}

func (st Settings) source() string {
	if d := os.Getenv("KK_SOURCE"); d != "" {
		return d
	}
	return st.Source
}

func (st Settings) startTimeout() time.Duration {
	return envDuration("KK_START_TIMEOUT", time.Duration(st.StartTimeoutSeconds)*time.Second)
}

func (st Settings) compactAfter() time.Duration {
	return envDuration("KK_COMPACT_AFTER", time.Duration(st.CompactAfterMinutes)*time.Minute)
}

func (st Settings) compactTokens() int {
	if n, _ := strconv.Atoi(os.Getenv("KK_COMPACT_TOKENS")); n > 0 {
		return n
	}
	return st.CompactTokens
}

func (s *Server) cfg() Settings {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.settings
}

type settingsView struct {
	Settings Settings `json:"settings"`
	// Env names the settings an environment variable overrides for this run.
	Env map[string]string `json:"env"`
}

func (s *Server) handleSettings(*http.Request) (any, error) {
	env := map[string]string{}
	for field, name := range settingsEnv {
		if os.Getenv(name) != "" {
			env[field] = name
		}
	}
	return settingsView{s.cfg(), env}, nil
}

// changedSettings names the settings that differ between a and b, without their values.
func changedSettings(a, b Settings) []string {
	var ma, mb map[string]any
	da, _ := yaml.Marshal(a)
	db, _ := yaml.Marshal(b)
	yaml.Unmarshal(da, &ma)
	yaml.Unmarshal(db, &mb)
	var keys []string
	for k := range mb {
		if !reflect.DeepEqual(ma[k], mb[k]) {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	return keys
}

func (s *Server) handleSaveSettings(r *http.Request) (any, error) {
	old := s.cfg()
	st := s.cfg()
	st.Inbox = maps.Clone(st.Inbox)
	if err := decode(r, &st); err != nil {
		return nil, err
	}
	if _, _, err := net.SplitHostPort(st.Addr); err != nil {
		return nil, fail(400, "address %q is not host:port", st.Addr)
	}
	if st.StartTimeoutSeconds < 1 || st.CompactAfterMinutes < 1 || st.CompactTokens < 1 {
		return nil, fail(400, "timeouts and the token limit must be positive")
	}
	for name, in := range st.Inbox {
		methods, ok := inboxMethods[name]
		if !ok || in.IntervalMinutes < 1 {
			return nil, fail(400, "inbox source %q needs a known name and an interval of at least a minute", name)
		}
		for m := range in.Methods {
			if !slices.Contains(methods, m) {
				return nil, fail(400, "inbox source %q has no method %q", name, m)
			}
		}
	}
	claude, _ := findHarness("claude")
	if !slices.Contains(claude.Models, st.CoordinatorModel) {
		return nil, fail(400, "unknown conductor model %q", st.CoordinatorModel)
	}
	dirs := []string{}
	for _, d := range st.SkillDirs {
		if d == "" {
			continue
		}
		dir, err := expandDir(d)
		if err != nil {
			return nil, err
		}
		dirs = append(dirs, dir)
	}
	st.SkillDirs = dirs
	if st.DefaultDir != "" {
		dir, err := expandDir(st.DefaultDir)
		if err != nil {
			return nil, err
		}
		st.DefaultDir = dir
	}
	data, err := yaml.Marshal(st)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(settingsPath(s.store.dir), data, 0o600); err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.settings = st
	s.mu.Unlock()
	if keys := changedSettings(old, st); len(keys) > 0 {
		log.Printf("settings changed: %s", strings.Join(keys, ", "))
	}
	s.syncIntegrations()
	if st.coordinator() {
		if err := s.ensureCoordinator(); err != nil {
			return nil, err
		}
	}
	return s.handleSettings(r)
}

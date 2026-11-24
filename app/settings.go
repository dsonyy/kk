package main

import (
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
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
}

// settingsEnv maps a setting to the environment variable that overrides it.
var settingsEnv = map[string]string{
	"addr": "KK_ADDR", "coordinator": "KK_NO_COORDINATOR", "source": "KK_SOURCE",
	"start_timeout_seconds": "KK_START_TIMEOUT", "compact_after_minutes": "KK_COMPACT_AFTER", "compact_tokens": "KK_COMPACT_TOKENS",
}

func settingsPath(dir string) string { return filepath.Join(dir, "settings.yaml") }

func loadSettings(dir string) Settings {
	h, _ := os.UserHomeDir()
	st := Settings{
		Addr: "127.0.0.1:7420", Coordinator: true, Source: filepath.Join(h, "repos", "kk"), SkillDirs: []string{},
		StartTimeoutSeconds: 20, CompactAfterMinutes: 55, CompactTokens: 50_000,
	}
	if data, err := os.ReadFile(settingsPath(dir)); err == nil {
		yaml.Unmarshal(data, &st)
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

func (s *Server) handleSaveSettings(r *http.Request) (any, error) {
	st := s.cfg()
	if err := decode(r, &st); err != nil {
		return nil, err
	}
	if _, _, err := net.SplitHostPort(st.Addr); err != nil {
		return nil, fail(400, "address %q is not host:port", st.Addr)
	}
	if st.StartTimeoutSeconds < 1 || st.CompactAfterMinutes < 1 || st.CompactTokens < 1 {
		return nil, fail(400, "timeouts and the token limit must be positive")
	}
	claude, _ := findHarness("claude")
	if !slices.Contains(claude.Models, st.CoordinatorModel) {
		return nil, fail(400, "unknown coordinator model %q", st.CoordinatorModel)
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
	if st.coordinator() {
		if err := s.ensureCoordinator(); err != nil {
			return nil, err
		}
	}
	return s.handleSettings(r)
}

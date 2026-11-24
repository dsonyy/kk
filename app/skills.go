package main

import (
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

type Skill struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	// Insert is what the user types to invoke the skill in this harness.
	Insert string `json:"insert"`
	Scope  string `json:"scope"`
}

// skillDirs lists where a harness loads skills from, user-wide first and then for the directory it runs in.
func skillDirs(harness string) (user, project []string) {
	switch harness {
	case "claude":
		return []string{".claude/skills", ".claude/commands"}, []string{".claude/skills", ".claude/commands"}
	case "codex":
		return []string{".codex/skills", ".codex/skills/.system", ".agents/skills"}, []string{".codex/skills", ".agents/skills"}
	case "cursor":
		return []string{".cursor/skills", ".cursor/skills-cursor", ".cursor/commands", ".agents/skills"}, []string{".cursor/skills", ".cursor/commands", ".agents/skills"}
	}
	return nil, nil
}

func readSkills(root, prefix, scope string, out map[string]Skill) {
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		path := filepath.Join(root, e.Name())
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		if st, err := os.Stat(path); err == nil && st.IsDir() {
			path = filepath.Join(path, "SKILL.md")
		} else if strings.HasSuffix(name, ".md") {
			name = strings.TrimSuffix(name, ".md")
		} else {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var meta struct{ Name, Description string }
		if rest, ok := strings.CutPrefix(string(data), "---\n"); ok {
			if front, _, ok := strings.Cut(rest, "\n---"); ok {
				yaml.Unmarshal([]byte(front), &meta)
			}
		}
		if meta.Name != "" {
			name = meta.Name
		}
		out[name] = Skill{Name: name, Description: firstLine(meta.Description, 200), Insert: prefix + name, Scope: scope}
	}
}

func (s *Server) handleSkills(r *http.Request) (any, error) {
	harness := r.URL.Query().Get("harness")
	if _, ok := findHarness(harness); !ok {
		return nil, fail(400, "unknown harness %q", harness)
	}
	prefix := "/"
	if harness == "codex" {
		prefix = "$"
	}
	home, _ := os.UserHomeDir()
	user, project := skillDirs(harness)
	found := map[string]Skill{}
	for _, d := range user {
		readSkills(filepath.Join(home, d), prefix, "user", found)
	}
	if dir, err := expandDir(r.URL.Query().Get("dir")); err == nil && dir != home {
		for _, d := range project {
			readSkills(filepath.Join(dir, d), prefix, "project", found)
		}
	}
	for _, d := range s.cfg().SkillDirs {
		readSkills(d, prefix, "extra", found)
	}
	out := make([]Skill, 0, len(found))
	for _, sk := range found {
		out = append(out, sk)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

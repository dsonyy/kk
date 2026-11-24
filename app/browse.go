package main

import (
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

type listing struct {
	Dir    string   `json:"dir"`
	Parent string   `json:"parent"`
	Dirs   []string `json:"dirs"`
}

// handleBrowse lists the directories inside dir for the folder browser. A dir that is not a directory starts at the home directory.
func handleBrowse(r *http.Request) (any, error) {
	dir, err := expandDir(r.URL.Query().Get("dir"))
	if err != nil {
		if dir, err = expandDir(""); err != nil {
			return nil, err
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fail(400, "cannot read %s", dir)
	}
	l := listing{Dir: dir, Dirs: []string{}}
	if parent := filepath.Dir(dir); parent != dir {
		l.Parent = parent
	}
	for _, e := range entries {
		// Stat follows a link, so a link to a directory is listed too.
		if st, err := os.Stat(filepath.Join(dir, e.Name())); err == nil && st.IsDir() {
			l.Dirs = append(l.Dirs, e.Name())
		}
	}
	slices.SortFunc(l.Dirs, func(a, b string) int { return strings.Compare(strings.ToLower(a), strings.ToLower(b)) })
	return l, nil
}

package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestBrowse(t *testing.T) {
	e := newEnv(t)
	os.Mkdir(filepath.Join(e.work, "beta"), 0o755)
	os.Mkdir(filepath.Join(e.work, "Alpha"), 0o755)
	os.Mkdir(filepath.Join(e.work, ".hidden"), 0o755)
	os.Mkdir(filepath.Join(e.work, "locked"), 0o000)
	os.WriteFile(filepath.Join(e.work, "file.txt"), []byte("x"), 0o644)
	os.Symlink(filepath.Join(e.work, "beta"), filepath.Join(e.work, "link"))
	os.Symlink(filepath.Join(e.work, "file.txt"), filepath.Join(e.work, "filelink"))
	browse := func(dir string) (int, listing) {
		t.Helper()
		resp, body := e.request("GET", "/api/browse?dir="+url.QueryEscape(dir), nil)
		var l listing
		json.Unmarshal(body, &l)
		return resp.StatusCode, l
	}

	code, l := browse(e.work)
	want := []string{".hidden", "Alpha", "beta", "link", "locked"}
	if code != 200 || l.Dir != e.work || l.Parent != filepath.Dir(e.work) || !slices.Equal(l.Dirs, want) {
		t.Fatalf("browse %s: %d %+v, want directories %v", e.work, code, l, want)
	}
	home := filepath.Dir(e.work)
	for _, dir := range []string{"", filepath.Join(e.work, "missing"), filepath.Join(e.work, "file.txt")} {
		if code, l := browse(dir); code != 200 || l.Dir != home {
			t.Fatalf("browse %q starts at %q, want the home directory %s: %d", dir, l.Dir, home, code)
		}
	}
	if code, l := browse("/"); code != 200 || l.Dir != "/" || l.Parent != "" {
		t.Fatalf("browse /: %d %+v, want no parent", code, l)
	}
	if code, l := browse(filepath.Join(e.work, "locked")); code != 400 {
		t.Fatalf("browse a directory that cannot be read: %d %+v", code, l)
	}

	resp, err := http.Get("http://" + e.addr + "/api/browse?dir=" + url.QueryEscape(e.work))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("browse without the token: %d", resp.StatusCode)
	}
}

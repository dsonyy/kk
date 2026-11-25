package main

import (
	"errors"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const worktreesDir = ".claude/worktrees"

func git(dir string, args ...string) (string, error) {
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		return "", errors.New(text)
	}
	return text, nil
}

// inWorktree reports whether the agent of c works in a linked git worktree, whether kk made it or not. It reads files
// because the list asks about every conversation, too often to run git for each.
func inWorktree(c Conversation) bool {
	if c.Repo != "" {
		return true
	}
	dir, err := filepath.EvalSymlinks(c.Dir)
	if err != nil {
		// A removed directory can only tell by its path.
		return strings.Contains(c.Dir, "/"+worktreesDir+"/")
	}
	for {
		data, err := os.ReadFile(filepath.Join(dir, ".git"))
		if errors.Is(err, fs.ErrNotExist) && filepath.Dir(dir) != dir {
			dir = filepath.Dir(dir)
			continue
		}
		// In a linked worktree .git is a file naming a directory that has a commondir file. A submodule's has none.
		gitdir, ok := strings.CutPrefix(strings.TrimSpace(string(data)), "gitdir: ")
		if !ok {
			return false
		}
		if !filepath.IsAbs(gitdir) {
			gitdir = filepath.Join(dir, gitdir)
		}
		_, err = os.Stat(filepath.Join(gitdir, "commondir"))
		return err == nil
	}
}

func handleRepo(r *http.Request) (any, error) {
	info := struct {
		Git    bool   `json:"git"`
		Branch string `json:"branch"`
	}{}
	dir, err := expandDir(r.URL.Query().Get("dir"))
	if err != nil {
		return info, nil
	}
	if inside, _ := git(dir, "rev-parse", "--is-inside-work-tree"); inside != "true" {
		return info, nil
	}
	info.Git = true
	if info.Branch, _ = git(dir, "branch", "--show-current"); info.Branch == "" {
		info.Branch, _ = git(dir, "rev-parse", "--short", "HEAD")
	}
	return info, nil
}

func worktreeBranch(title, id string) string {
	slug := strings.ToLower(nonSlug.ReplaceAllString(title, "-"))
	if len(slug) > 40 {
		slug = slug[:40]
	}
	return strings.Trim(strings.Trim(slug, "-")+"-"+id, "-")
}

// addWorktree makes a worktree of dir's repository on a new branch off dir's HEAD and returns dir's place in it.
// Worktrees sit inside the main checkout, where the agents already trust the folder.
func addWorktree(dir, branch string) (string, error) {
	list, err := git(dir, "worktree", "list", "--porcelain")
	if err != nil {
		return "", fail(400, "%s is not a git repository", dir)
	}
	checkout, _, _ := strings.Cut(strings.TrimPrefix(list, "worktree "), "\n")
	path := filepath.Join(checkout, worktreesDir, branch)
	if _, err := git(checkout, "check-ignore", "-q", path); err != nil {
		common, err := git(checkout, "rev-parse", "--path-format=absolute", "--git-common-dir")
		if err != nil {
			return "", err
		}
		os.MkdirAll(filepath.Join(common, "info"), 0o755)
		f, err := os.OpenFile(filepath.Join(common, "info", "exclude"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return "", err
		}
		f.WriteString("\n" + worktreesDir + "/\n")
		f.Close()
	}
	prefix, _ := git(dir, "rev-parse", "--show-prefix")
	if _, err := git(dir, "worktree", "add", "-b", branch, path); err != nil {
		return "", fail(400, "git worktree: %v", err)
	}
	return filepath.Join(path, prefix), nil
}

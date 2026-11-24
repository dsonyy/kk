package main

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const tmuxSession = "kk"

func tmuxCmd(args ...string) *exec.Cmd {
	if sock := os.Getenv("KK_TMUX"); sock != "" {
		args = append([]string{"-L", sock}, args...)
	}
	return exec.Command("tmux", args...)
}

func tmux(stdin string, args ...string) (string, error) {
	cmd := tmuxCmd(args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("tmux %s: %v: %s", args[0], err, strings.TrimSpace(string(out)))
	}
	return strings.TrimRight(string(out), "\n"), nil
}

func ensureSession() error {
	if _, err := tmux("", "has-session", "-t", "="+tmuxSession); err == nil {
		return nil
	}
	_, err := tmux("", "new-session", "-d", "-s", tmuxSession, "-n", "home", "-x", "200", "-y", "50")
	return err
}

func openWindow(name, dir string, env map[string]string, argv []string) (string, error) {
	if err := ensureSession(); err != nil {
		return "", err
	}
	args := []string{"new-window", "-d", "-P", "-F", "#{window_id}", "-t", tmuxSession + ":", "-n", name, "-c", dir}
	for k, v := range env {
		args = append(args, "-e", k+"="+v)
	}
	args = append(args, shellJoin(argv))
	id, err := tmux("", args...)
	if err != nil {
		return "", err
	}
	_, err = tmux("", "set-option", "-w", "-t", id, "remain-on-exit", "on")
	return id, err
}

func respawnWindow(id, dir string, env map[string]string, argv []string) error {
	args := []string{"respawn-pane", "-k", "-t", id, "-c", dir}
	for k, v := range env {
		args = append(args, "-e", k+"="+v)
	}
	_, err := tmux("", append(args, shellJoin(argv))...)
	return err
}

// windows maps window ids in the kk session to whether their process is still alive.
func windows() (map[string]bool, error) {
	out, err := tmux("", "list-panes", "-s", "-t", "="+tmuxSession, "-F", "#{window_id} #{pane_dead}")
	if err != nil {
		if strings.Contains(err.Error(), "can't find") || strings.Contains(err.Error(), "no server running") || strings.Contains(err.Error(), "error connecting") {
			return map[string]bool{}, nil
		}
		return nil, err
	}
	alive := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		if id, dead, ok := strings.Cut(line, " "); ok {
			alive[id] = dead != "1"
		}
	}
	return alive, nil
}

func capture(id string) (string, error) {
	return tmux("", "capture-pane", "-p", "-t", id)
}

func pasteText(id, text string) error {
	buf := "kk-" + strings.TrimPrefix(id, "@")
	if _, err := tmux(text, "load-buffer", "-b", buf, "-"); err != nil {
		return err
	}
	if _, err := tmux("", "paste-buffer", "-p", "-d", "-b", buf, "-t", id); err != nil {
		return err
	}
	// TUIs treat an Enter that arrives together with the paste as part of it.
	time.Sleep(150 * time.Millisecond)
	_, err := tmux("", "send-keys", "-t", id, "Enter")
	return err
}

var allowedKeys = map[string]bool{
	"Enter": true, "Escape": true, "Up": true, "Down": true, "Left": true, "Right": true,
	"Tab": true, "BTab": true, "C-c": true, "Space": true, "BSpace": true,
	"1": true, "2": true, "3": true, "4": true, "y": true, "n": true,
}

func sendKeys(id string, keys []string) error {
	for _, k := range keys {
		if !allowedKeys[k] {
			return fmt.Errorf("unsupported key %q", k)
		}
	}
	_, err := tmux("", append([]string{"send-keys", "-t", id}, keys...)...)
	return err
}

// hangUp ends the process in a window the way a closed terminal does and keeps the window with its dead pane,
// like an agent that exited by itself. A process that ignores the hangup is killed.
func hangUp(id string) error {
	out, err := tmux("", "display-message", "-p", "-t", id, "#{pane_dead} #{pane_pid}")
	if err != nil {
		return err
	}
	dead, pane, _ := strings.Cut(out, " ")
	pid, err := strconv.Atoi(pane)
	if dead == "1" || err != nil {
		return err
	}
	// The pane's process leads its own process group.
	syscall.Kill(-pid, syscall.SIGHUP)
	for range 30 {
		time.Sleep(100 * time.Millisecond)
		if alive, _ := windows(); !alive[id] {
			return nil
		}
	}
	return syscall.Kill(-pid, syscall.SIGKILL)
}

func killWindow(id string) error {
	_, err := tmux("", "kill-window", "-t", id)
	return err
}

func shellJoin(argv []string) string {
	q := make([]string, len(argv))
	for i, a := range argv {
		q[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	return strings.Join(q, " ")
}

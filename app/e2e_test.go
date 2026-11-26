package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"
)

var kkBin, fakeBin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "kk-test-bin")
	if err != nil {
		panic(err)
	}
	kkBin = filepath.Join(dir, "kk")
	fakeBin = filepath.Join(dir, "fake")
	os.Mkdir(fakeBin, 0o755)
	for _, build := range [][]string{{"-o", kkBin, "."}, {"-o", filepath.Join(fakeBin, "fakeagent"), "./testdata/fakeagent"}} {
		if out, err := exec.Command("go", append([]string{"build"}, build...)...).CombinedOutput(); err != nil {
			panic(string(out))
		}
	}
	for _, name := range []string{"claude", "codex", "cursor-agent", "gh"} {
		os.Symlink(filepath.Join(fakeBin, "fakeagent"), filepath.Join(fakeBin, name))
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

type env struct {
	t     *testing.T
	home  string
	fake  string
	work  string
	addr  string
	vars  []string
	proc  *exec.Cmd
	url   string
	token string
}

func freeAddr() string {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	defer l.Close()
	return l.Addr().String()
}

func newEnv(t *testing.T, extra ...string) *env {
	t.Helper()
	root := t.TempDir()
	e := &env{t: t, home: filepath.Join(root, "kk"), fake: filepath.Join(root, "fake"), work: filepath.Join(root, "work"), addr: freeAddr()}
	os.MkdirAll(e.fake, 0o755)
	os.MkdirAll(e.work, 0o755)
	codex := filepath.Join(root, "codex")
	os.MkdirAll(codex, 0o755)
	os.WriteFile(filepath.Join(codex, "models_cache.json"), []byte(`{"models":[{"slug":"gpt-test","visibility":"list"},{"slug":"hidden","visibility":"hide"}]}`), 0o644)
	sock := fmt.Sprintf("kk-test-%d", time.Now().UnixNano())
	e.vars = append([]string{
		"KK_HOME=" + e.home, "KK_ADDR=" + e.addr, "KK_TMUX=" + sock, "FAKE_HOME=" + e.fake, "CODEX_HOME=" + codex,
		"PATH=" + fakeBin + ":" + os.Getenv("PATH"), "KK_START_TIMEOUT=2s", "KK_SOURCE=" + root, "HOME=" + root,
		// The CLI tells agents from the user by this variable, which leaks in when the tests run inside a kk agent.
		"KK_CONVERSATION=",
	}, extra...)
	if !hasVar(extra, "KK_NO_COORDINATOR") && !hasVar(extra, "KK_COORDINATOR") {
		e.vars = append(e.vars, "KK_NO_COORDINATOR=1")
	}
	t.Cleanup(func() {
		e.stop()
		killTmux(t, sock)
	})
	e.start()
	return e
}

// killTmux ends the tmux server and waits for the processes in its panes, which exit after it and still write into the test's
// directories: zsh saves its history in HOME.
func killTmux(t *testing.T, sock string) {
	out, _ := exec.Command("tmux", "-L", sock, "list-panes", "-a", "-F", "#{pane_dead} #{pane_pid}").Output()
	exec.Command("tmux", "-L", sock, "kill-server").Run()
	deadline := time.Now().Add(10 * time.Second)
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		pid, err := strconv.Atoi(strings.TrimPrefix(line, "0 "))
		if err != nil {
			continue
		}
		for syscall.Kill(pid, 0) == nil {
			if time.Now().After(deadline) {
				t.Errorf("pane process %d outlived the tmux server", pid)
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func hasVar(vars []string, name string) bool {
	for _, v := range vars {
		if strings.HasPrefix(v, name+"=") {
			return true
		}
	}
	return false
}

func (e *env) environ() []string { return append(os.Environ(), e.vars...) }

func (e *env) start() {
	e.t.Helper()
	cmd := exec.Command(kkBin)
	cmd.Env = e.environ()
	out, _ := cmd.StdoutPipe()
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		e.t.Fatal(err)
	}
	e.proc = cmd
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil {
		e.t.Fatalf("server did not start: %v", err)
	}
	e.url = strings.TrimSpace(strings.TrimPrefix(line, "kk is running: "))
	e.token = e.url[strings.Index(e.url, "token=")+6:]
	go io.Copy(io.Discard, out)
}

func (e *env) stop() {
	if e.proc != nil && e.proc.Process != nil {
		e.proc.Process.Signal(syscall.SIGTERM)
		e.proc.Wait()
		e.proc = nil
	}
}

func (e *env) kk(args ...string) (string, error) {
	cmd := exec.Command(kkBin, args...)
	cmd.Env = e.environ()
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (e *env) mustKK(args ...string) string {
	e.t.Helper()
	out, err := e.kk(args...)
	if err != nil {
		e.t.Fatalf("kk %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(out)
}

// asKonduktor runs the kk CLI the way the konduktor does, from its own window.
func (e *env) asKonduktor(args ...string) string {
	e.t.Helper()
	cmd := exec.Command(kkBin, args...)
	cmd.Env = append(e.environ(), "KK_CONVERSATION="+coordinatorID)
	out, err := cmd.CombinedOutput()
	if err != nil {
		e.t.Fatalf("kk %v as the konduktor: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (e *env) conv(id string) convView {
	e.t.Helper()
	cmd := exec.Command("curl", "-s", "--unix-socket", filepath.Join(e.home, "kk.sock"), "http://kk/api/conversations/"+id)
	out, err := cmd.Output()
	if err != nil {
		e.t.Fatal(err)
	}
	var c convView
	json.Unmarshal(out, &c)
	return c
}

func (e *env) post(path string, body any) (int, string) {
	data, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", "http://kk"+path, bytes.NewReader(data))
	t := &http.Transport{Dial: func(_, _ string) (net.Conn, error) { return net.Dial("unix", filepath.Join(e.home, "kk.sock")) }}
	resp, err := (&http.Client{Transport: t}).Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(out)
}

func (e *env) waitFor(what string, id string, ok func(c convView) bool) convView {
	e.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var c convView
	for time.Now().Before(deadline) {
		c = e.conv(id)
		if ok(c) {
			return c
		}
		time.Sleep(100 * time.Millisecond)
	}
	screen, _ := e.kk("screen", id)
	e.t.Fatalf("timed out waiting for %s\nconversation: %+v\nscreen:\n%s", what, c.Conversation, screen)
	return c
}

func hasEvent(c convView, kind, text string) bool {
	for _, ev := range c.Events {
		if ev.Kind == kind && (ev.Text == text || ev.Input == text) {
			return true
		}
	}
	return false
}

func TestClaudeConversation(t *testing.T) {
	e := newEnv(t)
	id := e.mustKK("new", "claude", "-m", "opus", "-C", e.work, "hello", "world", "again", "today")
	c := e.waitFor("first answer", id, func(c convView) bool {
		return c.Status == statusIdle && hasEvent(c, kindAssistant, "echo: hello world again today")
	})
	if !hasEvent(c, kindUser, "hello world again today") || !hasEvent(c, kindTool, "ls") || !hasEvent(c, kindThinking, "Listing files") {
		t.Fatalf("events: %+v", c.Events)
	}
	for _, ev := range c.Events {
		if ev.Kind == kindTool && ev.Output != "a.txt\nb.txt\n" {
			t.Fatalf("tool output not attached: %+v", ev)
		}
	}
	c = e.waitFor("title", id, func(c convView) bool { return c.Title == "Title: hello world again" })
	if !c.Unread || c.LastMessage.IsZero() || c.Model != "opus" || c.Dir != e.work || c.Context != 1000 {
		t.Fatalf("conversation: %+v", c.Conversation)
	}
	argv, _ := os.ReadFile(filepath.Join(e.fake, "argv.log"))
	if !strings.Contains(string(argv), `"--model","opus"`) || !strings.Contains(string(argv), `"--session-id","`+c.Session+`"`) {
		t.Fatalf("argv: %s", argv)
	}

	if code, body := e.post("/api/conversations/"+id+"/send", map[string]string{"text": "line one\nline two"}); code != 200 {
		t.Fatalf("send: %d %s", code, body)
	}
	e.waitFor("multi-line reply", id, func(c convView) bool { return hasEvent(c, kindAssistant, "echo: line one\nline two") })
	if c := e.conv(id); !hasEvent(c, kindUser, "line one\nline two") {
		t.Fatalf("the reply did not arrive as one message: %+v", c.Events)
	}

	ls := e.mustKK("ls")
	if !strings.Contains(ls, id) || !strings.Contains(ls, "idle") || !strings.Contains(ls, "Title: line one") {
		t.Fatalf("ls: %s", ls)
	}
	show := e.mustKK("show", id)
	if !strings.Contains(show, "[tool Bash] ls") || !strings.Contains(show, "[assistant] echo: line one") {
		t.Fatalf("show: %s", show)
	}
	e.mustKK("send", id, "from", "the", "cli")
	e.waitFor("cli reply", id, func(c convView) bool { return hasEvent(c, kindAssistant, "echo: from the cli") })

	e.post("/api/conversations/"+id+"/seen", struct{}{})
	if c := e.conv(id); c.Unread {
		t.Fatal("still unread after seen")
	}
}

func TestPermissionNeedsAttention(t *testing.T) {
	e := newEnv(t)
	id := e.mustKK("new", "claude", "-C", e.work, "ask for permission")
	c := e.waitFor("attention", id, func(c convView) bool { return c.Attention != "" })
	if c.Attention != "Claude needs your permission to use Bash" {
		t.Fatalf("attention: %q", c.Attention)
	}
	if screen := e.mustKK("screen", id); !strings.Contains(screen, "Allow Bash?") {
		t.Fatalf("screen: %s", screen)
	}
	if out, err := e.kk("keys", id, "rm -rf"); err == nil {
		t.Fatalf("arbitrary keys accepted: %s", out)
	}
	e.mustKK("keys", id, "1", "Enter")
	c = e.waitFor("answer after permission", id, func(c convView) bool {
		return c.Attention == "" && c.Status == statusIdle && hasEvent(c, kindAssistant, "echo: ask for permission")
	})
}

func TestAPIErrorEndsTurn(t *testing.T) {
	e := newEnv(t)
	id := e.mustKK("new", "claude", "-C", e.work, "apierror now")
	c := e.waitFor("the failed turn to end", id, func(c convView) bool { return c.Status == statusIdle && c.Attention != "" })
	if !hasEvent(c, kindError, "API Error: The response stopped arriving. The response above may be incomplete.") || !strings.Contains(c.Attention, "API error") {
		t.Fatalf("conversation: %+v", c)
	}
	if ls := e.mustKK("ls"); !strings.Contains(ls, "ACTION REQUIRED") {
		t.Fatalf("ls: %s", ls)
	}
	e.mustKK("send", id, "continue")
	e.waitFor("the answer after the error", id, func(c convView) bool {
		return c.Attention == "" && c.Status == statusIdle && hasEvent(c, kindAssistant, "echo: continue")
	})
}

func TestAgentStuckBeforeStart(t *testing.T) {
	e := newEnv(t)
	id := e.mustKK("new", "claude", "-C", e.work, "TRUST me")
	c := e.waitFor("start timeout", id, func(c convView) bool { return c.Attention != "" })
	if c.Status != statusStarting || !strings.Contains(c.Attention, "has not started") {
		t.Fatalf("conversation: %+v", c.Conversation)
	}
	if screen := e.mustKK("screen", id); !strings.Contains(screen, "Do you trust this folder?") {
		t.Fatalf("screen: %s", screen)
	}
	e.mustKK("keys", id, "1", "Enter")
	e.waitFor("start after trust", id, func(c convView) bool { return c.Attention == "" && hasEvent(c, kindAssistant, "echo: TRUST me") })
}

func TestExitAndResume(t *testing.T) {
	e := newEnv(t)
	id := e.mustKK("new", "claude", "-C", e.work, "first")
	e.waitFor("answer", id, func(c convView) bool { return c.Status == statusIdle && hasEvent(c, kindAssistant, "echo: first") })
	e.mustKK("send", id, "exit")
	e.waitFor("exit", id, func(c convView) bool { return c.Status == statusExited })
	if code, _ := e.post("/api/conversations/"+id+"/send", map[string]string{"text": "hello"}); code != 409 {
		t.Fatalf("send to an exited agent: %d", code)
	}
	session := e.conv(id).Session
	e.mustKK("resume", id)
	e.waitFor("resumed", id, func(c convView) bool { return c.Status == statusIdle })
	e.mustKK("send", id, "second")
	c := e.waitFor("answer after resume", id, func(c convView) bool { return hasEvent(c, kindAssistant, "echo: second") })
	if !hasEvent(c, kindAssistant, "echo: first") || c.Session != session {
		t.Fatalf("history lost after resume: %+v", c)
	}
	argv, _ := os.ReadFile(filepath.Join(e.fake, "argv.log"))
	if !strings.Contains(string(argv), `"--resume","`+session+`"`) {
		t.Fatalf("argv: %s", argv)
	}
}

func TestCodexConversation(t *testing.T) {
	e := newEnv(t)
	var hs []Harness
	out, _ := exec.Command("curl", "-s", "--unix-socket", filepath.Join(e.home, "kk.sock"), "http://kk/api/harnesses").Output()
	json.Unmarshal(out, &hs)
	if len(hs) != 3 || hs[1].Name != "codex" || strings.Join(hs[1].Models, ",") != "gpt-test" {
		t.Fatalf("harnesses: %s", out)
	}
	id := e.mustKK("new", "codex", "-m", "gpt-test", "-C", e.work, "hi codex")
	c := e.waitFor("codex answer", id, func(c convView) bool { return c.Status == statusIdle && hasEvent(c, kindAssistant, "echo: hi codex") })
	if !hasEvent(c, kindUser, "hi codex") || !hasEvent(c, kindTool, "ls") || !hasEvent(c, kindThinking, "Listing files") || c.Context != 1000 || c.Session == "" {
		t.Fatalf("conversation: %+v", c)
	}
	e.mustKK("send", id, "exit")
	e.waitFor("exit", id, func(c convView) bool { return c.Status == statusExited })
	e.mustKK("resume", id)
	e.mustKK("send", id, "again")
	c = e.waitFor("answer after resume", id, func(c convView) bool { return hasEvent(c, kindAssistant, "echo: again") })
	argv, _ := os.ReadFile(filepath.Join(e.fake, "argv.log"))
	if !strings.Contains(string(argv), `"resume","`+c.Session+`"`) {
		t.Fatalf("argv: %s", argv)
	}
}

func TestCursorConversation(t *testing.T) {
	e := newEnv(t)
	id := e.mustKK("new", "cursor", "-C", e.work, "hi cursor")
	c := e.waitFor("cursor answer", id, func(c convView) bool { return c.Status == statusIdle && hasEvent(c, kindAssistant, "echo: hi cursor") })
	if !hasEvent(c, kindUser, "hi cursor") || !hasEvent(c, kindTool, "ls") || !c.Unread || c.Session == "" {
		t.Fatalf("conversation: %+v", c)
	}
	e.mustKK("send", id, "line one\nline two")
	e.waitFor("reply", id, func(c convView) bool {
		return c.Status == statusIdle && hasEvent(c, kindAssistant, "echo: line one\nline two")
	})
	e.mustKK("send", id, "exit")
	e.waitFor("exit", id, func(c convView) bool { return c.Status == statusExited })
	e.mustKK("resume", id)
	e.mustKK("send", id, "again")
	e.waitFor("answer after resume", id, func(c convView) bool { return hasEvent(c, kindAssistant, "echo: again") })
	argv, _ := os.ReadFile(filepath.Join(e.fake, "argv.log"))
	if !strings.Contains(string(argv), `"--resume","`+c.Session+`"`) || !strings.Contains(string(argv), `"--plugin-dir"`) {
		t.Fatalf("argv: %s", argv)
	}
}

func TestServerRestartKeepsAgents(t *testing.T) {
	e := newEnv(t)
	id := e.mustKK("new", "claude", "-C", e.work, "before restart")
	e.waitFor("answer", id, func(c convView) bool { return hasEvent(c, kindAssistant, "echo: before restart") })
	e.stop()
	if _, err := e.kk("ls"); err == nil {
		t.Fatal("ls works without a server")
	}
	e.start()
	c := e.waitFor("history after restart", id, func(c convView) bool { return hasEvent(c, kindAssistant, "echo: before restart") })
	if c.Status != statusIdle {
		t.Fatalf("status after restart: %s", c.Status)
	}
	e.mustKK("send", id, "after restart")
	e.waitFor("answer after restart", id, func(c convView) bool { return hasEvent(c, kindAssistant, "echo: after restart") })
	if out, err := e.kk(); err == nil || !strings.Contains(out, "already running") {
		t.Fatalf("second server: %v %s", err, out)
	}
}

func TestWindowNamedLikeSession(t *testing.T) {
	sock := fmt.Sprintf("kk-test-host-%d", time.Now().UnixNano())
	out, err := exec.Command("tmux", "-L", sock, "-f", "/dev/null", "new-session", "-d", "-s", "host", "-n", tmuxSession, "-P", "-F", "#{socket_path},#{pid},0 #{pane_id}", "sleep 600").Output()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { killTmux(t, sock) })
	client, pane, _ := strings.Cut(strings.TrimSpace(string(out)), " ")
	e := newEnv(t, "KK_TMUX="+sock, "TMUX="+client, "TMUX_PANE="+pane)
	// The agents inherit the environment of the tmux server, which this test started instead of kk.
	exec.Command("tmux", "-L", sock, "set-environment", "-g", "FAKE_HOME", e.fake).Run()
	id := e.mustKK("new", "claude", "-C", e.work, "hello")
	e.waitFor("answer", id, func(c convView) bool { return hasEvent(c, kindAssistant, "echo: hello") })
	// The server gives a launch one second before it believes that the window is gone.
	time.Sleep(2 * time.Second)
	if c := e.conv(id); c.Status != statusIdle {
		t.Fatalf("status of a running agent: %s", c.Status)
	}
}

func TestClose(t *testing.T) {
	e := newEnv(t)
	id := e.mustKK("new", "claude", "-C", e.work, "to close")
	e.waitFor("answer", id, func(c convView) bool { return c.Status == statusIdle })
	e.mustKK("close", id)
	if ls := e.mustKK("ls"); strings.Contains(ls, id) {
		t.Fatalf("closed conversation listed: %s", ls)
	}
	if out, _ := e.kk("screen", id); strings.TrimSpace(out) != "" {
		t.Fatalf("window still open: %s", out)
	}
}

func TestClosedStaysReadable(t *testing.T) {
	e := newEnv(t)
	id := e.mustKK("new", "claude", "-C", e.work, "first")
	e.waitFor("answer", id, func(c convView) bool { return c.Status == statusIdle && hasEvent(c, kindAssistant, "echo: first") })
	e.mustKK("close", id)
	e.stop()
	e.start()
	listed := func() (found convView) {
		var list []convView
		_, body := e.request("GET", "/api/conversations?all=1", nil)
		json.Unmarshal(body, &list)
		for _, c := range list {
			if c.ID == id {
				found = c
			}
		}
		return found
	}
	if c := listed(); !c.Closed || c.Unread || time.Since(c.ClosedAt) > time.Minute {
		t.Fatalf("closed conversation in the full list: %+v", c)
	}
	if c := e.conv(id); !hasEvent(c, kindAssistant, "echo: first") {
		t.Fatalf("messages of a closed conversation: %+v", c.Events)
	}
	e.mustKK("resume", id)
	e.waitFor("reopened", id, func(c convView) bool { return c.Status == statusIdle && !c.Closed })
	if ls := e.mustKK("ls"); !strings.Contains(ls, id) || listed().Closed || !listed().ClosedAt.IsZero() {
		t.Fatalf("reopened conversation not listed: %s", ls)
	}
	e.mustKK("send", id, "second")
	e.waitFor("answer after reopening", id, func(c convView) bool {
		return hasEvent(c, kindAssistant, "echo: first") && hasEvent(c, kindAssistant, "echo: second")
	})
}

func TestCreateValidation(t *testing.T) {
	e := newEnv(t)
	for _, args := range [][]string{{"new", "gemini", "x"}, {"new", "claude", "-C", "/nonexistent", "x"}, {"new", "claude"}} {
		if out, err := e.kk(args...); err == nil {
			t.Fatalf("kk %v succeeded: %s", args, out)
		}
	}
}

func TestSkillsPerHarness(t *testing.T) {
	e := newEnv(t)
	write := func(root, dir, name, description string) {
		path := filepath.Join(root, dir, name)
		os.MkdirAll(path, 0o755)
		os.WriteFile(filepath.Join(path, "SKILL.md"), []byte("---\nname: "+name+"\ndescription: "+description+"\n---\nbody\n"), 0o644)
	}
	write(filepath.Dir(e.home), ".claude/skills", "alpha", "User skill for Claude")
	write(e.work, ".agents/skills", "beta", "Project skill for Codex")
	skills := func(harness string) []Skill {
		out, err := exec.Command("curl", "-s", "-G", "--unix-socket", filepath.Join(e.home, "kk.sock"), "http://kk/api/skills",
			"--data-urlencode", "harness="+harness, "--data-urlencode", "dir="+e.work).Output()
		if err != nil {
			t.Fatal(err)
		}
		var list []Skill
		json.Unmarshal(out, &list)
		return list
	}
	if got := skills("claude"); len(got) != 1 || got[0] != (Skill{Name: "alpha", Description: "User skill for Claude", Insert: "/alpha", Scope: "user"}) {
		t.Fatalf("claude skills: %+v", got)
	}
	if got := skills("codex"); len(got) != 1 || got[0] != (Skill{Name: "beta", Description: "Project skill for Codex", Insert: "$beta", Scope: "project"}) {
		t.Fatalf("codex skills: %+v", got)
	}
}

func TestSettings(t *testing.T) {
	e := newEnv(t)
	extra := filepath.Join(e.work, "shared-skills")
	os.MkdirAll(filepath.Join(extra, "gamma"), 0o755)
	os.WriteFile(filepath.Join(extra, "gamma", "SKILL.md"), []byte("---\nname: gamma\ndescription: Shared\n---\n"), 0o644)
	curl := func(args ...string) string {
		out, err := exec.Command("curl", append([]string{"-s", "--unix-socket", filepath.Join(e.home, "kk.sock")}, args...)...).Output()
		if err != nil {
			t.Fatal(err)
		}
		return string(out)
	}
	put := func(body string) string {
		return curl("-X", "PUT", "-d", body, "http://kk/api/settings")
	}
	if out := put(`{"coordinator_model":"gpt"}`); !strings.Contains(out, "unknown konduktor model") {
		t.Fatalf("bad model accepted: %s", out)
	}
	body, _ := json.Marshal(map[string]any{"default_dir": e.work, "skill_dirs": []string{extra}, "start_timeout_seconds": 7})
	var saved settingsView
	json.Unmarshal([]byte(put(string(body))), &saved)
	if saved.Settings.DefaultDir != e.work || saved.Settings.StartTimeoutSeconds != 7 || saved.Env["start_timeout_seconds"] != "KK_START_TIMEOUT" {
		t.Fatalf("saved settings: %+v", saved)
	}
	if out := curl("http://kk/api/dirs"); !strings.HasPrefix(out, `["`+e.work+`"`) {
		t.Fatalf("default directory is not first: %s", out)
	}
	if out := curl("http://kk/api/skills?harness=cursor"); !strings.Contains(out, `"insert":"/gamma","scope":"extra"`) {
		t.Fatalf("extra skill directory not read: %s", out)
	}
	e.stop()
	e.start()
	if out := curl("http://kk/api/settings"); !strings.Contains(out, `"start_timeout_seconds":7`) {
		t.Fatalf("settings not kept across a restart: %s", out)
	}
}

func TestResumeSessionFromHistory(t *testing.T) {
	e := newEnv(t)
	session := "11111111-2222-4333-8444-555555555555"
	project := filepath.Join(filepath.Dir(e.home), ".claude", "projects", "-work")
	os.MkdirAll(project, 0o755)
	lines := `{"type":"user","timestamp":"2026-11-20T10:00:00Z","cwd":"` + e.work + `","message":{"content":"earlier question"}}` + "\n" +
		`{"type":"assistant","timestamp":"2026-11-20T10:30:00Z","message":{"content":[{"type":"text","text":"earlier answer"}]}}` + "\n" +
		`{"type":"ai-title","aiTitle":"Earlier work"}` + "\n"
	os.WriteFile(filepath.Join(project, session+".jsonl"), []byte(lines), 0o644)
	out, err := exec.Command("curl", "-s", "--unix-socket", filepath.Join(e.home, "kk.sock"), "http://kk/api/sessions?harness=claude").Output()
	if err != nil {
		t.Fatal(err)
	}
	var past []PastSession
	json.Unmarshal(out, &past)
	if len(past) != 1 || past[0].ID != session || past[0].Title != "Earlier work" || past[0].Dir != e.work || past[0].Messages != 2 || past[0].Ended.Sub(past[0].Started) != 30*time.Minute {
		t.Fatalf("past sessions: %s", out)
	}
	code, body := e.post("/api/conversations", map[string]string{"harness": "claude", "session": session, "prompt": "continue"})
	var c convView
	json.Unmarshal([]byte(body), &c)
	if code != 200 || c.Session != session || c.Dir != e.work || c.Title != "Earlier work" {
		t.Fatalf("resume: %d %s", code, body)
	}
	e.waitFor("answer in the resumed session", c.ID, func(c convView) bool { return hasEvent(c, kindAssistant, "echo: continue") })
	argv, _ := os.ReadFile(filepath.Join(e.fake, "argv.log"))
	if !strings.Contains(string(argv), `"--resume","`+session+`","--","continue"`) {
		t.Fatalf("argv: %s", argv)
	}
	if code, again := e.post("/api/conversations", map[string]string{"harness": "claude", "session": session}); code != 200 || !strings.Contains(again, `"id":"`+c.ID+`"`) {
		t.Fatalf("opening the same session twice: %d %s", code, again)
	}
}

func TestStatsReportsLoad(t *testing.T) {
	e := newEnv(t)
	var samples []Sample
	for deadline := time.Now().Add(5 * time.Second); len(samples) == 0 && time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		out, _ := exec.Command("curl", "-s", "--unix-socket", filepath.Join(e.home, "kk.sock"), "http://kk/api/stats").Output()
		json.Unmarshal(out, &samples)
	}
	if len(samples) == 0 || samples[0].RAM <= 0 || samples[0].RAM > 100 || samples[0].CPU < 0 || samples[0].CPU > 100 {
		t.Fatalf("samples: %+v", samples)
	}
}

func TestCoordinatorCompacts(t *testing.T) {
	e := newEnv(t, "KK_COORDINATOR=1", "KK_COMPACT_AFTER=2s")
	c := e.waitFor("coordinator start", coordinatorID, func(c convView) bool { return c.Status == statusIdle })
	if c.Dir != e.home {
		t.Fatalf("coordinator dir: %s", c.Dir)
	}
	md, err := os.ReadFile(filepath.Join(e.home, "coordinator.md"))
	if err != nil || !strings.Contains(string(md), "kk new <claude|codex|cursor>") || !strings.Contains(string(md), kkBin) {
		t.Fatalf("coordinator.md: %v %s", err, md)
	}
	argv, _ := os.ReadFile(filepath.Join(e.fake, "argv.log"))
	if !strings.Contains(string(argv), `"--append-system-prompt-file","`+filepath.Join(e.home, "coordinator.md")+`"`) {
		t.Fatalf("argv: %s", argv)
	}
	if ls := e.mustKK("ls"); strings.Contains(ls, coordinatorID) {
		t.Fatalf("coordinator listed as a conversation: %s", ls)
	}

	e.mustKK("send", coordinatorID, "small talk")
	e.waitFor("answer", coordinatorID, func(c convView) bool { return hasEvent(c, kindAssistant, "echo: small talk") })
	time.Sleep(3 * time.Second)
	if c := e.conv(coordinatorID); c.Compacted {
		t.Fatal("compacted below the token threshold")
	}
	e.mustKK("send", coordinatorID, "big context")
	e.waitFor("big answer", coordinatorID, func(c convView) bool { return c.Context == 60000 })
	// The note shows from the transcript as soon as it is read, the context only once the server's tick has stored it.
	e.waitFor("compaction to 3000 tokens", coordinatorID, func(c convView) bool {
		return hasEvent(c, kindNote, "Conversation compacted") && c.Context == 3000
	})

	e.stop()
	e.start()
	e.waitFor("coordinator after restart", coordinatorID, func(c convView) bool { return c.Status == statusIdle })
	if n := strings.Count(e.mustKK("screen", coordinatorID), "fake claude started"); n != 1 {
		t.Fatalf("coordinator restarted with the server: %d starts", n)
	}
}

func TestKonduktorTitleMigrates(t *testing.T) {
	e := newEnv(t, "KK_COORDINATOR=1")
	e.waitFor("konduktor start", coordinatorID, func(c convView) bool { return c.Status == statusIdle && c.Title == "konduktor" })
	e.stop()
	path := filepath.Join(e.home, "conversations", coordinatorID+".yaml")
	data, _ := os.ReadFile(path)
	os.WriteFile(path, []byte(strings.Replace(string(data), "title: konduktor", "title: Conductor", 1)), 0o600)
	e.start()
	if show := e.mustKK("show", coordinatorID); !strings.HasPrefix(show, "konduktor · claude") {
		t.Fatalf("show: %s", show)
	}
}

func TestWebAuth(t *testing.T) {
	e := newEnv(t)
	base := "http://" + e.addr
	resp, err := http.Get(base + "/api/conversations")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("without token: %d", resp.StatusCode)
	}
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	resp, err = client.Get(e.url)
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(page), "<script") {
		t.Fatalf("page: %d %s", resp.StatusCode, page)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-cache" {
		t.Fatalf("the page may be served from a cache after an upgrade: Cache-Control %q", cc)
	}
	resp, _ = client.Get(base + "/api/conversations")
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("with cookie: %d", resp.StatusCode)
	}
	req, _ := http.NewRequest("POST", base+"/api/conversations", strings.NewReader(`{}`))
	req.Header.Set("Origin", "http://evil.example")
	resp, _ = client.Do(req)
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("cross-origin write: %d", resp.StatusCode)
	}
}

func TestTerminal(t *testing.T) {
	e := newEnv(t)
	id := e.mustKK("new", "claude", "-C", e.work, "hello")
	e.waitFor("first answer", id, func(c convView) bool { return c.Status == statusIdle })
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	resp, err := client.Get(e.url)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	wsURL := "ws://" + e.addr + "/api/conversations/" + id + "/terminal"
	if _, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPClient: client, HTTPHeader: http.Header{"Origin": {"http://evil.example"}}}); err == nil {
		t.Fatal("cross-origin terminal accepted")
	}
	ws, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPClient: client})
	if err != nil {
		t.Fatal(err)
	}
	ws.Write(ctx, websocket.MessageText, []byte(`{"cols":100,"rows":30}`))
	var seen strings.Builder
	for !strings.Contains(seen.String(), "echo: hello") {
		_, data, err := ws.Read(ctx)
		if err != nil {
			t.Fatalf("terminal output: %v\n%q", err, seen.String())
		}
		seen.Write(data)
	}
	ws.Write(ctx, websocket.MessageBinary, []byte("typed in browser\r"))
	e.waitFor("typed reply", id, func(c convView) bool { return hasEvent(c, kindAssistant, "echo: typed in browser") })
	ws.Close(websocket.StatusNormalClosure, "")
	deadline := time.Now().Add(5 * time.Second)
	for {
		out, _ := exec.Command("tmux", "-L", strings.TrimPrefix(e.vars[2], "KK_TMUX="), "list-sessions", "-F", "#{session_name}").Output()
		if strings.TrimSpace(string(out)) == "kk" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("viewer session left behind: %s", out)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestRewind(t *testing.T) {
	for _, tc := range []struct {
		name, harness, target string
		files                 bool
		log                   string
	}{
		{"claude", "claude", "edit two\nsecond line", false, "claude: Restore conversation: edit two\n"},
		{"claude files", "claude", "edit two\nsecond line", true, "claude: Restore code and conversation: edit two\n"},
		{"claude none", "claude", "three", true, "claude: Restore conversation: three\n"},
		{"cursor", "cursor", "edit two\nsecond line", false, "cursor-agent: Restore conversation: edit two\n"},
		{"cursor files", "cursor", "edit two\nsecond line", true, "cursor-agent: Restore files and conversation: edit two\n"},
		{"codex", "codex", "edit two\nsecond line", false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			id := e.mustKK("new", tc.harness, "-C", e.work, "one")
			answered := func(text string) convView {
				return e.waitFor("answer to "+text, id, func(c convView) bool { return c.Status == statusIdle && hasEvent(c, kindAssistant, "echo: "+text) })
			}
			answered("one")
			for _, text := range []string{"edit two\nsecond line", "three"} {
				e.mustKK("send", id, text)
				answered(text)
			}
			c := e.conv(id)
			target := slices.IndexFunc(c.Events, func(ev Event) bool { return ev.Kind == kindUser && ev.Text == tc.target })
			if code, body := e.post("/api/conversations/"+id+"/rewind", map[string]any{"event": target + 1, "text": "x"}); code != 400 {
				t.Fatalf("rewind to an event that is not a message: %d %s", code, body)
			}
			if code, body := e.post("/api/conversations/"+id+"/rewind", map[string]any{"event": target, "was": "stale", "text": "x"}); code != 409 {
				t.Fatalf("rewind from a stale view: %d %s", code, body)
			}
			if code, body := e.post("/api/conversations/"+id+"/rewind", map[string]any{"event": target, "was": tc.target, "text": "changed\nmessage", "files": tc.files}); code != 200 {
				screen, _ := e.kk("screen", id)
				t.Fatalf("rewind: %d %s\n%s", code, body, screen)
			}
			c = answered("changed\nmessage")
			want := []string{"one", "edit two\nsecond line", "changed\nmessage"}
			if tc.target != "three" {
				want = []string{"one", "changed\nmessage"}
			}
			var users []string
			for _, ev := range c.Events {
				if ev.Kind == kindUser {
					users = append(users, ev.Text)
				}
			}
			if !slices.Equal(users, want) || hasEvent(c, kindAssistant, "echo: "+tc.target) || !hasEvent(c, kindAssistant, "echo: one") {
				t.Fatalf("events after the rewind: %+v", c.Events)
			}
			if log, _ := os.ReadFile(filepath.Join(e.fake, "rewind.log")); string(log) != tc.log {
				t.Fatalf("rewind.log: %q", log)
			}
		})
	}
}

func TestRewindSaysWhatIsRestored(t *testing.T) {
	e := newEnv(t)
	var hs []Harness
	out, _ := exec.Command("curl", "-s", "--unix-socket", filepath.Join(e.home, "kk.sock"), "http://kk/api/harnesses").Output()
	json.Unmarshal(out, &hs)
	for _, h := range hs {
		if h.Rewind == nil || h.Rewind.Conversation == "" || (h.Rewind.Files == "") != (h.Name == "codex") {
			t.Fatalf("%s: %+v", h.Name, h.Rewind)
		}
	}
	id := e.mustKK("new", "codex", "-C", e.work, "one")
	e.waitFor("answer", id, func(c convView) bool { return c.Status == statusIdle && hasEvent(c, kindAssistant, "echo: one") })
	if code, body := e.post("/api/conversations/"+id+"/rewind", map[string]any{"event": 0, "was": "one", "text": "x", "files": true}); code != 400 || !strings.Contains(body, "cannot restore files") {
		t.Fatalf("restoring files with codex: %d %s", code, body)
	}
}

func (e *env) request(method, path string, body io.Reader) (*http.Response, []byte) {
	e.t.Helper()
	req, _ := http.NewRequest(method, "http://kk"+path, body)
	t := &http.Transport{Dial: func(_, _ string) (net.Conn, error) { return net.Dial("unix", filepath.Join(e.home, "kk.sock")) }}
	resp, err := (&http.Client{Transport: t}).Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp, out
}

func (e *env) upload(name string, data []byte) Attachment {
	e.t.Helper()
	resp, out := e.request("POST", "/api/attachments?name="+url.QueryEscape(name), bytes.NewReader(data))
	var a Attachment
	if json.Unmarshal(out, &a); resp.StatusCode != 200 || a.Path == "" {
		e.t.Fatalf("upload %s: %d %s", name, resp.StatusCode, out)
	}
	return a
}

func TestAttachments(t *testing.T) {
	e := newEnv(t)
	png := []byte("\x89PNG\r\n\x1a\nnot really a picture")
	shot := e.upload("../my shot (1).png", png)
	if shot.Name != "my-shot-1-.png" || filepath.Dir(filepath.Dir(shot.Path)) != filepath.Join(e.home, "attachments") {
		t.Fatalf("attachment: %+v", shot)
	}
	if saved, _ := os.ReadFile(shot.Path); !bytes.Equal(saved, png) {
		t.Fatalf("saved file: %q", saved)
	}
	resp, body := e.request("GET", shot.URL, nil)
	if !bytes.Equal(body, png) || resp.Header.Get("Content-Type") != "image/png" || resp.Header.Get("X-Content-Type-Options") != "nosniff" || resp.Header.Get("Content-Disposition") != "" {
		t.Fatalf("image: %v %q", resp.Header, body)
	}

	page := e.upload("page.html", []byte("<script>alert(1)</script>"))
	if resp, _ := e.request("GET", page.URL, nil); !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/plain") {
		t.Fatalf("an uploaded page is served as %q", resp.Header.Get("Content-Type"))
	}
	svg := e.upload("drawing.svg", []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`))
	if resp, _ := e.request("GET", svg.URL, nil); resp.Header.Get("Content-Type") != "image/svg+xml" || resp.Header.Get("Content-Security-Policy") != "sandbox" {
		t.Fatalf("svg: %v", resp.Header)
	}
	archive := e.upload("data.bin", []byte{0, 1, 2, 3})
	if resp, _ := e.request("GET", archive.URL, nil); !strings.HasPrefix(resp.Header.Get("Content-Disposition"), "attachment") {
		t.Fatalf("binary: %v", resp.Header)
	}

	os.WriteFile(filepath.Join(e.home, "attachments", "secret"), []byte("secret"), 0o600)
	for _, path := range []string{"/api/attachments/%2e%2e/token", strings.TrimSuffix(shot.URL, shot.Name) + "%2e%2e%2fsecret", "/api/attachments/nope/" + shot.Name} {
		if resp, body := e.request("GET", path, nil); resp.StatusCode != 404 {
			t.Fatalf("GET %s: %d %s", path, resp.StatusCode, body)
		}
	}
	if resp, _ := e.request("POST", "/api/attachments?name=empty.txt", nil); resp.StatusCode != 400 {
		t.Fatalf("empty upload: %d", resp.StatusCode)
	}
	if resp, _ := e.request("POST", "/api/attachments?name=huge.bin", io.LimitReader(zeros{}, maxAttachment+1)); resp.StatusCode != 413 {
		t.Fatalf("oversized upload: %d", resp.StatusCode)
	}
	if left, _ := os.ReadDir(filepath.Join(e.home, "attachments")); len(left) != 5 {
		t.Fatalf("rejected uploads left files behind: %v", left)
	}

	for _, harness := range []string{"claude", "codex", "cursor"} {
		id := e.mustKK("new", harness, "-C", e.work, "look\n\n"+shot.Path)
		c := e.waitFor(harness+" first answer", id, func(c convView) bool { return c.Status == statusIdle && len(c.Events) > 1 })
		e.mustKK("send", id, "and these\n\n"+shot.Path+"\n"+page.Path)
		c = e.waitFor(harness+" reply", id, func(c convView) bool { return hasEvent(c, kindUser, "and these") })
		var got []string
		for _, ev := range c.Events {
			if ev.Kind == kindUser {
				for _, a := range ev.Attachments {
					got = append(got, ev.Text+":"+a.Name+":"+a.URL)
				}
			}
		}
		if show := e.mustKK("show", id); !strings.Contains(show, "[attachment] "+page.Path) {
			t.Fatalf("%s show: %s", harness, show)
		}
		// A message queued behind another reaches the agent glued to the last path of the first.
		e.mustKK("send", id, "first\n\n"+shot.Path+"second\n\n"+page.Path+"third")
		var glued Event
		e.waitFor(harness+" glued", id, func(c convView) bool {
			for _, ev := range c.Events {
				if ev.Kind == kindUser && strings.HasPrefix(ev.Text, "first") {
					glued = ev
				}
			}
			return glued.Kind != ""
		})
		if glued.Text != "first\n\nsecond\n\nthird" || len(glued.Attachments) != 2 || glued.Attachments[0].URL != shot.URL || glued.Attachments[1].Name != page.Name {
			t.Fatalf("%s glued message: %+v", harness, glued)
		}
		want := []string{"look:" + shot.Name + ":" + shot.URL, "and these:" + shot.Name + ":" + shot.URL, "and these:" + page.Name + ":" + page.URL}
		if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Fatalf("%s attachments: %v\nevents: %+v", harness, got, c.Events)
		}
	}
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

func TestWorktreeSession(t *testing.T) {
	e := newEnv(t)
	git := func(dir string, args ...string) string {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=kk", "-c", "user.email=kk@example.com"}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	repoInfo := func(dir string) string {
		out, _ := exec.Command("curl", "-s", "--unix-socket", filepath.Join(e.home, "kk.sock"), "http://kk/api/repo?dir="+dir).Output()
		return strings.TrimSpace(string(out))
	}
	if got := repoInfo(e.work); got != `{"git":false,"branch":""}` {
		t.Fatalf("repo info outside git: %s", got)
	}
	if out, err := e.kk("new", "claude", "-w", "-C", e.work, "no repo"); err == nil || !strings.Contains(out, "not a git repository") {
		t.Fatalf("worktree outside git: %v %s", err, out)
	}
	git(e.work, "init", "-q", "-b", "trunk")
	os.MkdirAll(filepath.Join(e.work, "sub"), 0o755)
	os.WriteFile(filepath.Join(e.work, "sub", "file"), []byte("x"), 0o644)
	git(e.work, "add", ".")
	git(e.work, "commit", "-q", "-m", "first")
	if got := repoInfo(e.work); got != `{"git":true,"branch":"trunk"}` {
		t.Fatalf("repo info: %s", got)
	}

	id := e.mustKK("new", "claude", "-w", "-C", filepath.Join(e.work, "sub"), "Fix the login page")
	c := e.waitFor("answer", id, func(c convView) bool { return hasEvent(c, kindAssistant, "echo: Fix the login page") })
	branch := "fix-the-login-page-" + id
	if got := git(c.Dir, "branch", "--show-current"); got != branch {
		t.Fatalf("branch in %s: %s", c.Dir, got)
	}
	if filepath.Base(c.Dir) != "sub" || git(c.Dir, "rev-parse", "HEAD") != git(e.work, "rev-parse", "HEAD") {
		t.Fatalf("worktree dir %s is not the chosen subdirectory at the repository's commit", c.Dir)
	}
	if git(c.Dir, "rev-parse", "--git-dir") == git(e.work, "rev-parse", "--git-dir") || git(e.work, "branch", "--show-current") != "trunk" {
		t.Fatalf("the agent runs in the chosen checkout, not in a worktree: %s", c.Dir)
	}
	if git(e.work, "status", "--porcelain") != "" {
		t.Fatalf("worktree shows in the repository's status: %s", git(e.work, "status", "--porcelain"))
	}
	if cwd, _ := os.ReadFile(filepath.Join(e.fake, "cwd.log")); strings.TrimSpace(string(cwd)) != filepath.Join(git(c.Dir, "rev-parse", "--show-toplevel"), "sub") {
		t.Fatalf("agent cwd: %s", cwd)
	}
	if c.Project != filepath.Join(e.work, "sub") {
		t.Fatalf("project of a worktree session: %s", c.Project)
	}
	plain := e.mustKK("new", "claude", "-C", e.work, "hello")
	if p := e.conv(plain).Project; p != e.work {
		t.Fatalf("project of a session in the chosen directory: %s", p)
	}
	// A session resumed from history runs in the worktree and carries no repo. Git names the worktree by its real path.
	if p := (&Conversation{Dir: c.Dir}).Project(); p != filepath.Join(git(e.work, "rev-parse", "--show-toplevel"), "sub") {
		t.Fatalf("project of a session resumed in a worktree: %s", p)
	}
	out, _ := exec.Command("curl", "-s", "--unix-socket", filepath.Join(e.home, "kk.sock"), "http://kk/api/dirs").Output()
	if strings.Contains(string(out), "worktrees") || !strings.Contains(string(out), filepath.Join(e.work, "sub")) {
		t.Fatalf("recent dirs: %s", out)
	}
	if code, body := e.post("/api/conversations", map[string]any{"harness": "claude", "session": "x", "worktree": true, "dir": e.work}); code != 400 {
		t.Fatalf("worktree with an earlier session: %d %s", code, body)
	}
}

func TestRewindKeepsAttachments(t *testing.T) {
	e := newEnv(t)
	file := e.upload("notes.txt", []byte("notes"))
	id := e.mustKK("new", "claude", "-C", e.work, "one")
	e.waitFor("answer", id, func(c convView) bool { return c.Status == statusIdle && hasEvent(c, kindAssistant, "echo: one") })
	e.mustKK("send", id, "look\n\n"+file.Path)
	c := e.waitFor("answer with a file", id, func(c convView) bool { return c.Status == statusIdle && hasEvent(c, kindUser, "look") })
	target := slices.IndexFunc(c.Events, func(ev Event) bool { return ev.Kind == kindUser && ev.Text == "look" })
	if code, body := e.post("/api/conversations/"+id+"/rewind", map[string]any{"event": target, "was": "look", "text": "look again\n\n" + file.Path}); code != 200 {
		t.Fatalf("rewind: %d %s", code, body)
	}
	c = e.waitFor("answer to the edited message", id, func(c convView) bool { return c.Status == statusIdle && hasEvent(c, kindUser, "look again") })
	if ev := c.Events[target]; len(ev.Attachments) != 1 || ev.Attachments[0].Path != file.Path || hasEvent(c, kindUser, "look") {
		t.Fatalf("events after the rewind: %+v", c.Events)
	}
}

var allHarnesses = []string{"claude", "codex", "cursor"}

func TestInterrupt(t *testing.T) {
	e := newEnv(t)
	for _, h := range allHarnesses {
		id := e.mustKK("new", h, "-C", e.work, "slow task")
		c := e.waitFor(h+" working", id, func(c convView) bool { return c.Status == statusWorking && hasEvent(c, kindUser, "slow task") })
		if strings.Join(c.Actions, ",") != "interrupt,stop" {
			t.Fatalf("%s actions while working: %v", h, c.Actions)
		}
		e.mustKK("interrupt", id)
		c = e.waitFor(h+" interrupted", id, func(c convView) bool { return c.Status == statusIdle && hasEvent(c, kindNote, "Interrupted") })
		// kk cannot tell an idle Cursor from one that works without tools, so its interrupt stays available.
		if want := map[bool]string{false: "reset,stop", true: "interrupt,reset,stop"}[h == "cursor"]; strings.Join(c.Actions, ",") != want {
			t.Fatalf("%s actions while idle: %v", h, c.Actions)
		}
		if h != "cursor" {
			if out, err := e.kk("interrupt", id); err == nil {
				t.Fatalf("%s: interrupt of an idle agent accepted: %s", h, out)
			}
		}
		e.mustKK("send", id, "after")
		e.waitFor(h+" answer after the interrupt", id, func(c convView) bool {
			return c.Status == statusIdle && hasEvent(c, kindAssistant, "echo: after")
		})
	}
}

func TestStop(t *testing.T) {
	e := newEnv(t)
	for _, h := range allHarnesses {
		id := e.mustKK("new", h, "-C", e.work, "slow task")
		c := e.waitFor(h+" working", id, func(c convView) bool { return c.Status == statusWorking && hasEvent(c, kindUser, "slow task") })
		session := c.Session
		e.mustKK("stop", id)
		c = e.waitFor(h+" stopped", id, func(c convView) bool { return c.Status == statusExited })
		if len(c.Actions) != 0 || !hasEvent(c, kindUser, "slow task") {
			t.Fatalf("%s after stop: %v %+v", h, c.Actions, c.Events)
		}
		if ls := e.mustKK("ls"); !strings.Contains(ls, id) {
			t.Fatalf("%s: stopped conversation not listed: %s", h, ls)
		}
		if screen := e.mustKK("screen", id); !strings.Contains(screen, "slow task") {
			t.Fatalf("%s: the window with the last screen is gone: %s", h, screen)
		}
		if out, err := e.kk("stop", id); err == nil {
			t.Fatalf("%s: stop of an exited agent accepted: %s", h, out)
		}
		e.mustKK("resume", id)
		e.waitFor(h+" resumed", id, func(c convView) bool { return c.Status == statusIdle })
		e.mustKK("send", id, "second")
		c = e.waitFor(h+" answer after resume", id, func(c convView) bool { return hasEvent(c, kindAssistant, "echo: second") })
		if !hasEvent(c, kindUser, "slow task") || c.Session != session {
			t.Fatalf("%s: history lost after stop and resume: %+v", h, c)
		}
	}
}

func TestReset(t *testing.T) {
	e := newEnv(t)
	lastArgv := func() string {
		argv, _ := os.ReadFile(filepath.Join(e.fake, "argv.log"))
		lines := strings.Split(strings.TrimSpace(string(argv)), "\n")
		return lines[len(lines)-1]
	}
	for _, h := range allHarnesses {
		id := e.mustKK("new", h, "-C", e.work, "first")
		c := e.waitFor(h+" first answer", id, func(c convView) bool { return c.Status == statusIdle && hasEvent(c, kindAssistant, "echo: first") })
		old := c.Session
		e.mustKK("reset", id)
		if c = e.conv(id); c.Session == old || len(c.Events) != 0 || c.Context != 0 {
			t.Fatalf("%s after reset: %+v", h, c)
		}
		e.waitFor(h+" ready after reset", id, func(c convView) bool { return c.Status == statusIdle })
		e.mustKK("send", id, "second")
		c = e.waitFor(h+" answer after reset", id, func(c convView) bool { return c.Status == statusIdle && hasEvent(c, kindAssistant, "echo: second") })
		fresh := c.Session
		if hasEvent(c, kindAssistant, "echo: first") || fresh == "" || fresh == old {
			t.Fatalf("%s: old conversation still shown after reset: %+v", h, c)
		}
		e.mustKK("stop", id)
		e.waitFor(h+" stopped", id, func(c convView) bool { return c.Status == statusExited })
		e.mustKK("resume", id)
		// A resumed Cursor counts as started at once, so the agent's start is awaited by its arguments.
		e.waitFor(h+" resumed in the new session", id, func(c convView) bool {
			return c.Status == statusIdle && strings.Contains(lastArgv(), `esume","`+fresh+`"`)
		})
	}

	id := e.mustKK("new", "claude", "-C", e.work, "slow task")
	e.waitFor("working", id, func(c convView) bool { return c.Status == statusWorking })
	if out, err := e.kk("reset", id); err == nil {
		t.Fatalf("reset of a working agent accepted: %s", out)
	}
	e.mustKK("interrupt", id)
	old := e.waitFor("interrupted", id, func(c convView) bool { return c.Status == statusIdle }).Session
	e.mustKK("reset", id)
	e.waitFor("new session", id, func(c convView) bool { return c.Session != old })
	e.mustKK("stop", id)
	e.waitFor("stopped", id, func(c convView) bool { return c.Status == statusExited })
	e.mustKK("resume", id)
	e.waitFor("restarted", id, func(c convView) bool { return c.Status == statusIdle })
	if argv := lastArgv(); strings.Contains(argv, "slow task") {
		t.Fatalf("the first prompt was sent again after a reset: %s", argv)
	}
}

func userMessages(events []Event) []string {
	var out []string
	for _, ev := range events {
		if ev.Kind == kindUser {
			line := ev.Text
			for _, a := range ev.Attachments {
				line += " [" + a.Name + "]"
			}
			out = append(out, line)
		}
	}
	return out
}

func TestMessagesStaySeparate(t *testing.T) {
	e := newEnv(t)
	shot := e.upload("shot.png", []byte("\x89PNG"))
	id := e.mustKK("new", "claude", "-C", e.work, "hello")
	e.waitFor("first answer", id, func(c convView) bool { return c.Status == statusIdle && len(c.Events) > 1 })

	e.mustKK("send", id, "first\n\n"+shot.Path)
	e.mustKK("send", id, "second")
	c := e.waitFor("the answer to the second message", id, func(c convView) bool {
		return c.Status == statusIdle && slices.ContainsFunc(c.Events, func(ev Event) bool { return ev.Kind == kindAssistant && strings.Contains(ev.Text, "second") })
	})
	if got, want := userMessages(c.Events), []string{"hello", "first [shot.png]", "second"}; !slices.Equal(got, want) {
		t.Fatalf("messages: %q, want %q", got, want)
	}

	sent := make(chan string)
	for _, text := range []string{"alpha", "beta", "gamma"} {
		go func() {
			_, out := e.post("/api/conversations/"+id+"/send", map[string]string{"text": text})
			sent <- out
		}()
	}
	for range 3 {
		<-sent
	}
	c = e.waitFor("the three messages sent at once", id, func(c convView) bool { return c.Status == statusIdle && len(userMessages(c.Events)) == 6 })
	got := userMessages(c.Events)[3:]
	slices.Sort(got)
	if want := []string{"alpha", "beta", "gamma"}; !slices.Equal(got, want) {
		t.Fatalf("messages sent at once: %q, want %q", got, want)
	}
}

func TestQueuedMessages(t *testing.T) {
	e := newEnv(t)
	shot := e.upload("shot.png", []byte("\x89PNG"))
	id := e.mustKK("new", "claude", "-C", e.work, "slow task")
	e.waitFor("working", id, func(c convView) bool { return c.Status == statusWorking && hasEvent(c, kindUser, "slow task") })

	e.mustKK("send", id, "one\n\n"+shot.Path)
	e.mustKK("send", id, "two")
	c := e.waitFor("two queued messages", id, func(c convView) bool { return len(c.Queued) == 2 })
	if got, want := userMessages(c.Queued), []string{"one [shot.png]", "two"}; !slices.Equal(got, want) {
		t.Fatalf("queued: %q, want %q", got, want)
	}
	if got, want := userMessages(c.Events), []string{"slow task"}; !slices.Equal(got, want) {
		t.Fatalf("messages while queued: %q, want %q", got, want)
	}

	e.mustKK("send", id, "boundary")
	c = e.waitFor("the agent to take the queue", id, func(c convView) bool { return len(c.Queued) == 0 && hasEvent(c, kindUser, "boundary") })
	if got, want := userMessages(c.Events), []string{"slow task", "one [shot.png]", "two", "boundary"}; !slices.Equal(got, want) {
		t.Fatalf("messages: %q, want %q", got, want)
	}

	e.mustKK("send", id, "left behind")
	e.waitFor("a queued message", id, func(c convView) bool { return len(c.Queued) == 1 })
	e.mustKK("stop", id)
	e.mustKK("resume", id)
	e.waitFor("the resumed agent", id, func(c convView) bool { return c.Status == statusIdle })
	e.mustKK("send", id, "slow again")
	c = e.waitFor("working again", id, func(c convView) bool { return c.Status == statusWorking && hasEvent(c, kindUser, "slow again") })
	if len(c.Queued) != 0 {
		t.Fatalf("a queue lost with the agent is still shown: %q", userMessages(c.Queued))
	}
}

func TestWhoseTurn(t *testing.T) {
	e := newEnv(t, "KK_COORDINATOR=1")
	e.waitFor("konduktor start", coordinatorID, func(c convView) bool { return c.Status == statusIdle })
	if c := e.conv(coordinatorID); c.WaitingFor != waitingUser {
		t.Fatalf("the konduktor waits for %q", c.WaitingFor)
	}
	if out, err := e.kk("escalate", coordinatorID, "why"); err == nil {
		t.Fatalf("the konduktor escalated: %s", out)
	}

	id := e.asKonduktor("new", "claude", "-C", e.work, "hello")
	c := e.waitFor("a stop that waits for the konduktor", id, func(c convView) bool { return c.Status == statusIdle && c.WaitingFor == waitingKonduktor })
	if ls := e.mustKK("ls"); !strings.Contains(ls, "idle             konduktor") {
		t.Fatalf("ls: %s", ls)
	}
	if show := e.mustKK("show", id); !strings.Contains(show, "Waiting for the konduktor") {
		t.Fatalf("show: %s", show)
	}
	if out, err := e.kk("escalate", id, " "); err == nil {
		t.Fatalf("escalated without a reason: %s", out)
	}
	e.asKonduktor("escalate", id, "Pick a database:", "Postgres or SQLite?\nmore")
	c = e.conv(id)
	if c.WaitingFor != waitingUser || c.Escalation != "Pick a database: Postgres or SQLite?" {
		t.Fatalf("after escalate: %q %q", c.WaitingFor, c.Escalation)
	}
	if show := e.mustKK("show", id); !strings.Contains(show, "Waiting for the user") || !strings.Contains(show, "Escalated to the user: Pick a database") {
		t.Fatalf("show: %s", show)
	}
	e.stop()
	e.start()
	if c = e.conv(id); c.WaitingFor != waitingUser || c.Escalation == "" {
		t.Fatalf("escalation lost in a restart: %q %q", c.WaitingFor, c.Escalation)
	}

	e.asKonduktor("send", id, "slow work")
	e.waitFor("work that clears the escalation", id, func(c convView) bool { return c.Status == statusWorking && c.WaitingFor == "" && c.Escalation == "" })
	if out, err := e.kk("escalate", id, "too early"); err == nil {
		t.Fatalf("escalated a working agent: %s", out)
	}
	e.asKonduktor("interrupt", id)
	e.waitFor("back to the konduktor", id, func(c convView) bool { return c.Status == statusIdle && c.WaitingFor == waitingKonduktor })

	e.mustKK("send", id, "from the user")
	e.waitFor("a stop that waits for the user who wrote", id, func(c convView) bool {
		return hasEvent(c, kindAssistant, "echo: from the user") && c.Status == statusIdle && c.WaitingFor == waitingUser
	})

	// A permission prompt waits on a working agent; an escalation there lasts until the agent goes on.
	e.asKonduktor("send", id, "ask for permission")
	e.waitFor("attention for the konduktor", id, func(c convView) bool { return c.Attention != "" && c.WaitingFor == waitingKonduktor })
	e.asKonduktor("escalate", id, "Allow Bash?")
	time.Sleep(1500 * time.Millisecond)
	if c = e.conv(id); c.Escalation != "Allow Bash?" || c.WaitingFor != waitingUser {
		t.Fatalf("escalation on attention: %q %q", c.WaitingFor, c.Escalation)
	}
	e.mustKK("keys", id, "1", "Enter")
	e.waitFor("the answer the user allowed", id, func(c convView) bool {
		return hasEvent(c, kindAssistant, "echo: ask for permission") && c.Status == statusIdle && c.Escalation == "" && c.WaitingFor == waitingUser
	})

	codex := e.mustKK("new", "codex", "-C", e.work, "hi codex")
	e.waitFor("a codex stop for the user who started it", codex, func(c convView) bool { return c.Status == statusIdle && c.WaitingFor == waitingUser })
	e.asKonduktor("send", codex, "again")
	e.waitFor("a codex stop for the konduktor", codex, func(c convView) bool {
		return hasEvent(c, kindAssistant, "echo: again") && c.Status == statusIdle && c.WaitingFor == waitingKonduktor
	})
	e.asKonduktor("escalate", codex, "Review the plan")
	e.asKonduktor("send", codex, "exit")
	c = e.waitFor("exit", codex, func(c convView) bool { return c.Status == statusExited })
	if c.WaitingFor != waitingKonduktor || c.Escalation != "" {
		t.Fatalf("after exit: %q %q", c.WaitingFor, c.Escalation)
	}
	e.mustKK("resume", codex)
	e.waitFor("a resume by the user that waits for the user", codex, func(c convView) bool { return c.Status == statusIdle && c.WaitingFor == waitingUser })
}

func TestNoKonduktorTurn(t *testing.T) {
	e := newEnv(t)
	id := e.asKonduktor("new", "claude", "-C", e.work, "hello")
	e.waitFor("a stop for the user", id, func(c convView) bool { return c.Status == statusIdle && c.WaitingFor == waitingUser })
	code, body := e.post("/api/conversations", map[string]any{"harness": "claude", "dir": e.work, "prompt": "hello", "conductor": true})
	var handed convView
	if err := json.Unmarshal([]byte(body), &handed); code != 200 || err != nil {
		t.Fatalf("create: %d %s", code, body)
	}
	e.waitFor("a handed over stop for the user", handed.ID, func(c convView) bool { return c.Status == statusIdle && c.WaitingFor == waitingUser })
}

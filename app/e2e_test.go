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
	"os"
	"os/exec"
	"path/filepath"
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
	for _, name := range []string{"claude", "codex", "cursor-agent"} {
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
	}, extra...)
	if !hasVar(extra, "KK_NO_COORDINATOR") && !hasVar(extra, "KK_COORDINATOR") {
		e.vars = append(e.vars, "KK_NO_COORDINATOR=1")
	}
	t.Cleanup(func() {
		e.stop()
		exec.Command("tmux", "-L", sock, "kill-server").Run()
	})
	e.start()
	return e
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
	c := e.waitFor("first answer", id, func(c convView) bool { return c.Status == statusIdle && hasEvent(c, kindAssistant, "echo: hello world again today") })
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
	c = e.waitFor("answer after permission", id, func(c convView) bool { return c.Attention == "" && c.Status == statusIdle && hasEvent(c, kindAssistant, "echo: ask for permission") })
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
	e.waitFor("reply", id, func(c convView) bool { return c.Status == statusIdle && hasEvent(c, kindAssistant, "echo: line one\nline two") })
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

func TestCreateValidation(t *testing.T) {
	e := newEnv(t)
	for _, args := range [][]string{{"new", "gemini", "x"}, {"new", "claude", "-C", "/nonexistent", "x"}, {"new", "claude"}} {
		if out, err := e.kk(args...); err == nil {
			t.Fatalf("kk %v succeeded: %s", args, out)
		}
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
	c = e.waitFor("compaction", coordinatorID, func(c convView) bool { return hasEvent(c, kindNote, "Conversation compacted") })
	if c.Context != 3000 {
		t.Fatalf("context after compaction: %d", c.Context)
	}

	e.stop()
	e.start()
	e.waitFor("coordinator after restart", coordinatorID, func(c convView) bool { return c.Status == statusIdle })
	if n := strings.Count(e.mustKK("screen", coordinatorID), "fake claude started"); n != 1 {
		t.Fatalf("coordinator restarted with the server: %d starts", n)
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

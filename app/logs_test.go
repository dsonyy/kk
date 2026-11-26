package main

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"net/http/cookiejar"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func (e *env) log() string {
	data, _ := os.ReadFile(filepath.Join(e.home, "kk.log"))
	return string(data)
}

func (e *env) waitLog(what string, ok func(log string) bool) string {
	e.t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if l := e.log(); ok(l) {
			return l
		}
	}
	e.t.Fatalf("log has no %s:\n%s", what, e.log())
	return ""
}

func TestLogs(t *testing.T) {
	e := newEnv(t)
	id := e.mustKK("new", "claude", "-C", e.work, "secret prompt words")
	e.waitFor("first answer", id, func(c convView) bool { return c.Status == statusIdle })
	e.mustKK("send", id, "secret follow up")
	e.waitFor("second answer", id, func(c convView) bool { return hasEvent(c, kindAssistant, "echo: secret follow up") })
	exec.Command("curl", "-s", "--unix-socket", filepath.Join(e.home, "kk.sock"), "-X", "PUT", "-d", `{"start_timeout_seconds":9}`, "http://kk/api/settings").Run()
	e.post("/api/logs", map[string]string{"error": "TypeError: x is undefined\nat main.js:1"})
	e.asKonduktor("group", id, "Log tab")
	log := e.waitLog("events", func(l string) bool {
		for _, want := range []string{
			"kk started on " + e.addr,
			"POST /api/conversations by user",
			"conversation " + id + " created: claude in " + e.work,
			"hook SessionStart " + id,
			"hook UserPromptSubmit " + id,
			"conversation " + id + ": starting -> idle",
			"conversation " + id + ": idle -> working",
			"tmux new-window kk:",
			"tmux paste-buffer @",
			"POST /api/conversations/" + id + "/send by user",
			"settings changed: start_timeout_seconds",
			"web client error: TypeError: x is undefined at main.js:1",
			"POST /api/conversations/" + id + "/group by agent coordinator",
			"grouping " + id,
			"conversation " + id + ": group changed",
		} {
			if !strings.Contains(l, want) {
				return false
			}
		}
		return true
	})
	for _, secret := range []string{"secret", e.token} {
		if strings.Contains(log, secret) {
			t.Fatalf("log holds %q:\n%s", secret, log)
		}
	}
	if code, out := e.post("/api/conversations/nope/send", map[string]string{"text": "secret"}); code != 404 {
		t.Fatalf("send to a missing conversation: %d %s", code, out)
	}
	e.waitLog("failed request", func(l string) bool {
		return strings.Contains(l, "POST /api/conversations/nope/send by user failed (404): no conversation nope")
	})

	e.stop()
	e.start()
	log = e.waitLog("second start", func(l string) bool { return strings.Count(l, "kk started on") == 2 })
	if !strings.Contains(log, "kk stopping") || !strings.Contains(log, "hook SessionStart "+id) {
		t.Fatalf("log not kept across a restart:\n%s", log)
	}

	// The Logs tab gets the tail of the file and then every new line.
	conn, err := net.Dial("unix", filepath.Join(e.home, "kk.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.Write([]byte("GET /api/logs HTTP/1.1\r\nHost: kk\r\n\r\n"))
	rd := bufio.NewReader(conn)
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	readUntil := func(want string) {
		t.Helper()
		for {
			line, err := rd.ReadString('\n')
			if err != nil {
				t.Fatalf("stream ended before %q: %v", want, err)
			}
			if strings.HasPrefix(line, "data: ") && strings.Contains(line, want) {
				return
			}
		}
	}
	readUntil("hook SessionStart " + id)
	e.post("/api/logs", map[string]string{"error": "live line"})
	readUntil("web client error: live line")
}

func TestLogStaysBounded(t *testing.T) {
	e := newEnv(t)
	e.stop()
	old := strings.Repeat("2026/01/01 00:00:00 old line\n", logMax/28+100)
	os.WriteFile(filepath.Join(e.home, "kk.log"), []byte(old), 0o600)
	e.start()
	log := e.waitLog("start", func(l string) bool { return strings.Contains(l, "kk started on") })
	if len(log) > logMax || !strings.HasPrefix(log, "2026/01/01 00:00:00 old line\n") {
		t.Fatalf("log of %d bytes after it grew past %d, starts %q", len(log), logMax, log[:40])
	}
}

func TestTmuxTab(t *testing.T) {
	e := newEnv(t)
	id := e.mustKK("new", "claude", "-C", e.work, "hello")
	e.waitFor("first answer", id, func(c convView) bool { return c.Status == statusIdle })
	sock := strings.TrimPrefix(e.vars[2], "KK_TMUX=")
	tmuxOut := func(args ...string) string {
		out, _ := exec.Command("tmux", append([]string{"-L", sock}, args...)...).Output()
		return strings.TrimSpace(string(out))
	}
	before := tmuxOut("display-message", "-p", "-t", "=kk:", "#{window_id}")
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	resp, err := client.Get(e.url)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "ws://"+e.addr+"/api/tmux", &websocket.DialOptions{HTTPClient: client})
	if err != nil {
		t.Fatal(err)
	}
	ws.Write(ctx, websocket.MessageText, []byte(`{"cols":120,"rows":30}`))
	var seen strings.Builder
	// The status bar lists the windows: the home shell and the agent.
	for !strings.Contains(seen.String(), "[kk] 0:home") || !strings.Contains(seen.String(), "1:Title: hello") {
		_, data, err := ws.Read(ctx)
		if err != nil {
			t.Fatalf("tmux tab output: %v\n%q", err, seen.String())
		}
		seen.Write(data)
	}
	viewer := func() string {
		for _, line := range strings.Split(tmuxOut("list-sessions", "-F", "#{session_name} #{window_index}"), "\n") {
			if strings.HasPrefix(line, "kk-view-") {
				return line
			}
		}
		return ""
	}
	start := viewer()
	ws.Write(ctx, websocket.MessageBinary, []byte("\x02n"))
	for deadline := time.Now().Add(5 * time.Second); viewer() == start; time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("prefix n did not switch the viewer's window: %s", start)
		}
	}
	if now := tmuxOut("display-message", "-p", "-t", "=kk:", "#{window_id}"); now != before {
		t.Fatalf("the kk session switched from %s to %s", before, now)
	}
	ws.Close(websocket.StatusNormalClosure, "")
	e.waitLog("detach", func(l string) bool {
		return strings.Contains(l, "attached to the session") && strings.Contains(l, "detached")
	})
}

package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

const linearTestKey = "lin_api_TESTKEY0123456789"

func (e *env) inbox() inboxView {
	e.t.Helper()
	_, out := e.request("GET", "/api/inbox", nil)
	var v inboxView
	if err := json.Unmarshal(out, &v); err != nil {
		e.t.Fatalf("inbox: %v %s", err, out)
	}
	return v
}

func (e *env) waitInbox(what string, ok func(v inboxView) bool) inboxView {
	e.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		v := e.inbox()
		if ok(v) {
			return v
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("timed out waiting for %s: %+v", what, v)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func idle(v inboxView) bool {
	for _, s := range v.Sources {
		if s.Running {
			return false
		}
	}
	return true
}

func sourceOf(v inboxView, name string) sourceView {
	for _, s := range v.Sources {
		if s.Name == name {
			return s
		}
	}
	return sourceView{}
}

// refreshInbox refreshes everything and waits until the runs it started have ended.
func (e *env) refreshInbox(source string) inboxView {
	e.t.Helper()
	e.waitInbox("earlier runs", idle)
	since := time.Now()
	if code, out := e.post("/api/inbox/refresh", map[string]string{"source": source}); code != 200 {
		e.t.Fatalf("refresh: %d %s", code, out)
	}
	return e.waitInbox("refresh", func(v inboxView) bool {
		for _, s := range v.Sources {
			if (source == "" || s.Name == source) && s.Setup == "" && s.Enabled && s.Checked.Before(since) {
				return false
			}
		}
		return idle(v)
	})
}

func statusOf(v inboxView, title string) string {
	for _, it := range v.Items {
		if it.Title == title {
			return it.Status
		}
	}
	return ""
}

func writeFile(t *testing.T, path, data string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func linearServer(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != linearTestKey {
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"errors":[{"message":"Authentication required"}]}`)
			return
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), "assignedIssues") {
			t.Errorf("unexpected query %s", body)
		}
		io.WriteString(w, `{"data":{
			"viewer":{"assignedIssues":{"nodes":[
				{"identifier":"ENG-1","title":"Fix the export","description":"Exports fail on large files.","url":"https://linear.app/acme/issue/ENG-1","updatedAt":"2026-11-20T10:00:00Z","creator":{"name":"Ada"}}]}},
			"notifications":{"nodes":[
				{"id":"n1","type":"issueCommentMention","readAt":null,"createdAt":"2026-11-21T10:00:00Z","actor":{"name":"Bob"},
				 "issue":{"identifier":"ENG-2","title":"Search is slow","description":"d","url":"https://linear.app/acme/issue/ENG-2"},
				 "comment":{"body":"Can you look at this?","url":"https://linear.app/acme/issue/ENG-2#comment-1"}},
				{"id":"n2","type":"issueNewComment","readAt":"2026-11-21T11:00:00Z","createdAt":"2026-11-21T10:00:00Z","issue":{"identifier":"ENG-3","title":"Read","url":"https://linear.app/acme/issue/ENG-3"}}]}}}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

const slackAnswer = "Here is your inbox:\n```json\n" + `{"items":[
{"kind":"mention","channel":"eng","title":"#eng: review the rollout","text":"Can you review the rollout plan?","author":"Cleo","url":"https://acme.slack.com/archives/C1/p1700000000000100","time":"2026-11-22T09:00:00Z"},
{"kind":"direct message","title":"Dan: quick question","text":"Do you have a minute?","author":"Dan","url":"https://acme.slack.com/archives/D2/p1700000000000200","time":"2026-11-23T09:00:00Z"},
{"kind":"mention","title":"Not a Slack link","text":"x","author":"Eve","url":"http://evil.example/p1","time":"2026-11-24T09:00:00Z"}]}` + "\n```"

func TestInbox(t *testing.T) {
	linear := linearServer(t)
	e := newEnv(t, "KK_LINEAR_URL="+linear.URL)
	e.waitInbox("first poll", func(v inboxView) bool { return idle(v) && !sourceOf(v, "github").Checked.IsZero() })

	v := e.inbox()
	if s := sourceOf(v, "linear"); !strings.Contains(s.Setup, "API key") {
		t.Fatalf("linear without a key should ask for one: %+v", s)
	}

	gh := filepath.Join(e.fake, "gh")
	writeFile(t, filepath.Join(gh, "review.json"), `[{"title":"Add the inbox","body":"Body of the PR","url":"https://github.com/acme/kk/pull/7","updatedAt":"2026-11-24T08:00:00Z","author":{"login":"eve"},"repository":{"nameWithOwner":"acme/kk"}}]`)
	assigned := `[{"title":"Crash on start","body":"Steps","url":"https://github.com/acme/kk/issues/3","updatedAt":"2026-11-19T08:00:00Z","isPullRequest":false,"author":{"login":"fay"},"repository":{"nameWithOwner":"acme/kk"}}]`
	writeFile(t, filepath.Join(gh, "assigned.json"), assigned)
	writeFile(t, filepath.Join(gh, "mentions.json"), `[{"title":"Add the inbox","body":"Body of the PR","url":"https://github.com/acme/kk/pull/7","updatedAt":"2026-11-24T08:00:00Z","author":{"login":"eve"},"repository":{"nameWithOwner":"acme/kk"}}]`)
	writeFile(t, filepath.Join(gh, "notifications.json"), `[
		{"id":"1","reason":"ci_activity","updated_at":"2026-11-25T08:00:00Z","subject":{"title":"CI failed","url":null,"type":"CheckSuite"},"repository":{"full_name":"acme/kk","html_url":"https://github.com/acme/kk"}}]
		[{"id":"2","reason":"review_requested","updated_at":"2026-11-24T09:00:00Z","subject":{"title":"Add the inbox","url":"https://api.github.com/repos/acme/kk/pulls/7","type":"PullRequest"},"repository":{"full_name":"acme/kk","html_url":"https://github.com/acme/kk"}},
		{"id":"3","reason":"comment","updated_at":"2026-11-18T09:00:00Z","subject":{"title":"Old issue","url":"https://api.github.com/repos/acme/kk/issues/2","type":"Issue"},"repository":{"full_name":"acme/kk","html_url":"https://github.com/acme/kk"}}]`)
	writeFile(t, filepath.Join(e.fake, "slack.out"), slackAnswer)

	if code, out := e.post("/api/inbox/linear-key", nil); code != 405 {
		t.Fatalf("the key is set with PUT only: %d %s", code, out)
	}
	resp, out := e.request("PUT", "/api/inbox/linear-key", strings.NewReader(`{"key":"`+linearTestKey+`"}`))
	if resp.StatusCode != 200 || strings.Contains(string(out), linearTestKey) {
		t.Fatalf("set key: %d %s", resp.StatusCode, out)
	}
	if st, err := os.Stat(filepath.Join(e.home, "linear.key")); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("key file: %v %v", st, err)
	}

	v = e.refreshInbox("")
	var got []string
	for _, it := range v.Items {
		got = append(got, it.Source+" "+it.Kind+" "+it.Title)
	}
	want := []string{
		"github review request Add the inbox",
		"slack direct message Dan: quick question",
		"slack mention #eng: review the rollout",
		"linear mention ENG-2 Search is slow",
		"linear assigned issue ENG-1 Fix the export",
		"github assigned issue Crash on start",
		"github comment Old issue",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("items:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if it := v.Items[len(v.Items)-1]; it.URL != "https://github.com/acme/kk/issues/2" {
		t.Fatalf("notification link: %s", it.URL)
	}
	if it := v.Items[3]; it.Description != "Can you look at this?" || it.Author != "Bob" {
		t.Fatalf("linear comment: %+v", it)
	}
	for _, path := range []string{"/api/inbox", "/api/settings", "/api/inbox/linear-key"} {
		if _, out := e.request("GET", path, nil); strings.Contains(string(out), linearTestKey) {
			t.Fatalf("%s returns the key", path)
		}
	}

	var args []string
	data, _ := os.ReadFile(filepath.Join(e.fake, "slack-args.json"))
	json.Unmarshal(data, &args)
	line := strings.Join(args, " ")
	i := slices.Index(args, "--allowedTools")
	if !strings.Contains(line, "--permission-mode dontAsk") || !strings.Contains(line, "--tools  ") || i < 0 {
		t.Fatalf("the Slack run must be read-only: %s", line)
	}
	for _, tool := range strings.Split(args[i+1], ",") {
		name := strings.TrimPrefix(tool, slackRun.prefix)
		if !strings.HasPrefix(name, "slack_search_") && !strings.HasPrefix(name, "slack_read_") {
			t.Fatalf("the Slack run may use %s", tool)
		}
	}

	if denied := slices.Index(args, "--disallowedTools"); denied < 0 || !strings.Contains(args[denied+1], slackRun.prefix+"slack_send_message,") {
		t.Fatalf("the Slack run must deny the write tools: %s", line)
	}

	if code, _ := e.post("/api/inbox/status", map[string]string{"source": "github", "id": "https://github.com/acme/kk/issues/3", "status": "done"}); code != 400 {
		t.Fatalf("an unknown status should be refused: %d", code)
	}
	if code, out := e.post("/api/inbox/status", map[string]string{"source": "github", "id": "https://github.com/acme/kk/issues/3", "status": "closed"}); code != 200 {
		t.Fatalf("close: %d %s", code, out)
	}
	if code, out := e.post("/api/inbox/status", map[string]string{"source": "linear", "id": "issue:ENG-1", "status": "in_progress"}); code != 200 {
		t.Fatalf("move: %d %s", code, out)
	}
	writeFile(t, filepath.Join(gh, "assigned.json"), `[]`)
	if v = e.refreshInbox("github"); statusOf(v, "Crash on start") != "" {
		t.Fatalf("an item the source no longer returns should leave the inbox: %+v", v.Items)
	}
	writeFile(t, filepath.Join(gh, "assigned.json"), assigned)
	v = e.refreshInbox("")
	if statusOf(v, "Crash on start") != "closed" || statusOf(v, "ENG-1 Fix the export") != "in_progress" || statusOf(v, "ENG-2 Search is slow") != "backlog" || len(v.Items) != 7 {
		t.Fatalf("a refresh should keep the statuses: %+v", v.Items)
	}

	writeFile(t, filepath.Join(e.fake, "slack.out"), "Here are your messages: none worth noting.")
	v = e.refreshInbox("slack")
	if s := sourceOf(v, "slack"); !strings.Contains(s.Error, "expected JSON") || len(v.Items) != 7 {
		t.Fatalf("bad Slack output should keep the items and record the error: %+v %d", s, len(v.Items))
	}

	e.stop()
	e.start()
	v = e.inbox()
	if len(v.Items) != 7 || sourceOf(v, "slack").Error == "" || statusOf(v, "Crash on start") != "closed" {
		t.Fatalf("the inbox did not survive a restart: %+v", v)
	}

	writeFile(t, filepath.Join(e.fake, "slack-error"), "API Error: 401 authentication_error")
	v = e.refreshInbox("slack")
	if s := sourceOf(v, "slack"); !strings.Contains(s.Error, "401") || s.Setup != "" {
		t.Fatalf("a failed Slack run is an error, not a missing connector: %+v", s)
	}
	os.Remove(filepath.Join(e.fake, "slack-error"))
	writeFile(t, filepath.Join(e.fake, "slack-connector"), "failed")
	v = e.refreshInbox("slack")
	if s := sourceOf(v, "slack"); !strings.Contains(s.Error, "connector is failed") || s.Setup != "" {
		t.Fatalf("a Slack connector that fails to connect is an error: %+v", s)
	}

	writeFile(t, filepath.Join(e.fake, "slack-connector"), "not loaded")
	v = e.refreshInbox("slack")
	if s := sourceOf(v, "slack"); !strings.Contains(s.Error, "did not load") || s.Setup != "" {
		t.Fatalf("connectors that did not load in time are an error: %+v", s)
	}

	writeFile(t, filepath.Join(e.fake, "slack-connector"), "none")
	writeFile(t, filepath.Join(gh, "logged-out"), "")
	v = e.refreshInbox("")
	if s := sourceOf(v, "slack"); !strings.Contains(s.Setup, "Connect Slack") {
		t.Fatalf("slack without the connector: %+v", s)
	}
	if s := sourceOf(v, "github"); !strings.Contains(s.Setup, "gh auth login") {
		t.Fatalf("gh logged out: %+v", s)
	}
	if len(v.Items) != 2 || v.Items[0].Source != "linear" {
		t.Fatalf("only linear items should be left: %+v", v.Items)
	}

	e.request("PUT", "/api/inbox/linear-key", strings.NewReader(`{"key":""}`))
	if _, err := os.Stat(filepath.Join(e.home, "linear.key")); !os.IsNotExist(err) {
		t.Fatal("clearing the key should remove the file")
	}
}

const gmailAnswer = `{"items":[
{"id":"19a1b2c3d4e5f601","kind":"request","title":"Contract review","text":"Please review the attached contract by Friday.","author":"Gus","time":"2026-11-23T09:00:00Z"},
{"id":"19a1b2c3d4e5f602","kind":"awaiting reply","title":"Lunch next week?","text":"Asks which day works for you.","author":"Hana","time":"2026-11-24T09:00:00Z","url":"https://evil.example/x"},
{"id":"../evil","kind":"question","title":"Bad id","text":"x","author":"Ivo","time":"2026-11-24T10:00:00Z"},
{"id":"19a1b2c3d4e5f603","kind":"newsletter","title":"Weekly digest","text":"x","author":"News","time":"2026-11-24T11:00:00Z"}]}`

func TestInboxGmail(t *testing.T) {
	e := newEnv(t)
	writeFile(t, filepath.Join(e.fake, "gmail.out"), gmailAnswer)
	v := e.refreshInbox("gmail")
	var got []string
	for _, it := range v.Items {
		if it.Source == "gmail" {
			got = append(got, it.Kind+" "+it.Title+" "+it.Author+" "+it.URL)
		}
	}
	want := []string{
		"awaiting reply Lunch next week? Hana https://mail.google.com/mail/u/0/#inbox/19a1b2c3d4e5f602",
		"request Contract review Gus https://mail.google.com/mail/u/0/#inbox/19a1b2c3d4e5f601",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("gmail items:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	var args []string
	data, _ := os.ReadFile(filepath.Join(e.fake, "gmail-args.json"))
	json.Unmarshal(data, &args)
	line := strings.Join(args, " ")
	allowed, denied := slices.Index(args, "--allowedTools"), slices.Index(args, "--disallowedTools")
	if !strings.Contains(line, "--permission-mode dontAsk") || !strings.Contains(line, "--tools  ") || allowed < 0 || denied < 0 {
		t.Fatalf("the Gmail run must be read-only: %s", line)
	}
	read := []string{"mcp__claude_ai_Gmail__search_threads", "mcp__claude_ai_Gmail__get_thread", "mcp__claude_ai_Gmail__get_message", "mcp__claude_ai_Gmail__list_labels"}
	if !slices.Equal(strings.Split(args[allowed+1], ","), read) {
		t.Fatalf("the Gmail run may use only the read tools: %s", args[allowed+1])
	}
	for _, tool := range []string{"send_message", "reply", "forward", "create_draft", "trash_thread", "label_thread", "mark_thread_spam", "update_message_labels"} {
		if !slices.Contains(strings.Split(args[denied+1], ","), "mcp__claude_ai_Gmail__"+tool) {
			t.Fatalf("the Gmail run must deny %s: %s", tool, args[denied+1])
		}
	}

	writeFile(t, filepath.Join(e.fake, "gmail-error"), "API Error: 500 overloaded")
	v = e.refreshInbox("gmail")
	if s := sourceOf(v, "gmail"); !strings.Contains(s.Error, "Gmail run failed") || s.Setup != "" || len(v.Items) != 2 {
		t.Fatalf("a failed Gmail run is an error and keeps the items: %+v", s)
	}
	os.Remove(filepath.Join(e.fake, "gmail-error"))
	for _, status := range []string{"none", "needs-auth"} {
		writeFile(t, filepath.Join(e.fake, "gmail-connector"), status)
		v = e.refreshInbox("gmail")
		if s := sourceOf(v, "gmail"); !strings.Contains(s.Setup, "Connect Gmail") || s.Error != "" || len(v.Items) != 0 {
			t.Fatalf("gmail with connector %s: %+v %d", status, s, len(v.Items))
		}
	}
}

func TestInboxMigratesDismissed(t *testing.T) {
	e := newEnv(t)
	e.stop()
	writeFile(t, filepath.Join(e.home, "inbox.yaml"), `items:
  - {source: github, id: a, kind: comment, title: Dismissed, url: "https://github.com/acme/kk/issues/1", time: 2026-11-20T10:00:00Z}
  - {source: github, id: b, kind: comment, title: Kept, url: "https://github.com/acme/kk/issues/2", time: 2026-11-21T10:00:00Z}
sources:
  github: {checked: 2099-01-01T00:00:00Z}
dismissed: ["github:a", "github:gone"]
`)
	for range 2 {
		e.start()
		v := e.inbox()
		if statusOf(v, "Dismissed") != "closed" || statusOf(v, "Kept") != "backlog" || len(v.Items) != 2 {
			t.Fatalf("dismissed items should be closed: %+v", v.Items)
		}
		e.stop()
	}
	data, _ := os.ReadFile(filepath.Join(e.home, "inbox.yaml"))
	if strings.Contains(string(data), "dismissed") || !strings.Contains(string(data), "github:gone") {
		t.Fatalf("the dismissed list should become status records:\n%s", data)
	}
}

func TestInboxSettings(t *testing.T) {
	e := newEnv(t)
	_, out := e.request("GET", "/api/settings", nil)
	var view settingsView
	json.Unmarshal(out, &view)
	for _, name := range []string{"github", "linear", "slack", "gmail"} {
		if view.Settings.Inbox[name] != (InboxSettings{true, 15, 60}) {
			t.Fatalf("defaults: %+v", view.Settings.Inbox)
		}
	}
	view.Settings.Inbox["slack"] = InboxSettings{false, 30, 120}
	data, _ := json.Marshal(view.Settings)
	if resp, out := e.request("PUT", "/api/settings", strings.NewReader(string(data))); resp.StatusCode != 200 {
		t.Fatalf("save: %s", out)
	}
	if s := sourceOf(e.inbox(), "slack"); s.Enabled || s.Work != 30 || s.Off != 120 {
		t.Fatalf("slack settings: %+v", s)
	}
	for _, bad := range []InboxSettings{{true, 0, 60}, {true, 15, 0}} {
		view.Settings.Inbox["github"] = bad
		data, _ = json.Marshal(view.Settings)
		if resp, _ := e.request("PUT", "/api/settings", strings.NewReader(string(data))); resp.StatusCode != 400 {
			t.Fatalf("intervals %+v should be refused", bad)
		}
	}
	if s := sourceOf(e.inbox(), "github"); s.Work != 15 || s.Off != 60 {
		t.Fatalf("a refused save changed the settings: %+v", s)
	}
}

func TestInboxSettingsAddGmail(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, settingsPath(dir), "inbox:\n  slack: {enabled: false, interval_minutes: 30}\n")
	in := loadSettings(dir).Inbox
	if in["slack"] != (InboxSettings{false, 15, 60}) || in["gmail"] != (InboxSettings{true, 15, 60}) {
		t.Fatalf("settings saved before Gmail and working hours should keep enabled and get the default intervals: %+v", in)
	}
}

func at(hour, minute int) time.Time { return time.Date(2026, 11, 25, hour, minute, 0, 0, time.Local) }

func TestInboxInterval(t *testing.T) {
	c := InboxSettings{true, 15, 60}
	for _, tc := range []struct {
		now  time.Time
		want time.Duration
	}{
		{at(7, 59), time.Hour},
		{at(8, 0), 15 * time.Minute},
		{at(19, 59), 15 * time.Minute},
		{at(20, 0), time.Hour},
		{at(0, 0), time.Hour},
	} {
		if got := c.interval(tc.now); got != tc.want {
			t.Errorf("interval at %s = %s, want %s", tc.now.Format("15:04"), got, tc.want)
		}
	}
}

// blockingSource counts its fetches and holds each one until release is closed.
type blockingSource struct {
	fetches *atomic.Int32
	release chan struct{}
}

func (blockingSource) name() string { return "github" }
func (blockingSource) ready() error { return nil }
func (s blockingSource) fetch(context.Context) ([]InboxItem, error) {
	s.fetches.Add(1)
	<-s.release
	return nil, nil
}

func testInbox(t *testing.T, checked time.Time) (*Inbox, blockingSource) {
	src := blockingSource{&atomic.Int32{}, make(chan struct{})}
	in := &Inbox{dir: t.TempDir(), sources: []inboxSource{src}, running: map[string]bool{}, ctx: context.Background(),
		settings: func() map[string]InboxSettings { return map[string]InboxSettings{"github": {true, 15, 60}} },
		state:    inboxState{Sources: map[string]*sourceState{"github": {Checked: checked}}, Status: map[string]itemStatus{}}}
	return in, src
}

func TestInboxCrossesWorkingHours(t *testing.T) {
	for _, tc := range []struct {
		checked     time.Time
		early, late time.Time
	}{
		{at(7, 30), at(7, 59), at(8, 0)},
		{at(7, 59), at(8, 13), at(8, 14)},
		{at(19, 50), at(20, 49), at(20, 50)},
		{at(23, 30), at(0, 29).AddDate(0, 0, 1), at(0, 30).AddDate(0, 0, 1)},
	} {
		in, src := testInbox(t, tc.checked)
		in.tick(tc.early)
		if running(in) {
			t.Fatalf("checked at %s: a check started at %s", tc.checked.Format("15:04"), tc.early.Format("15:04"))
		}
		in.tick(tc.late)
		if !running(in) {
			t.Fatalf("checked at %s: no check started at %s", tc.checked.Format("15:04"), tc.late.Format("15:04"))
		}
		close(src.release)
		waitFor(t, func() bool { return !running(in) })
	}
}

func running(in *Inbox) bool {
	in.mu.Lock()
	defer in.mu.Unlock()
	return in.running["github"]
}

func TestInboxOneRunPerSource(t *testing.T) {
	in, src := testInbox(t, time.Time{})
	in.tick(at(9, 0))
	waitFor(t, func() bool { return src.fetches.Load() == 1 })
	in.tick(at(9, 30))
	in.refresh(src)
	if n := src.fetches.Load(); n != 1 {
		t.Fatalf("%d runs of one source at once", n)
	}
	close(src.release)
	waitFor(t, func() bool { return !running(in) })
	if n := src.fetches.Load(); n != 1 {
		t.Fatalf("%d runs of one source at once", n)
	}
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !ok(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
	}
}

// A connector run that outlives the server keeps writing where the server no longer looks, and here into a test's removed directory.
func TestInboxStopsRunsWithServer(t *testing.T) {
	bin := t.TempDir()
	os.WriteFile(filepath.Join(bin, "claude"), []byte("#!/bin/sh\necho $$ > \"$FAKE_HOME/run.$$\"\nexec sleep 600\n"), 0o755)
	e := newEnv(t, "PATH="+bin+":"+fakeBin+":"+os.Getenv("PATH"))
	var runs []string
	for deadline := time.Now().Add(10 * time.Second); len(runs) < 2; time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("connector runs started: %v, want slack and gmail", runs)
		}
		runs, _ = filepath.Glob(filepath.Join(e.fake, "run.*"))
	}
	e.stop()
	for _, run := range runs {
		pid, _ := strconv.Atoi(strings.TrimPrefix(filepath.Ext(run), "."))
		if syscall.Kill(pid, 0) == nil {
			syscall.Kill(pid, syscall.SIGKILL)
			t.Errorf("connector run %d outlived the server", pid)
		}
	}
}

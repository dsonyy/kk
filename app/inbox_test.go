package main

import (
	"context"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
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
	time.Sleep(500 * time.Millisecond)
	if v := e.inbox(); !sourceOf(v, "github").Checked.IsZero() || !idle(v) {
		t.Fatalf("a source was checked before the page asked: %+v", v.Sources)
	}
	e.post("/api/inbox/tick", struct{}{})
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
	if !strings.Contains(line, "--permission-mode dontAsk") || !strings.Contains(line, "--tools ToolSearch,CronList ") || i < 0 {
		t.Fatalf("the Slack run must be read-only: %s", line)
	}
	for _, tool := range strings.Split(args[i+1], ",")[1:] {
		name := strings.TrimPrefix(tool, slackRun.prefix)
		if !strings.HasPrefix(name, "slack_search_") && !strings.HasPrefix(name, "slack_read_") {
			t.Fatalf("the Slack run may use %s", tool)
		}
	}

	if denied := slices.Index(args, "--disallowedTools"); denied < 0 || !strings.Contains(args[denied+1], slackRun.prefix+"slack_send_message,") {
		t.Fatalf("the Slack run must deny the write tools: %s", line)
	}

	for _, status := range []string{"done", "todo"} {
		if code, _ := e.post("/api/inbox/status", map[string]string{"source": "github", "id": "https://github.com/acme/kk/issues/3", "status": status}); code != 400 {
			t.Fatalf("status %q should be refused: %d", status, code)
		}
	}
	for _, m := range []struct{ source, id, status string }{
		{"github", "https://github.com/acme/kk/issues/3", "in_progress"},
		{"github", "https://github.com/acme/kk/pull/7", "dismissed"},
		{"github", "https://github.com/acme/kk/issues/2", "closed"},
		{"linear", "issue:ENG-1", "in_progress"},
	} {
		if code, out := e.post("/api/inbox/status", map[string]string{"source": m.source, "id": m.id, "status": m.status}); code != 200 {
			t.Fatalf("move to %s: %d %s", m.status, code, out)
		}
	}

	writeFile(t, filepath.Join(gh, "assigned.json"), `[]`)
	writeFile(t, filepath.Join(gh, "notifications.json"), `[]`)
	v = e.refreshInbox("github")
	if statusOf(v, "Crash on start") != "closed" || statusOf(v, "Old issue") != "closed" || statusOf(v, "Add the inbox") != "dismissed" || len(v.Items) != 7 {
		t.Fatalf("items out of the backlog should stay when the source no longer returns them, and those in progress close: %+v", v.Items)
	}
	writeFile(t, filepath.Join(e.fake, "slack.out"), strings.Replace(slackAnswer, "https://acme.slack.com/archives/D2", "http://evil.example/D2", 1))
	if v = e.refreshInbox("slack"); statusOf(v, "Dan: quick question") != "" || len(v.Items) != 6 {
		t.Fatalf("a backlog item the source no longer returns should leave the inbox: %+v", v.Items)
	}
	writeFile(t, filepath.Join(e.fake, "slack.out"), slackAnswer)

	writeFile(t, filepath.Join(gh, "assigned.json"), assigned)
	writeFile(t, filepath.Join(gh, "review.json"), `[{"title":"Add the inbox","body":"New push","url":"https://github.com/acme/kk/pull/7","updatedAt":"2026-11-25T08:00:00Z","author":{"login":"eve"},"repository":{"nameWithOwner":"acme/kk"}}]`)
	v = e.refreshInbox("")
	if statusOf(v, "Crash on start") != "in_progress" || statusOf(v, "Add the inbox") != "dismissed" || statusOf(v, "Old issue") != "closed" ||
		statusOf(v, "ENG-1 Fix the export") != "in_progress" || statusOf(v, "ENG-2 Search is slow") != "backlog" || len(v.Items) != 7 {
		t.Fatalf("an item kk closed should reopen when it comes back, and a dismissed one stay dismissed after new activity: %+v", v.Items)
	}

	writeFile(t, filepath.Join(e.fake, "slack.out"), "Here are your messages: none worth noting.")
	v = e.refreshInbox("slack")
	if s := sourceOf(v, "slack"); !strings.Contains(s.Error, "expected JSON") || len(v.Items) != 7 {
		t.Fatalf("bad Slack output should keep the items and record the error: %+v %d", s, len(v.Items))
	}

	e.stop()
	e.start()
	v = e.inbox()
	if len(v.Items) != 7 || sourceOf(v, "slack").Error == "" || statusOf(v, "Crash on start") != "in_progress" {
		t.Fatalf("the inbox did not survive a restart: %+v", v)
	}

	writeFile(t, filepath.Join(e.fake, "slack-error"), "")
	v = e.refreshInbox("slack")
	if s := sourceOf(v, "slack"); !strings.Contains(s.Error, "API error") || s.Setup != "" {
		t.Fatalf("a failed Slack run is an error, not a missing connector: %+v", s)
	}
	os.Remove(filepath.Join(e.fake, "slack-error"))

	writeFile(t, filepath.Join(e.fake, "slack-connector"), "none")
	writeFile(t, filepath.Join(gh, "logged-out"), "")
	v = e.refreshInbox("")
	if s, c := sourceOf(v, "slack"), e.conv(integrationID("slack")); !strings.Contains(s.Setup, "Connect Slack") || c.Attention != "" {
		t.Fatalf("slack without the connector: %+v %q", s, c.Attention)
	}
	if s := sourceOf(v, "github"); !strings.Contains(s.Setup, "gh auth login") {
		t.Fatalf("gh logged out: %+v", s)
	}
	var left []string
	for _, it := range v.Items {
		left = append(left, it.Source+" "+it.Status)
	}
	if want := []string{"github dismissed", "linear backlog", "linear in_progress", "github in_progress", "github closed"}; !slices.Equal(left, want) || statusOf(v, "Crash on start") != "in_progress" {
		t.Fatalf("a source that is not set up should hide its backlog and keep the rest as it was: %v", left)
	}
	os.Remove(filepath.Join(e.fake, "slack-connector"))
	if v = e.refreshInbox("slack"); len(v.Items) != 7 {
		t.Fatalf("the backlog should come back once the source is set up again: %+v", v.Items)
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
	if !strings.Contains(line, "--permission-mode dontAsk") || !strings.Contains(line, "--tools ToolSearch,CronList ") || allowed < 0 || denied < 0 {
		t.Fatalf("the Gmail run must be read-only: %s", line)
	}
	read := []string{"ToolSearch", "mcp__claude_ai_Gmail__search_threads", "mcp__claude_ai_Gmail__get_thread", "mcp__claude_ai_Gmail__get_message", "mcp__claude_ai_Gmail__list_labels"}
	if !slices.Equal(strings.Split(args[allowed+1], ","), read) {
		t.Fatalf("the Gmail run may use only the read tools: %s", args[allowed+1])
	}
	for _, tool := range []string{"send_message", "reply", "forward", "create_draft", "trash_thread", "label_thread", "mark_thread_spam", "update_message_labels"} {
		if !slices.Contains(strings.Split(args[denied+1], ","), "mcp__claude_ai_Gmail__"+tool) {
			t.Fatalf("the Gmail run must deny %s: %s", tool, args[denied+1])
		}
	}

	writeFile(t, filepath.Join(e.fake, "gmail-error"), "")
	v = e.refreshInbox("gmail")
	if s := sourceOf(v, "gmail"); !strings.Contains(s.Error, "Gmail run ended on an API error") || s.Setup != "" || len(v.Items) != 2 {
		t.Fatalf("a failed Gmail run is an error and keeps the items: %+v", s)
	}
	os.Remove(filepath.Join(e.fake, "gmail-error"))
	writeFile(t, filepath.Join(e.fake, "gmail-connector"), "none")
	v = e.refreshInbox("gmail")
	if s, c := sourceOf(v, "gmail"), e.conv(integrationID("gmail")); !strings.Contains(s.Setup, "Connect Gmail") || s.Error != "" || len(v.Items) != 0 || c.Attention != "" {
		t.Fatalf("gmail without the connector: %+v %d %q", s, len(v.Items), c.Attention)
	}
}

func TestInboxMigratesDismissed(t *testing.T) {
	e := newEnv(t)
	e.stop()
	writeFile(t, filepath.Join(e.home, "inbox.yaml"), `items:
  - {source: github, id: a, kind: comment, title: Dismissed, url: "https://github.com/acme/kk/issues/1", time: 2026-11-20T10:00:00Z}
  - {source: github, id: b, kind: comment, title: Kept, url: "https://github.com/acme/kk/issues/2", time: 2026-11-21T10:00:00Z}
  - {source: github, id: c, kind: comment, title: To do, url: "https://github.com/acme/kk/issues/3", time: 2026-11-22T10:00:00Z}
  - {source: github, id: d, kind: comment, title: Closed earlier, url: "https://github.com/acme/kk/issues/4", time: 2026-11-22T10:00:00Z}
  - {source: github, id: e, kind: comment, title: Closed, url: "https://github.com/acme/kk/issues/5", time: 2026-11-22T10:00:00Z}
sources:
  github: {checked: 2099-01-01T00:00:00Z}
status:
  "github:c": {status: todo, changed: 2026-11-22T11:00:00Z}
  "github:d": {status: closed}
  "github:e": {status: closed, changed: 2026-11-22T11:00:00Z}
dismissed: ["github:a", "github:gone"]
`)
	for range 2 {
		e.start()
		v := e.inbox()
		if statusOf(v, "Dismissed") != "dismissed" || statusOf(v, "Closed earlier") != "dismissed" || statusOf(v, "Closed") != "closed" ||
			statusOf(v, "Kept") != "backlog" || statusOf(v, "To do") != "backlog" || len(v.Items) != 5 {
			t.Fatalf("dismissed items should be dismissed and to do the backlog: %+v", v.Items)
		}
		e.stop()
	}
	data, _ := os.ReadFile(filepath.Join(e.home, "inbox.yaml"))
	if strings.Contains(string(data), "\ndismissed:") || strings.Contains(string(data), "todo") || !strings.Contains(string(data), "github:gone") {
		t.Fatalf("the dismissed list should become status records:\n%s", data)
	}
}

func on(m string) map[string]bool { return map[string]bool{m: true} }

func TestInboxSettings(t *testing.T) {
	e := newEnv(t)
	_, out := e.request("GET", "/api/settings", nil)
	var view settingsView
	json.Unmarshal(out, &view)
	for name, m := range map[string]string{"github": "gh", "linear": "api_key", "slack": "connector", "gmail": "connector"} {
		if s := view.Settings.Inbox[name]; s.IntervalMinutes != 15 || !maps.Equal(s.Methods, on(m)) {
			t.Fatalf("defaults: %+v", view.Settings.Inbox)
		}
	}
	view.Settings.Inbox["slack"] = InboxSettings{IntervalMinutes: 30, Methods: map[string]bool{"connector": false}}
	data, _ := json.Marshal(view.Settings)
	if resp, out := e.request("PUT", "/api/settings", strings.NewReader(string(data))); resp.StatusCode != 200 {
		t.Fatalf("save: %s", out)
	}
	if s := sourceOf(e.inbox(), "slack"); s.Enabled || s.Interval != 30 || !maps.Equal(s.Methods, map[string]bool{"connector": false}) {
		t.Fatalf("slack settings: %+v", s)
	}
	for _, bad := range []InboxSettings{{IntervalMinutes: 0, Methods: on("gh")}, {IntervalMinutes: 15, Methods: on("api_key")}} {
		view.Settings.Inbox["github"] = bad
		data, _ = json.Marshal(view.Settings)
		if resp, _ := e.request("PUT", "/api/settings", strings.NewReader(string(data))); resp.StatusCode != 400 {
			t.Fatalf("settings %+v should be refused", bad)
		}
	}
	if s := sourceOf(e.inbox(), "github"); s.Interval != 15 || !s.Enabled {
		t.Fatalf("a refused save changed the settings: %+v", s)
	}
	data, _ = os.ReadFile(settingsPath(e.home))
	if strings.Contains(string(data), "enabled") {
		t.Fatalf("settings.yaml keeps the old key:\n%s", data)
	}
}

func TestInboxSettingsEarlierVersions(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, settingsPath(dir), "inbox:\n  slack: {enabled: false, work_minutes: 30, off_minutes: 90}\n  github: {enabled: true, interval_minutes: 5}\n")
	in := loadSettings(dir).Inbox
	if s := in["slack"]; s.IntervalMinutes != 15 || !maps.Equal(s.Methods, map[string]bool{"connector": false}) || s.Enabled != nil {
		t.Fatalf("slack from working hours: %+v", s)
	}
	if s := in["github"]; s.IntervalMinutes != 5 || !maps.Equal(s.Methods, on("gh")) {
		t.Fatalf("github from interval: %+v", s)
	}
	if s := in["gmail"]; s.IntervalMinutes != 15 || !maps.Equal(s.Methods, on("connector")) {
		t.Fatalf("a source missing from the file should get the defaults: %+v", s)
	}
}

func TestInboxInterval(t *testing.T) {
	checked := time.Date(2026, 11, 25, 7, 30, 0, 0, time.Local)
	for _, at := range []time.Time{checked.Add(14 * time.Minute), checked.Add(15 * time.Minute), checked.Add(10 * time.Hour)} {
		in, src := testInbox(t, checked)
		in.tick(at)
		if want := at.Sub(checked) >= 15*time.Minute; running(in) != want {
			t.Fatalf("checked at %s, tick at %s: running %v", checked.Format("15:04"), at.Format("15:04"), running(in))
		}
		close(src.release)
		waitFor(t, func() bool { return !running(in) })
	}
}

func TestInboxKeepsBounded(t *testing.T) {
	in, _ := testInbox(t, time.Time{})
	var items []InboxItem
	for i := range keptMax + 50 {
		id := strconv.Itoa(i)
		items = append(items, InboxItem{ID: id, Title: id, URL: "https://github.com/acme/kk/issues/" + id})
		in.state.Status["github:"+id] = itemStatus{Status: statusDismissed, Changed: time.Now().Add(time.Duration(i) * time.Minute)}
	}
	in.setItems("github", items)
	in.setItems("github", nil)
	if len(in.state.Items) != keptMax || in.state.Items[0].ID != strconv.Itoa(keptMax+49) {
		t.Fatalf("kept %d items, the first %s", len(in.state.Items), in.state.Items[0].ID)
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
	in := &Inbox{dir: t.TempDir(), sources: []inboxSource{src}, running: map[string]bool{}, cancels: map[string]context.CancelFunc{}, ctx: context.Background(),
		settings: func() map[string]InboxSettings { return map[string]InboxSettings{"github": {IntervalMinutes: 15, Methods: on("gh")}} },
		state:    inboxState{Sources: map[string]*sourceState{"github": {Checked: checked}}, Status: map[string]itemStatus{}}}
	return in, src
}

func running(in *Inbox) bool {
	in.mu.Lock()
	defer in.mu.Unlock()
	return in.running["github"]
}

func TestInboxOneRunPerSource(t *testing.T) {
	in, src := testInbox(t, time.Time{})
	in.tick(time.Now())
	waitFor(t, func() bool { return src.fetches.Load() == 1 })
	in.tick(time.Now().Add(time.Hour))
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


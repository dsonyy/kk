package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
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
	writeFile(t, filepath.Join(gh, "assigned.json"), `[{"title":"Crash on start","body":"Steps","url":"https://github.com/acme/kk/issues/3","updatedAt":"2026-11-19T08:00:00Z","isPullRequest":false,"author":{"login":"fay"},"repository":{"nameWithOwner":"acme/kk"}}]`)
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
		name := strings.TrimPrefix(tool, slackTool)
		if !strings.HasPrefix(name, "slack_search_") && !strings.HasPrefix(name, "slack_read_") {
			t.Fatalf("the Slack run may use %s", tool)
		}
	}

	if code, out := e.post("/api/inbox/dismiss", map[string]string{"source": "github", "id": "https://github.com/acme/kk/issues/3"}); code != 200 {
		t.Fatalf("dismiss: %d %s", code, out)
	}
	v = e.refreshInbox("github")
	if slices.ContainsFunc(v.Items, func(it InboxItem) bool { return it.Title == "Crash on start" }) || len(v.Items) != 6 {
		t.Fatalf("dismissed item came back: %+v", v.Items)
	}

	writeFile(t, filepath.Join(e.fake, "slack.out"), "Here are your messages: none worth noting.")
	v = e.refreshInbox("slack")
	if s := sourceOf(v, "slack"); !strings.Contains(s.Error, "expected JSON") || len(v.Items) != 6 {
		t.Fatalf("bad Slack output should keep the items and record the error: %+v %d", s, len(v.Items))
	}

	e.stop()
	e.start()
	v = e.inbox()
	if len(v.Items) != 6 || sourceOf(v, "slack").Error == "" {
		t.Fatalf("the inbox did not survive a restart: %+v", v)
	}

	writeFile(t, filepath.Join(e.fake, "slack.out"), slackMissing)
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

func TestInboxSettings(t *testing.T) {
	e := newEnv(t)
	_, out := e.request("GET", "/api/settings", nil)
	var view settingsView
	json.Unmarshal(out, &view)
	if view.Settings.Inbox["slack"] != (InboxSettings{true, 60}) || view.Settings.Inbox["github"] != (InboxSettings{true, 15}) {
		t.Fatalf("defaults: %+v", view.Settings.Inbox)
	}
	view.Settings.Inbox["slack"] = InboxSettings{false, 120}
	data, _ := json.Marshal(view.Settings)
	if resp, out := e.request("PUT", "/api/settings", strings.NewReader(string(data))); resp.StatusCode != 200 {
		t.Fatalf("save: %s", out)
	}
	if s := sourceOf(e.inbox(), "slack"); s.Enabled || s.Interval != 120 {
		t.Fatalf("slack settings: %+v", s)
	}
	view.Settings.Inbox["github"] = InboxSettings{true, 0}
	data, _ = json.Marshal(view.Settings)
	if resp, _ := e.request("PUT", "/api/settings", strings.NewReader(string(data))); resp.StatusCode != 400 {
		t.Fatal("an interval of 0 should be refused")
	}
	if s := sourceOf(e.inbox(), "github"); s.Interval != 15 {
		t.Fatalf("a refused save changed the settings: %+v", s)
	}
}

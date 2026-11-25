package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"
)

// githubSource reads the GitHub account of the gh CLI that is logged in on the machine.
type githubSource struct{}

const githubMentionDays = 14

func (githubSource) name() string { return "github" }

func (githubSource) ready() error {
	if _, err := exec.LookPath("gh"); err != nil {
		return errors.New("Install the GitHub CLI (gh) and run gh auth login.")
	}
	return nil
}

// gh runs a gh command that prints JSON lists; a paginated call prints one list per page, back to back.
func gh[T any](ctx context.Context, args ...string) ([]T, error) {
	cmd := exec.CommandContext(ctx, "gh", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	data, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if strings.Contains(msg, "gh auth login") {
			return nil, notSetUp("Run gh auth login in a terminal.")
		}
		return nil, fmt.Errorf("gh %s: %s", args[0], firstLine(msg, 200))
	}
	var all []T
	dec := json.NewDecoder(bytes.NewReader(data))
	for {
		var page []T
		if err := dec.Decode(&page); err == io.EOF {
			return all, nil
		} else if err != nil {
			return nil, fmt.Errorf("gh %s printed no JSON list", args[0])
		}
		all = append(all, page...)
	}
}

type ghSearchItem struct {
	Title         string    `json:"title"`
	Body          string    `json:"body"`
	URL           string    `json:"url"`
	UpdatedAt     time.Time `json:"updatedAt"`
	IsPullRequest bool      `json:"isPullRequest"`
	Author        struct {
		Login string `json:"login"`
	} `json:"author"`
	Repository struct {
		NameWithOwner string `json:"nameWithOwner"`
	} `json:"repository"`
}

type ghNotification struct {
	ID        string    `json:"id"`
	Reason    string    `json:"reason"`
	UpdatedAt time.Time `json:"updated_at"`
	Subject   struct {
		Title string `json:"title"`
		URL   string `json:"url"`
		Type  string `json:"type"`
	} `json:"subject"`
	Repository struct {
		FullName string `json:"full_name"`
		HTMLURL  string `json:"html_url"`
	} `json:"repository"`
}

const ghSearchFields = "title,body,url,updatedAt,author,repository"

// fetch runs the most specific query first; an item that several queries return keeps the kind of the first.
func (githubSource) fetch(ctx context.Context) ([]InboxItem, error) {
	since := time.Now().AddDate(0, 0, -githubMentionDays).Format("2006-01-02")
	searches := []struct {
		kind string
		args []string
	}{
		{"review request", []string{"search", "prs", "--review-requested=@me", "--state=open", "--limit=30", "--json", ghSearchFields}},
		{"assigned", []string{"search", "issues", "--assignee=@me", "--state=open", "--include-prs", "--limit=30", "--json", ghSearchFields + ",isPullRequest"}},
		{"mention", []string{"search", "issues", "--mentions=@me", "--state=open", "--include-prs", "--updated=>=" + since, "--limit=30", "--json", ghSearchFields}},
	}
	var items []InboxItem
	seen := map[string]bool{}
	add := func(it InboxItem) {
		if !seen[it.ID] {
			seen[it.ID] = true
			items = append(items, it)
		}
	}
	for _, s := range searches {
		found, err := gh[ghSearchItem](ctx, s.args...)
		if err != nil {
			return nil, err
		}
		for _, f := range found {
			kind := s.kind
			if kind == "assigned" {
				kind = map[bool]string{true: "assigned pull request", false: "assigned issue"}[f.IsPullRequest]
			}
			add(InboxItem{ID: f.URL, Kind: kind, Title: f.Title, Description: f.Body, URL: f.URL, Author: f.Author.Login, Time: f.UpdatedAt})
		}
	}
	notes, err := gh[ghNotification](ctx, "api", "--paginate", "notifications?per_page=100")
	if err != nil {
		return nil, err
	}
	for _, n := range notes {
		// CI runs notify on every push and would bury everything else.
		if n.Reason == "ci_activity" {
			continue
		}
		url := githubHTML(n.Subject.URL)
		id := url
		if url == "" {
			url, id = n.Repository.HTMLURL, "thread:"+n.ID
		}
		add(InboxItem{ID: id, Kind: githubReason(n.Reason), Title: n.Subject.Title, Description: n.Repository.FullName + " · " + strings.ReplaceAll(n.Reason, "_", " "), URL: url, Time: n.UpdatedAt})
	}
	return items, nil
}

// githubHTML turns an API address of an issue, pull request or commit into the address of its page.
func githubHTML(api string) string {
	rest, ok := strings.CutPrefix(api, "https://api.github.com/repos/")
	if !ok {
		return ""
	}
	r := strings.NewReplacer("/pulls/", "/pull/", "/commits/", "/commit/")
	return "https://github.com/" + r.Replace(rest)
}

func githubReason(reason string) string {
	switch reason {
	case "review_requested":
		return "review request"
	case "mention", "team_mention":
		return "mention"
	case "assign":
		return "assigned"
	case "comment":
		return "comment"
	}
	return "notification"
}

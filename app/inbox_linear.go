package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// linearSource reads Linear's GraphQL API with a personal API key the user pasted in Settings.
type linearSource struct{ dir string }

const linearQuery = `{
  viewer {
    assignedIssues(first: 50, orderBy: updatedAt, filter: {state: {type: {nin: ["completed", "canceled"]}}}) {
      nodes { identifier title description url updatedAt creator { name } }
    }
  }
  notifications(first: 50) {
    nodes {
      id type readAt createdAt
      actor { name }
      ... on IssueNotification { issue { identifier title description url } comment { body url } }
    }
  }
}`

func linearKeyPath(dir string) string { return filepath.Join(dir, "linear.key") }

func (l linearSource) key() string {
	data, _ := os.ReadFile(linearKeyPath(l.dir))
	return strings.TrimSpace(string(data))
}

func (linearSource) name() string { return "linear" }

func (l linearSource) ready() error {
	if l.key() == "" {
		return errors.New("Paste a personal API key from Linear, Settings, Security and access, in Settings below.")
	}
	return nil
}

func linearURL() string {
	if u := os.Getenv("KK_LINEAR_URL"); u != "" {
		return u
	}
	return "https://api.linear.app/graphql"
}

type linearIssue struct {
	Identifier  string    `json:"identifier"`
	Title       string    `json:"title"`
	Description string    `json:"description"`
	URL         string    `json:"url"`
	UpdatedAt   time.Time `json:"updatedAt"`
	Creator     *struct {
		Name string `json:"name"`
	} `json:"creator"`
}

func (l linearSource) fetch(ctx context.Context) ([]InboxItem, error) {
	body, _ := json.Marshal(map[string]string{"query": linearQuery})
	req, err := http.NewRequestWithContext(ctx, "POST", linearURL(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", l.key())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("Linear: %v", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, errors.New("Linear refused the API key.")
	}
	var out struct {
		Data struct {
			Viewer struct {
				AssignedIssues struct{ Nodes []linearIssue } `json:"assignedIssues"`
			} `json:"viewer"`
			Notifications struct {
				Nodes []struct {
					ID        string    `json:"id"`
					Type      string    `json:"type"`
					ReadAt    *string   `json:"readAt"`
					CreatedAt time.Time `json:"createdAt"`
					Actor     *struct {
						Name string `json:"name"`
					} `json:"actor"`
					Issue   *linearIssue `json:"issue"`
					Comment *struct {
						Body string `json:"body"`
						URL  string `json:"url"`
					} `json:"comment"`
				}
			} `json:"notifications"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("Linear answered %s with no JSON", resp.Status)
	}
	if len(out.Errors) > 0 {
		return nil, fmt.Errorf("Linear: %s", firstLine(out.Errors[0].Message, 200))
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Linear answered %s", resp.Status)
	}
	var items []InboxItem
	for _, i := range out.Data.Viewer.AssignedIssues.Nodes {
		it := InboxItem{ID: "issue:" + i.Identifier, Kind: "assigned issue", Title: i.Identifier + " " + i.Title, Description: i.Description, URL: i.URL, Time: i.UpdatedAt}
		if i.Creator != nil {
			it.Author = i.Creator.Name
		}
		items = append(items, it)
	}
	for _, n := range out.Data.Notifications.Nodes {
		if n.ReadAt != nil || n.Issue == nil {
			continue
		}
		it := InboxItem{ID: "notification:" + n.ID, Kind: linearKind(n.Type), Title: n.Issue.Identifier + " " + n.Issue.Title, Description: n.Issue.Description, URL: n.Issue.URL, Time: n.CreatedAt}
		if n.Comment != nil {
			it.Description, it.URL = n.Comment.Body, n.Comment.URL
		}
		if n.Actor != nil {
			it.Author = n.Actor.Name
		}
		items = append(items, it)
	}
	return items, nil
}

func linearKind(t string) string {
	switch {
	case strings.Contains(t, "Mention"):
		return "mention"
	case strings.Contains(t, "Comment") || strings.Contains(t, "Reply"):
		return "comment"
	case strings.Contains(t, "Assigned"):
		return "assigned issue"
	}
	return "notification"
}

func (s *Server) handleLinearKey(r *http.Request) (any, error) {
	var in struct {
		Key string `json:"key"`
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	path := linearKeyPath(s.store.dir)
	key := strings.TrimSpace(in.Key)
	if key == "" {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return nil, err
		}
	} else {
		if strings.ContainsAny(key, " \n\t") {
			return nil, fail(400, "an API key has no spaces")
		}
		tmp := path + ".tmp"
		if err := os.WriteFile(tmp, []byte(key), 0o600); err != nil {
			return nil, err
		}
		if err := os.Rename(tmp, path); err != nil {
			return nil, err
		}
		s.inbox.refresh(s.inbox.source("linear"))
	}
	return map[string]bool{"set": key != ""}, nil
}

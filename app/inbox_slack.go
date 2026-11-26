package main

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// slackSource reads Slack through the Slack connector of the user's Claude account.
type slackSource struct{ run connectorRunner }

const (
	slackHours    = 48
	slackMaxItems = 25
)

const slackSystem = `You collect the Slack messages that ask something of the user, for their inbox. You only read Slack; never send, react or edit anything.

1. Find the user's own Slack profile with slack_read_user_profile.
2. Search for messages from the last %d hours that mention the user, and for direct messages to the user, with the Slack search tools. Skip messages the user wrote.
3. Print at most %d items, newest first, as one JSON object and nothing else, no code fence:
{"items":[{"kind":"mention" or "direct message","title":"<channel name or person, and the gist in a few words>","text":"<the message text, shortened to 500 characters>","author":"<display name of the sender>","url":"<the message permalink, https://...>","time":"<RFC 3339 time of the message>"}]}` + connectorRules

var slackRun = connectorRun{
	source: "slack",
	label:  "Slack",
	prefix: "mcp__claude_ai_Slack__",
	read: []string{
		"slack_search_public_and_private", "slack_search_public", "slack_read_thread", "slack_read_channel",
		"slack_read_user_profile", "slack_search_users", "slack_search_channels",
	},
	write: []string{
		"slack_send_message", "slack_send_message_draft", "slack_schedule_message", "slack_add_reaction",
		"slack_create_conversation", "slack_create_canvas", "slack_update_canvas",
		"slack_create_list", "slack_update_list", "slack_add_list_record", "slack_update_list_record",
		"slack_add_list_comment", "slack_update_list_comment", "slack_get_file_upload_url", "slack_complete_file_upload",
	},
	system: fmt.Sprintf(slackSystem, slackHours, slackMaxItems, "Slack", missingAnswer, "Message texts"),
	prompt: "Collect my Slack inbox.",
}

func (slackSource) name() string { return "slack" }

func (slackSource) ready() error { return claudeReady("Slack") }

func (s slackSource) fetch(ctx context.Context) ([]InboxItem, error) {
	answer, err := s.run(ctx, slackRun)
	if err != nil {
		return nil, err
	}
	var items []InboxItem
	for _, a := range answer {
		u, err := url.Parse(a.URL)
		t, terr := time.Parse(time.RFC3339, a.Time)
		if err != nil || u.Scheme != "https" || !strings.HasSuffix(u.Host, ".slack.com") || terr != nil || a.Title == "" ||
			(a.Kind != "mention" && a.Kind != "direct message") {
			continue
		}
		items = append(items, InboxItem{ID: u.Host + u.Path, Kind: a.Kind, Title: a.Title, Description: a.Text, URL: a.URL, Author: a.Author, Time: t})
	}
	return items, nil
}

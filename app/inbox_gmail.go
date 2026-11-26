package main

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"time"
)

// gmailSource reads mail through the Gmail connector of the user's Claude account.
type gmailSource struct{ run connectorRunner }

const (
	gmailDays     = 7
	gmailMaxItems = 25
	gmailMaxOpen  = 15
)

const gmailSystem = `You collect the emails that need the user's attention, for their inbox. You only read mail; never send, draft, label, archive, trash or mark anything.

1. Find the user's own address: search for threads from:me and look at the sender.
2. Search the threads of the last %d days with search_threads, for example "in:inbox newer_than:%dd -category:promotions -category:social -category:updates -category:forums".
3. Keep a thread only when a person asks something of the user in it: a direct ask, a question or a request addressed to them, or a thread that waits for their reply. Skip newsletters, notifications, promotions, receipts, calendar invites, mailing lists, automated or no-reply mail, and threads whose last message the user wrote.
4. Decide from the search results where you can. Open a thread with get_thread only when they do not show what is asked, and open at most %d threads.
5. Print at most %d items, one per thread, newest first, as one JSON object and nothing else, no code fence:
{"items":[{"id":"<the Gmail thread id>","kind":"question" or "request" or "awaiting reply","title":"<the subject>","text":"<what is asked of the user, in at most 500 characters>","author":"<name of the sender of the last message>","time":"<RFC 3339 time of the last message>"}]}`

const gmailPrefix = "mcp__claude_ai_Gmail__"

var gmailRead = []string{"search_threads", "get_thread", "get_message", "list_labels"}

var gmailRun = connectorRun{
	source: "gmail",
	label:  "Gmail",
	prefix: gmailPrefix,
	read:   gmailRead,
	write: []string{
		"send_message", "reply", "forward", "create_draft", "update_draft", "delete_draft",
		"trash_message", "trash_thread", "untrash_message", "untrash_thread",
		"label_message", "label_thread", "unlabel_message", "unlabel_thread", "update_message_labels",
		"apply_sensitive_message_label", "apply_sensitive_thread_label", "create_label", "update_label", "delete_label",
		"mark_message_spam", "mark_thread_spam", "unmark_message_spam", "unmark_thread_spam",
	},
	system: fmt.Sprintf(gmailSystem, gmailDays, gmailDays, gmailMaxOpen, gmailMaxItems) + connectorRules("Gmail", gmailPrefix, gmailRead, "Emails"),
	prompt: "Collect my Gmail inbox.",
}

var gmailThread = regexp.MustCompile(`^[0-9A-Za-z_-]{6,64}$`)

var gmailKinds = []string{"question", "request", "awaiting reply"}

func (gmailSource) name() string { return "gmail" }

func (gmailSource) ready() error { return claudeReady("Gmail") }

func (s gmailSource) fetch(ctx context.Context) ([]InboxItem, error) {
	answer, err := s.run(ctx, gmailRun)
	if err != nil {
		return nil, err
	}
	var items []InboxItem
	for _, a := range answer {
		t, terr := time.Parse(time.RFC3339, a.Time)
		if !gmailThread.MatchString(a.ID) || terr != nil || a.Title == "" || !slices.Contains(gmailKinds, a.Kind) {
			continue
		}
		// The link is built from the thread id, never taken from the answer.
		items = append(items, InboxItem{ID: a.ID, Kind: a.Kind, Title: a.Title, Description: a.Text, URL: "https://mail.google.com/mail/u/0/#inbox/" + a.ID, Author: a.Author, Time: t})
	}
	return items, nil
}

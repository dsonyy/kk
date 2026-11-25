package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// connectorRun asks a headless Claude Code run to read a service through a claude.ai connector of the user's Claude account.
// The run may use only the read tools; dontAsk mode refuses every other tool, and the write tools are denied as well.
type connectorRun struct {
	label  string
	server string
	prefix string
	read   []string
	write  []string
	system string
	prompt string
}

// connectorItem is one item of the run's answer, before the source checks it.
type connectorItem struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	Title  string `json:"title"`
	Text   string `json:"text"`
	Author string `json:"author"`
	URL    string `json:"url"`
	Time   string `json:"time"`
}

func (c connectorRun) tools(names []string) string {
	full := make([]string, len(names))
	for i, t := range names {
		full[i] = c.prefix + t
	}
	return strings.Join(full, ",")
}

func claudeReady(label string) error {
	if _, err := exec.LookPath("claude"); err != nil {
		return fmt.Errorf("Install Claude Code and log in; the %s connector of your Claude account is used.", label)
	}
	return nil
}

func (c connectorRun) fetch(ctx context.Context, dir string) ([]connectorItem, error) {
	cmd := exec.CommandContext(ctx, "claude", "-p", "--model", "sonnet", "--no-session-persistence", "--setting-sources", "", "--tools", "",
		"--permission-mode", "dontAsk", "--allowedTools", c.tools(c.read), "--disallowedTools", c.tools(c.write), "--disable-slash-commands",
		"--output-format", "stream-json", "--verbose", "--system-prompt", c.system,
		"--", fmt.Sprintf("%s Now is %s.", c.prompt, time.Now().Format(time.RFC3339)))
	cmd.Dir = dir
	// claude -p asks the model before the claude.ai connectors have connected, unless told to wait for them.
	cmd.Env = append(os.Environ(), "MCP_CONNECTION_NONBLOCKING=false", "MCP_CONNECT_TIMEOUT_MS=30000")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	items, perr := c.parse(out)
	if errors.Is(perr, errNoRun) && err != nil {
		return nil, fmt.Errorf("claude exited: %v %s", err, firstLine(stderr.String(), 200))
	}
	return items, perr
}

var errNoRun = errors.New("the run printed no result")

// parse reads the run's events: the init event lists the claude.ai connectors and their status, the result holds the answer.
// The answer itself is never logged, since it holds message text.
func (c connectorRun) parse(out []byte) ([]connectorItem, error) {
	status, text, failed, done, connectors := "", "", false, false, false
	for _, line := range bytes.Split(out, []byte("\n")) {
		var ev struct {
			Type       string `json:"type"`
			Subtype    string `json:"subtype"`
			IsError    bool   `json:"is_error"`
			Result     string `json:"result"`
			MCPServers []struct {
				Name   string `json:"name"`
				Status string `json:"status"`
				Source string `json:"source"`
			} `json:"mcp_servers"`
		}
		if json.Unmarshal(line, &ev) != nil {
			continue
		}
		switch {
		case ev.Type == "system" && ev.Subtype == "init":
			for _, m := range ev.MCPServers {
				connectors = connectors || m.Source == "claudeai"
				if m.Name == c.server {
					status = m.Status
				}
			}
		case ev.Type == "result":
			done, failed, text = true, ev.IsError || ev.Subtype != "success", ev.Result
			if text == "" {
				text = ev.Subtype
			}
		}
	}
	switch {
	case !done:
		return nil, errNoRun
	case failed:
		return nil, fmt.Errorf("the %s run failed: %s", c.label, firstLine(text, 200))
	case !connectors:
		// The run lists no claude.ai connector at all when it gave up waiting for them.
		return nil, errors.New("the claude.ai connectors did not load in time")
	case status == "" || status == "needs-auth":
		return nil, notSetUp(fmt.Sprintf("Connect %s to your Claude account at claude.ai, Settings, Connectors.", c.label))
	case status != "connected":
		return nil, fmt.Errorf("the %s connector is %s", c.label, status)
	}
	text = strings.TrimSpace(text)
	if i, j := strings.Index(text, "{"), strings.LastIndex(text, "}"); i >= 0 && j > i {
		text = text[i : j+1]
	}
	var answer struct {
		Items []connectorItem `json:"items"`
	}
	if err := json.Unmarshal([]byte(text), &answer); err != nil || answer.Items == nil {
		return nil, fmt.Errorf("the %s run did not answer with the expected JSON", c.label)
	}
	return answer.Items, nil
}

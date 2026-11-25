package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// Usage is what an agent's models read and wrote, and what that costs at API list prices.
type Usage struct {
	// Input excludes the tokens read from or written to the prompt cache.
	Input      int `yaml:"input,omitempty" json:"input"`
	CacheRead  int `yaml:"cache_read,omitempty" json:"cache_read"`
	CacheWrite int `yaml:"cache_write,omitempty" json:"cache_write"`
	Output     int `yaml:"output,omitempty" json:"output"`
	// Cost is in US dollars, for the tokens of the models kk has a price for.
	Cost float64 `yaml:"cost,omitempty" json:"cost"`
	// Unpriced are the models whose tokens are counted without a price.
	Unpriced []string `yaml:"unpriced,omitempty" json:"unpriced,omitempty"`
}

func (u *Usage) add(o Usage) {
	u.Input += o.Input
	u.CacheRead += o.CacheRead
	u.CacheWrite += o.CacheWrite
	u.Output += o.Output
	u.Cost += o.Cost
	for _, m := range o.Unpriced {
		if !slices.Contains(u.Unpriced, m) {
			u.Unpriced = append(u.Unpriced, m)
		}
	}
}

func (u Usage) equal(o Usage) bool {
	return u.Input == o.Input && u.CacheRead == o.CacheRead && u.CacheWrite == o.CacheWrite && u.Output == o.Output && u.Cost == o.Cost && slices.Equal(u.Unpriced, o.Unpriced)
}

// price is in US dollars per million tokens.
type price struct{ input, cacheRead, write5m, write1h, output float64 }

func (p price) times(f float64) price {
	return price{p.input * f, p.cacheRead * f, p.write5m * f, p.write1h * f, p.output * f}
}

// List prices from https://platform.claude.com/docs/en/about-claude/pricing, 2026-11-25.
var claudePrices = map[string]price{
	"claude-fable-5-2":  {8, 0.20, 10, 16, 40},
	"claude-fable-5-1":  {10, 0.25, 12.5, 20, 50},
	"claude-fable-5":    {10, 1, 12.5, 20, 50},
	"claude-opus-5-5":   {4, 0.20, 5, 8, 20},
	"claude-opus-5":     {5, 0.50, 6.25, 10, 25},
	"claude-opus-4-8":   {5, 0.50, 6.25, 10, 25},
	"claude-opus-4-7":   {5, 0.50, 6.25, 10, 25},
	"claude-opus-4-6":   {5, 0.50, 6.25, 10, 25},
	"claude-opus-4-5":   {5, 0.50, 6.25, 10, 25},
	"claude-opus-4-1":   {15, 1.50, 18.75, 30, 75},
	"claude-opus-4":     {15, 1.50, 18.75, 30, 75},
	"claude-sonnet-5-5": {2, 0.10, 2.5, 4, 10},
	"claude-sonnet-5":   {2, 0.20, 2.5, 4, 10},
	"claude-sonnet-4-6": {3, 0.30, 3.75, 6, 15},
	"claude-sonnet-4-5": {3, 0.30, 3.75, 6, 15},
	"claude-sonnet-4":   {3, 0.30, 3.75, 6, 15},
	"claude-haiku-5-5":  {0.10, 0.01, 0.125, 0.20, 0.50},
	"claude-haiku-4-5":  {1, 0.10, 1.25, 2, 5},
}

var (
	// Claude Haiku 5.5 costs more for a request whose prompt is over 100,000 tokens.
	claudeLongPrices = map[string]price{"claude-haiku-5-5": {0.50, 0.05, 0.625, 1, 2.50}}
	// Fast mode doubles every price of the models that have it.
	claudeFast = []string{"claude-fable-5-2", "claude-opus-5-5", "claude-opus-5", "claude-opus-4-8"}
)

const (
	claudeLongPrompt = 100_000
	// claudeSearch is the price of one web search.
	claudeSearch = 0.01
	// claudeUS is the multiplier for inference kept in the US.
	claudeUS = 1.1
)

var modelDate = regexp.MustCompile(`-\d{8}$`)

type claudeUsage struct {
	Input       int    `json:"input_tokens"`
	CacheRead   int    `json:"cache_read_input_tokens"`
	CacheCreate int    `json:"cache_creation_input_tokens"`
	Output      int    `json:"output_tokens"`
	Speed       string `json:"speed"`
	Geo         string `json:"inference_geo"`
	CacheWrite  *struct {
		Hour int `json:"ephemeral_1h_input_tokens"`
	} `json:"cache_creation"`
	ServerTools struct {
		Searches int `json:"web_search_requests"`
	} `json:"server_tool_use"`
}

// claudeRequest prices the usage of one request to the model.
func claudeRequest(model string, u claudeUsage) Usage {
	out := Usage{Input: u.Input, CacheRead: u.CacheRead, CacheWrite: u.CacheCreate, Output: u.Output}
	base := modelDate.ReplaceAllString(model, "")
	p, ok := claudePrices[base]
	if long, has := claudeLongPrices[base]; has && u.Input+u.CacheRead+u.CacheCreate > claudeLongPrompt {
		p = long
	}
	if u.Speed == "fast" {
		ok = ok && slices.Contains(claudeFast, base)
		p = p.times(2)
	}
	if u.Geo == "us" {
		p = p.times(claudeUS)
	}
	if !ok {
		out.Unpriced = []string{model}
		return out
	}
	hour := 0
	if u.CacheWrite != nil {
		hour = min(u.CacheWrite.Hour, u.CacheCreate)
	}
	out.Cost = (float64(u.Input)*p.input + float64(u.CacheRead)*p.cacheRead + float64(u.CacheCreate-hour)*p.write5m +
		float64(hour)*p.write1h + float64(u.Output)*p.output) / 1e6
	out.Cost += float64(u.ServerTools.Searches) * claudeSearch
	return out
}

// meterClaude counts one assistant line. Claude Code writes a line per content block of a message, each with the
// usage of the whole message, so a message counts once, at its last line.
func meterClaude(t *Tail, l *claudeLine) {
	m := l.Message
	if l.Type != "assistant" || m.Usage == nil || m.Model == "<synthetic>" {
		return
	}
	u := claudeRequest(m.Model, *m.Usage)
	if m.ID != "" {
		if before, ok := t.metered[m.ID]; ok {
			t.Usage.add(negate(before))
		}
		t.metered[m.ID] = u
	}
	t.Usage.add(u)
}

func negate(u Usage) Usage {
	return Usage{Input: -u.Input, CacheRead: -u.CacheRead, CacheWrite: -u.CacheWrite, Output: -u.Output, Cost: -u.Cost}
}

// parseClaudeUsage reads only the usage of a Claude Code transcript, for the files of its subagents.
func parseClaudeUsage(t *Tail, raw []byte) {
	var l claudeLine
	if json.Unmarshal(raw, &l) == nil {
		meterClaude(t, &l)
	}
}

// claudeSubagents lists the transcripts of the subagents of a Claude Code session, which sit next to its own.
func claudeSubagents(transcript string) []string {
	paths, _ := filepath.Glob(filepath.Join(strings.TrimSuffix(transcript, ".jsonl"), "subagents", "*.jsonl"))
	return paths
}

// List prices from https://developers.openai.com/api/docs/pricing, 2026-11-25. The second price applies to a request
// with more than 272,000 input tokens; a model without one has no price for such a request.
var codexPrices = map[string][2]price{
	"gpt-5.5":       {{input: 5, cacheRead: 0.50, output: 30}, {input: 10, cacheRead: 1, output: 45}},
	"gpt-5.6-luna":  {{input: 0.20, cacheRead: 0.02, output: 1.20}, {input: 0.40, cacheRead: 0.04, output: 1.80}},
	"gpt-5.6-sol":   {{input: 4, cacheRead: 0.40, output: 20}, {input: 8, cacheRead: 0.80, output: 30}},
	"gpt-5.6-terra": {{input: 2, cacheRead: 0.20, output: 12}, {input: 4, cacheRead: 0.40, output: 18}},
	"gpt-6-astra":   {{input: 10, cacheRead: 1, output: 50}, {input: 20, cacheRead: 2, output: 75}},
	"gpt-5.3-codex": {{input: 1.75, cacheRead: 0.175, output: 14}},
}

const codexLongPrompt = 272_000

// codexCount is a token count of a Codex rollout. Input includes the cached input, and output the reasoning.
type codexCount struct {
	Input      int `json:"input_tokens"`
	Cached     int `json:"cached_input_tokens"`
	CacheWrite int `json:"cache_write_input_tokens"`
	Output     int `json:"output_tokens"`
	Total      int `json:"total_tokens"`
}

// meterCodex counts a token_count event. Codex reports the running total of the thread, also across a resume, so
// what a request used is the difference to the total before it. A total that falls starts the count again.
func meterCodex(t *Tail, total, last codexCount) {
	d := total
	if prev := t.codexTotal; total.Total >= prev.Total {
		d = codexCount{total.Input - prev.Input, total.Cached - prev.Cached, total.CacheWrite - prev.CacheWrite, total.Output - prev.Output, total.Total - prev.Total}
	}
	t.codexTotal = total
	if d.Total == 0 {
		return
	}
	u := Usage{Input: d.Input - d.Cached, CacheRead: d.Cached, Output: d.Output}
	tiers, ok := codexPrices[t.model]
	p := tiers[0]
	if last.Input > codexLongPrompt {
		p = tiers[1]
		ok = ok && p.input > 0
	}
	// The pricing page has cache write prices for two cache lifetimes, and the rollout does not say which one applies.
	if !ok || d.CacheWrite > 0 {
		u.Unpriced = []string{t.model}
	} else {
		u.Cost = (float64(u.Input)*p.input + float64(u.CacheRead)*p.cacheRead + float64(u.Output)*p.output) / 1e6
	}
	t.Usage.add(u)
}

// codexChildren maps the rollouts of Codex subagents to the thread that spawned them. A rollout's parent never
// changes, so each file is read once.
var codexChildren = struct {
	parent map[string]string
}{parent: map[string]string{}}

// codexSubagents lists the rollouts of the subagents of a Codex thread, theirs included, next to the thread's rollout
// or anywhere in Codex's history. The caller holds s.mu.
func codexSubagents(thread, rollout string) []string {
	near, _ := filepath.Glob(filepath.Join(filepath.Dir(rollout), "*.jsonl"))
	all, _ := filepath.Glob(filepath.Join(codexHome(), "sessions", "*", "*", "*", "rollout-*.jsonl"))
	children := map[string][]string{}
	for _, p := range append(near, all...) {
		parent, ok := codexChildren.parent[p]
		if !ok {
			parent = codexParent(p)
			codexChildren.parent[p] = parent
		}
		if parent != "" && !slices.Contains(children[parent], p) {
			children[parent] = append(children[parent], p)
		}
	}
	var out []string
	for queue := []string{thread}; len(queue) > 0; queue = queue[1:] {
		for _, p := range children[queue[0]] {
			out = append(out, p)
			queue = append(queue, codexThread(p))
		}
	}
	return out
}

// codexParent reads the thread that spawned a rollout's thread from its first line.
func codexParent(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	var meta struct {
		Payload struct {
			Source struct {
				Subagent struct {
					Spawn struct {
						Parent string `json:"parent_thread_id"`
					} `json:"thread_spawn"`
				} `json:"subagent"`
			} `json:"source"`
		} `json:"payload"`
	}
	json.NewDecoder(f).Decode(&meta)
	return meta.Payload.Source.Subagent.Spawn.Parent
}

// codexThread is the thread id at the end of a rollout's name.
func codexThread(path string) string {
	name := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	if len(name) < 36 {
		return ""
	}
	return name[len(name)-36:]
}

func meterCodexLine(t *Tail, l *codexLine) {
	switch {
	case l.Type == "turn_context" && l.Payload.Model != "":
		t.model = l.Payload.Model
	case l.Type == "event_msg" && l.Payload.Type == "token_count" && l.Payload.Info != nil && l.Payload.Info.Total != nil:
		meterCodex(t, *l.Payload.Info.Total, l.Payload.Info.Last)
	}
}

// parseCodexUsage reads only the usage of a Codex rollout, for the rollouts of its subagents.
func parseCodexUsage(t *Tail, raw []byte) {
	var l codexLine
	if json.Unmarshal(raw, &l) == nil {
		meterCodexLine(t, &l)
	}
}

// subagents lists, per harness that reports usage, the transcripts of the subagents of a conversation's current one.
var subagents = map[string]func(c Conversation) []string{
	"claude": func(c Conversation) []string { return claudeSubagents(c.Transcript) },
	"codex": func(c Conversation) []string {
		if c.Session == "" {
			return nil
		}
		return codexSubagents(c.Session, c.Transcript)
	},
}

// usageParser reads the usage of a subagent's transcript, per harness that reports usage. Cursor CLI 2026.11 does not:
// its transcript has none, and its only hook with usage, afterAgentResponse, runs for user and project hooks, not for a plugin.
var usageParser = map[string]func(*Tail, []byte){"claude": parseClaudeUsage, "codex": parseCodexUsage}

// TotalUsage adds up the usage of every transcript of the conversation.
func (c *Conversation) TotalUsage() Usage {
	var u Usage
	for _, t := range c.Usage {
		u.add(t)
	}
	slices.Sort(u.Unpriced)
	return u
}

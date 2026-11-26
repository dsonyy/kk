package main

import (
	"cmp"
	"encoding/json"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
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

// Activity is what happened in one transcript file: what its models used, in all, per local day and per model, and
// how many turns and tool calls it holds. A rewound branch stays counted; a harness that rewrites its transcript drops it.
type Activity struct {
	Usage  `yaml:",inline"`
	Days   map[string]Usage `yaml:"days,omitempty" json:"days,omitempty"`
	Models map[string]Usage `yaml:"models,omitempty" json:"models,omitempty"`
	Turns  int              `yaml:"turns,omitempty" json:"turns"`
	Tools  map[string]int   `yaml:"tools,omitempty" json:"tools,omitempty"`
}

// meter counts the usage of a request made at a time to a model. A request whose time is not recorded has no day.
func (a *Activity) meter(at time.Time, model string, u Usage) {
	a.Usage.add(u)
	if !at.IsZero() {
		a.Days = addUsage(a.Days, at.Local().Format(time.DateOnly), u)
	}
	a.Models = addUsage(a.Models, model, u)
}

func (a *Activity) add(o Activity) {
	a.Usage.add(o.Usage)
	a.Turns += o.Turns
	for day, u := range o.Days {
		a.Days = addUsage(a.Days, day, u)
	}
	for model, u := range o.Models {
		a.Models = addUsage(a.Models, model, u)
	}
	for tool, n := range o.Tools {
		a.tool(tool, n)
	}
}

func (a *Activity) tool(name string, n int) {
	if a.Tools == nil {
		a.Tools = map[string]int{}
	}
	a.Tools[name] += n
}

func addUsage(m map[string]Usage, key string, u Usage) map[string]Usage {
	if m == nil {
		m = map[string]Usage{}
	}
	sum := m[key]
	sum.add(u)
	m[key] = sum
	return m
}

// clone copies the activity, whose maps its tail keeps changing.
func (a Activity) clone() Activity {
	var c Activity
	c.add(a)
	slices.Sort(c.Unpriced)
	return c
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

// claudeUsage is the usage of a Claude Code message. A message that used a server-side tool such as the advisor made
// several calls, each recorded in Calls; the advisor's carries the advisor's model. The top-level counts add up only
// the calls of the message's own model, and Claude Code 2.1 before iterations records just them.
type claudeUsage struct {
	claudeCall
	Speed       string `json:"speed"`
	Geo         string `json:"inference_geo"`
	ServerTools struct {
		Searches int `json:"web_search_requests"`
	} `json:"server_tool_use"`
	Calls []claudeCall `json:"iterations"`
}

type claudeCall struct {
	Input       int `json:"input_tokens"`
	CacheRead   int `json:"cache_read_input_tokens"`
	CacheCreate int `json:"cache_creation_input_tokens"`
	Output      int `json:"output_tokens"`
	CacheWrite  *struct {
		Hour int `json:"ephemeral_1h_input_tokens"`
	} `json:"cache_creation"`
	// Model is set only on a call to another model than the message's.
	Model string `json:"model"`
}

func (c claudeCall) prompt() int { return c.Input + c.CacheRead + c.CacheCreate }

func (u claudeUsage) calls() []claudeCall {
	if len(u.Calls) == 0 {
		return []claudeCall{u.claudeCall}
	}
	return u.Calls
}

// context is the prompt of the last call of the message's own model.
func (u claudeUsage) context() int {
	calls := u.calls()
	for i := len(calls) - 1; i >= 0; i-- {
		if calls[i].Model == "" {
			return calls[i].prompt()
		}
	}
	return 0
}

// claudeRequest prices one call to a model. Fast mode is a setting of the message's own model, so it prices only its
// calls; the region applies to every call.
func claudeRequest(model string, c claudeCall, fast bool, geo string) Usage {
	out := Usage{Input: c.Input, CacheRead: c.CacheRead, CacheWrite: c.CacheCreate, Output: c.Output}
	base := modelDate.ReplaceAllString(model, "")
	p, ok := claudePrices[base]
	if long, has := claudeLongPrices[base]; has && c.prompt() > claudeLongPrompt {
		p = long
	}
	if fast {
		ok = ok && slices.Contains(claudeFast, base)
		p = p.times(2)
	}
	if geo == "us" {
		p = p.times(claudeUS)
	}
	if !ok {
		out.Unpriced = []string{model}
		return out
	}
	hour := 0
	if c.CacheWrite != nil {
		hour = min(c.CacheWrite.Hour, c.CacheCreate)
	}
	out.Cost = (float64(c.Input)*p.input + float64(c.CacheRead)*p.cacheRead + float64(c.CacheCreate-hour)*p.write5m +
		float64(hour)*p.write1h + float64(c.Output)*p.output) / 1e6
	return out
}

// claudeMessage prices every call of a message, by model. Its web searches are charged once, to its own model.
func claudeMessage(model string, u claudeUsage) map[string]Usage {
	out := map[string]Usage{}
	for _, c := range u.calls() {
		m := cmp.Or(c.Model, model)
		r := claudeRequest(m, c, u.Speed == "fast" && c.Model == "", u.Geo)
		sum := out[m]
		sum.add(r)
		out[m] = sum
	}
	if r := out[model]; r.Unpriced == nil {
		r.Cost += float64(u.ServerTools.Searches) * claudeSearch
		out[model] = r
	}
	return out
}

// meterClaude counts one assistant line. Claude Code writes a line per content block of a message, each with the
// usage of the whole message, so a message counts once, at its last line.
func meterClaude(t *Tail, l *claudeLine) {
	m := l.Message
	if l.Type != "assistant" || m.Usage == nil || m.Model == "<synthetic>" {
		return
	}
	calls := claudeMessage(m.Model, *m.Usage)
	if m.ID != "" {
		if before, ok := t.metered[m.ID]; ok {
			for model, u := range before.calls {
				t.Activity.meter(before.at, model, negate(u))
			}
		}
		t.metered[m.ID] = metered{l.Timestamp, calls}
	}
	for model, u := range calls {
		t.Activity.meter(l.Timestamp, model, u)
	}
}

// metered is the usage counted for a Claude Code message, by model, and when.
type metered struct {
	at    time.Time
	calls map[string]Usage
}

func negate(u Usage) Usage {
	return Usage{Input: -u.Input, CacheRead: -u.CacheRead, CacheWrite: -u.CacheWrite, Output: -u.Output, Cost: -u.Cost}
}

// parseClaudeUsage reads only the usage and the context of a Claude Code transcript, for the files of its subagents.
func parseClaudeUsage(t *Tail, raw []byte) {
	var l claudeLine
	if json.Unmarshal(raw, &l) == nil {
		meterClaude(t, &l)
		if l.Type == "assistant" && l.Message.Usage != nil {
			t.Context = l.Message.Usage.context()
		}
		if l.Type == "assistant" && l.Message.Model != "" && l.Message.Model != "<synthetic>" {
			t.model = l.Message.Model
		}
		t.clock(l.Timestamp)
	}
}

// clock keeps the times of the first and the last line of a subagent's transcript.
func (t *Tail) clock(at time.Time) {
	if at.IsZero() {
		return
	}
	if t.first.IsZero() {
		t.first = at
	}
	if at.After(t.Last) {
		t.Last = at
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
func meterCodex(t *Tail, at time.Time, total, last codexCount) {
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
	t.Activity.meter(at, t.model, u)
}

// codexSpawns holds how the thread of each rollout was spawned, empty for a thread nobody spawned. A rollout's spawn
// never changes, so each file is read once.
var codexSpawns = struct {
	sync.Mutex
	of map[string]codexSpawn
}{of: map[string]codexSpawn{}}

// codexSpawn is the thread that spawned a subagent's thread, and the name and role Codex gave the subagent.
type codexSpawn struct {
	Parent   string `json:"parent_thread_id"`
	Nickname string `json:"agent_nickname"`
	Role     string `json:"agent_role"`
}

// codexSubagents lists the rollouts of the subagents of a Codex thread, theirs included, next to the thread's rollout
// or anywhere in Codex's history.
func codexSubagents(thread, rollout string) []string {
	near, _ := filepath.Glob(filepath.Join(filepath.Dir(rollout), "*.jsonl"))
	all, _ := filepath.Glob(filepath.Join(codexHome(), "sessions", "*", "*", "*", "rollout-*.jsonl"))
	children := map[string][]string{}
	for _, p := range append(near, all...) {
		if parent := codexSpawnOf(p).Parent; parent != "" && !slices.Contains(children[parent], p) {
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

// codexSpawnOf reads how a rollout's thread was spawned from its first line.
func codexSpawnOf(path string) codexSpawn {
	codexSpawns.Lock()
	defer codexSpawns.Unlock()
	if spawn, ok := codexSpawns.of[path]; ok {
		return spawn
	}
	f, err := os.Open(path)
	if err != nil {
		return codexSpawn{}
	}
	defer f.Close()
	var meta struct {
		Payload struct {
			Source struct {
				Subagent struct {
					Spawn codexSpawn `json:"thread_spawn"`
				} `json:"subagent"`
			} `json:"source"`
		} `json:"payload"`
	}
	json.NewDecoder(f).Decode(&meta)
	codexSpawns.of[path] = meta.Payload.Source.Subagent.Spawn
	return meta.Payload.Source.Subagent.Spawn
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
		meterCodex(t, l.Timestamp, *l.Payload.Info.Total, l.Payload.Info.Last)
	}
}

// parseCodexUsage reads only the usage of a Codex rollout, for the rollouts of its subagents.
func parseCodexUsage(t *Tail, raw []byte) {
	var l codexLine
	if json.Unmarshal(raw, &l) == nil {
		meterCodexLine(t, &l)
		t.clock(l.Timestamp)
		switch l.Payload.Type {
		case "task_started":
			t.busy = true
		case "task_complete", "turn_aborted":
			t.busy = false
		}
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

// TotalActivity adds up the activity of every transcript of the conversation.
func (c *Conversation) TotalActivity() Activity {
	var a Activity
	for _, t := range c.Transcripts {
		a.add(t)
	}
	slices.Sort(a.Unpriced)
	return a
}

// activityRow is a conversation as the Stats tab counts it.
type activityRow struct {
	ID      string `json:"id"`
	Role    string `json:"role,omitempty"`
	Harness string `json:"harness"`
	Project string `json:"project"`
	// Metered tells whether the harness reports usage. Without it the row's usage is unknown, not zero.
	Metered     bool      `json:"metered"`
	Created     time.Time `json:"created"`
	LastMessage time.Time `json:"last_message"`
	Closed      bool      `json:"closed"`
	Activity
}

func (s *Server) handleActivity(*http.Request) (any, error) {
	out := []activityRow{}
	for _, c := range s.store.list() {
		out = append(out, activityRow{c.ID, c.Role, c.Harness, c.Project(), usageParser[c.Harness] != nil, c.Created, c.LastMessage, c.Closed, c.TotalActivity()})
	}
	return out, nil
}

// remeter counts the usage of every Claude Code transcript that conversations keep, and the context of the current
// one, again, so that what an earlier version of kk stored follows the current count. The tick does so only for open
// conversations. It keeps the turns and tools, which only the full parser counts, and what is stored for a file that
// is gone.
func (s *Server) remeter() {
	for _, c := range s.store.list() {
		if c.Harness != "claude" {
			continue
		}
		fresh, context := map[string]Activity{}, -1
		for path := range c.Transcripts {
			t := newTail(path)
			if _, err := t.read(parseClaudeUsage); err == nil {
				fresh[path] = t.Activity.clone()
				if path == c.Transcript {
					context = t.Context
				}
			}
		}
		s.store.update(c.ID, func(c *Conversation) bool {
			changed := false
			if context >= 0 && c.Context != context {
				c.Context, changed = context, true
			}
			for path, f := range fresh {
				a, ok := c.Transcripts[path]
				if !ok || reflect.DeepEqual([]any{a.Usage, a.Days, a.Models}, []any{f.Usage, f.Days, f.Models}) {
					continue
				}
				a.Usage, a.Days, a.Models = f.Usage, f.Days, f.Models
				c.Transcripts = maps.Clone(c.Transcripts)
				c.Transcripts[path], changed = a, true
			}
			return changed
		})
	}
}

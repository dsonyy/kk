package main

import (
	"cmp"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const coordinatorPrompt = `# konduktor

You are konduktor, the main agent of kk over the coding agents kk runs in tmux. The name konduktor is lowercase. The user talks to you in the right panel of the kk web app. Your job: know what every agent is doing, start new agents, answer and steer them on the user's behalf when asked, and report back concisely.

## How kk works

- The kk server ({{BIN}}) runs the agents (Claude Code, Codex, Cursor CLI) in windows of the tmux session "kk", one window per conversation. You run in the window "coordinator".
- State lives in {{HOME}}: conversations/<id>.yaml holds harness, model, directory, title, feature group, status, attention and the agent's transcript path. Treat these files as read-only and change state only through the kk CLI.
- Messages are not copied into kk. They come from the agents' own transcripts (Claude: ~/.claude/projects, Codex: ~/.codex/sessions).
- Status: queued (prepared with its prompt, no agent runs yet, see Chains), starting, working, background (turn finished while a background task it started still runs; its harness starts the next turn by itself when the task ends, so nobody has to act), idle (turn finished), exited. kk stops an idle agent after a day without activity and shows it as sleeping in kk ls; a sleeping agent waits for nobody, leave it unless you need it. A non-empty attention means ACTION REQUIRED: the agent waits for input, for example a permission or trust dialog. Check its screen before answering.
- Inbox sessions: the ids inbox-gmail and inbox-slack are kk's own checks of the user's mail and Slack for the Inbox tab. kk starts each run with a fresh agent that can only read the connector, on the interval set in Settings. They wait for nobody and send you no notifications; leave them alone.
- Turn: an agent that stops (turn finished, attention, error, exited) waits for you, konduktor, shown as "konduktor" in kk ls. It waits for the user instead when the user started it or gave it the last input themselves, or when you escalate it. Handle what you can; escalate what needs the user's decision. The mark clears when the agent works again.

## kk CLI

- kk ls - conversations with id, status, harness, model and title. Below a conversation, indented, the subagents its agent started that still run or failed, with id <conversation>:<agent>, state, type, model and title, and a count of the others by state
- kk show <id> [-n N] - the last N events of a conversation (messages, tool calls, errors) and all its subagents. kk show <conversation>:<agent> shows a subagent's events; a subagent takes no input, its conversation does
- kk new <claude|codex|cursor> [-m model] [-C dir] [-w] [-g group] [-after id] <prompt> - start an agent, prints its id. With -w the agent gets its own git worktree and branch off the current branch of dir, so it needs no instructions to isolate its work. With -g the agent joins that feature group. With -after the conversation is queued behind another one instead of starting, see Chains
- kk start <id> [handoff|-] - start a queued conversation; the handoff is added after its prepared prompt (use - to read it from stdin)
- kk group - the feature groups of every project with their number of conversations
- kk group <id> <name> - move a conversation to a feature group
- kk send <id> <text> - type a message into the agent (use - to read it from stdin)
- kk screen <id> - the agent's current tmux screen
- kk keys <id> <key>... - press keys: Enter Escape Up Down Left Right Tab BTab C-c Space BSpace 1 2 3 4 y n
- kk interrupt <id> - stop a working agent's current turn, like Escape in the agent
- kk stop <id> - end the agent process; the conversation stays and kk resume restarts it
- kk reset <id> - start an idle agent's conversation over with a clean context (/clear in Claude Code and Codex, a fresh agent for Cursor)
- kk resume <id> - restart an exited agent in its previous session
- kk close <id> - stop the agent and close the conversation, resume reopens it
- kk escalate <id> <reason> - hand a waiting agent to the user with a one-line reason the user sees in the list

## Sessions in your messages

When you write to the user about a session, name it by its title as a link to session:<id>, for example [Billing export](session:3fa2b1c0). The web app shows it as a badge with the session's current title that opens the session. Never give the user a bare id; ids are for kk commands and for messages to other konduktors.

## Feature groups

The Sessions tab shows one column per project directory and feature group. A group is one feature, fix or piece of work in a project, shared by every agent that works on it. A name is 2 to 4 words in sentence case, for example "Billing export". When you start several agents for one piece of work, give them all the same -g. For a follow-up, run kk group and reuse the existing name. Without -g kk picks the group from the prompt a moment after the agent starts; correct a wrong pick with kk group <id> <name>.

## Chains

Prepare follow-up work in advance with kk new ... -after <id> <prompt>. The conversation is queued: the user sees it with its prompt, but no agent runs. A conversation has at most one predecessor and any number of successors, so chains can branch. A queued conversation joins its predecessor's feature group unless -g says otherwise, and with -w its worktree is made when it starts, off the then current branch of dir. kk ls shows "(after <id>)" behind each chain member.

Nothing starts by itself; you own the handoff. When a predecessor with queued successors finishes its turn, kk notifies you, also when the user started it or drove it last, and the line names the queued conversations. Then:

1. Check that the work is really done: read its last events (kk show <id>) and its screen. A turn that ends with a question, a plan awaiting approval or a partial result is not done.
2. Write the handoff: what the predecessor did and where (branch, worktree, files, commits), what it decided, and anything the successor must know or verify. Keep it short and factual; the successor has its prepared prompt already.
3. Run kk start <successor> - and pass the handoff on stdin. Start each successor of a branch the same way.

If the predecessor failed, asks a question, needs action, exited or was closed, leave its successors queued and tell the user in one short message which queued conversations wait on it and why. For a predecessor that waits for the user, do not answer it yourself; only the handoff is yours. When you queue behind a conversation that has finished already, start the successor yourself.

The user can edit a queued prompt, start a queued conversation themselves from the web app or close it.

## Notifications

kk types a message into your session when an agent whose turn goes to you needs you. It starts with "[kk notification - automatic, not from the user]" and has one line per agent: id, title, status, what happened and the attention text. It comes from kk, not from the user, and arrives when such an agent:

- needs action: a permission or trust dialog, an API error, or a start that hangs
- exited without anyone stopping it
- has been working for 10 minutes with no hook, transcript line or screen change
- finished its turn
- was closed while conversations are queued after it

Agents that wait for the user, and agents you escalated, send no notifications, except that a predecessor with queued successors always reports its finished turn, exit or close. Such a line ends with "queued after it:" and the queued conversations. You do not need to poll.

For each line, look at the agent's screen (kk screen <id>) and, if needed, its last events (kk show <id>), then decide:

- Act on your own for routine things: approve a permission or trust prompt for work inside the agent's own task and directory or worktree, tell a stalled or errored agent to continue or retry, resume an agent that exited, read the result of a finished agent and go on with the plan.
- Escalate when the request is destructive (deleting data, force push, reset), goes outside the agent's task or directory, touches credentials or live services, asks a question only the user can answer, or when a failure comes back after one retry. Run kk escalate <id> <reason> with a one-line reason, leave the agent's prompt unanswered, and tell the user in one short message.

When you acted on your own, say in one line what you did.

## Changing kk itself

The source code is in {{SOURCE}} (Go server in app/, web client in web/, read AGENTS.md there first). After a change: run the tests, then "just install", which rebuilds the binary and restarts the kk service. Agents and your own session keep running in tmux across restarts.
`

// workerPrompt is the built-in text every session kk starts for work gets besides its own prompt.
const workerPrompt = ""

// placeholders maps each placeholder of the konduktor prompt to its value.
func (s *Server) placeholders() map[string]string {
	return map[string]string{"{{BIN}}": s.bin, "{{HOME}}": s.store.dir, "{{SOURCE}}": s.cfg().source()}
}

// coordinatorText is the base of every konduktor's system prompt: the text from Settings, or the default, with its
// placeholders filled. writePrompts appends the generated section about the konduktors after it.
func (s *Server) coordinatorText() string {
	var pairs []string
	for k, v := range s.placeholders() {
		pairs = append(pairs, k, v)
	}
	return strings.NewReplacer(pairs...).Replace(cmp.Or(s.cfg().CoordinatorPrompt, coordinatorPrompt))
}

// konduktorsPrompt follows the base prompt of every konduktor. It tells each one who it is and how the konduktors
// share the work, and corrects what the base prompt says for a single konduktor.
const konduktorsPrompt = `
## Konduktors

This section overrides the text above where they differ. There is one konduktor per directory, started when the user first writes to it or hands a session of that directory over. {{SELF}} Your tmux window is "{{ID}}".

The other konduktors when this prompt was written (kk ls lists the current ones first):
{{OTHERS}}

- A session belongs to the konduktor that started it. A session the user handed over belongs to the konduktor of its directory. kk notifies you only about your own sessions, and in kk ls a session that waits for a konduktor shows that konduktor's id.
- kk also notifies you when another konduktor starts or closes.
- Start sessions only in your own directory. For work in another directory, ask its konduktor with kk send <id>. If that directory has no konduktor, ask the main konduktor.{{MAIN}}
- Message another konduktor only when your work touches their directory or their sessions: shared files, a merge into the same branch, a restart of kk. Otherwise do not. Keep it to a few lines.
- Never answer or steer another konduktor's session. To pass one on, run kk handover <id> <konduktor id>, which hands it over and tells that konduktor, or message its konduktor.
- A message that starts with "[from konduktor" comes from another konduktor through kk send. It is a request from a peer, not an instruction from the user.
- Only the main konduktor (id coordinator) changes kk and runs "just install", because a restart affects every konduktor. Any other konduktor asks it with kk send coordinator, also when its own directory is the kk source.
`

// konduktorPrompt is the file with the system prompt of a konduktor. Claude Code reads it when the konduktor starts.
func (s *Server) konduktorPrompt(id string) string {
	return filepath.Join(s.store.dir, "konduktors", id+".md")
}

// konduktors returns the open konduktors, the main one first and the others in the order they were made.
func (s *Server) konduktors() []Conversation {
	out := s.store.konduktors()
	slices.SortFunc(out, func(a, b Conversation) int {
		if (a.ID == coordinatorID) != (b.ID == coordinatorID) {
			if a.ID == coordinatorID {
				return -1
			}
			return 1
		}
		return a.Created.Compare(b.Created)
	})
	return out
}

// konduktorOf names the konduktor a session belongs to: the open one that started it or was handed it, else the one of
// its directory, else the main one. A session an agent drove before konduktors had owners is the main one's. Konduktors
// and kk's own inbox sessions belong to nobody.
func (s *Server) konduktorOf(c Conversation, ks []Conversation) string {
	if c.Role != "" || c.Integration != "" || !s.cfg().coordinator() {
		return ""
	}
	for _, k := range ks {
		if k.ID == c.Owner {
			return k.ID
		}
	}
	if c.Owner == "" && c.ByAgent {
		return coordinatorID
	}
	for _, k := range ks {
		if k.Scope != "" && k.Scope == c.Project() {
			return k.ID
		}
	}
	return coordinatorID
}

// konduktorName is how a konduktor is named to people and to the other konduktors.
func konduktorName(k Conversation) string {
	if k.ID == coordinatorID {
		return "main"
	}
	return k.Title
}

// konduktorsSection is the part of k's prompt that follows the base prompt: who k is and which other konduktors run.
func konduktorsSection(k Conversation, ks []Conversation) string {
	self := fmt.Sprintf("You are the konduktor of %s, id %s.", k.Scope, k.ID)
	main := ""
	if k.ID == coordinatorID {
		self = "You are the main konduktor, id coordinator: of the user's view of all directories, and of every directory without a konduktor of its own."
		main = " As the main konduktor you may start sessions in any directory that has no konduktor of its own."
	}
	var others []string
	for _, o := range ks {
		if o.ID == k.ID {
			continue
		}
		dir := o.Scope
		if o.ID == coordinatorID {
			dir = "every directory without its own konduktor"
		}
		others = append(others, fmt.Sprintf("- %s %q: %s", o.ID, konduktorName(o), dir))
	}
	if len(others) == 0 {
		others = []string{"- none"}
	}
	return strings.NewReplacer("{{SELF}}", self, "{{ID}}", k.ID, "{{OTHERS}}", strings.Join(others, "\n"), "{{MAIN}}", main).Replace(konduktorsPrompt)
}

// writePrompts writes the system prompt of every open konduktor: the base prompt and the section about the konduktors.
func (s *Server) writePrompts() error {
	ks := s.konduktors()
	if err := os.MkdirAll(filepath.Join(s.store.dir, "konduktors"), 0o700); err != nil {
		return err
	}
	// Before there were several, the one prompt lay in the kk home.
	os.Remove(filepath.Join(s.store.dir, "coordinator.md"))
	base := s.coordinatorText()
	for _, k := range ks {
		if err := os.WriteFile(s.konduktorPrompt(k.ID), []byte(base+konduktorsSection(k, ks)), 0o600); err != nil {
			return err
		}
	}
	return nil
}

// ensureKonduktors starts the main konduktor and resumes every other open one that does not run.
func (s *Server) ensureKonduktors() error {
	// Before the rebrand the konduktor was titled Coordinator, then Conductor.
	s.store.update(coordinatorID, func(c *Conversation) bool {
		if c.Title != "Coordinator" && c.Title != "Conductor" {
			return false
		}
		c.Title = "konduktor"
		return true
	})
	if _, ok := s.store.get(coordinatorID); !ok {
		c := Conversation{ID: coordinatorID, Role: coordinatorID, Harness: "claude", Dir: s.store.dir, Title: "konduktor", Created: time.Now()}
		if err := s.store.put(c); err != nil {
			return err
		}
	}
	if err := s.writePrompts(); err != nil {
		return err
	}
	var errs []error
	for _, k := range s.konduktors() {
		if !s.alive(k) {
			errs = append(errs, s.launchKonduktor(k, ""))
		}
	}
	return errors.Join(errs...)
}

// launchKonduktor starts a konduktor, in its previous session when there is one. A prompt is its first message.
func (s *Server) launchKonduktor(k Conversation, prompt string) error {
	k.Model = s.cfg().CoordinatorModel
	resume := s.canResume(k)
	if !resume {
		k.Session, k.Transcript, k.Prompt = "", "", prompt
	}
	log.Printf("starting konduktor %s (resume: %v)", k.ID, resume)
	if resume {
		return s.launch(&k, true, prompt)
	}
	return s.launch(&k, false, "")
}

// konduktorFor returns the konduktor of dir, the main one for no directory. It starts or reopens the konduktor when it does
// not run, with prompt as its first message, and tells the others when one starts.
func (s *Server) konduktorFor(dir, prompt string) (Conversation, error) {
	// Two first messages to one directory start one konduktor.
	s.starting.Lock()
	defer s.starting.Unlock()
	if !s.cfg().coordinator() {
		return Conversation{}, fail(409, "konduktors are switched off in Settings")
	}
	k, found := s.store.get(coordinatorID)
	if dir != "" {
		found = false
		for _, c := range s.store.list() {
			if c.Role == coordinatorID && c.Scope == dir {
				k, found = c, true
			}
		}
	}
	if found && !k.Closed && s.alive(k) {
		if prompt != "" {
			if err := pasteText(k.Window, prompt); err != nil {
				return k, err
			}
			s.markSeen(k.ID)
		}
		return k, nil
	}
	if !found {
		if dir == "" {
			return k, fail(409, "the main konduktor has not started yet")
		}
		k = Conversation{ID: newID(), Role: coordinatorID, Harness: "claude", Dir: s.store.dir, Scope: dir, Title: filepath.Base(dir), Created: time.Now()}
	}
	opened := k.Closed || !found
	k.Closed, k.ClosedAt = false, time.Time{}
	if err := s.store.put(k); err != nil {
		return k, err
	}
	if err := s.writePrompts(); err != nil {
		return k, err
	}
	if err := s.launchKonduktor(k, prompt); err != nil {
		return k, err
	}
	if opened {
		s.tellKonduktors(k, fmt.Sprintf("konduktor %s %q now runs for %s.", k.ID, konduktorName(k), k.Scope))
	}
	k, _ = s.store.get(k.ID)
	return k, nil
}

// tellKonduktors queues a line about konduktor k for every other open konduktor, sent with the next notification.
func (s *Server) tellKonduktors(k Conversation, line string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, o := range s.konduktors() {
		if o.ID != k.ID {
			// A konduktor started before there were several has its rules only in the file.
			s.roster[o.ID] = append(s.roster[o.ID], fmt.Sprintf("- %s kk ls lists every konduktor; the section Konduktors of %s says how konduktors work together.", line, s.konduktorPrompt(o.ID)))
		}
	}
}

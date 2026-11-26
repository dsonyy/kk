# AGENTS.md

kk is a web conductor for coding agents. The server starts Claude Code, Codex and Cursor CLI in windows of the tmux session `kk`, reads their hooks and transcripts, and shows the conversations in a single-page app. The conductor, a long-running Claude Code agent, runs in the window `coordinator`. Internal names keep the word coordinator so a running conductor and `~/.kk` carry over.

Design and plan: `~/brain/projects/kk/`.

## Always

- English in code, UI text and commits.
- Reproduce a bug with a failing end-to-end test before fixing it.
- `just test` before committing. Tests need `tmux` and `curl`; the UI tests in `app/ui_test.go` need Chrome and are skipped without it.

## Never

- Never edit `web/dist/` or `web/node_modules/`.
- Never write to agent configuration (`~/.claude.json`, `~/.claude/settings.json`, `~/.codex/config.toml`). Hooks are passed per run with `--settings` and `-c`.
- Never store kk state outside `~/.kk`. State files are YAML; the only JSON file is `~/.kk/hooks/claude.json`, because Claude Code requires it.
- Never log message contents, prompts, titles, group names, mail or Slack texts, hook payloads, tokens or keys. Log events and metadata with `log.Printf`, one line each, only when something happens. The log is plain text in `~/.kk/kk.log`, which keeps its newer half past 1 MB.
- Never copy agent messages into `~/.kk`. The agents' transcripts are the source.

## Layout

- `app/` - Go server and CLI in one binary. No arguments: server. Arguments: client over `~/.kk/kk.sock`.
- `app/harness.go` - how each agent is started and resumed. `app/transcript.go` - transcript parsers into one event model.
- `app/usage.go` - token usage and cost per harness, with list price tables taken from the vendors' pricing pages. Cursor CLI reports no usage. Counts and cost per transcript file go into the conversation's YAML; the Stats tab reads them through `/api/activity`.
- `app/rewind.go` - how each harness goes back to an earlier message. It drives the harness's own rewind in the terminal and checks the screen at each step.
- `app/testdata/fakeagent` - fake `claude`, `codex` and `cursor-agent` for the end-to-end tests. Keep it in step with the real formats; `testdata/*.jsonl` are trimmed real transcripts.
- `web/` - Vite, TypeScript and Preact, built into `web/dist` and embedded.

## UI conventions

- Rectangles, black and greys, no rounded corners. Colors only as CSS variables in `web/src/app.css`.
- No accent stripes (thick left border, one-sided inset shadow) to mark or highlight. Use a 1px frame, a background or a dot.
- Buttons are dark, never filled white. The main action gets `.primary`. A box that asks for a decision uses `.box-warn`.
- `--font-ui` for interface text, `--font-mono` only for paths, code, commands and ids.
- Buttons and field labels use `Button` and `Field` from `web/src/mnemonic.tsx`, which assign keyboard mnemonics. Do not hard-code mnemonics.
- Sentence case. The brand name konduktor stays lowercase; in the logo every k is bold and the other letters are not.

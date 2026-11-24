# AGENTS.md

kk is a web conductor for coding agents. The server starts Claude Code, Codex and Cursor CLI in windows of the tmux session `kk`, reads their hooks and transcripts, and shows the conversations in a single-page app. The conductor, a long-running Claude Code agent, runs in the window `coordinator`. Internal names keep the word coordinator so a running conductor and `~/.kk` carry over.

Design and plan: `~/brain/projects/kk/`.

## Always

- English in code, UI text and commits.
- Reproduce a bug with a failing end-to-end test before fixing it.
- `just test` before committing. Tests need `tmux` and `curl`.

## Never

- Never edit `web/dist/` or `web/node_modules/`.
- Never write to agent configuration (`~/.claude.json`, `~/.claude/settings.json`, `~/.codex/config.toml`). Hooks are passed per run with `--settings` and `-c`.
- Never store kk state outside `~/.kk`. State files are YAML; the only JSON file is `~/.kk/hooks/claude.json`, because Claude Code requires it.
- Never copy agent messages into `~/.kk`. The agents' transcripts are the source.

## Layout

- `app/` - Go server and CLI in one binary. No arguments: server. Arguments: client over `~/.kk/kk.sock`.
- `app/harness.go` - how each agent is started and resumed. `app/transcript.go` - transcript parsers into one event model.
- `app/rewind.go` - how each harness goes back to an earlier message. It drives the harness's own rewind in the terminal and checks the screen at each step.
- `app/testdata/fakeagent` - fake `claude`, `codex` and `cursor-agent` for the end-to-end tests. Keep it in step with the real formats; `testdata/*.jsonl` are trimmed real transcripts.
- `web/` - Vite, TypeScript and Preact, built into `web/dist` and embedded.

## UI conventions

- Rectangles, black and greys, no rounded corners. Colors only as CSS variables in `web/src/app.css`.
- Buttons are dark, never filled white. The main action gets `.primary`. A box that asks for a decision uses `.box-warn`.
- `--font-ui` for interface text, `--font-mono` only for paths, code, commands and ids.
- Buttons and field labels use `Button` and `Field` from `web/src/mnemonic.tsx`, which assign keyboard mnemonics. Do not hard-code mnemonics.
- Sentence case.

# kk

Web conductor for coding agents on tmux: Claude Code, Codex and Cursor CLI side by side, with a coordinator agent that knows all of them.

## Install

Needs Go, Node.js, tmux, curl and [just](https://just.systems), plus the agent CLIs you want to use.

```sh
just install    # builds, installs ~/.local/bin/kk and starts the kk user service
kk url          # web address with the access token
tmux attach -t kk
```

Without the service: `just build && ./kk`.

## Use

- New session: prompt, program, model and directory. Each session gets its own window in the tmux session `kk`.
- A blinking tile needs you: a permission prompt, a trust dialog or a question. Answer in the reply box or with the screen keys.
- Each session has controls in its header: Interrupt stops the current turn, Kill ends the agent process and keeps the session for Resume, Reset starts the conversation over with a clean context. A control shows only when the session's program and state allow it.
- The coordinator on the right is a Claude Code session that can list, start and steer the other agents through the `kk` CLI (`kk help`).
- State lives in `~/.kk`.

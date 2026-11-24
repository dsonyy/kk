# kk

Web conductor for coding agents on tmux: Claude Code, Codex and Cursor CLI side by side, with a main agent, the conductor, that knows all of them.

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
- Every session that stops waits either for the conductor or for you. It waits for you when you started it or wrote to it last, or when the conductor escalated it with a reason; otherwise the conductor handles it. "Your turn" has a filled square, "Conductor's turn" a hollow one.
- A blinking tile needs you: a permission prompt, a trust dialog or a question. Answer in the reply box or with the screen keys. The same prompt on a session the conductor handles has an orange border and does not blink.
- Each session has controls in its header: Interrupt stops the current turn, Kill ends the agent process and keeps the session for Resume, Reset starts the conversation over with a clean context. A control shows only when the session's program and state allow it.
- The conductor on the right is a Claude Code session that can list, start and steer the other agents through the `kk` CLI (`kk help`), and hand one to you with `kk escalate`.
- State lives in `~/.kk`.

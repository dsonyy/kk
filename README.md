# kk

konduktor, kk for short, runs coding agents on tmux and shows them on the web: Claude Code, Codex and Cursor CLI side by side, with a main agent, konduktor, that knows all of them. The two k's of konduktor give kk its name.

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
- Every session that stops waits either for konduktor or for you. It waits for you when you started it or acted on it last, or when konduktor escalated it with a reason. Otherwise konduktor handles it. "Your turn" has a filled square, "konduktor's turn" a hollow one.
- A blinking tile needs you: a permission prompt, a trust dialog or a question. Answer in the reply box or with the screen keys. The same prompt on a session konduktor handles has an orange border and does not blink.
- Each session has controls in its header: Interrupt stops the current turn, Kill ends the agent process and keeps the session for Resume, Reset starts the conversation over with a clean context. A control shows only when the session's program and state allow it.
- konduktor on the right is a Claude Code session that can list, start and steer the other agents through the `kk` CLI (`kk help`), and hand one to you with `kk escalate`.
- kk tells konduktor when an agent whose turn goes to it needs action (a permission or trust dialog, an API error), exits without being stopped, works for 10 minutes with no visible progress, or finishes its turn. konduktor handles routine cases and escalates the rest to you.
- State lives in `~/.kk`.

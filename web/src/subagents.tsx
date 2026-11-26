import { useEffect, useState } from "preact/hooks";
import { usageLabel, usageTitle, when, type Conversation, type Subagent } from "./api";
import type { Row } from "./chain";

const STATES: Record<Subagent["state"], string> = { running: "Running", done: "Done", failed: "Failed", stopped: "Stopped" };

// The sessions whose finished subagents are unfolded, shared by the sidebar and the Sessions page until the page reloads.
// Closing a session folds them again: it takes its subagents with it.
const unfolded = new Set<string>();
const listeners = new Set<() => void>();
const key = (c: Conversation) => (c.closed ? `${c.id} closed` : c.id);

export function useUnfolded(): [(c: Conversation) => boolean, (c: Conversation, on: boolean) => void] {
  const [, render] = useState(0);
  useEffect(() => {
    const listener = () => render((n) => n + 1);
    listeners.add(listener);
    return () => void listeners.delete(listener);
  }, []);
  const set = (c: Conversation, on: boolean) => {
    if (on) unfolded.add(key(c));
    else unfolded.delete(key(c));
    listeners.forEach((l) => l());
  };
  return [(c) => unfolded.has(key(c)), set];
}

// AgentsFold sits on a session's card and folds its finished subagents away or brings them back.
export function AgentsFold({ c, n }: { c: Conversation; n?: number }) {
  const [isOpen, setOpen] = useUnfolded();
  if (!n) return null;
  const on = isOpen(c);
  const toggle = (e: Event) => {
    e.stopPropagation();
    setOpen(c, !on);
  };
  return (
    <div
      class={on ? "agents-fold on" : "agents-fold"}
      role="button"
      tabIndex={0}
      aria-expanded={on}
      title={on ? "Hide the finished subagents" : "Show the finished subagents"}
      onClick={toggle}
      onKeyDown={(e) => (e.key === "Enter" || e.key === " ") && toggle(e)}
    >
      <i class="icon icon-agent" />
      <span>
        {n} finished {n === 1 ? "subagent" : "subagents"}
      </span>
      <i class="icon icon-chevron" />
    </div>
  );
}

// AgentTile is the card of a subagent, smaller than a session's and indented by how deep it was started.
export function AgentTile({ row, on, onOpen }: { row: Row; on: boolean; onOpen: () => void }) {
  const a = row.a!;
  const cls = ["tile", "subagent", a.state, on && "on", row.c.closed && "closed"].filter(Boolean).join(" ");
  return (
    <li class={cls} data-chain={a.id} style={{ marginLeft: `${12 + row.depth * 12}px` }} onClick={onOpen} title={a.title}>
      <div class="tile-top">
        <i class="icon icon-agent" role="img" aria-label="Subagent" />
        <div class="title">{a.title || "Untitled"}</div>
        {a.worktree && <i class="icon icon-worktree" role="img" aria-label="Git worktree" title={`Works in a git worktree: ${a.worktree}${a.branch ? ` on ${a.branch}` : ""}`} />}
      </div>
      <div class="meta">
        <span class="kind" title={[a.type, a.model].filter(Boolean).join(" · ")}>
          {[a.type, a.model?.replace(/^claude-/, "")].filter(Boolean).join(" · ") || "Subagent"}
        </span>
        <span class="state">
          <i class={`dot agent-${a.state}`} />
          {STATES[a.state]}
        </span>
      </div>
      <div class="meta">
        <span title={usageTitle(a.usage, row.c.harness)}>{usageLabel(a.usage)}</span>
        <span>Last {when(a.last)}</span>
      </div>
    </li>
  );
}

export const agentState = (a: Subagent) => STATES[a.state];

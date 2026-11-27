import { useState } from "preact/hooks";
import { act, api, harnessLabel, type Conversation } from "./api";
import { arrange, ChainLinks, ChainRefs } from "./chain";
import { byActivity, byName, compare, groupTime, time, yourTurn, type Sort } from "./filter";
import { placedAt, TitleIcon } from "./schedule";
import { AgentsFold, AgentTile, useUnfolded } from "./subagents";

type Column = { project: string; group: string; sessions: Conversation[] };

function state(c: Conversation): [string, string] {
  if (c.closed) return ["closed", "Closed"];
  if (c.attention) return ["attention", "Action required"];
  if (c.run_done) return ["called", "Scheduled run done"];
  if (c.sleeping) return ["sleeping", "Sleeping"];
  return [c.status, { queued: "Queued", starting: "Starting", working: "Working", background: "Background working", idle: "Idle", exited: "Exited" }[c.status]];
}

// A column follows its first session in the chosen order. By title it follows its group name, the sessions without a group
// last; by activity the columns with work in progress come before the finished ones.
function columns(list: Conversation[], sort: Sort): Column[] {
  const out = new Map<string, Column>();
  for (const c of [...list].sort(compare(sort))) {
    const key = `${c.project}\n${c.group ?? ""}`;
    if (!out.has(key)) out.set(key, { project: c.project, group: c.group ?? "", sessions: [] });
    out.get(key)!.sessions.push(c);
  }
  const cols = [...out.values()];
  if (sort === "active") return cols.sort((a, b) => byActivity(a.sessions, b.sessions));
  if (sort !== "title") return cols;
  return cols.sort((a, b) => Number(!a.group) - Number(!b.group) || byName(a.group, b.group) || byName(a.project, b.project));
}

export function CardTitle({ c }: { c: Conversation }) {
  return (
    <div class="tile-top">
      <TitleIcon c={c} />
      <div class="title">{c.title || "Untitled"}</div>
      {c.in_worktree && <i class="icon icon-worktree" role="img" aria-label="Git worktree" title={`Works in a git worktree: ${c.dir}`} />}
      <Star c={c} />
    </div>
  );
}

// The star shows the click at once; the list catches up when the server publishes the change.
export function Star({ c }: { c: Conversation }) {
  const [want, setWant] = useState<boolean>();
  if (want === !!c.starred) setWant(undefined);
  const on = want ?? !!c.starred;
  const toggle = (e: Event) => {
    e.stopPropagation();
    setWant(!on);
    act(async () => {
      try {
        await api(on ? "DELETE" : "PUT", `/api/conversations/${c.id}/star`);
      } catch (err) {
        setWant(undefined);
        throw err;
      }
    });
  };
  return (
    <button type="button" class={on ? "star on" : "star"} aria-label="Starred" aria-pressed={on} title={on ? "Remove the star" : "Star this session"} onClick={toggle}>
      <i class={on ? "icon icon-star-filled" : "icon icon-star"} />
    </button>
  );
}

export const dayKey = (t: string | number) => new Date(t).toLocaleDateString("en-CA");

export function dayLabel(key: string): string {
  const today = new Date();
  const yesterday = new Date(today.getFullYear(), today.getMonth(), today.getDate() - 1);
  if (key === dayKey(today.getTime())) return "Today";
  if (key === dayKey(yesterday.getTime())) return "Yesterday";
  return new Date(`${key}T00:00`).toLocaleDateString("en-GB", { day: "numeric", month: "short", year: "numeric" });
}

// A column sits once, under the day it was last active or finished when sorting by activity, else the day of its newest session.
// A scheduled session counts by its planned run of today, or by its last run.
function days(list: Conversation[], sort: Sort): [string, Column[]][] {
  const out = new Map<string, Column[]>();
  for (const col of columns(list, sort)) {
    const planned = col.sessions.map(placedAt);
    const rest = col.sessions.filter((_, i) => planned[i] === undefined);
    const base = !rest.length ? -Infinity : sort === "active" ? groupTime(rest) : Math.max(...rest.map((c) => time(c.created)));
    const day = dayKey(Math.max(base, ...planned.filter((t) => t !== undefined)));
    out.set(day, [...(out.get(day) ?? []), col]);
  }
  return [...out.entries()].sort((a, b) => b[0].localeCompare(a[0]));
}

type BoardProps = {
  list: Conversation[];
  // Every session, for the chain neighbours of a shown one that are filtered out.
  all: Conversation[];
  sort: Sort;
  empty: string;
  // Whether each column names its directory, which only All needs.
  projects: boolean;
  // Whether a column is folded, or undefined when it cannot fold.
  folded: (project: string, group: string) => boolean | undefined;
  onFold: (project: string, group: string, on: boolean) => void;
  onOpen: (id: string) => void;
};

export function Board({ list, all, sort, empty, projects, folded, onFold, onOpen }: BoardProps) {
  const [unfolded] = useUnfolded();
  if (list.length === 0) return <div class="board empty">{empty}</div>;
  return (
    <div class="board">
      {days(list, sort).map(([day, cols]) => (
        <section class="day" key={day}>
          <h2 class="day-label">{dayLabel(day)}</h2>
          <div class="day-columns">
            {cols.map((col) => {
              const shut = col.group ? folded(col.project, col.group) : undefined;
              const foldable = shut !== undefined;
              const fold = !!shut;
              const chains = arrange(col.sessions, all, unfolded);
              return (
                <section class={fold ? "column folded" : "column"} key={`${col.project}\n${col.group}`}>
                  <header
                    title={`${col.project}\n${col.group || "Ungrouped"}`}
                    class={foldable ? "foldable" : undefined}
                    aria-expanded={foldable ? !fold : undefined}
                    onClick={foldable ? () => onFold(col.project, col.group, !fold) : undefined}
                  >
                    {projects && <span class="project">{col.project.replace(/\/+$/, "").split("/").pop() || "/"}</span>}
                    <div class="name">
                      {foldable && <i class="icon icon-chevron" />}
                      <h1 class={col.group ? "" : "muted"}>{col.group || "Ungrouped"}</h1>
                      {foldable && <span class="count">{col.sessions.length}</span>}
                    </div>
                  </header>
                  {!fold && (
                    <ChainLinks chains={chains}>
                      <ul class="tiles">
                        {chains.order.map((r) => {
                          if (r.a) return <AgentTile key={r.id} row={r} on={false} onOpen={() => onOpen(r.id)} />;
                          const c = r.c;
                          const [cls, label] = state(c);
                          return (
                            <li key={c.id} class={`tile ${cls}${yourTurn(c) ? " yours" : ""}`} data-chain={c.id} onClick={() => onOpen(c.id)} title={c.attention || undefined}>
                              <CardTitle c={c} />
                              <ChainRefs chains={chains} id={c.id} />
                              <div class="meta">
                                <span>{harnessLabel(c.harness)}</span>
                                <span class="state">
                                  <i class={`dot ${cls} ${c.integration ? "integration" : ""}`} />
                                  {label}
                                </span>
                              </div>
                              {c.attention && <div class="attention-text">{c.attention}</div>}
                              <AgentsFold c={c} hidden={chains.folded.get(c.id)} />
                            </li>
                          );
                        })}
                      </ul>
                    </ChainLinks>
                  )}
                </section>
              );
            })}
          </div>
        </section>
      ))}
    </div>
  );
}

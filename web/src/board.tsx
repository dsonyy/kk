import { Fragment } from "preact";
import { harnessLabel, type Conversation } from "./api";

type Column = { project: string; group: string; sessions: Conversation[] };

function state(c: Conversation): [string, string] {
  if (c.closed) return ["closed", "Closed"];
  if (c.attention) return ["attention", "Action required"];
  return [c.status, { starting: "Starting", working: "Working", idle: "Idle", exited: "Exited" }[c.status]];
}

// Columns keep their place while agents work: projects and groups are ordered by their first session, not by activity.
// The sessions without a group come last in their project.
function columns(list: Conversation[]): Column[] {
  const sorted = [...list].sort((a, b) => a.created.localeCompare(b.created));
  const projects = [...new Set(sorted.map((c) => c.project))];
  const out = new Map<string, Column>();
  for (const c of sorted) {
    const key = `${projects.indexOf(c.project)}\n${c.group ?? ""}`;
    if (!out.has(key)) out.set(key, { project: c.project, group: c.group ?? "", sessions: [] });
    out.get(key)!.sessions.unshift(c);
  }
  return [...out.values()].sort((a, b) => projects.indexOf(a.project) - projects.indexOf(b.project) || Number(!a.group) - Number(!b.group));
}

const dayKey = (iso: string) => new Date(iso).toLocaleDateString("en-CA");

function dayLabel(key: string): string {
  const today = new Date();
  const yesterday = new Date(today.getFullYear(), today.getMonth(), today.getDate() - 1);
  if (key === dayKey(today.toISOString())) return "Today";
  if (key === dayKey(yesterday.toISOString())) return "Yesterday";
  return new Date(`${key}T00:00`).toLocaleDateString("en-GB", { day: "numeric", month: "short", year: "numeric" });
}

// A column sits once, under the day of its newest session.
function days(list: Conversation[]): [string, Column[]][] {
  const out = new Map<string, Column[]>();
  for (const col of columns(list)) {
    const day = dayKey(col.sessions[0].created);
    out.set(day, [...(out.get(day) ?? []), col]);
  }
  return [...out.entries()].sort((a, b) => b[0].localeCompare(a[0]));
}

export function Board({ list, empty, onOpen }: { list: Conversation[]; empty: string; onOpen: (id: string) => void }) {
  if (list.length === 0) return <div class="board empty">{empty}</div>;
  return (
    <div class="board">
      {days(list).map(([day, cols]) => (
        <section class="day" key={day}>
          <h2 class="day-label">{dayLabel(day)}</h2>
          <div class="day-columns">
            {cols.map((col) => (
              <section class="column" key={`${col.project}\n${col.group}`}>
                <header title={`${col.project}\n${col.group || "Ungrouped"}`}>
                  <span class="project">{col.project.replace(/\/+$/, "").split("/").pop() || "/"}</span>
                  <h1 class={col.group ? "" : "muted"}>{col.group || "Ungrouped"}</h1>
                </header>
                <ul class="tiles">
                  {col.sessions.map((c, i) => {
                    const [cls, label] = state(c);
                    return (
                      <Fragment key={c.id}>
                        {i > 0 && <li class="link" aria-hidden="true" />}
                        <li class={`tile ${cls}`} onClick={() => onOpen(c.id)} title={c.attention || undefined}>
                          <div class="title">{c.title || "Untitled"}</div>
                          <div class="meta">
                            <span>{harnessLabel(c.harness)}</span>
                            <span class="state">
                              <i class={`dot ${cls}`} />
                              {label}
                            </span>
                          </div>
                          {c.attention && <div class="attention-text">{c.attention}</div>}
                        </li>
                      </Fragment>
                    );
                  })}
                </ul>
              </section>
            ))}
          </div>
        </section>
      ))}
    </div>
  );
}

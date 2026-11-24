import { useEffect, useState } from "preact/hooks";
import { act, api, harnessLabel, type Conversation } from "./api";

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
    out.get(key)!.sessions.push(c);
  }
  const cols = [...out.values()].sort((a, b) => projects.indexOf(a.project) - projects.indexOf(b.project) || Number(!a.group) - Number(!b.group));
  for (const col of cols) col.sessions.sort((a, b) => Number(!!a.closed) - Number(!!b.closed) || b.created.localeCompare(a.created));
  return cols;
}

// The board shows every session, closed ones too, which the list of the Agents tab leaves out.
export function Board({ stamp, onOpen }: { stamp: unknown; onOpen: (id: string) => void }) {
  const [list, setList] = useState<Conversation[] | null>(null);
  useEffect(() => {
    act(async () => setList(await api<Conversation[]>("GET", "/api/conversations?all=1")));
  }, [stamp]);
  if (!list) return <div class="board" />;
  if (list.length === 0) return <div class="board empty">No sessions yet.</div>;
  return (
    <div class="board">
      {columns(list).map((col) => (
        <section class="column" key={`${col.project}\n${col.group}`}>
          <header title={`${col.project}\n${col.group || "Ungrouped"}`}>
            <span class="project">{col.project.replace(/\/+$/, "").split("/").pop() || "/"}</span>
            <h1 class={col.group ? "" : "muted"}>{col.group || "Ungrouped"}</h1>
          </header>
          <ul class="tiles">
            {col.sessions.map((c) => {
              const [cls, label] = state(c);
              return (
                <li key={c.id} class={`tile ${cls}`} onClick={() => onOpen(c.id)} title={c.attention || undefined}>
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
              );
            })}
          </ul>
        </section>
      ))}
    </div>
  );
}

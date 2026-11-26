import type { ComponentChildren } from "preact";
import { useEffect, useState } from "preact/hooks";
import { act, api, flash, harnessLabel, type Harness, type Settings, type Skill } from "./api";
import { DirField } from "./dirpicker";
import { InboxSettings } from "./inbox";
import { Button, Field, Scope } from "./mnemonic";

type Prompt = "coordinator_prompt" | "worker_prompt";
type View = { settings: Settings; env: Record<string, string>; defaults: Record<Prompt, string>; placeholders: Record<string, string> };

export function SettingsView() {
  const [view, setView] = useState<View>();
  const [draft, setDraft] = useState<Settings>();
  const [harnesses, setHarnesses] = useState<Harness[]>([]);
  const [skills, setSkills] = useState<Record<string, Skill[]>>({});

  const loadSkills = (list: Harness[], dir: string) =>
    list.forEach((h) =>
      act(async () => {
        const found = await api<Skill[]>("GET", `/api/skills?harness=${h.name}&dir=${encodeURIComponent(dir)}`);
        setSkills((s) => ({ ...s, [h.name]: found }));
      }),
    );

  useEffect(() => {
    act(async () => {
      const v = await api<View>("GET", "/api/settings");
      setView(v);
      setDraft(v.settings);
      const list = await api<Harness[]>("GET", "/api/harnesses");
      setHarnesses(list);
      loadSkills(list, v.settings.default_dir);
    });
  }, []);

  if (!view || !draft) return <main class="settings muted">Loading…</main>;

  const set = <K extends keyof Settings>(key: K, value: Settings[K]) => setDraft({ ...draft, [key]: value });
  const noteText = (key: keyof Settings, text: string) => (view.env[key] ? `${text} Set by ${view.env[key]} for this run, so the value here is not used.` : text);
  const note = (key: keyof Settings, text: string) => <span class="hint">{noteText(key, text)}</span>;
  const number = (key: "start_timeout_seconds" | "compact_after_minutes" | "compact_tokens") => (
    <input type="number" min={1} value={draft[key]} onInput={(e) => set(key, Number(e.currentTarget.value))} />
  );
  const prompt = (key: Prompt, label: string, rows: number, hint: string, below?: ComponentChildren) => {
    const edited = draft[key] !== "" && draft[key].trim() !== view.defaults[key].trim();
    return (
      <div class="prompt-setting" data-prompt={key}>
        <Field label={label}>
          <span class="hint">{hint}</span>
          <textarea class="mono" rows={rows} spellcheck={false} value={draft[key] || view.defaults[key]} onInput={(e) => set(key, e.currentTarget.value)} />
        </Field>
        <div class="prompt-state">
          <span class={edited ? "badge on" : "badge"}>{edited ? "Edited" : "Default"}</span>
          <Button label="Reset to default" disabled={!edited} onClick={() => set(key, "")} />
        </div>
        {below}
      </div>
    );
  };
  const dirty = JSON.stringify(draft) !== JSON.stringify(view.settings);
  const save = (e: SubmitEvent) => {
    e.preventDefault();
    act(async () => {
      const v = await api<View>("PUT", "/api/settings", draft);
      setView(v);
      setDraft(v.settings);
      loadSkills(harnesses, v.settings.default_dir);
      flash("Settings saved.");
    });
  };

  return (
    <main class="settings" data-scope="settings">
      <Scope>
        <form onSubmit={save as any}>
          <h1>New sessions</h1>
          <DirField
            label="Default directory"
            hint={noteText("default_dir", "The folder a new session starts in. Empty means the most recently used one.")}
            value={draft.default_dir}
            onValue={(dir) => set("default_dir", dir)}
          />
          <Field label="Start timeout, seconds">
            {note("start_timeout_seconds", "How long an agent may take to start before kk flags it as waiting for input.")}
            {number("start_timeout_seconds")}
          </Field>

          <h1>Skills</h1>
          <Field label="Extra skill directories">
            {note("skill_dirs", "One folder per line. kk suggests the skills in them for every program, next to the ones each program loads itself.")}
            <textarea class="mono" rows={3} value={draft.skill_dirs.join("\n")} onInput={(e) => set("skill_dirs", e.currentTarget.value.split("\n"))} />
          </Field>
          <table class="skills">
            {harnesses.map((h) => {
              const list = skills[h.name];
              const count = (scope: string) => list?.filter((s) => s.scope === scope).length ?? 0;
              return (
                <tr key={h.name}>
                  <td>{harnessLabel(h.name)}</td>
                  <td>{list ? `${list.length} skills` : "…"}</td>
                  <td class="muted">{list && `${count("user")} user, ${count("project")} in the default directory, ${count("extra")} extra`}</td>
                </tr>
              );
            })}
          </table>

          <h1>konduktor</h1>
          <label class="check">
            <input type="checkbox" checked={draft.coordinator} onChange={(e) => set("coordinator", e.currentTarget.checked)} />
            Run konduktor
          </label>
          {note("coordinator", "Turning it on starts it now. Turning it off applies the next time kk starts.")}
          <Field label="Model">
            {note("coordinator_model", "Used the next time konduktor starts.")}
            <select value={draft.coordinator_model} onChange={(e) => set("coordinator_model", e.currentTarget.value)}>
              {(harnesses.find((h) => h.name === "claude")?.models ?? [draft.coordinator_model]).map((m) => (
                <option value={m} key={m}>
                  {m || "Default"}
                </option>
              ))}
            </select>
          </Field>
          <Field label="Compact after idle, minutes">
            {note("compact_after_minutes", "kk compacts konduktor's conversation after this much idle time.")}
            {number("compact_after_minutes")}
          </Field>
          <Field label="Compact above, tokens">
            {note("compact_tokens", "Only when the conversation is at least this large.")}
            {number("compact_tokens")}
          </Field>
          <DirField label="Source directory" hint={noteText("source", "Where the kk source is. konduktor is told to change kk there.")} value={draft.source} onValue={(dir) => set("source", dir)} />

          <h1>Prompts</h1>
          {prompt(
            "coordinator_prompt",
            "konduktor prompt",
            24,
            "The system prompt of konduktor. A new konduktor gets it at once, a running one after its next restart; saving does not restart it. Empty means the default.",
            <div class="placeholders">
              <span class="hint">kk fills in these placeholders:</span>
              <dl>
                {Object.keys(view.placeholders)
                  .sort()
                  .flatMap((name) => [
                    <dt key={name} class="mono">{name}</dt>,
                    <dd key={name + "="} class="mono">{view.placeholders[name]}</dd>,
                  ])}
              </dl>
            </div>,
          )}
          {prompt(
            "worker_prompt",
            "Worker prompt",
            8,
            "Given to every session kk starts for work, besides its own prompt: as a system prompt in Claude Code and Codex, before the first prompt in Cursor. New sessions get it at once, running Claude Code and Codex sessions after their next restart, running Cursor sessions not at all. The inbox checks never get it. At most 8000 bytes.",
          )}

          <h1>Inbox</h1>
          <InboxSettings value={draft.inbox} onValue={(v) => set("inbox", v)} />

          <h1>Server</h1>
          <Field label="Address">
            {note("addr", "Host and port of the web app. Applies the next time kk starts.")}
            <input class="mono" value={draft.addr} onInput={(e) => set("addr", e.currentTarget.value)} />
          </Field>

          <div class="row">
            <Button label="Revert" disabled={!dirty} onClick={() => setDraft(view.settings)} />
            <Button label="Save" class="primary" type="submit" disabled={!dirty} />
          </div>
        </form>
      </Scope>
    </main>
  );
}

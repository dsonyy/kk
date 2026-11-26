import { useState } from "preact/hooks";
import { act, api, type Conversation } from "./api";
import { Button, Check, Field, Scope } from "./mnemonic";

const DAYS = ["Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"];
// The week starts on Monday.
const WEEK = [1, 2, 3, 4, 5, 6, 0];

export function ScheduleIcon({ c }: { c: Conversation }) {
  if (!c.schedule) return null;
  return <i class="icon icon-clock schedule-icon" role="img" aria-label="Scheduled" title={`Scheduled: ${scheduleText(c)}`} />;
}

export function scheduleText(c: Conversation): string {
  const s = c.schedule;
  if (!s) return "";
  const days = s.days.length === 7 ? "Every day" : s.days.join() === "1,2,3,4,5" ? "Weekdays" : WEEK.filter((d) => s.days.includes(d)).map((d) => DAYS[d]).join(", ");
  return `${days} ${s.times.map((t) => t.slice(0, 5)).join(", ")}`;
}

// Where the Sessions tab places a scheduled session: at today's planned run when there is one, else at its last run.
export function placedAt(c: Conversation): number | undefined {
  const s = c.schedule;
  if (!s) return undefined;
  const now = new Date();
  if (s.days.includes(now.getDay()) && s.times.length) {
    const [h, m] = s.times[0].split(":").map(Number);
    return new Date(now.getFullYear(), now.getMonth(), now.getDate(), h, m).getTime();
  }
  return c.last_run ? Date.parse(c.last_run) : undefined;
}

export function ScheduleDialog({ c, onClose }: { c: Conversation; onClose: () => void }) {
  const [days, setDays] = useState<number[]>(c.schedule?.days ?? [1, 2, 3, 4, 5]);
  const [times, setTimes] = useState<string[]>(c.schedule?.times.map((t) => t.slice(0, 5)) ?? ["09:00"]);
  const [prompt, setPrompt] = useState(c.schedule?.prompt ?? "");
  const filled = times.filter(Boolean);
  const save = (e?: SubmitEvent) => {
    e?.preventDefault();
    act(async () => {
      await api("PUT", `/api/conversations/${c.id}/schedule`, { days, times: filled, prompt });
      onClose();
    });
  };
  const remove = () =>
    act(async () => {
      await api("DELETE", `/api/conversations/${c.id}/schedule`);
      onClose();
    });
  const setTime = (i: number, value: string) => setTimes(times.map((t, j) => (j === i ? value : t)));
  return (
    <div class="overlay" data-scope="schedule" onPointerDown={(e) => e.target === e.currentTarget && onClose()}>
      <Scope>
        <form class="new-session schedule" onSubmit={save as any} onKeyDown={(e) => e.key === "Escape" && onClose()}>
          <h1>Schedule</h1>
          <p class="hint">
            kk resumes the session at these local times and sends the prompt. A working agent is not interrupted; the run waits until its turn ends. A run missed while kk or
            the laptop was off is made up once on the same day.
          </p>
          <div class="field">
            <span>Days</span>
            <div class="schedule-days">
              {WEEK.map((d) => (
                <Check key={d} label={DAYS[d]} checked={days.includes(d)} onChange={(e) => setDays(e.currentTarget.checked ? [...days, d] : days.filter((x) => x !== d))} />
              ))}
            </div>
          </div>
          <div class="field">
            <span>Times</span>
            <div class="schedule-times">
              {times.map((t, i) => (
                <span class="schedule-time" key={i}>
                  <input type="time" value={t} aria-label={`Time ${i + 1}`} onInput={(e) => setTime(i, e.currentTarget.value)} />
                  {times.length > 1 && (
                    <button type="button" class="small" aria-label="Remove time" title="Remove this time" onClick={() => setTimes(times.filter((_, j) => j !== i))}>
                      <i class="icon icon-close" />
                    </button>
                  )}
                </span>
              ))}
              <Button label="Add time" icon="plus" class="small" onClick={() => setTimes([...times, ""])} />
            </div>
          </div>
          <Field label="Prompt">
            <textarea value={prompt} placeholder="What the agent does on each run" onInput={(e) => setPrompt(e.currentTarget.value)} />
          </Field>
          <div class="row">
            {c.schedule && <Button label="Remove schedule" icon="close" onClick={remove} />}
            <Button label="Cancel" onClick={onClose} />
            <Button label="Save" icon="clock" class="primary" type="submit" disabled={!days.length || !filled.length || !prompt.trim()} />
          </div>
        </form>
      </Scope>
    </div>
  );
}

import type { ComponentChildren } from "preact";
import { useEffect, useRef, useState } from "preact/hooks";
import { api, count, dollars, harnessLabel, newTokens, usageTokens, type Usage } from "./api";
import { Button } from "./mnemonic";

// A row is a conversation with what happened in all of its transcripts. Usage is unknown, not zero, when metered is false.
type Row = Usage & {
  id: string;
  role?: string;
  harness: string;
  project: string;
  metered: boolean;
  created: string;
  last_message: string;
  closed: boolean;
  days?: Record<string, Usage>;
  models?: Record<string, Usage>;
  turns: number;
  tools?: Record<string, number>;
};

type Metric = "cost" | "tokens";
// cls picks the series color, the same for a harness in every chart.
type Series = { key: string; label: string; cls: string };
type Column = { label: string; title: string; parts: Record<string, number> };

const HARNESSES = ["claude", "codex", "cursor"];
const DAYS = 30;
const ESTIMATE = "Cost at API list prices. With a subscription it is an estimate, not what you pay.";

const day = (d: Date) => `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, "0")}-${String(d.getDate()).padStart(2, "0")}`;
const months = ["Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"];
const dayLabel = (key: string) => `${+key.slice(8)} ${months[+key.slice(5, 7) - 1]}`;
const value = (u: Usage, m: Metric) => (m === "cost" ? u.cost : usageTokens(u));
const fmt = (m: Metric) => (m === "cost" ? dollars : (v: number) => count(Math.round(v)));
const sum = (xs: number[]) => xs.reduce((a, b) => a + b, 0);

function duration(ms: number) {
  const m = Math.round(ms / 60000);
  if (m < 1) return "< 1 min";
  if (m < 60) return `${m} min`;
  const h = ms / 3600000;
  return h < 48 ? `${h.toFixed(h < 10 ? 1 : 0)} h` : `${Math.round(h / 24)} days`;
}

function median(xs: number[]) {
  const s = [...xs].sort((a, b) => a - b);
  const n = s.length;
  return n % 2 ? s[(n - 1) / 2] : (s[n / 2 - 1] + s[n / 2]) / 2;
}

// niceMax rounds the top of an axis up to 1, 2, 2.5 or 5 times a power of ten.
function niceMax(v: number) {
  if (v <= 0) return 1;
  const p = 10 ** Math.floor(Math.log10(v));
  return [1, 2, 2.5, 5, 10].map((f) => f * p).find((x) => x >= v)!;
}

function Swatch({ cls }: { cls: string }) {
  return <i class={`swatch ${cls}`} />;
}

function Legend({ series }: { series: Series[] }) {
  if (series.length < 2) return null;
  return (
    <div class="chart-legend">
      {series.map((s) => (
        <span key={s.key}>
          <Swatch cls={s.cls} />
          {s.label}
        </span>
      ))}
    </div>
  );
}

// Hover shows the values of the bar under the pointer, or a hint while none is.
function Hover({ text }: { text: string }) {
  return <div class={text ? "chart-hover" : "chart-hover muted"}>{text || "Point at a bar for its values"}</div>;
}

function breakdown(series: Series[], parts: Record<string, number>, f: (v: number) => string) {
  const shown = series.filter((s) => parts[s.key]);
  const total = sum(shown.map((s) => parts[s.key]));
  const each = shown.map((s) => `${s.label} ${f(parts[s.key])}`);
  return shown.length > 1 ? `${each.join(" · ")} · total ${f(total)}` : each[0] ?? f(0);
}

// Columns draws stacked vertical bars with a value axis.
function Columns({ columns, series, format }: { columns: Column[]; series: Series[]; format: (v: number) => string }) {
  const [hover, setHover] = useState("");
  const top = niceMax(Math.max(0, ...columns.map((c) => sum(Object.values(c.parts)))));
  const plot = useRef<HTMLDivElement>(null);
  const [width, setWidth] = useState(400);
  useEffect(() => {
    const seen = new ResizeObserver(([e]) => setWidth(e.contentRect.width));
    seen.observe(plot.current!);
    return () => seen.disconnect();
  }, []);
  // A label takes about 60 pixels.
  const every = Math.ceil(columns.length / Math.max(2, Math.floor(width / 60)));
  return (
    <div class="chart">
      <Hover text={hover} />
      <div class="chart-columns" onPointerLeave={() => setHover("")}>
        <div class="chart-axis">
          {[top, top / 2, 0].map((v) => (
            <span key={v}>{format(v)}</span>
          ))}
        </div>
        <div class="chart-plot" ref={plot}>
          <i class="chart-grid" style={{ bottom: "50%" }} />
          {columns.map((c) => {
            const text = `${c.title}: ${breakdown(series, c.parts, format)}`;
            return (
              <div class="chart-column" key={c.title} title={text} onPointerEnter={() => setHover(text)} onClick={() => setHover(text)}>
                {series.map((s) => c.parts[s.key] > 0 && <i key={s.key} class={s.cls} style={{ height: `${(c.parts[s.key] / top) * 100}%` }} />)}
              </div>
            );
          })}
        </div>
        <div />
        <div class="chart-labels">
          {columns.map(
            (c, i) =>
              (columns.length - 1 - i) % every === 0 && (
                <span key={c.title} style={i === columns.length - 1 ? { right: 0 } : { left: `${((i + 0.5) / columns.length) * 100}%`, transform: "translateX(-50%)" }}>
                  {c.label}
                </span>
              ),
          )}
        </div>
      </div>
      <Legend series={series.filter((s) => columns.some((c) => c.parts[s.key] > 0))} />
    </div>
  );
}

type Bar = { label: string; parts: Record<string, number>; note?: string };

// Rows draws stacked horizontal bars, longest first.
function Rows({ bars, series, format, empty }: { bars: Bar[]; series: Series[]; format: (v: number) => string; empty: string }) {
  const [hover, setHover] = useState("");
  if (bars.length === 0) return <p class="muted">{empty}</p>;
  const total = (b: Bar) => sum(Object.values(b.parts));
  const top = Math.max(...bars.map(total)) || 1;
  return (
    <div class="chart">
      <Hover text={hover} />
      <div class="chart-rows" onPointerLeave={() => setHover("")}>
        {bars.map((b) => {
          const text = `${b.label}: ${b.note ?? breakdown(series, b.parts, format)}`;
          return (
            <div class="chart-row" key={b.label} title={text} onPointerEnter={() => setHover(text)} onClick={() => setHover(text)}>
              <span class="chart-row-label">{b.label}</span>
              <span class="chart-track">
                {series.map((s) => b.parts[s.key] > 0 && <i key={s.key} class={s.cls} style={{ width: `${(b.parts[s.key] / top) * 100}%` }} />)}
              </span>
              <span class="chart-row-value">{b.note ?? format(total(b))}</span>
            </div>
          );
        })}
      </div>
      <Legend series={series.filter((s) => bars.some((b) => b.parts[s.key] > 0))} />
    </div>
  );
}

function Section({ title, note, children }: { title: string; note?: string; children: ComponentChildren }) {
  return (
    <section class="stats-card">
      <h2>{title}</h2>
      {children}
      {note && <p class="note">{note}</p>}
    </section>
  );
}

export function Stats() {
  const [rows, setRows] = useState<Row[] | null>(null);
  const [metric, setMetric] = useState<Metric>("cost");
  useEffect(() => {
    let stop = false;
    const load = () => api<Row[]>("GET", "/api/activity").then((r) => !stop && setRows(r), () => {});
    load();
    const t = setInterval(load, 30000);
    return () => {
      stop = true;
      clearInterval(t);
    };
  }, []);
  if (!rows) return <main class="stats" />;
  if (rows.length === 0)
    return (
      <main class="stats">
        <p class="muted">No sessions yet.</p>
      </main>
    );

  const present = HARNESSES.filter((h) => rows.some((r) => r.harness === h));
  const harnesses: Series[] = present.map((h) => ({ key: h, label: harnessLabel(h), cls: `s${HARNESSES.indexOf(h)}` }));
  const metered = rows.filter((r) => r.metered);
  const unmetered = present.filter((h) => !rows.some((r) => r.harness === h && r.metered));
  const f = fmt(metric);
  const byHarness = (pick: (r: Row) => number, list = rows) => Object.fromEntries(present.map((h) => [h, sum(list.filter((r) => r.harness === h).map(pick))]));
  const unpriced = [...new Set(metered.flatMap((r) => r.unpriced ?? []))].sort();
  const noUsage = unmetered.length ? `No data from ${unmetered.map(harnessLabel).join(" and ")}, which does not report token usage.` : "";
  const usageNote = (m: Metric) => [noUsage, m === "cost" && unpriced.length > 0 && `No list price for ${unpriced.join(", ")}, so its tokens add no cost.`].filter(Boolean).join(" ");

  // Totals
  const tiles: { label: string; total: string; parts: Record<string, number>; format: (v: number) => string; only?: string[]; title?: string }[] = [
    { label: "Sessions", total: String(rows.length), parts: byHarness(() => 1), format: String },
    { label: "Turns", total: count(sum(rows.map((r) => r.turns))), parts: byHarness((r) => r.turns), format: count },
    { label: "Tool calls", total: count(sum(rows.map((r) => sum(Object.values(r.tools ?? {}))))), parts: byHarness((r) => sum(Object.values(r.tools ?? {}))), format: count },
    { label: "Tokens", total: count(sum(metered.map(newTokens))), parts: byHarness(newTokens, metered), format: count, only: unmetered, title: "Input, cache writes and output, as on the session cards" },
    { label: "Cache reads", total: count(sum(metered.map((r) => r.cache_read))), parts: byHarness((r) => r.cache_read, metered), format: count, only: unmetered, title: "The context read again on each request" },
    { label: "Cost", total: dollars(sum(metered.map((r) => r.cost))), parts: byHarness((r) => r.cost, metered), format: dollars, only: unmetered, title: ESTIMATE },
  ];

  // Per day over the last DAYS days, local time.
  const keys = Array.from({ length: DAYS }, (_, i) => day(new Date(Date.now() - (DAYS - 1 - i) * 86400000)));
  const usageDays: Column[] = keys.map((k) => ({
    label: dayLabel(k),
    title: dayLabel(k),
    parts: Object.fromEntries(present.map((h) => [h, sum(metered.filter((r) => r.harness === h).map((r) => (r.days?.[k] ? value(r.days[k], metric) : 0)))])),
  }));
  const sessionDays: Column[] = keys.map((k) => ({
    label: dayLabel(k),
    title: dayLabel(k),
    parts: Object.fromEntries(present.map((h) => [h, rows.filter((r) => r.harness === h && day(new Date(r.created)) === k).length])),
  }));

  // By model and by project
  const models = new Map<string, Record<string, number>>();
  const modelNote = new Map<string, string>();
  for (const r of metered)
    for (const [m, u] of Object.entries(r.models ?? {})) {
      const parts = models.get(m) ?? {};
      parts[r.harness] = (parts[r.harness] ?? 0) + value(u, metric);
      models.set(m, parts);
      if (metric === "cost" && u.unpriced?.length) modelNote.set(m, `${count(newTokens(u))} tokens, no list price`);
    }
  const modelBars: Bar[] = [...models].map(([label, parts]) => ({ label, parts, note: modelNote.get(label) })).sort((a, b) => sum(Object.values(b.parts)) - sum(Object.values(a.parts)));
  const projects = new Map<string, Record<string, number>>();
  for (const r of metered) {
    const parts = projects.get(r.project) ?? {};
    parts[r.harness] = (parts[r.harness] ?? 0) + value(r, metric);
    projects.set(r.project, parts);
  }
  const projectBars: Bar[] = [...projects]
    .map(([label, parts]) => ({ label: label.split("/").slice(-2).join("/"), parts }))
    .sort((a, b) => sum(Object.values(b.parts)) - sum(Object.values(a.parts)))
    .slice(0, 10);

  // Token mix per harness
  const kinds: Series[] = [
    { key: "input", label: "Input", cls: "s0" },
    { key: "cache_read", label: "Cache read", cls: "s1" },
    { key: "cache_write", label: "Cache write", cls: "s2" },
    { key: "output", label: "Output", cls: "s3" },
  ];
  const mix: Bar[] = present
    .filter((h) => !unmetered.includes(h))
    .map((h) => {
      const list = metered.filter((r) => r.harness === h);
      return { label: harnessLabel(h), parts: Object.fromEntries(kinds.map((k) => [k.key, sum(list.map((r) => r[k.key as keyof Usage] as number))])) };
    })
    .filter((b) => sum(Object.values(b.parts)) > 0);

  // Session length: from creation to the last message.
  const buckets: [string, number][] = [
    ["< 10 min", 10 * 60000],
    ["10-30 min", 30 * 60000],
    ["30-60 min", 60 * 60000],
    ["1-3 h", 3 * 3600000],
    ["3-12 h", 12 * 3600000],
    ["12-48 h", 48 * 3600000],
    ["> 48 h", Infinity],
  ];
  const span = (r: Row) => new Date(r.last_message).getTime() - new Date(r.created).getTime();
  const timed = rows.filter((r) => new Date(r.last_message).getFullYear() > 2000 && span(r) >= 0);
  const lengths: Column[] = buckets.map(([label, upTo], i) => {
    const from = i ? buckets[i - 1][1] : 0;
    return { label, title: label, parts: Object.fromEntries(present.map((h) => [h, timed.filter((r) => r.harness === h && span(r) >= from && span(r) < upTo).length])) };
  });

  // Per session, by harness
  const perSession = present.map((h) => {
    const list = rows.filter((r) => r.harness === h);
    const priced = metered.filter((r) => r.harness === h && !r.unpriced?.length);
    const length = timed.filter((r) => r.harness === h).map(span);
    const tools = sum(list.map((r) => sum(Object.values(r.tools ?? {}))));
    const turns = sum(list.map((r) => r.turns));
    return {
      h,
      sessions: list.length,
      turns: list.length ? median(list.map((r) => r.turns)).toFixed(0) : "-",
      tools: turns ? (tools / turns).toFixed(1) : "-",
      length: length.length ? duration(median(length)) : "-",
      cost: priced.length ? dollars(sum(priced.map((r) => r.cost)) / priced.length) : "no data",
    };
  });

  // Most used tools
  const tools = new Map<string, Record<string, number>>();
  for (const r of rows)
    for (const [t, n] of Object.entries(r.tools ?? {})) {
      const parts = tools.get(t) ?? {};
      parts[r.harness] = (parts[r.harness] ?? 0) + n;
      tools.set(t, parts);
    }
  const toolBars: Bar[] = [...tools]
    .map(([label, parts]) => ({ label, parts }))
    .sort((a, b) => sum(Object.values(b.parts)) - sum(Object.values(a.parts)))
    .slice(0, 12);

  return (
    <main class="stats">
      <div class="stats-head">
        <p class="muted">
          {rows.length} sessions through kk, {rows.filter((r) => r.closed).length} of them closed. Costs are at API list prices; with a subscription they are an estimate.
        </p>
        <div class="toggle">
          <Button label="Cost" class={metric === "cost" ? "on" : ""} onClick={() => setMetric("cost")} title={ESTIMATE} />
          <Button label="Tokens" class={metric === "tokens" ? "on" : ""} onClick={() => setMetric("tokens")} />
        </div>
      </div>
      <div class="tiles-row">
        {tiles.map((t) => (
          <div class="stat" key={t.label} title={t.title}>
            <span class="stat-label">{t.label}</span>
            <span class="stat-value">{t.total}</span>
            {present.map((h) => (
              <span class="stat-part" key={h}>
                {harnessLabel(h)} <b>{t.only?.includes(h) ? "no data" : t.format(t.parts[h])}</b>
              </span>
            ))}
          </div>
        ))}
      </div>
      <div class="stats-grid">
        <Section title={`${metric === "cost" ? "Cost" : "Tokens and cache reads"} per day, last ${DAYS} days`} note={usageNote(metric)}>
          {metered.length ? <Columns columns={usageDays} series={harnesses.filter((s) => !unmetered.includes(s.key))} format={f} /> : <p class="muted">No harness here reports usage.</p>}
        </Section>
        <Section title={`Sessions started per day, last ${DAYS} days`}>
          <Columns columns={sessionDays} series={harnesses} format={(v) => (Number.isInteger(v) ? String(v) : v.toFixed(1))} />
        </Section>
        <Section title={`${metric === "cost" ? "Cost" : "Tokens and cache reads"} by model`} note={usageNote(metric)}>
          <Rows bars={modelBars} series={harnesses} format={f} empty="No harness here reports usage." />
        </Section>
        <Section title={`${metric === "cost" ? "Cost" : "Tokens and cache reads"} by project, top 10`} note={usageNote(metric)}>
          <Rows bars={projectBars} series={harnesses} format={f} empty="No harness here reports usage." />
        </Section>
        <Section title="Token mix" note={noUsage}>
          <Rows bars={mix} series={kinds} format={count} empty="No harness here reports usage." />
        </Section>
        <Section title="Session length" note="From the start of a session to its last message.">
          <Columns columns={lengths} series={harnesses} format={(v) => (Number.isInteger(v) ? String(v) : v.toFixed(1))} />
        </Section>
        <Section title="Per session" note={["Medians, except tool calls per turn. Average cost counts the sessions whose models all have a list price.", noUsage].filter(Boolean).join(" ")}>
          <table class="stats-table">
            <thead>
              <tr>
                <th />
                <th>Sessions</th>
                <th>Turns</th>
                <th>Tool calls per turn</th>
                <th>Length</th>
                <th>Average cost</th>
              </tr>
            </thead>
            <tbody>
              {perSession.map((p) => (
                <tr key={p.h}>
                  <th>{harnessLabel(p.h)}</th>
                  <td>{p.sessions}</td>
                  <td>{p.turns}</td>
                  <td>{p.tools}</td>
                  <td>{p.length}</td>
                  <td>{p.cost}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </Section>
        <Section title="Most used tools">
          <Rows bars={toolBars} series={harnesses} format={count} empty="No tool calls yet." />
        </Section>
      </div>
    </main>
  );
}

import { useEffect, useState } from "preact/hooks";
import { api } from "./api";

type Sample = { time: string; cpu: number; ram: number; gpu: number | null };

const KEEP = 150;
const SERIES = ["cpu", "ram", "gpu"] as const;

export function Load() {
  const [samples, setSamples] = useState<Sample[]>([]);
  useEffect(() => {
    let stop = false;
    const poll = async () => {
      try {
        const list = await api<Sample[]>("GET", "/api/stats");
        if (!stop) setSamples(list);
      } catch {}
    };
    poll();
    const t = setInterval(poll, 2000);
    return () => {
      stop = true;
      clearInterval(t);
    };
  }, []);

  const last = samples[samples.length - 1];
  const points = (key: (typeof SERIES)[number]) =>
    samples
      .map((s, i) => [KEEP - samples.length + i, s[key]] as const)
      .filter(([, v]) => v !== null)
      .map(([x, v]) => `${x},${(100 - v!).toFixed(1)}`)
      .join(" ");
  return (
    <div class="load">
      <svg viewBox={`0 0 ${KEEP - 1} 100`} preserveAspectRatio="none">
        <line x1="0" x2={KEEP - 1} y1="50" y2="50" class="grid" />
        {SERIES.map((key) => (
          <polyline key={key} class={key} points={points(key)} />
        ))}
      </svg>
      <div class="legend">
        {SERIES.map((key) => (
          <span key={key} class={key}>
            <i />
            {key.toUpperCase()} {last?.[key] == null ? "-" : `${Math.round(last[key]!)}%`}
          </span>
        ))}
      </div>
    </div>
  );
}

import { useEffect, useRef } from "preact/hooks";

type V = [number, number, number];

// A tetrakis hexahedron: each cube face is split into four triangles around its centre, which is pushed out. A triangle on
// face axis a whose base edge lies at axis b takes tone (b - a) mod 3 - 1, so two triangles that share an edge differ.
const FACES: { v: V[]; tone: number }[] = [];
for (let a = 0; a < 3; a++)
  for (const s of [-1, 1])
    for (const k of [1, 2])
      for (const t of [-1, 1]) {
        const at = (pa: number, pb: number, pc: number) => {
          const v: V = [0, 0, 0];
          v[a] = pa;
          v[(a + k) % 3] = pb;
          v[(a + 3 - k) % 3] = pc;
          return v;
        };
        FACES.push({ v: [at(s * 1.7, 0, 0), at(s, t, -1), at(s, t, 1)], tone: k - 1 });
      }

const FPS = 30;
const BOUNCE = 3.4;
const CAMERA = 7;
const LIGHT = norm([-0.5, -0.8, -0.6]);

type Style = { light: V; dark: V; edge: string; shadow: string };

function norm(v: V): V {
  const l = Math.hypot(...v);
  return [v[0] / l, v[1] / l, v[2] / l];
}

function readStyle(canvas: HTMLCanvasElement): Style {
  const css = getComputedStyle(canvas);
  const ctx = canvas.getContext("2d")!;
  const rgb = (name: string): V => {
    ctx.fillStyle = css.getPropertyValue(name);
    const hex = ctx.fillStyle;
    return [1, 3, 5].map((i) => parseInt(hex.slice(i, i + 2), 16)) as V;
  };
  return { light: rgb("--glenz-light"), dark: rgb("--glenz-dark"), edge: css.getPropertyValue("--glenz-edge"), shadow: css.getPropertyValue("--glenz-shadow") };
}

function draw(canvas: HTMLCanvasElement, style: Style, t: number) {
  const ctx = canvas.getContext("2d")!;
  const dpr = window.devicePixelRatio || 1;
  const w = canvas.clientWidth;
  const h = canvas.clientHeight;
  if (canvas.width !== Math.round(w * dpr) || canvas.height !== Math.round(h * dpr)) {
    canvas.width = Math.round(w * dpr);
    canvas.height = Math.round(h * dpr);
  }
  ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
  ctx.clearRect(0, 0, w, h);

  const [ax, ay, az] = [t * 0.21, t * 0.33 + 0.6, t * 0.13 + 0.3];
  const rot = ([x, y, z]: V): V => {
    [y, z] = [y * Math.cos(ax) - z * Math.sin(ax), y * Math.sin(ax) + z * Math.cos(ax)];
    [x, z] = [x * Math.cos(ay) + z * Math.sin(ay), -x * Math.sin(ay) + z * Math.cos(ay)];
    [x, y] = [x * Math.cos(az) - y * Math.sin(az), x * Math.sin(az) + y * Math.cos(az)];
    return [x, y, z];
  };

  // The bounce is a parabola. Each landing squashes the solid, and it wobbles like jelly until the wobble dies out.
  const size = h * 0.16;
  const since = t % BOUNCE;
  const u = since / BOUNCE;
  const lift = (h - size * 4.2) * 4 * u * (1 - u);
  const sy = 1 - 0.22 * Math.exp(-since / 0.32) * Math.cos(since * 13);
  const sx = 1 / Math.sqrt(sy);

  const tris = FACES.map(({ v, tone }) => {
    const r = v.map(rot);
    const e1 = r[1].map((c, i) => c - r[0][i]);
    const e2 = r[2].map((c, i) => c - r[0][i]);
    const n = norm([e1[1] * e2[2] - e1[2] * e2[1], e1[2] * e2[0] - e1[0] * e2[2], e1[0] * e2[1] - e1[1] * e2[0]]);
    const mid = [0, 1, 2].map((i) => (r[0][i] + r[1][i] + r[2][i]) / 3) as V;
    if (n[0] * mid[0] + n[1] * mid[1] + n[2] * mid[2] < 0) n.forEach((_, i) => (n[i] = -n[i]));
    const p = r.map(([x, y, z]) => [(x * CAMERA * size * sx) / (CAMERA + z), (y * CAMERA * size * sy) / (CAMERA + z)]);
    return { p, tone, n, depth: mid[2] };
  });
  const bottom = Math.max(...tris.flatMap((tr) => tr.p.map((p) => p[1])));
  const margin = size * 2.2;
  const cx = w / 2 + (w / 2 - margin) * (0.75 * Math.sin(t * 0.17) + 0.25 * Math.sin(t * 0.41 + 1));
  const cy = h - 1 - bottom - lift;

  const near = 1 - lift / h;
  const rx = size * 1.7 * sx * (0.6 + 0.4 * near);
  const shadow = ctx.createRadialGradient(cx, h, 0, cx, h, rx);
  shadow.addColorStop(0, style.shadow);
  shadow.addColorStop(1, "transparent");
  ctx.save();
  ctx.translate(0, h);
  ctx.scale(1, 0.16);
  ctx.translate(0, -h);
  ctx.fillStyle = shadow;
  ctx.globalAlpha = 0.25 + 0.55 * near;
  ctx.fillRect(cx - rx, h - rx, rx * 2, rx);
  ctx.restore();

  tris.sort((a, b) => b.depth - a.depth);
  ctx.lineJoin = "round";
  ctx.lineWidth = 0.75;
  ctx.strokeStyle = style.edge;
  for (const { p, tone, n } of tris) {
    const front = n[2] < 0;
    const lit = Math.max(0, -(n[0] * LIGHT[0] + n[1] * LIGHT[1] + n[2] * LIGHT[2]));
    // The dark tone takes a little of the light one where it faces the light, so it still reads on a near black background.
    const f = tone === 0 ? 0.6 + 0.4 * lit : 0.1 + 0.25 * lit;
    const [r, g, b] = style.dark.map((c, i) => Math.round(c + (style.light[i] - c) * f));
    ctx.beginPath();
    ctx.moveTo(cx + p[0][0], cy + p[0][1]);
    ctx.lineTo(cx + p[1][0], cy + p[1][1]);
    ctx.lineTo(cx + p[2][0], cy + p[2][1]);
    ctx.closePath();
    ctx.fillStyle = `rgb(${r} ${g} ${b})`;
    ctx.globalAlpha = tone === 0 ? (front ? 0.72 : 0.32) : front ? 0.5 : 0.35;
    ctx.fill();
    ctx.globalAlpha = front ? 0.4 : 0.1;
    ctx.stroke();
  }
  ctx.globalAlpha = 1;
}

// It draws only while the canvas is shown and the page is visible, at most FPS times a second, and draws one still frame
// under prefers-reduced-motion.
export function Glenz() {
  const ref = useRef<HTMLCanvasElement>(null);
  useEffect(() => {
    const canvas = ref.current!;
    const still = window.matchMedia("(prefers-reduced-motion: reduce)").matches;
    let style: Style;
    let frame = 0;
    let last = 0;
    const tick = (now: number) => {
      frame = 0;
      if (document.hidden || !canvas.clientWidth) return;
      if (now - last >= 1000 / FPS - 2) {
        last = now;
        draw(canvas, style, now / 1000);
      }
      frame = requestAnimationFrame(tick);
    };
    const start = () => {
      if (document.hidden || !canvas.clientWidth) return;
      style = readStyle(canvas);
      if (still) draw(canvas, style, 1.3);
      else if (!frame) frame = requestAnimationFrame(tick);
    };
    const resize = new ResizeObserver(start);
    resize.observe(canvas);
    document.addEventListener("visibilitychange", start);
    return () => {
      resize.disconnect();
      document.removeEventListener("visibilitychange", start);
      cancelAnimationFrame(frame);
    };
  }, []);
  return <canvas ref={ref} class="glenz" aria-hidden="true" />;
}

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
const BOUNCE = 6.8;
// The height of a bounce in pixels, the one the solid had in its own 110 px strip.
const LIFT = 36;
const SIZE = 18;
// The still frame rests on the floor at the far right, just above the system load chart.
const STILL = 14 * BOUNCE - 0.01;
const CAMERA = 7;
const LIGHT = norm([-0.5, -0.8, -0.6]);

type Style = { light: V; dark: V; edge: V; shadow: V };
type Tri = { p: number[][]; r: V[]; n: V; tone: number; front: boolean; depth: number };
type Frame = { tris: Tri[]; cx: number; cy: number; shadow: { x: number; rx: number; alpha: number } };

function norm(v: V): V {
  const l = Math.hypot(...v);
  return [v[0] / l, v[1] / l, v[2] / l];
}

function parse(css: string): number[] {
  return (css.match(/[\d.]+/g) ?? []).map(Number);
}

function readStyle(canvas: HTMLCanvasElement): Style {
  const css = getComputedStyle(canvas);
  const probe = document.createElement("canvas").getContext("2d")!;
  const rgb = (name: string): V => {
    probe.fillStyle = css.getPropertyValue(name);
    const hex = probe.fillStyle;
    return [1, 3, 5].map((i) => parseInt(hex.slice(i, i + 2), 16)) as V;
  };
  return { light: rgb("--glenz-light"), dark: rgb("--glenz-dark"), edge: rgb("--glenz-edge"), shadow: rgb("--glenz-shadow") };
}

function frame(t: number, w: number, floor: number): Frame {
  const [ax, ay, az] = [t * 0.105, t * 0.165 + 0.6, t * 0.065 + 0.3];
  const rot = ([x, y, z]: V): V => {
    [y, z] = [y * Math.cos(ax) - z * Math.sin(ax), y * Math.sin(ax) + z * Math.cos(ax)];
    [x, z] = [x * Math.cos(ay) + z * Math.sin(ay), -x * Math.sin(ay) + z * Math.cos(ay)];
    [x, y] = [x * Math.cos(az) - y * Math.sin(az), x * Math.sin(az) + y * Math.cos(az)];
    return [x, y, z];
  };

  // The bounce is a parabola. Each landing squashes the solid, and it wobbles like jelly until the wobble dies out.
  const since = t % BOUNCE;
  const u = since / BOUNCE;
  const lift = LIFT * 4 * u * (1 - u);
  const sy = 1 - 0.22 * Math.exp(-since / 0.32) * Math.cos(since * 13);
  const sx = 1 / Math.sqrt(sy);

  const tris = FACES.map(({ v, tone }) => {
    const r = v.map(rot);
    const e1 = r[1].map((c, i) => c - r[0][i]);
    const e2 = r[2].map((c, i) => c - r[0][i]);
    const n = norm([e1[1] * e2[2] - e1[2] * e2[1], e1[2] * e2[0] - e1[0] * e2[2], e1[0] * e2[1] - e1[1] * e2[0]]);
    const mid = [0, 1, 2].map((i) => (r[0][i] + r[1][i] + r[2][i]) / 3) as V;
    if (n[0] * mid[0] + n[1] * mid[1] + n[2] * mid[2] < 0) n.forEach((_, i) => (n[i] = -n[i]));
    const p = r.map(([x, y, z]) => [(x * CAMERA * SIZE * sx) / (CAMERA + z), (y * CAMERA * SIZE * sy) / (CAMERA + z)]);
    return { p, r, n, tone, front: n[2] < 0, depth: mid[2] };
  });
  tris.sort((a, b) => b.depth - a.depth);
  const bottom = Math.max(...tris.flatMap((tr) => tr.p.map((p) => p[1])));
  const margin = SIZE * 2.2;
  const cx = w / 2 + (w / 2 - margin) * (0.75 * Math.sin(t * 0.085) + 0.25 * Math.sin(t * 0.205 + 1));
  const near = 1 - lift / (3 * LIFT);
  return { tris, cx, cy: floor - 1 - bottom - lift, shadow: { x: cx, rx: SIZE * 1.7 * sx * (0.6 + 0.4 * near), alpha: 0.25 + 0.55 * near } };
}

// The floor is the top of the system load, the element after the canvas.
function size(canvas: HTMLCanvasElement) {
  const dpr = window.devicePixelRatio || 1;
  const [w, h] = [canvas.clientWidth, canvas.clientHeight];
  if (canvas.width !== Math.round(w * dpr) || canvas.height !== Math.round(h * dpr)) {
    canvas.width = Math.round(w * dpr);
    canvas.height = Math.round(h * dpr);
  }
  return { w, h, dpr, floor: (canvas.nextElementSibling as HTMLElement).offsetTop };
}

// The face colour: the dark tone takes a little of the light one where it faces the light, so it still reads on a near
// black background.
function shade(style: Style, tone: number, n: V): V {
  const lit = Math.max(0, -(n[0] * LIGHT[0] + n[1] * LIGHT[1] + n[2] * LIGHT[2]));
  const f = tone === 0 ? 0.6 + 0.4 * lit : 0.1 + 0.25 * lit;
  return style.dark.map((c, i) => c + (style.light[i] - c) * f) as V;
}

const fillAlpha = (tone: number, front: boolean) => (tone === 0 ? (front ? 0.72 : 0.32) : front ? 0.5 : 0.35);
const edgeAlpha = (front: boolean) => (front ? 0.4 : 0.1);

function draw2d(canvas: HTMLCanvasElement, style: Style, t: number) {
  const ctx = canvas.getContext("2d")!;
  const { w, h, dpr, floor } = size(canvas);
  ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
  ctx.clearRect(0, 0, w, h);
  const { tris, cx, cy, shadow } = frame(t, w, floor);

  const gradient = ctx.createRadialGradient(shadow.x, floor, 0, shadow.x, floor, shadow.rx);
  gradient.addColorStop(0, `rgb(${style.shadow.join(" ")})`);
  gradient.addColorStop(1, "transparent");
  ctx.save();
  ctx.translate(0, floor);
  ctx.scale(1, 0.16);
  ctx.translate(0, -floor);
  ctx.fillStyle = gradient;
  ctx.globalAlpha = shadow.alpha;
  ctx.fillRect(shadow.x - shadow.rx, floor - shadow.rx, shadow.rx * 2, shadow.rx);
  ctx.restore();

  ctx.lineJoin = "round";
  ctx.lineWidth = 0.75;
  ctx.strokeStyle = `rgb(${style.edge.join(" ")})`;
  for (const { p, tone, n, front } of tris) {
    ctx.beginPath();
    ctx.moveTo(cx + p[0][0], cy + p[0][1]);
    ctx.lineTo(cx + p[1][0], cy + p[1][1]);
    ctx.lineTo(cx + p[2][0], cy + p[2][1]);
    ctx.closePath();
    ctx.fillStyle = `rgb(${shade(style, tone, n).map(Math.round).join(" ")})`;
    ctx.globalAlpha = fillAlpha(tone, front);
    ctx.fill();
    ctx.globalAlpha = edgeAlpha(front);
    ctx.stroke();
  }
  ctx.globalAlpha = 1;
}

// A map of the page in greys from the positions and colours of its elements: boxes, borders and a bar per line of text. The
// page is nearly black, so the map stretches the dark end of the range to white.
function pageMap(skip: Element): HTMLCanvasElement {
  const [W, H] = [window.innerWidth, window.innerHeight];
  const scale = 0.5;
  const map = document.createElement("canvas");
  map.width = Math.ceil(W * scale);
  map.height = Math.ceil(H * scale);
  const ctx = map.getContext("2d")!;
  ctx.scale(scale, scale);
  ctx.fillStyle = "#000";
  ctx.fillRect(0, 0, W, H);
  const grey = (css: string) => {
    const [r, g, b, a = 1] = parse(css);
    if (!a) return "";
    const l = Math.min(1, Math.max(0, ((0.2126 * r + 0.7152 * g + 0.0722 * b) / 255 - 0.04) / 0.25));
    return `rgb(${Array(3).fill(Math.round(l * a * 255)).join(" ")})`;
  };
  const range = document.createRange();
  for (const el of document.body.querySelectorAll("*")) {
    if (el === skip) continue;
    const box = el.getBoundingClientRect();
    if (!box.width || !box.height || box.bottom < 0 || box.top > H || box.right < 0 || box.left > W) continue;
    const css = getComputedStyle(el);
    if (css.visibility === "hidden") continue;
    const bg = grey(css.backgroundColor);
    if (bg) {
      ctx.fillStyle = bg;
      ctx.fillRect(box.left, box.top, box.width, box.height);
    }
    const border = parseFloat(css.borderTopWidth) ? grey(css.borderTopColor) : "";
    if (border) {
      ctx.strokeStyle = border;
      ctx.lineWidth = 3;
      ctx.strokeRect(box.left, box.top, box.width, box.height);
    }
    const text = grey(css.color);
    for (const node of el.childNodes) {
      if (node.nodeType !== Node.TEXT_NODE || !node.textContent!.trim() || !text) continue;
      range.selectNodeContents(node);
      ctx.fillStyle = text;
      for (const line of range.getClientRects()) {
        const [left, right] = [Math.max(line.left, box.left), Math.min(line.right, box.right)];
        if (right > left) ctx.fillRect(left, line.top + line.height * 0.2, right - left, line.height * 0.6);
      }
    }
  }
  return map;
}

const VERTEX = `#version 300 es
in vec2 pos;
in vec3 rot;
in vec3 normal;
in vec3 bary;
in vec2 kind;
uniform vec2 size;
out vec3 vRot;
flat out vec3 vNormal;
out vec3 vBary;
flat out vec2 vKind;
void main() {
  gl_Position = vec4(pos / size * vec2(2, -2) + vec2(-1, 1), 0, 1);
  vRot = rot;
  vNormal = normal;
  vBary = bary;
  vKind = kind;
}`;

// kind.x is the tone, 2 for the shadow; kind.y is 1 on faces turned to the viewer. The colour is premultiplied.
const FRAGMENT = `#version 300 es
precision highp float;
in vec3 vRot;
flat in vec3 vNormal;
in vec3 vBary;
flat in vec2 vKind;
uniform vec3 light, dark, edge, shadow, lightDir;
uniform sampler2D page;
uniform vec2 anchor, reach, spread;
out vec4 color;
void main() {
  if (vKind.x > 1.5) {
    float a = (1.0 - smoothstep(0.0, 1.0, length(vBary.xy))) * vBary.z;
    color = vec4(shadow * a, a);
    return;
  }
  bool front = vKind.y > 0.5;
  float lit = max(0.0, -dot(vNormal, lightDir));
  vec3 c = mix(dark, light, vKind.x < 0.5 ? 0.6 + 0.4 * lit : 0.1 + 0.25 * lit);
  float a = vKind.x < 0.5 ? (front ? 0.72 : 0.32) : (front ? 0.5 : 0.35);
  color = vec4(c * a, a);
  vec3 near = smoothstep(vec3(0), fwidth(vBary) * 1.5, vBary);
  float e = (1.0 - min(min(near.x, near.y), near.z)) * (front ? 0.4 : 0.1);
  color = vec4(edge * e, e) + color * (1.0 - e);
  if (front) {
    // The page is a plane between the viewer and the solid. The reflected ray is bent far out so each face shows a
    // large part of the page, not only what is next to the sidebar.
    vec3 view = normalize(vRot + vec3(0, 0, ${CAMERA.toFixed(1)}));
    vec3 ray = reflect(view, vNormal);
    vec2 uv = anchor + ray.xy * reach + vRot.xy * spread;
    float fresnel = 0.75 + 0.25 * pow(1.0 - abs(dot(view, vNormal)), 2.0);
    float m = smoothstep(0.12, 0.55, texture(page, uv).r);
    color = mix(color, vec4(1), m * 0.85 * fresnel);
  }
}`;

const STRIDE = 13;

function webgl(canvas: HTMLCanvasElement) {
  // Software WebGL costs far more than the 2D canvas, so only a GPU gets it. A canvas keeps its first context, so the
  // renderer is checked on another one.
  const probe = document.createElement("canvas").getContext("webgl2");
  const info = probe?.getExtension("WEBGL_debug_renderer_info");
  if (!probe || /swiftshader|llvmpipe|software/i.test(info ? probe.getParameter(info.UNMASKED_RENDERER_WEBGL) : "")) return null;
  probe.getExtension("WEBGL_lose_context")?.loseContext();
  const gl = canvas.getContext("webgl2", { antialias: true });
  if (!gl) return null;
  const program = gl.createProgram()!;
  for (const [type, src] of [
    [gl.VERTEX_SHADER, VERTEX],
    [gl.FRAGMENT_SHADER, FRAGMENT],
  ] as const) {
    const shader = gl.createShader(type)!;
    gl.shaderSource(shader, src);
    gl.compileShader(shader);
    gl.attachShader(program, shader);
  }
  gl.linkProgram(program);
  if (!gl.getProgramParameter(program, gl.LINK_STATUS)) return null;
  gl.useProgram(program);
  gl.bindBuffer(gl.ARRAY_BUFFER, gl.createBuffer());
  let offset = 0;
  for (const [name, n] of [["pos", 2], ["rot", 3], ["normal", 3], ["bary", 3], ["kind", 2]] as const) {
    const at = gl.getAttribLocation(program, name);
    gl.enableVertexAttribArray(at);
    gl.vertexAttribPointer(at, n, gl.FLOAT, false, STRIDE * 4, offset * 4);
    offset += n;
  }
  gl.bindTexture(gl.TEXTURE_2D, gl.createTexture());
  for (const p of [gl.TEXTURE_WRAP_S, gl.TEXTURE_WRAP_T]) gl.texParameteri(gl.TEXTURE_2D, p, gl.MIRRORED_REPEAT);
  gl.texParameteri(gl.TEXTURE_2D, gl.TEXTURE_MIN_FILTER, gl.LINEAR_MIPMAP_LINEAR);
  gl.enable(gl.BLEND);
  gl.blendFunc(gl.ONE, gl.ONE_MINUS_SRC_ALPHA);
  const uniform = (name: string) => gl.getUniformLocation(program, name);
  gl.uniform3f(uniform("lightDir"), ...LIGHT);
  gl.uniform2f(uniform("reach"), 1.4, 1.1);
  gl.uniform2f(uniform("spread"), 0.1, 0.13);
  const data = new Float32Array((FACES.length * 3 + 6) * STRIDE);
  const bary: V[] = [[1, 0, 0], [0, 1, 0], [0, 0, 1]];

  return {
    style(style: Style) {
      for (const key of ["light", "dark", "edge", "shadow"] as const) gl.uniform3f(uniform(key), ...(style[key].map((c) => c / 255) as V));
    },
    map(map: HTMLCanvasElement) {
      gl.texImage2D(gl.TEXTURE_2D, 0, gl.RGBA, gl.RGBA, gl.UNSIGNED_BYTE, map);
      gl.generateMipmap(gl.TEXTURE_2D);
    },
    draw(t: number, box: DOMRect) {
      const { w, h, dpr, floor } = size(canvas);
      const { tris, cx, cy, shadow } = frame(t, w, floor);
      let i = 0;
      const put = (...v: number[]) => {
        data.set(v, i);
        i += STRIDE;
      };
      const ry = shadow.rx * 0.16;
      for (const [x, y] of [[-1, -1], [1, -1], [1, 1], [-1, -1], [1, 1], [-1, 1]])
        put(shadow.x + x * shadow.rx, floor + y * ry, 0, 0, 0, 0, 0, 0, x, y, shadow.alpha, 2, 0);
      for (const { p, r, n, tone, front } of tris)
        for (let k = 0; k < 3; k++) put(cx + p[k][0], cy + p[k][1], ...r[k], ...n, ...bary[k], tone, front ? 1 : 0);
      gl.viewport(0, 0, w * dpr, h * dpr);
      gl.uniform2f(uniform("size"), w, h);
      gl.uniform2f(uniform("anchor"), (box.left + cx) / window.innerWidth, (box.top + cy) / window.innerHeight);
      gl.clearColor(0, 0, 0, 0);
      gl.clear(gl.COLOR_BUFFER_BIT);
      gl.bufferData(gl.ARRAY_BUFFER, data, gl.STREAM_DRAW);
      gl.drawArrays(gl.TRIANGLES, 0, i / STRIDE);
    },
  };
}

// It draws only while the canvas is shown and the page is visible, at most FPS times a second, and draws one still frame
// under prefers-reduced-motion. With WebGL the faces also reflect a map of the page, which is drawn again when the canvas
// is shown or resized, when the window is resized and when the page prop changes.
export function Glenz({ page }: { page: string }) {
  const ref = useRef<HTMLCanvasElement>(null);
  const remap = useRef(() => {});
  useEffect(() => {
    const canvas = ref.current!;
    const still = window.matchMedia("(prefers-reduced-motion: reduce)").matches;
    const gl = webgl(canvas);
    let style: Style;
    let box: DOMRect;
    let frame = 0;
    let last = 0;
    const draw = (t: number) => (gl ? gl.draw(t, box) : draw2d(canvas, style, t));
    const tick = (now: number) => {
      frame = 0;
      if (document.hidden || !canvas.clientWidth) return;
      if (now - last >= 1000 / FPS - 2) {
        last = now;
        draw(now / 1000);
      }
      frame = requestAnimationFrame(tick);
    };
    const shown = () => !document.hidden && canvas.clientWidth > 0;
    remap.current = () => {
      if (!gl || !shown()) return;
      gl.map(pageMap(canvas));
      box = canvas.getBoundingClientRect();
      if (still) draw(STILL);
    };
    const start = () => {
      if (!shown()) return;
      style = readStyle(canvas);
      gl?.style(style);
      remap.current();
      if (still) draw(STILL);
      else if (!frame) frame = requestAnimationFrame(tick);
    };
    const resize = new ResizeObserver(start);
    resize.observe(canvas);
    window.addEventListener("resize", remap.current);
    document.addEventListener("visibilitychange", start);
    return () => {
      resize.disconnect();
      window.removeEventListener("resize", remap.current);
      document.removeEventListener("visibilitychange", start);
      cancelAnimationFrame(frame);
    };
  }, []);
  useEffect(() => remap.current(), [page]);
  return <canvas ref={ref} class="glenz" aria-hidden="true" />;
}

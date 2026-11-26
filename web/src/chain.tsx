import type { ComponentChildren, JSX } from "preact";
import { useLayoutEffect, useRef, useState } from "preact/hooks";
import type { Conversation } from "./api";
import { time } from "./filter";

// An edge leads from a session to one that follows it. It skips when the sessions between them are not in the list.
type Edge = { from: string; to: string; skip: boolean };

export type Chains = {
  // The list with every chain kept together, a predecessor above the sessions that follow it.
  order: Conversation[];
  edges: Edge[];
  // The lane of the line that leaves a session, counted outwards from the cards.
  lane: Map<string, number>;
  lanes: number;
  // The chain neighbours of a session that are not in the list, such as filtered out, folded or in another group.
  before: Map<string, Conversation>;
  after: Map<string, Conversation[]>;
};

// arrange keeps the chains of a list together. A chain sits where its first member in the list's order is, its members
// in dependency order below it, the successors of one session oldest first.
export function arrange(list: Conversation[], all: Conversation[]): Chains {
  const byId = new Map(all.map((c) => [c.id, c]));
  const listed = new Set(list.map((c) => c.id));
  const parent = new Map<string, string>();
  const before = new Map<string, Conversation>();
  for (const c of list) {
    let p = c.after;
    for (const seen = new Set<string>(); p && !listed.has(p) && !seen.has(p); p = byId.get(p)?.after) seen.add(p);
    if (p && listed.has(p)) parent.set(c.id, p);
    if (p !== c.after && c.after && byId.has(c.after)) before.set(c.id, byId.get(c.after)!);
  }
  const kids = new Map<string, Conversation[]>();
  const after = new Map<string, Conversation[]>();
  for (const c of all) {
    if (!c.after) continue;
    if (parent.get(c.id) === c.after) kids.set(c.after, [...(kids.get(c.after) ?? []), c]);
    else if (listed.has(c.after) && !listed.has(c.id)) after.set(c.after, [...(after.get(c.after) ?? []), c]);
  }
  for (const c of list) {
    const p = parent.get(c.id);
    if (p && p !== c.after) kids.set(p, [...(kids.get(p) ?? []), c]);
  }
  for (const k of [...kids.values(), ...after.values()]) k.sort((a, b) => time(a.created) - time(b.created));

  const order: Conversation[] = [];
  const edges: Edge[] = [];
  const lane = new Map<string, number>();
  const placed = new Set<string>();
  // A line keeps clear of the lines of the sessions below it, which lie inside its span.
  const walk = (c: Conversation): number => {
    placed.add(c.id);
    order.push(c);
    let outer = -1;
    for (const k of kids.get(c.id) ?? []) {
      edges.push({ from: c.id, to: k.id, skip: k.after !== c.id });
      outer = Math.max(outer, walk(k));
    }
    if (!kids.has(c.id)) return -1;
    lane.set(c.id, outer + 1);
    return outer + 1;
  };
  const listedById = new Map(list.map((c) => [c.id, c]));
  for (const c of list) {
    if (placed.has(c.id)) continue;
    let root = c;
    while (parent.has(root.id)) root = listedById.get(parent.get(root.id)!)!;
    walk(root);
  }
  const lanes = lane.size ? Math.max(...lane.values()) + 1 : 0;
  return { order, edges, lane, lanes, before, after };
}

const PORT = 7;
const STEP = 7;
// Where a line enters a card, level with its title, and where it leaves it, near the bottom.
const IN = 17;
const OUT = 13;

export const gutter = (lanes: number) => (lanes ? PORT + lanes * STEP + 3 : 0);

type Box = { top: number; bottom: number; right: number };

// ChainLinks draws the edges in the gutter on the right of the cards: a line leaves the right side of a predecessor and
// enters the right side of each session that follows it.
export function ChainLinks({ chains, hot, children }: { chains: Chains; hot?: (id: string) => boolean; children: ComponentChildren }) {
  const ref = useRef<HTMLDivElement>(null);
  const [boxes, setBoxes] = useState<Map<string, Box>>(new Map());
  const key = chains.edges.map((e) => `${e.from}>${e.to}`).join();
  useLayoutEffect(() => {
    const el = ref.current;
    if (!el || !chains.edges.length) return;
    const measure = () => {
      const base = el.getBoundingClientRect();
      const next = new Map<string, Box>();
      el.querySelectorAll<HTMLElement>("[data-chain]").forEach((t) => {
        const r = t.getBoundingClientRect();
        if (r.height) next.set(t.dataset.chain!, { top: r.top - base.top, bottom: r.bottom - base.top, right: r.right - base.left });
      });
      setBoxes((cur) => (same(cur, next) ? cur : next));
    };
    measure();
    const observer = new ResizeObserver(measure);
    observer.observe(el);
    el.querySelectorAll("[data-chain]").forEach((t) => observer.observe(t));
    return () => observer.disconnect();
  }, [key, chains.order.length]);

  const paths: JSX.Element[] = [];
  const from = new Map<string, Edge[]>();
  for (const e of chains.edges) from.set(e.from, [...(from.get(e.from) ?? []), e]);
  for (const [id, out] of from) {
    const a = boxes.get(id);
    const ends = out.map((e) => [e, boxes.get(e.to)] as const).filter((x): x is readonly [Edge, Box] => !!x[1]);
    if (!a || !ends.length) continue;
    const x = a.right + PORT + chains.lane.get(id)! * STEP + 0.5;
    const y0 = Math.round(a.bottom - OUT) + 0.5;
    const yEnd = Math.round(ends[ends.length - 1][1].top + IN) + 0.5;
    const on = hot?.(id);
    paths.push(<path key={`t${id}`} class={on ? "trunk on" : "trunk"} d={`M${a.right} ${y0}H${x}V${yEnd}`} />);
    paths.push(<rect key={`p${id}`} class={on ? "port on" : "port"} x={a.right} y={y0 - 1.5} width={3} height={3} />);
    for (const [e, b] of ends) {
      const y = Math.round(b.top + IN) + 0.5;
      const lit = on || hot?.(e.to);
      const cls = [lit && "on", e.skip && "skip"].filter(Boolean).join(" ");
      // A lit successor lights its whole way from the predecessor; only its own leg shows the skip.
      if (lit && !on) paths.push(<path key={`w${e.to}`} class="leg on" d={`M${a.right} ${y0}H${x}V${y}`} />);
      paths.push(<path key={`l${e.to}`} class={`leg ${cls}`} d={`M${x} ${y}H${b.right + 5}`} />);
      paths.push(<path key={`h${e.to}`} class={`head ${cls}`} d={`M${b.right + 5} ${y - 3.5}L${b.right + 1} ${y}L${b.right + 5} ${y + 3.5}Z`} />);
    }
  }
  return (
    <div class="chained" ref={ref} style={{ "--gutter": `${gutter(chains.lanes)}px` }}>
      {children}
      {paths.length > 0 && (
        <svg class="chain-links" aria-hidden="true">
          {paths}
        </svg>
      )}
    </div>
  );
}

function same(a: Map<string, Box>, b: Map<string, Box>) {
  if (a.size !== b.size) return false;
  for (const [k, v] of a) {
    const w = b.get(k);
    if (!w || w.top !== v.top || w.bottom !== v.bottom || w.right !== v.right) return false;
  }
  return true;
}

// ChainRefs names the chain neighbours a card has outside the drawn lines.
export function ChainRefs({ chains, id }: { chains: Chains; id: string }) {
  const before = chains.before.get(id);
  const after = chains.after.get(id) ?? [];
  if (!before && !after.length) return null;
  return (
    <div class="chain-refs">
      {before && (
        <div class="chain-ref" title={`After ${before.title || "Untitled"}`}>
          <i class="icon icon-chain" />
          <span>After</span>
          <bdi>{before.title || "Untitled"}</bdi>
        </div>
      )}
      {after.map((c) => (
        <div class="chain-ref" key={c.id} title={`Then ${c.title || "Untitled"}`}>
          <i class="icon icon-chain" />
          <span>Then</span>
          <bdi>{c.title || "Untitled"}</bdi>
        </div>
      ))}
    </div>
  );
}

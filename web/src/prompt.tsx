import type { ComponentProps, RefObject } from "preact";
import { useEffect, useRef, useState } from "preact/hooks";
import { api, type Skill } from "./api";

const cache = new Map<string, Promise<Skill[]>>();

export function useSkills(harness: string, dir: string): Skill[] {
  const [skills, setSkills] = useState<Skill[]>([]);
  useEffect(() => {
    let stop = false;
    const timer = setTimeout(() => {
      const key = `${harness}\n${dir}`;
      if (!cache.has(key)) cache.set(key, api<Skill[]>("GET", `/api/skills?harness=${encodeURIComponent(harness)}&dir=${encodeURIComponent(dir)}`));
      cache.get(key)!.then(
        (list) => !stop && setSkills(list),
        () => cache.delete(key),
      );
    }, 200);
    return () => {
      stop = true;
      clearTimeout(timer);
    };
  }, [harness, dir]);
  return skills;
}

type Props = Omit<ComponentProps<"textarea">, "value" | "onInput" | "ref"> & {
  value: string;
  onValue: (value: string) => void;
  skills: Skill[];
  up?: boolean;
  onEnter?: (e: KeyboardEvent) => void;
  inputRef?: RefObject<HTMLTextAreaElement | null>;
};

// The word under the caret, when it starts with the character that invokes a skill.
function token(text: string, caret: number): [number, string] | null {
  const start = text.slice(0, caret).search(/(?<=^|\s)[/$][\w:.-]*$/);
  return start < 0 ? null : [start, text.slice(start, caret)];
}

export function PromptInput({ value, onValue, skills, up, onEnter, inputRef, ...rest }: Props) {
  const own = useRef<HTMLTextAreaElement>(null);
  const area = inputRef ?? own;
  const [caret, setCaret] = useState(0);
  const [index, setIndex] = useState(0);
  const [dismissed, setDismissed] = useState("");
  const tok = token(value, caret);
  const query = tok?.[1].slice(1).toLowerCase() ?? "";
  const matches =
    tok && tok[1] !== dismissed
      ? skills
          .filter((s) => s.insert[0] === tok[1][0] && (s.name.toLowerCase().includes(query) || s.description.toLowerCase().includes(query)))
          .sort((a, b) => Number(b.name.toLowerCase().startsWith(query)) - Number(a.name.toLowerCase().startsWith(query)))
          .slice(0, 50)
      : [];
  const active = Math.min(index, matches.length - 1);

  const pick = (s: Skill) => {
    const [start, word] = tok!;
    const next = value.slice(0, start) + s.insert + " " + value.slice(start + word.length);
    onValue(next);
    const at = start + s.insert.length + 1;
    setCaret(at);
    requestAnimationFrame(() => area.current?.setSelectionRange(at, at));
  };

  return (
    <div class="prompt">
      <textarea
        {...rest}
        ref={area}
        value={value}
        onInput={(e) => {
          onValue(e.currentTarget.value);
          setCaret(e.currentTarget.selectionStart);
          setIndex(0);
          setDismissed("");
        }}
        onClick={(e) => setCaret(e.currentTarget.selectionStart)}
        onKeyUp={(e) => setCaret(e.currentTarget.selectionStart)}
        onBlur={() => setDismissed(tok?.[1] ?? "")}
        onKeyDown={(e) => {
          if (matches.length) {
            const move = { ArrowDown: 1, ArrowUp: -1 }[e.key];
            if (move) {
              e.preventDefault();
              setIndex((active + move + matches.length) % matches.length);
              return;
            }
            if ((e.key === "Enter" && !e.ctrlKey && !e.metaKey) || e.key === "Tab") {
              e.preventDefault();
              pick(matches[active]);
              return;
            }
            if (e.key === "Escape") {
              e.preventDefault();
              e.stopPropagation();
              setDismissed(tok![1]);
              return;
            }
          }
          if (e.key === "Enter") onEnter?.(e);
        }}
      />
      {matches.length > 0 && (
        <ul class={up ? "suggest up" : "suggest"}>
          {matches.map((s, i) => (
            <li
              key={s.insert}
              class={i === active ? "on" : ""}
              ref={(el) => {
                if (i === active) el?.scrollIntoView({ block: "nearest" });
              }}
              onMouseDown={(e) => {
                e.preventDefault();
                pick(s);
              }}
            >
              <span class="mono">{s.insert}</span>
              <span class="desc">{s.description}</span>
              <span class="scope">{s.scope}</span>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

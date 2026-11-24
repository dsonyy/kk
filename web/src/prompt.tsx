import type { ComponentProps, RefObject } from "preact";
import { useEffect, useRef, useState } from "preact/hooks";
import { api, flash, upload, type Attachment, type Skill } from "./api";

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

// A file of the message being written. It has no url and no path until its upload ends.
export type Draft = { key: number; name: string; url?: string; path?: string };

let drafts = 0;

export function useAttachments() {
  const [items, setItems] = useState<Draft[]>([]);
  const add = (files: Iterable<File>) => {
    for (const file of files) {
      const key = ++drafts;
      setItems((list) => [...list, { key, name: file.name }]);
      upload(file).then(
        (a) => setItems((list) => list.map((d) => (d.key === key ? { key, ...a } : d))),
        (e) => {
          setItems((list) => list.filter((d) => d.key !== key));
          flash(`${file.name}: ${(e as Error).message}`);
        },
      );
    }
  };
  const pick = () => {
    const input = document.createElement("input");
    input.type = "file";
    input.multiple = true;
    input.onchange = () => add(input.files ?? []);
    input.click();
  };
  return { items, setItems, add, pick, uploading: items.some((d) => !d.path) };
}

// The agent gets each attachment as a line with its path, after the text.
export function withPaths(text: string, files: Attachment[]): string {
  return [text.replace(/\s+$/, ""), files.map((f) => f.path).join("\n")].filter(Boolean).join("\n\n");
}

const IMAGE = /\.(png|jpe?g|gif|webp|svg)$/i;

export function Attachments({ items, onRemove }: { items: { name: string; url?: string }[]; onRemove?: (index: number) => void }) {
  if (!items.length) return null;
  return (
    <ul class="attachments">
      {items.map((a, i) => (
        <li key={i}>
          {!a.url ? (
            <span class="file uploading">
              <i class="spinner" />
              <span class="name">{a.name}</span>
            </span>
          ) : IMAGE.test(a.name) ? (
            <a href={a.url} target="_blank" rel="noreferrer" title={a.name}>
              <img src={a.url} alt={a.name} />
            </a>
          ) : (
            <a class="file" href={a.url} target="_blank" rel="noreferrer" title={a.name}>
              <i class="icon icon-file" />
              <span class="name">{a.name}</span>
            </a>
          )}
          {onRemove && (
            <button type="button" aria-label={`Remove ${a.name}`} title="Remove" onClick={() => onRemove(i)}>
              <i class="icon icon-close" />
            </button>
          )}
        </li>
      ))}
    </ul>
  );
}

type Props = Omit<ComponentProps<"textarea">, "value" | "onInput" | "ref"> & {
  value: string;
  onValue: (value: string) => void;
  skills: Skill[];
  attachments: ReturnType<typeof useAttachments>;
  up?: boolean;
  onEnter?: (e: KeyboardEvent) => void;
  inputRef?: RefObject<HTMLTextAreaElement | null>;
};

// The word under the caret, when it starts with the character that invokes a skill.
function token(text: string, caret: number): [number, string] | null {
  const start = text.slice(0, caret).search(/(?<=^|\s)[/$][\w:.-]*$/);
  return start < 0 ? null : [start, text.slice(start, caret)];
}

export function PromptInput({ value, onValue, skills, attachments, up, onEnter, inputRef, ...rest }: Props) {
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

  // A file can be dropped anywhere on the panel that holds the prompt.
  const add = useRef(attachments.add);
  add.current = attachments.add;
  useEffect(() => {
    const panel = area.current?.closest<HTMLElement>(".conversation");
    if (!panel) return;
    const files = (e: DragEvent) => e.dataTransfer?.types.includes("Files");
    const over = (e: DragEvent) => {
      if (!files(e)) return;
      e.preventDefault();
      panel.classList.add("dropping");
    };
    const leave = (e: DragEvent) => {
      if (!panel.contains(e.relatedTarget as Node | null)) panel.classList.remove("dropping");
    };
    const drop = (e: DragEvent) => {
      if (!files(e)) return;
      e.preventDefault();
      panel.classList.remove("dropping");
      add.current(e.dataTransfer!.files);
      area.current?.focus();
    };
    panel.addEventListener("dragover", over);
    panel.addEventListener("dragleave", leave);
    panel.addEventListener("drop", drop);
    return () => {
      panel.removeEventListener("dragover", over);
      panel.removeEventListener("dragleave", leave);
      panel.removeEventListener("drop", drop);
      panel.classList.remove("dropping");
    };
  }, []);

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
      <Attachments items={attachments.items} onRemove={(i) => attachments.setItems(attachments.items.filter((_, at) => at !== i))} />
      <textarea
        {...rest}
        ref={area}
        value={value}
        onPaste={(e) => {
          const files = e.clipboardData?.files;
          // A copy from a spreadsheet or a document carries its text and a rendered picture of it; the text is what was meant.
          if (!files?.length || e.clipboardData!.types.includes("text/html")) return;
          e.preventDefault();
          attachments.add(files);
        }}
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

import { createContext, type ComponentChildren, type ComponentProps } from "preact";
import { useContext, useRef } from "preact/hooks";

// Keyboard mnemonics: every button and field label underlines its first free meaningful letter within its scope.
// Pressing the letter clicks the button or focuses the field; Alt+letter works while typing.

const ScopeContext = createContext<Set<string>>(new Set());

export function Scope(props: { children: ComponentChildren }) {
  return <ScopeContext.Provider value={new Set()}>{props.children}</ScopeContext.Provider>;
}

function useMnemonic(label: string): [number, string] {
  const used = useContext(ScopeContext);
  const kept = useRef<string | null>(null);
  const letters = [...label.matchAll(/[A-Za-z]/g)].map((m) => m.index!);
  const starts = letters.filter((i) => i === 0 || !/[A-Za-z]/.test(label[i - 1]));
  const order = [...starts, ...letters];
  let index = order.find((i) => label[i].toLowerCase() === kept.current) ?? order.find((i) => !used.has(label[i].toLowerCase())) ?? -1;
  if (index < 0) return [-1, ""];
  const key = label[index].toLowerCase();
  used.add(key);
  kept.current = key;
  return [index, key];
}

function Underlined({ label, index }: { label: string; index: number }) {
  if (index < 0) return <>{label}</>;
  return (
    <span>
      {label.slice(0, index)}
      <u class="mnemonic">{label[index]}</u>
      {label.slice(index + 1)}
    </span>
  );
}

type ButtonProps = Omit<ComponentProps<"button">, "label"> & { label: string; icon?: string };

export function Button({ label, icon, ...rest }: ButtonProps) {
  const [index, key] = useMnemonic(label);
  return (
    <button type="button" {...rest} data-key={key}>
      {icon && <i class={`icon icon-${icon}`} />}
      <Underlined label={label} index={index} />
    </button>
  );
}

export function Field(props: { label: string; children: ComponentChildren }) {
  const [index, key] = useMnemonic(props.label);
  return (
    <label class="field" data-key={key}>
      <Underlined label={props.label} index={index} />
      {props.children}
    </label>
  );
}

document.addEventListener("keydown", (e) => {
  if (e.ctrlKey || e.metaKey || e.key.length !== 1 || !/[a-z]/i.test(e.key)) return;
  const target = e.target as HTMLElement;
  const typing = target.closest?.("input, textarea, select, [contenteditable]");
  if (typing && !e.altKey) return;
  const key = e.key.toLowerCase();
  const root = document.querySelector(".overlay") ?? document;
  const candidates = [...root.querySelectorAll<HTMLElement>(`[data-key="${key}"]`)].filter(
    (el) => el.getClientRects().length > 0 && !(el as HTMLButtonElement).disabled,
  );
  if (!candidates.length) return;
  const scope = document.activeElement?.closest("[data-scope]");
  const el = candidates.find((c) => scope && scope.contains(c)) ?? candidates[0];
  e.preventDefault();
  if (el.matches("label")) el.querySelector<HTMLElement>("input, select, textarea")?.focus();
  else el.click();
});

import type { ComponentChildren } from "preact";
import { useId, useState } from "preact/hooks";
import { act, api } from "./api";
import { Button, Check, Field } from "./mnemonic";

type Listing = { dir: string; parent: string; dirs: string[] };

type Props = { label: string; hint: ComponentChildren; value: string; onValue: (dir: string) => void; recent?: string[] };

// DirField is a path chosen in the folder dialog of the machine kk runs on, picked from the recently used directories, or typed by hand.
// Where kk cannot show that dialog, a browser of the server's folders opens in the page instead.
export function DirField({ label, hint, value, onValue, recent = [] }: Props) {
  const [at, setAt] = useState<Listing>();
  const [choosing, setChoosing] = useState(false);
  const list = useId();
  const [hidden, setHidden] = useState(false);
  const browse = (dir: string) => act(async () => setAt(await api<Listing>("GET", `/api/browse?dir=${encodeURIComponent(dir)}`)));
  const choose = () =>
    act(async () => {
      setChoosing(true);
      try {
        const answer = await api<{ dir: string; native: boolean }>("POST", "/api/pick", { dir: value });
        if (!answer.native) await browse(value);
        else if (answer.dir) onValue(answer.dir);
      } finally {
        setChoosing(false);
      }
    });
  const pick = (dir: string) => {
    onValue(dir);
    setAt(undefined);
  };
  const shown = at?.dirs.filter((name) => hidden || !name.startsWith(".")) ?? [];

  return (
    <div
      class="field"
      onKeyDown={(e) => {
        if (e.key !== "Escape" || !at) return;
        e.stopPropagation();
        setAt(undefined);
      }}
    >
      <Field label={label}>
        <span class="hint">{hint}</span>
        <span class="dir-input">
          <input class="mono" list={list} value={value} onInput={(e) => onValue(e.currentTarget.value)} />
          <Button label="Browse" icon="folder" class={at ? "on" : ""} title="Choose a folder on the machine kk runs on" disabled={choosing} onClick={() => (at ? setAt(undefined) : choose())} />
        </span>
        <datalist id={list}>
          {recent.map((d) => (
            <option value={d} key={d} />
          ))}
        </datalist>
      </Field>
      {/* Hidden instead of unmounted, so its buttons keep their mnemonics when it opens again. */}
      <div class="browser" hidden={!at}>
        <div class="browser-head">
          <Button label="Up" class="small" disabled={!at?.parent} onClick={() => browse(at!.parent)} />
          <span class="mono path" title={at?.dir}>
            <bdi>{at?.dir}</bdi>
          </span>
        </div>
        <ul>
          {shown.map((name) => (
            <li key={name}>
              <button type="button" onClick={() => browse(`${at!.dir === "/" ? "" : at!.dir}/${name}`)}>
                <i class="icon icon-folder" />
                <span class="mono">{name}</span>
              </button>
            </li>
          ))}
          {!shown.length && <li class="empty">No folders here.</li>}
        </ul>
        <div class="browser-foot">
          <Check label="Hidden folders" checked={hidden} onChange={(e) => setHidden(e.currentTarget.checked)} />
          <Button label="Use this folder" class="primary" onClick={() => pick(at!.dir)} />
        </div>
      </div>
      {recent.length > 0 && (
        <div class="badges" aria-label="Recently used directories">
          {recent.slice(0, 5).map((d) => (
            <button type="button" class={d === value ? "badge mono on" : "badge mono"} title={d} key={d} onClick={() => pick(d)}>
              <bdi>{d}</bdi>
            </button>
          ))}
        </div>
      )}
    </div>
  );
}

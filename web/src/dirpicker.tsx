import { useState } from "preact/hooks";
import { act, api } from "./api";
import { Button, Check, Field } from "./mnemonic";

type Listing = { dir: string; parent: string; dirs: string[] };

// DirField is a path typed by hand, picked from the recently used directories, or chosen in a browser of the server's folders.
export function DirField({ value, onValue, recent }: { value: string; onValue: (dir: string) => void; recent: string[] }) {
  const [at, setAt] = useState<Listing>();
  const [hidden, setHidden] = useState(false);
  const browse = (dir: string) => act(async () => setAt(await api<Listing>("GET", `/api/browse?dir=${encodeURIComponent(dir)}`)));
  const pick = (dir: string) => {
    onValue(dir);
    setAt(undefined);
  };
  const shown = at?.dirs.filter((name) => hidden || !name.startsWith(".")) ?? [];

  return (
    <div class="field">
      <Field label="Directory">
        <span class="hint">The folder the agent starts in and works on.</span>
        <span class="dir-input">
          <input class="mono" list="kk-dirs" value={value} onInput={(e) => onValue(e.currentTarget.value)} />
          <Button label="Browse" icon="folder" class={at ? "on" : ""} title="Choose a folder on the machine kk runs on" onClick={() => (at ? setAt(undefined) : browse(value))} />
        </span>
        <datalist id="kk-dirs">
          {recent.map((d) => (
            <option value={d} key={d} />
          ))}
        </datalist>
      </Field>
      {at && (
        <div
          class="browser"
          onKeyDown={(e) => {
            if (e.key !== "Escape") return;
            e.stopPropagation();
            setAt(undefined);
          }}
        >
          <div class="browser-head">
            <Button label="Up" class="small" disabled={!at.parent} onClick={() => browse(at.parent)} />
            <span class="mono path" title={at.dir}>
              <bdi>{at.dir}</bdi>
            </span>
          </div>
          <ul>
            {shown.map((name) => (
              <li key={name}>
                <button type="button" onClick={() => browse(`${at.dir === "/" ? "" : at.dir}/${name}`)}>
                  <i class="icon icon-folder" />
                  <span class="mono">{name}</span>
                </button>
              </li>
            ))}
            {!shown.length && <li class="empty">No folders here.</li>}
          </ul>
          <div class="browser-foot">
            <Check label="Hidden folders" checked={hidden} onChange={(e) => setHidden(e.currentTarget.checked)} />
            <Button label="Use this folder" class="primary" onClick={() => pick(at.dir)} />
          </div>
        </div>
      )}
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

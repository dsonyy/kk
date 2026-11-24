import type { ComponentChildren } from "preact";
import { useEffect, useLayoutEffect, useRef, useState } from "preact/hooks";
import { api, flash, when, type Conversation, type Event, type Harness, type Rewind } from "./api";
import { Button } from "./mnemonic";

let harnesses: Promise<Harness[]> | undefined;

// useRewind tells how the harness of c goes back to an earlier message, while it can: the agent has to be idle.
export function useRewind(c: Conversation): Rewind | undefined {
  const [rewind, setRewind] = useState<Rewind>();
  useEffect(() => {
    harnesses ??= api<Harness[]>("GET", "/api/harnesses");
    harnesses.then(
      (list) => setRewind(list.find((h) => h.name === c.harness)?.rewind),
      () => (harnesses = undefined),
    );
  }, [c.harness]);
  return c.status === "idle" && !c.attention ? rewind : undefined;
}

type Props = { id: string; index: number; event: Event; rewind?: Rewind; children: ComponentChildren };

export function UserMessage({ id, index, event, rewind, children }: Props) {
  const [hot, setHot] = useState(false);
  const [editing, setEditing] = useState<Rewind>();
  if (editing) return <EditMessage id={id} index={index} event={event} rewind={editing} onClose={() => setEditing(undefined)} />;
  return (
    <div class="msg user" title={when(event.time)} data-event={index} onMouseEnter={() => setHot(true)} onMouseLeave={() => setHot(false)}>
      {children}
      {rewind && hot && (
        <Button label="Edit" icon="edit" class="small edit" title="Go back to this message and send it again, changed" onClick={() => setEditing(rewind)} />
      )}
    </div>
  );
}

function EditMessage({ id, index, event, rewind, onClose }: Omit<Props, "children" | "rewind"> & { rewind: Rewind; onClose: () => void }) {
  const [text, setText] = useState(event.text ?? "");
  const [busy, setBusy] = useState(false);
  const area = useRef<HTMLTextAreaElement>(null);
  useLayoutEffect(() => {
    const el = area.current!;
    el.style.height = "auto";
    el.style.height = `${el.scrollHeight + 2}px`;
  }, [text]);
  useEffect(() => area.current!.focus(), []);
  const send = async (files: boolean) => {
    setBusy(true);
    try {
      await api("POST", `/api/conversations/${id}/rewind`, { event: index, text, files });
      onClose();
    } catch (e) {
      flash((e as Error).message);
      setBusy(false);
    }
  };
  return (
    <div class="rewind box-warn">
      <textarea
        ref={area}
        rows={1}
        value={text}
        disabled={busy}
        onInput={(e) => setText(e.currentTarget.value)}
        onKeyDown={(e) => e.key === "Escape" && onClose()}
      />
      <p>
        <i class="icon icon-alert" />
        <b>Send from here.</b> {rewind.conversation}
      </p>
      {rewind.files && (
        <p>
          <b>Restore files and send.</b> {rewind.files}
        </p>
      )}
      <div class="row">
        {busy && <span class="muted">Going back in the agent's terminal</span>}
        <Button label="Cancel" class="small" disabled={busy} onClick={onClose} />
        {rewind.files && <Button label="Restore files and send" class="small" disabled={busy || !text.trim()} onClick={() => send(true)} />}
        <Button label="Send from here" icon="send" class="small primary" disabled={busy || !text.trim()} onClick={() => send(false)} />
      </div>
    </div>
  );
}

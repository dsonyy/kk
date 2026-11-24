import { useEffect, useState } from "preact/hooks";
import { act, api, type Conversation } from "./api";
import { Button } from "./mnemonic";

// The server lists in c.actions which controls apply to the session's harness and state.
export function Controls({ c, reload }: { c: Conversation; reload: () => void }) {
  const [confirm, setConfirm] = useState("");
  useEffect(() => {
    if (!confirm) return;
    const t = setTimeout(() => setConfirm(""), 3000);
    return () => clearTimeout(t);
  }, [confirm]);
  const run = (action: string) => act(async () => (await api("POST", `/api/conversations/${c.id}/${action}`, {}), reload()));
  const confirmed = (action: string) => () => {
    setConfirm(confirm === action ? "" : action);
    if (confirm === action) run(action);
  };
  return (
    <>
      {c.actions.includes("interrupt") && (
        <Button label="Interrupt" icon="interrupt" class="small" title="Stop the current turn, the agent keeps running" onClick={() => run("interrupt")} />
      )}
      {c.actions.includes("reset") && (
        <Button label={confirm === "reset" ? "Confirm reset" : "Reset"} icon="reset" class="small" title="Start the conversation over with a clean context" onClick={confirmed("reset")} />
      )}
      {c.actions.includes("stop") && (
        <Button label={confirm === "stop" ? "Confirm kill" : "Kill"} icon="power" class="small" title="Stop the agent process, the session stays and can be resumed" onClick={confirmed("stop")} />
      )}
    </>
  );
}

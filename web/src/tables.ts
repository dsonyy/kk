// The widths the user dragged the columns of a message's tables to, by message and table, so that they outlast the
// re-renders of the message while the conversation updates.
const widths = new Map<string, number[]>();
const MIN = 48;
// A column whose cells have no more words than this holds dates, names, numbers and paths, which read badly on two lines.
const SHORT = 3;

// fitTables keeps the short columns of a message's tables on one line, gives each header a handle to drag its column's
// width by, and applies the widths dragged before.
export function fitTables(root: HTMLElement, key: string) {
  root.querySelectorAll("table").forEach((table, t) => {
    const rows = [...table.rows];
    const heads = [...(rows[0]?.cells ?? [])];
    heads.forEach((head, i) => {
      if (rows.every((r) => (r.cells[i]?.textContent?.match(/\S+/g)?.length ?? 0) <= SHORT)) rows.forEach((r) => r.cells[i]?.classList.add("short"));
      const handle = document.createElement("span");
      handle.className = "col-resize";
      handle.ariaHidden = "true";
      handle.onpointerdown = (e) => drag(e, handle, table, i, `${key}\0${t}`);
      handle.ondblclick = () => setWidth(table, i, `${key}\0${t}`, undefined);
      head.append(handle);
    });
    widths.get(`${key}\0${t}`)?.forEach((w, i) => w && setWidth(table, i, `${key}\0${t}`, w));
  });
}

function setWidth(table: HTMLTableElement, column: number, id: string, width: number | undefined) {
  const saved = widths.get(id) ?? [];
  saved[column] = width!;
  widths.set(id, saved);
  for (const row of table.rows) {
    const cell = row.cells[column];
    if (!cell) continue;
    cell.classList.toggle("sized", width !== undefined);
    cell.style.width = cell.style.minWidth = cell.style.maxWidth = width === undefined ? "" : `${width}px`;
  }
}

function drag(e: PointerEvent, handle: HTMLElement, table: HTMLTableElement, column: number, id: string) {
  if (e.button !== 0) return;
  e.preventDefault();
  const head = table.rows[0].cells[column];
  const start = head.getBoundingClientRect().width - e.clientX;
  handle.setPointerCapture(e.pointerId);
  handle.classList.add("dragging");
  handle.onpointermove = (m) => setWidth(table, column, id, Math.max(MIN, Math.round(start + m.clientX)));
  handle.onpointerup = handle.onpointercancel = () => {
    handle.onpointermove = handle.onpointerup = handle.onpointercancel = null;
    handle.classList.remove("dragging");
  };
}

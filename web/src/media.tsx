import { act, flash, when, type Attachment, type Event } from "./api";
import { Button, Scope } from "./mnemonic";
import { IMAGE } from "./prompt";

export type Medium = Attachment & { path: string; time: string };

// media lists the pictures of a conversation newest first, each file once with the time of the last message that
// names it: the ones the user attached and the ones a message names.
export function media(events: Event[]): Medium[] {
  const out = new Map<string, Medium>();
  for (const e of [...events].reverse()) {
    const files = [...(e.attachments ?? []).filter((a) => IMAGE.test(a.name)), ...(e.images ?? [])];
    for (const f of files) if (f.path && !out.has(f.path)) out.set(f.path, { ...f, path: f.path, time: e.time });
  }
  return [...out.values()];
}

function clipboard() {
  if (!navigator.clipboard) throw new Error("The browser allows copying only on localhost or over https.");
  return navigator.clipboard;
}

// The clipboard takes PNG only, so other pictures are drawn into a canvas first. The item gets the promise of the
// picture, so the click still counts as the user's when the picture arrives.
async function png(url: string): Promise<Blob> {
  const blob = await (await fetch(url)).blob();
  if (blob.type === "image/png") return blob;
  const img = new Image();
  img.src = url;
  await img.decode();
  const canvas = Object.assign(document.createElement("canvas"), { width: img.naturalWidth, height: img.naturalHeight });
  canvas.getContext("2d")!.drawImage(img, 0, 0);
  return new Promise((resolve, reject) => canvas.toBlob((b) => (b ? resolve(b) : reject(new Error("The image could not be converted."))), "image/png"));
}

const copyImage = (m: Medium) =>
  act(async () => {
    await clipboard().write([new ClipboardItem({ "image/png": png(m.url) })]);
    flash("Image copied.");
  });

const copyPath = (m: Medium) =>
  act(async () => {
    await clipboard().writeText(m.path);
    flash("Path copied.");
  });

// Media shows every picture of a conversation as a thumbnail that opens in the image viewer.
export function Media({ items }: { items: Medium[] }) {
  if (!items.length) return <div class="media empty">No images in this conversation.</div>;
  return (
    <div class="media">
      {items.map((m) => (
        <figure key={m.path} data-scope="media">
          <a class="image" href={m.url} title={m.path} target="_blank" rel="noreferrer">
            <img src={m.url} alt={m.name} loading="lazy" />
          </a>
          <figcaption>
            <span class="name mono" title={m.path}>
              {m.name}
            </span>
            <time dateTime={m.time}>{when(m.time)}</time>
          </figcaption>
          <Scope>
            <div class="row">
              <Button label="Open" class="small" onClick={(e) => e.currentTarget.closest("figure")!.querySelector("a")!.click()} />
              <Button label="Copy image" class="small" onClick={() => copyImage(m)} />
              <Button label="Copy path" class="small" onClick={() => copyPath(m)} />
            </div>
          </Scope>
        </figure>
      ))}
    </div>
  );
}

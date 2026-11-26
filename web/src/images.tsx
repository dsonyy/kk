import { useEffect, useLayoutEffect, useRef, useState } from "preact/hooks";
import type { Attachment } from "./api";
import { Button, Scope } from "./mnemonic";

// A link around every picture lets the keyboard reach it, and a modified click opens it in a new tab.
export function linkImages(root: HTMLElement) {
  for (const img of root.querySelectorAll("img[src]")) {
    if (img.closest("a")) continue;
    const link = document.createElement("a");
    link.href = img.getAttribute("src")!;
    link.target = "_blank";
    link.rel = "noreferrer";
    if (!img.getAttribute("alt")) link.ariaLabel = "Image";
    img.replaceWith(link);
    link.append(img);
  }
}

const IMAGE_PATH = /^\/(?!\/|api\/).*\.(png|jpe?g|gif|webp)$/i;

// showImages shows the local pictures that the server found in a message. A markdown image gets the address that serves
// it. A path in the text, in code or as a link target becomes a link, and the pictures of a block follow the block as
// thumbnails; a code block of nothing but such paths becomes the thumbnails. A path the server did not find stays text.
export function showImages(box: DocumentFragment, images: Attachment[]) {
  const doc = box.ownerDocument;
  const byPath = new Map(images.map((i) => [i.path!, i]));
  const found = (s: string) => byPath.get(s) ?? byPath.get(decode(s));
  const link = (a: HTMLAnchorElement, image: Attachment) => {
    Object.assign(a, { href: image.url, title: image.path, target: "_blank", rel: "noreferrer" });
    a.classList.add("image");
  };

  for (const img of box.querySelectorAll("img")) {
    const src = img.getAttribute("src") ?? "";
    const image = found(src);
    if (image) Object.assign(img, { src: image.url, title: image.path });
    else if (IMAGE_PATH.test(decode(src))) img.replaceWith(Object.assign(doc.createElement("code"), { textContent: decode(src) }));
  }
  for (const a of box.querySelectorAll<HTMLAnchorElement>("a[href]")) {
    const image = found(a.getAttribute("href")!);
    if (image) link(a, image);
  }
  const paths = [...byPath.keys()].sort((a, b) => b.length - a.length).map((p) => p.replace(/[.*+?^${}()|[\]\\]/g, "\\$&"));
  // The same bounds as the server's: no word or scheme before the path, nothing but punctuation after it.
  const named = new RegExp(`(?<![^\\s"'(<[\`*_])(?:${paths.join("|")})(?![^\\s"'<>()[\\]\`]*[^\\s"'<>()[\\]\`.,;:!?*_])`, "g");
  const texts: Text[] = [];
  const walk = doc.createTreeWalker(box, NodeFilter.SHOW_TEXT);
  while (walk.nextNode()) if (!walk.currentNode.parentElement?.closest("a")) texts.push(walk.currentNode as Text);
  for (const text of texts) {
    const parts: Node[] = [];
    let last = 0;
    for (const m of text.data.matchAll(named)) {
      const a = doc.createElement("a");
      a.textContent = m[0];
      link(a, byPath.get(m[0])!);
      parts.push(doc.createTextNode(text.data.slice(last, m.index)), a);
      last = m.index + m[0].length;
    }
    if (!parts.length) continue;
    text.replaceWith(...parts, text.data.slice(last));
  }

  for (const block of [...box.children]) {
    const list = [...new Set([...block.querySelectorAll<HTMLAnchorElement>("a.image:not(:has(img))")].map((a) => a.title))].map((p) => byPath.get(p)!);
    if (!list.length) continue;
    const strip = doc.createElement("div");
    strip.className = "images";
    for (const image of list) {
      const a = doc.createElement("a");
      link(a, image);
      const img = Object.assign(doc.createElement("img"), { src: image.url, alt: image.name, loading: "lazy" });
      a.append(img, Object.assign(doc.createElement("span"), { textContent: image.name }));
      strip.append(a);
    }
    const onlyPaths = block.nodeName === "PRE" && block.textContent!.split("\n").every((l) => !l.trim() || byPath.has(l.trim()));
    if (onlyPaths) block.replaceWith(strip);
    else block.after(strip);
  }
}

function decode(s: string) {
  try {
    return decodeURI(s);
  } catch {
    return s;
  }
}

type Picture = { src: string; alt: string; caption: string };

// pictures lists the pictures of a message once each, in the order they appear.
function pictures(message: Element): Picture[] {
  const out: Picture[] = [];
  for (const img of message.querySelectorAll("img")) {
    const src = img.getAttribute("src")!;
    if (!out.some((p) => p.src === src)) out.push({ src, alt: img.alt, caption: img.closest("a")?.title || img.title });
  }
  return out;
}

// ImageViewer opens a picture of a message large, with its path, and steps through the other pictures of the message.
// The modal dialog keeps the focus inside, closes on Escape and gives the focus back; the class "overlay" keeps the
// keyboard mnemonics inside too.
export function ImageViewer() {
  const [view, setView] = useState<{ list: Picture[]; at: number }>();
  const dialog = useRef<HTMLDialogElement>(null);
  useEffect(() => {
    const open = (e: MouseEvent) => {
      if (e.ctrlKey || e.metaKey || e.shiftKey || e.altKey) return;
      const target = e.target as Element;
      const link = target.closest("a.image");
      // Enter on a link clicks the link, not the picture in it.
      const img = link || (target.matches("a") && !target.textContent?.trim() ? target.querySelector("img") : target.closest("img"));
      if (!img?.closest(".feed :is(.md, .attachments)")) return;
      e.preventDefault();
      const src = link ? link.getAttribute("href")! : img.getAttribute("src")!;
      const list = pictures(img.closest(".msg") ?? img.closest(".md")!);
      setView({ list, at: Math.max(0, list.findIndex((p) => p.src === src)) });
    };
    document.addEventListener("click", open);
    return () => document.removeEventListener("click", open);
  }, []);
  const opened = !!view;
  useLayoutEffect(() => {
    if (opened) dialog.current?.showModal();
  }, [opened]);
  if (!view) return null;
  const { list, at } = view;
  const image = list[at];
  const step = (by: number) => setView({ list, at: (at + by + list.length) % list.length });
  const stay = (e: MouseEvent) => e.stopPropagation();
  return (
    <dialog
      ref={dialog}
      class="overlay viewer"
      aria-label={image.caption || image.alt || "Image"}
      onClose={() => setView(undefined)}
      onClick={() => dialog.current!.close()}
      onKeyDown={(e) => {
        if (list.length > 1 && (e.key === "ArrowLeft" || e.key === "ArrowRight")) step(e.key === "ArrowLeft" ? -1 : 1);
      }}
    >
      <Scope>
        <div class="viewer-bar">
          {list.length > 1 && (
            <span class="viewer-steps" onClick={stay}>
              <Button label="Previous" class="small" onClick={() => step(-1)} />
              <span>
                {at + 1} / {list.length}
              </span>
              <Button label="Next" class="small" onClick={() => step(1)} />
            </span>
          )}
          <Button label="Close" icon="close" autofocus />
        </div>
        <div class="stage">
          <img src={image.src} alt={image.alt} />
        </div>
        {image.caption && (
          <p class="caption mono" onClick={stay}>
            {image.caption}
          </p>
        )}
      </Scope>
    </dialog>
  );
}

import { useEffect, useLayoutEffect, useRef, useState } from "preact/hooks";
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

// ImageViewer opens a picture of a message large. The modal dialog keeps the focus inside, closes on Escape and gives
// the focus back; the class "overlay" keeps the keyboard mnemonics inside too.
export function ImageViewer() {
  const [image, setImage] = useState<{ src: string; alt: string }>();
  const dialog = useRef<HTMLDialogElement>(null);
  useEffect(() => {
    const open = (e: MouseEvent) => {
      if (e.ctrlKey || e.metaKey || e.shiftKey || e.altKey) return;
      const target = e.target as Element;
      // Enter on a link clicks the link, not the picture in it.
      const img = target.matches("a") && !target.textContent?.trim() ? target.querySelector("img") : target.closest("img");
      if (!img?.closest(".feed :is(.md, .attachments)")) return;
      e.preventDefault();
      setImage({ src: img.getAttribute("src")!, alt: img.alt });
    };
    document.addEventListener("click", open);
    return () => document.removeEventListener("click", open);
  }, []);
  useLayoutEffect(() => dialog.current?.showModal(), [image]);
  if (!image) return null;
  return (
    <dialog ref={dialog} class="overlay viewer" aria-label={image.alt || "Image"} onClose={() => setImage(undefined)} onClick={() => dialog.current!.close()}>
      <Scope>
        <Button label="Close" icon="close" />
        <div class="stage">
          <img src={image.src} alt={image.alt} />
        </div>
      </Scope>
    </dialog>
  );
}

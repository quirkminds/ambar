import { useEffect, useRef, useState } from "react";
import { Lightbox } from "../lab/interior-lightbox.tsx";

type ActiveImage = {
  src: string;
  alt: string;
  width?: number;
  height?: number;
};

export function DocsImageLightbox() {
  const [activeImage, setActiveImage] = useState<ActiveImage | null>(null);
  const [open, setOpen] = useState(false);
  const originRef = useRef<HTMLElement | null>(null);

  useEffect(() => {
    const content = document.querySelector<HTMLElement>("[data-docs-content]");

    if (!content) return;

    const cleanups: (() => void)[] = [];

    for (const image of content.querySelectorAll("img")) {
      const parent = image.parentElement;

      if (!parent || parent.closest("a")) continue;

      const originalClass = image.className;
      const trigger = document.createElement("button");
      trigger.type = "button";
      trigger.className = "group inline-flex max-w-full cursor-zoom-in rounded-md align-middle focus-visible:outline-2 focus-visible:outline-solid focus-visible:outline-ring";
      trigger.setAttribute("aria-label", `Open image: ${image.alt || "Documentation image"}`);
      image.classList.add("h-auto", "max-w-full");
      parent.replaceChild(trigger, image);
      trigger.append(image);

      const onClick = () => {
        originRef.current = trigger;
        setActiveImage({
          src: image.currentSrc || image.src,
          alt: image.alt || "Documentation image",
          width: image.naturalWidth || undefined,
          height: image.naturalHeight || undefined,
        });
        setOpen(true);
      };

      trigger.addEventListener("click", onClick);
      cleanups.push(() => {
        trigger.removeEventListener("click", onClick);
        image.className = originalClass;

        if (trigger.parentElement) trigger.replaceWith(image);
      });
    }

    return () => cleanups.forEach((cleanup) => cleanup());
  }, []);

  return (
    <Lightbox
      open={open}
      onClose={() => setOpen(false)}
      originRef={originRef}
      src={activeImage?.src ?? ""}
      alt={activeImage?.alt ?? "Documentation image"}
      caption={activeImage?.alt}
      width={activeImage?.width}
      height={activeImage?.height}
      maxScale={5}
    />
  );
}

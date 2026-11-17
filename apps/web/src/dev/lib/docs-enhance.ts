// Enhances the rendered docs once they load: copy buttons (code blocks and table cells holding one value), a link
// to each section, GitHub alerts (`> [!NOTE]`), tables that scroll on narrow screens, and a sidebar that folds
// away on them. The Markdown stays plain and reads the same on GitHub. Icons: Phosphor, bold.
import { cn } from "@ambar/ui/lib/utils";

const RESET_MS = 1600;

const ALERTS = {
  note: { label: "Note", icon: "text-sky-deep dark:text-sky", border: "border-sky-deep! dark:border-sky!", path: "M108,84a16,16,0,1,1,16,16A16,16,0,0,1,108,84Zm128,44A108,108,0,1,1,128,20,108.12,108.12,0,0,1,236,128Zm-24,0a84,84,0,1,0-84,84A84.09,84.09,0,0,0,212,128Zm-72,36.68V132a20,20,0,0,0-20-20,12,12,0,0,0-4,23.32V168a20,20,0,0,0,20,20,12,12,0,0,0,4-23.32Z" },
  tip: { label: "Tip", icon: "text-leaf", border: "border-leaf!", path: "M180,232a12,12,0,0,1-12,12H88a12,12,0,0,1,0-24h80A12,12,0,0,1,180,232Zm40-128a91.51,91.51,0,0,1-35.17,72.35A12.26,12.26,0,0,0,180,186v2a20,20,0,0,1-20,20H96a20,20,0,0,1-20-20v-2a12,12,0,0,0-4.7-9.51A91.57,91.57,0,0,1,36,104.52C35.73,54.69,76,13.2,125.79,12A92,92,0,0,1,220,104Zm-24,0a68,68,0,0,0-69.65-68C89.56,36.88,59.8,67.55,60,104.38a67.71,67.71,0,0,0,26.1,53.19A35.87,35.87,0,0,1,100,184h56.1A36.13,36.13,0,0,1,170,157.49,67.68,67.68,0,0,0,196,104Zm-20.07-5.32a48.5,48.5,0,0,0-31.91-40,12,12,0,0,0-8,22.62,24.31,24.31,0,0,1,16.09,20,12,12,0,0,0,23.86-2.64Z" },
  important: { label: "Important", icon: "text-foreground", border: "border-foreground!", path: "M84,100A12,12,0,0,1,96,88h64a12,12,0,0,1,0,24H96A12,12,0,0,1,84,100Zm12,52h64a12,12,0,0,0,0-24H96a12,12,0,0,0,0,24ZM236,56V184a20,20,0,0,1-20,20H157.89l-12.52,21.92a20,20,0,0,1-34.74,0L98.11,204H40a20,20,0,0,1-20-20V56A20,20,0,0,1,40,36H216A20,20,0,0,1,236,56Zm-24,4H44V180h61.07a12,12,0,0,1,10.42,6.05L128,207.94l12.51-21.89A12,12,0,0,1,150.93,180H212Z" },
  warning: { label: "Warning", icon: "text-destructive", border: "border-destructive!", path: "M240.26,186.1,152.81,34.23h0a28.74,28.74,0,0,0-49.62,0L15.74,186.1a27.45,27.45,0,0,0,0,27.71A28.31,28.31,0,0,0,40.55,228h174.9a28.31,28.31,0,0,0,24.79-14.19A27.45,27.45,0,0,0,240.26,186.1Zm-20.8,15.7a4.46,4.46,0,0,1-4,2.2H40.55a4.46,4.46,0,0,1-4-2.2,3.56,3.56,0,0,1,0-3.73L124,46.2a4.77,4.77,0,0,1,8,0l87.44,151.87A3.56,3.56,0,0,1,219.46,201.8ZM116,136V104a12,12,0,0,1,24,0v32a12,12,0,0,1-24,0Zm28,40a16,16,0,1,1-16-16A16,16,0,0,1,144,176Z" },
  caution: { label: "Caution", icon: "text-destructive", border: "border-destructive!", path: "M116,132V80a12,12,0,0,1,24,0v52a12,12,0,0,1-24,0ZM236,91.55v72.9a19.86,19.86,0,0,1-5.86,14.14l-51.55,51.55A19.85,19.85,0,0,1,164.45,236H91.55a19.85,19.85,0,0,1-14.14-5.86L25.86,178.59A19.86,19.86,0,0,1,20,164.45V91.55a19.86,19.86,0,0,1,5.86-14.14L77.41,25.86A19.85,19.85,0,0,1,91.55,20h72.9a19.85,19.85,0,0,1,14.14,5.86l51.55,51.55A19.86,19.86,0,0,1,236,91.55Zm-24,1.66L162.79,44H93.21L44,93.21v69.58L93.21,212h69.58L212,162.79ZM128,156a16,16,0,1,0,16,16A16,16,0,0,0,128,156Z" },
} as const;

const ALERT_MARKER = /^\s*\[!(note|tip|important|warning|caution)\][^\S\n]*\n?/i;

const content = document.querySelector<HTMLElement>("[data-docs-content]");

const status = document.querySelector<HTMLElement>("[data-docs-status]");

const timers = new WeakMap<HTMLElement, ReturnType<typeof setTimeout>>();

/** Copies value and shows the outcome on target (data-copied / data-failed) and to screen readers. */
async function copy(value: string, target: HTMLElement, feedback?: HTMLElement) {
  let copied = true;

  try {
    await navigator.clipboard.writeText(value);
  } catch {
    copied = false;
  }

  const message = copied ? "Copied" : "Couldn't copy";
  target.toggleAttribute("data-copied", copied);
  target.toggleAttribute("data-failed", !copied);

  if (feedback) {
    feedback.textContent = message;
    feedback.className = cn("ml-2 text-xs font-medium", copied ? "text-link" : "text-destructive");
  }

  if (status) {
    // Cleared first, so the same message is announced again on the next copy.
    status.textContent = "";
    requestAnimationFrame(() => (status.textContent = message));
  }

  clearTimeout(timers.get(target));
  timers.set(
    target,
    setTimeout(() => {
      target.removeAttribute("data-copied");
      target.removeAttribute("data-failed");

      if (feedback) feedback.textContent = "";
    }, RESET_MS),
  );
}

function svg(path: string, className: string) {
  const ns = "http://www.w3.org/2000/svg";
  const icon = document.createElementNS(ns, "svg");
  icon.setAttribute("viewBox", "0 0 256 256");
  icon.setAttribute("fill", "currentColor");
  icon.setAttribute("aria-hidden", "true");
  icon.setAttribute("class", className);
  const outline = document.createElementNS(ns, "path");
  outline.setAttribute("d", path);
  icon.append(outline);

  return icon;
}

function enhanceCodeBlocks(root: HTMLElement) {
  root.addEventListener("click", (event) => {
    if (!(event.target instanceof Element)) return;

    const button = event.target.closest<HTMLButtonElement>("[data-copy-code]");
    const code = button?.closest("[data-code-block]")?.querySelector("pre code");

    if (button && code) void copy((code.textContent ?? "").replace(/\n$/, ""), button);
  });
}

/** A cell holding just one inline code (an environment variable, an error code, a log event) copies it. */
function enhanceValueCells(root: HTMLElement) {
  for (const cell of root.querySelectorAll("td")) {
    const code = cell.querySelector(":scope > code:only-child");
    const value = code?.textContent ?? "";

    if (!code || value === "" || cell.textContent?.trim() !== value) continue;

    const button = document.createElement("button");
    button.type = "button";
    button.title = "Copy";
    button.className =
      "cursor-copy rounded-md text-left outline-hidden hover:[&_code]:bg-border focus-visible:outline-2 focus-visible:outline-solid focus-visible:outline-offset-2 focus-visible:outline-ring";
    const feedback = document.createElement("span");
    feedback.setAttribute("aria-hidden", "true");
    code.replaceWith(button);
    button.append(code);
    cell.append(feedback);
    button.addEventListener("click", () => void copy(value, button, feedback));
  }
}

function enhanceHeadings(root: HTMLElement) {
  for (const heading of root.querySelectorAll<HTMLHeadingElement>("h2[id], h3[id]")) {
    const link = document.createElement("a");
    link.href = `#${heading.id}`;
    link.textContent = "#";
    link.setAttribute("aria-label", `Link to this section: ${heading.textContent ?? ""}`);
    link.className =
      "ml-2 rounded font-sans font-normal text-muted-foreground! no-underline! opacity-0 transition-opacity group-hover/heading:opacity-100 hover:text-foreground! focus-visible:opacity-100 focus-visible:outline-2 focus-visible:outline-solid focus-visible:outline-ring pointer-coarse:opacity-100 motion-reduce:transition-none";
    heading.classList.add("group/heading");
    heading.append(link);
  }
}

function isAlertKind(kind: string): kind is keyof typeof ALERTS {
  return Object.hasOwn(ALERTS, kind);
}

function enhanceAlerts(root: HTMLElement) {
  for (const quote of root.querySelectorAll("blockquote")) {
    const first = quote.querySelector(":scope > p");
    const marker = first?.firstChild;
    const match = marker?.nodeType === Node.TEXT_NODE ? ALERT_MARKER.exec(marker.textContent ?? "") : null;
    const kind = match?.[1]?.toLowerCase() ?? "";

    if (!first || !marker || !match || !isAlertKind(kind)) continue;

    const alert = ALERTS[kind];
    marker.textContent = (marker.textContent ?? "").slice(match[0].length);

    if (first.firstChild?.nodeName === "BR") first.firstChild.remove();

    if (first.textContent?.trim() === "" && first.children.length === 0) first.remove();

    const title = document.createElement("p");
    title.className = "mt-0! mb-2! flex items-center gap-2 font-semibold text-foreground";
    title.append(svg(alert.path, cn("size-4 shrink-0", alert.icon)), alert.label);
    quote.prepend(title);
    quote.dataset.alert = kind;
    quote.classList.add(alert.border, "text-foreground/85!", "[&>p:last-child]:mb-0", "[&>p]:mt-0");
  }
}

function enhanceTables(root: HTMLElement) {
  for (const table of root.querySelectorAll("table")) {
    const wrapper = document.createElement("div");
    wrapper.className = "my-6 max-w-full overflow-x-auto [&>table]:my-0";
    table.replaceWith(wrapper);
    wrapper.append(table);
  }
}

/** On narrow screens the sidebar starts folded, so the page opens on its content. */
function foldSidebar() {
  const nav = document.querySelector<HTMLDetailsElement>("[data-docs-nav]");

  if (!nav) return;

  const wide = matchMedia("(min-width: 64rem)");
  const sync = () => (nav.open = wide.matches);
  sync();
  wide.addEventListener("change", sync);
}

if (content) {
  enhanceCodeBlocks(content);
  enhanceValueCells(content);
  enhanceHeadings(content);
  enhanceAlerts(content);
  enhanceTables(content);
}

foldSidebar();

// Shiki transformer for Markdown code blocks: wraps each block with a copy button, rendered into the HTML so it
// needs no island per block. docs-enhance.ts copies on click; the look and the feedback follow CopyButton
// (@ambar/ui/components/copy-button). Icons: Phosphor Copy and Check, bold.
import type { AstroUserConfig } from "astro";

import { cn } from "@ambar/ui/lib/utils";
import { PRESS_ACTIVE, TACTILE_BASE, TONE } from "@ambar/ui/lib/tactile";

type ShikiTransformer = NonNullable<NonNullable<NonNullable<AstroUserConfig["markdown"]>["shikiConfig"]>["transformers"]>[number];

type HastRoot = Parameters<NonNullable<ShikiTransformer["root"]>>[0];

type HastElement = Extract<HastRoot["children"][number], { type: "element" }>;

const COPY_PATH =
  "M216,28H88A12,12,0,0,0,76,40V76H40A12,12,0,0,0,28,88V216a12,12,0,0,0,12,12H168a12,12,0,0,0,12-12V180h36a12,12,0,0,0,12-12V40A12,12,0,0,0,216,28ZM156,204H52V100H156Zm48-48H180V88a12,12,0,0,0-12-12H100V52H204Z";

const CHECK_PATH = "M232.49,80.49l-128,128a12,12,0,0,1-17,0l-56-56a12,12,0,1,1,17-17L96,183,215.51,63.51a12,12,0,0,1,17,17Z";

// Each pair of layers shares one grid cell, so the button keeps its size while the feedback swaps in.
const LAYER = "col-start-1 row-start-1 transition-opacity duration-200 motion-reduce:transition-none";

/** hast keeps classes as a list. */
function classes(...values: string[]) {
  return cn(...values).split(" ");
}

function element(tagName: string, properties: HastElement["properties"], children: HastElement["children"] = []): HastElement {
  return { type: "element", tagName, properties, children };
}

function icon(path: string, className: string[]) {
  return element("svg", { xmlns: "http://www.w3.org/2000/svg", viewBox: "0 0 256 256", fill: "currentColor", ariaHidden: "true", className }, [
    element("path", { d: path }),
  ]);
}

function text(value: string, className: string[]) {
  return element("span", { ariaHidden: "true", className }, [{ type: "text", value }]);
}

const copyButton: HastElement = {
  type: "element",
  tagName: "button",
  properties: {
    type: "button",
    dataCopyCode: "",
    ariaLabel: "Copy code",
    className: classes(
      TACTILE_BASE,
      TONE.cloud,
      PRESS_ACTIVE,
      "absolute top-2 right-2 h-8 gap-1.5 rounded-lg px-2.5 text-xs [&_svg]:size-3.5",
      // Out of the way until wanted on a mouse; always there on touch, where there is no hover.
      "opacity-0 group-hover/code:opacity-100 group-focus-within/code:opacity-100 pointer-coarse:opacity-100",
    ),
  },
  children: [
    element("span", { className: ["grid"] }, [
      icon(COPY_PATH, classes(LAYER, "in-data-copied:opacity-0")),
      icon(CHECK_PATH, classes(LAYER, "text-link opacity-0 in-data-copied:opacity-100")),
    ]),
    element("span", { className: ["grid"] }, [
      text("Copy", classes(LAYER, "in-data-copied:opacity-0 in-data-failed:opacity-0")),
      text("Copied", classes(LAYER, "text-link opacity-0 in-data-copied:opacity-100")),
      text("Couldn't copy", classes(LAYER, "text-destructive opacity-0 in-data-failed:opacity-100")),
    ]),
  ],
};

export const docsCodeBlock: ShikiTransformer = {
  name: "ambar:docs-code-block",
  root(root) {
    const index = root.children.findIndex((node) => node.type === "element" && node.tagName === "pre");
    const pre = root.children[index];

    if (pre?.type !== "element") return;

    root.children[index] = element("div", { dataCodeBlock: "", className: ["group/code", "relative"] }, [pre, structuredClone(copyButton)]);
  },
};

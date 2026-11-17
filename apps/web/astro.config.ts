import react from "@astrojs/react";
import tailwindcss from "@tailwindcss/vite";
import type { AstroIntegration } from "astro";
import { defineConfig, envField } from "astro/config";

import { docsCodeBlock } from "./src/dev/lib/docs-code-block.ts";
import { devSections } from "./src/dev/sections.ts";

/** Design system gallery under /dev. Injected only for `astro dev`, never shipped in the build. */
function devGallery(): AstroIntegration {
  return {
    name: "ambar:dev-gallery",
    hooks: {
      "astro:config:setup": ({ command, injectRoute }) => {
        if (command !== "dev") {
          return;
        }

        injectRoute({ pattern: "/dev", entrypoint: "./src/dev/pages/index.astro" });
        injectRoute({ pattern: "/dev/docs", entrypoint: "./src/dev/pages/docs/index.astro" });
        injectRoute({ pattern: "/dev/docs/search.json", entrypoint: "./src/dev/pages/docs/search.json.ts" });
        injectRoute({ pattern: "/dev/docs/[...slug]", entrypoint: "./src/dev/pages/docs/[...slug].astro" });

        for (const section of devSections) {
          injectRoute({
            pattern: `/dev/${section.slug}`,
            entrypoint: `./src/dev/pages/${section.slug}.astro`,
          });
        }
      },
    },
  };
}

export default defineConfig({
  site: "https://ambar.page",
  integrations: [react(), devGallery()],
  // Only the engineering docs (/dev/docs) render Markdown. Both themes are emitted as CSS variables and the docs
  // layout picks one with the .dark class, so code follows the page's theme.
  markdown: {
    shikiConfig: {
      themes: { light: "github-light", dark: "github-dark" },
      defaultColor: false,
      transformers: [docsCodeBlock],
    },
  },
  // Public by nature (bundled into the page): URLs only, never secrets. Validated at dev/build start.
  env: {
    schema: {
      PUBLIC_APP_URL: envField.string({ context: "client", access: "public", url: true }),
      // Unset: the keep flow uses the mock clients (code 123456, no emails).
      PUBLIC_API_URL: envField.string({ context: "client", access: "public", url: true, optional: true }),
    },
  },
  vite: {
    plugins: [tailwindcss()],
  },
  server: { port: 4321 },
});

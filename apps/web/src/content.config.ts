import { defineCollection } from "astro:content";
import { glob } from "astro/loaders";

export const collections = {
  engineeringDocs: defineCollection({
    loader: glob({
      base: new URL("../../../docs", import.meta.url),
      pattern: ["**/*.md", "!local/**"],
      generateId: ({ entry }) => {
        const id = entry.replace(/\.md$/i, "").toLowerCase();

        return id;
      },
    }),
  }),
};

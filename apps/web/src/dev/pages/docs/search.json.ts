import type { APIRoute } from "astro";
import { getCollection } from "astro:content";
import { docGroupLabel, docHref, docTitle, searchText } from "../../lib/docs.ts";

export const prerender = false;

export const GET: APIRoute = async () => {
  const docs = await getCollection("engineeringDocs");

  const entries = docs.map((doc) => ({
    title: docTitle(doc),
    href: docHref(doc),
    group: docGroupLabel(doc.id.includes("/") ? doc.id.split("/")[0] : "project"),
    text: searchText(doc.body ?? ""),
  }));

  return new Response(JSON.stringify(entries), {
    headers: {
      "Content-Type": "application/json; charset=utf-8",
      "Cache-Control": "no-store",
      "X-Content-Type-Options": "nosniff",
    },
  });
};

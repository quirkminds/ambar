import type { CollectionEntry } from "astro:content";

export type EngineeringDoc = CollectionEntry<"engineeringDocs">;

const groupOrder = ["tutorials", "how-to", "reference", "explanation", "decisions", "concepts", "project"];

const groupLabels = new Map([
  ["tutorials", "Tutorials"],
  ["how-to", "How-to guides"],
  ["reference", "Reference"],
  ["explanation", "Explanations"],
  ["decisions", "Decisions"],
  ["concepts", "Concepts"],
  ["project", "Project"],
]);

export function docTitle(doc: EngineeringDoc): string {
  return doc.body?.match(/^#\s+(.+)$/m)?.[1]?.trim() ?? doc.id;
}

export function docHref(doc: EngineeringDoc): string {
  return doc.id === "readme" ? "/dev/docs" : `/dev/docs/${doc.id}`;
}

export function docGroup(doc: EngineeringDoc): string {
  const folder = doc.id.split("/")[0];

  return doc.id.includes("/") ? folder : "project";
}

export function docGroupLabel(group: string): string {
  return groupLabels.get(group) ?? group;
}

export function groupedDocs(docs: EngineeringDoc[]) {
  const groups = new Map<string, EngineeringDoc[]>();

  for (const doc of docs) {
    if (doc.id === "readme") continue;
    const group = docGroup(doc);
    groups.set(group, [...(groups.get(group) ?? []), doc]);
  }

  return [...groups.entries()]
    .sort(([left], [right]) => {
      const leftOrder = groupOrder.indexOf(left);
      const rightOrder = groupOrder.indexOf(right);

      return (leftOrder < 0 ? groupOrder.length : leftOrder) - (rightOrder < 0 ? groupOrder.length : rightOrder);
    })
    .map(([id, entries]) => ({
      id,
      label: docGroupLabel(id),
      entries: entries.sort((left, right) => docTitle(left).localeCompare(docTitle(right))),
    }));
}

/** The docs before and after one, in the order the sidebar lists them (the overview first). */
export function docNeighbours(docs: EngineeringDoc[], activeId: string) {
  const overview = docs.find((doc) => doc.id === "readme");
  const order = [...(overview ? [overview] : []), ...groupedDocs(docs).flatMap((group) => group.entries)];
  const index = order.findIndex((doc) => doc.id === activeId);

  return { previous: index > 0 ? order[index - 1] : undefined, next: index >= 0 ? order[index + 1] : undefined };
}

export function searchText(markdown: string): string {
  return markdown
    .replace(/^---\s*[\s\S]*?\n---\s*/m, "")
    .replace(/```[\s\S]*?```/g, " ")
    .replace(/!\[([^\]]*)\]\([^)]+\)/g, "$1")
    .replace(/\[([^\]]+)\]\(([^)]+)\)/g, "$1 $2")
    .replace(/[`*_>#|~-]/g, " ")
    .replace(/\s+/g, " ")
    .trim();
}

type SearchEntry = { title: string; href: string; group: string; text: string };

const dialog = document.querySelector<HTMLDialogElement>("[data-docs-search-dialog]");

const openButton = document.querySelector<HTMLButtonElement>("[data-docs-search-open]");

const input = document.querySelector<HTMLInputElement>("#docs-search-input");

const results = document.querySelector<HTMLDivElement>("[data-docs-search-results]");

if (dialog && openButton && input && results) {
  let index: SearchEntry[] | undefined;
  let pending: Promise<SearchEntry[]> | undefined;

  const loadIndex = (): Promise<SearchEntry[]> => {
    if (index) return Promise.resolve(index);

    if (!pending) {
      pending = fetch("/dev/docs/search.json", { headers: { Accept: "application/json" } })
        .then(async (response) => {
          if (!response.ok) throw new Error("Search index unavailable");

          // SAFETY: this same-origin endpoint is our fixed, server-generated documentation index.
          return (await response.json()) as SearchEntry[];
        })
        .then((entries) => {
          index = entries.filter((entry) => entry.href.startsWith("/dev/docs"));

          return index;
        })
        .catch(() => {
          pending = undefined;

          return [];
        });
    }

    return pending;
  };

  const addResult = (entry: SearchEntry, query: string) => {
    const link = document.createElement("a");
    link.href = entry.href;
    link.className = "block rounded-xl px-3 py-3 hover:bg-muted focus-visible:outline-2 focus-visible:outline-solid focus-visible:outline-ring";

    const heading = document.createElement("span");
    heading.className = "block font-medium";
    heading.textContent = entry.title;
    const context = document.createElement("span");
    context.className = "mt-0.5 block text-xs text-muted-foreground";
    context.textContent = `${entry.group} · ${snippet(entry.text, query)}`;
    link.append(heading, context);
    results?.append(link);
  };

  const snippet = (text: string, query: string) => {
    const normalized = text.toLocaleLowerCase();
    const position = normalized.indexOf(query.toLocaleLowerCase());

    if (position < 0) return text.slice(0, 150);
    const start = Math.max(0, position - 55);
    const end = Math.min(text.length, position + query.length + 95);

    return `${start ? "…" : ""}${text.slice(start, end)}${end < text.length ? "…" : ""}`;
  };

  const search = async () => {
    const query = input.value.trim();
    results.replaceChildren();

    if (query.length < 2) {
      results.textContent = "Type at least two characters to search.";
      results.className = "max-h-[min(60svh,32rem)] overflow-y-auto p-5 text-sm text-muted-foreground";

      return;
    }

    results.textContent = "Searching…";
    results.className = "max-h-[min(60svh,32rem)] overflow-y-auto p-5 text-sm text-muted-foreground";
    const entries = await loadIndex();

    if (input.value.trim() !== query) return;

    const words = query.toLocaleLowerCase().split(/\s+/).filter(Boolean);

    const matches = entries
      .map((entry) => {
        const searchable = `${entry.title} ${entry.group} ${entry.text}`.toLocaleLowerCase();
        const matched = words.filter((word) => searchable.includes(word)).length;

        return { entry, matched, exactTitle: entry.title.toLocaleLowerCase().includes(query.toLocaleLowerCase()) };
      })
      .filter(({ matched }) => matched === words.length)
      .sort((left, right) => Number(right.exactTitle) - Number(left.exactTitle) || right.matched - left.matched)
      .slice(0, 10);

    results.replaceChildren();
    results.className = "max-h-[min(60svh,32rem)] overflow-y-auto p-2";

    if (matches.length === 0) {
      results.className += " p-5 text-sm text-muted-foreground";
      results.textContent = "No matching documents.";

      return;
    }

    for (const { entry } of matches) addResult(entry, query);
  };

  openButton.addEventListener("click", async () => {
    dialog.showModal();
    input.focus();
    await loadIndex();
    void search();
  });
  input.addEventListener("input", () => void search());
  dialog.addEventListener("click", (event) => {
    if (event.target === dialog) dialog.close();
  });
  document.addEventListener("keydown", (event) => {
    if ((event.metaKey || event.ctrlKey) && event.key.toLowerCase() === "k") {
      event.preventDefault();

      if (!dialog.open) openButton.click();
    }
  });
}

import { useEffect, useRef, useState } from "react";
import { useNavigate } from "react-router-dom";
import { useTranslation } from "react-i18next";
import { Search } from "lucide-react";
import { searchFiles, type SearchHit } from "../api/client";
import { translateApiError } from "../api/errors";
import { cn } from "../lib/cn";

// SearchBar is a header-mounted FTS input that queries the backend's
// /api/search endpoint. Results are rendered in a dropdown anchored to
// the input and collapse when the user clicks outside or picks a
// result. The component debounces its own calls so a user typing
// "report-final" doesn't hammer the backend with seven partial
// queries.
export default function SearchBar() {
  const nav = useNavigate();
  const { t } = useTranslation();
  const [query, setQuery] = useState("");
  const [hits, setHits] = useState<SearchHit[]>([]);
  const [open, setOpen] = useState(false);
  const [loading, setLoading] = useState(false);
  // `searched` flips true once a query's response has resolved so the
  // dropdown can show an explicit "no matches" state. Without it a
  // zero-result query would simply render nothing, leaving the user
  // unsure whether the search ran at all.
  const [searched, setSearched] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const wrapRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    // Close the dropdown when the user clicks outside the wrapper.
    // Important for keyboard-only users too — Escape also closes.
    const onDocClick = (e: MouseEvent) => {
      if (!wrapRef.current?.contains(e.target as Node)) {
        setOpen(false);
      }
    };
    document.addEventListener("mousedown", onDocClick);
    return () => document.removeEventListener("mousedown", onDocClick);
  }, []);

  useEffect(() => {
    if (!query.trim()) {
      setHits([]);
      setError(null);
      setLoading(false);
      setSearched(false);
      return;
    }
    // A new query supersedes the previous one, so drop the previous
    // query's results and error immediately rather than leaving them on
    // screen (showing matches for "rep" while the box reads "report")
    // until this query's response arrives.
    setHits([]);
    setError(null);
    setSearched(false);
    // 250 ms is a comfortable compromise: fast enough to feel live,
    // slow enough to skip intermediate keystrokes on fast typists.
    //
    // `cancelled` guards against out-of-order responses: clearing the
    // debounce timer in the cleanup cannot abort a request that has
    // already fired, so on slow networks an older query's response can
    // resolve after a newer one. Without this guard the late response
    // would overwrite the current results (and reset loading) for a
    // query the user has already moved on from. Every state mutation in
    // the callback — including setLoading — sits behind the guard so a
    // superseded effect can never touch state for the current query.
    let cancelled = false;
    const handle = window.setTimeout(async () => {
      if (cancelled) return;
      setLoading(true);
      try {
        const resp = await searchFiles(query.trim(), { limit: 20 });
        if (cancelled) return;
        setHits(resp.hits);
        setError(null);
        setSearched(true);
        setOpen(true);
      } catch (err) {
        if (cancelled) return;
        setError(translateApiError(err, t));
      } finally {
        if (!cancelled) setLoading(false);
      }
    }, 250);
    return () => {
      cancelled = true;
      window.clearTimeout(handle);
    };
  }, [query, t]);

  const pick = (hit: SearchHit) => {
    setOpen(false);
    setQuery("");
    if (hit.type === "folder") {
      nav(`/drive/folder/${hit.id}`);
    } else {
      // File hit: open its containing folder. A file at the workspace
      // root has folder_id === null and lives directly under /drive, so
      // route there rather than leaving the click a no-op.
      nav(hit.folder_id ? `/drive/folder/${hit.folder_id}` : "/drive");
    }
  };

  const showDropdown = open && (loading || !!error || hits.length > 0 || searched);

  return (
    <div ref={wrapRef} className="relative w-full sm:w-80">
      <Search
        aria-hidden="true"
        className="pointer-events-none absolute left-3 top-1/2 h-4 w-4 -translate-y-1/2 text-muted"
      />
      <input
        type="search"
        aria-label={t("search.ariaLabel")}
        placeholder={t("search.placeholder")}
        value={query}
        onFocus={() => query && setOpen(true)}
        onChange={(e) => setQuery(e.target.value)}
        onKeyDown={(e) => {
          if (e.key === "Escape") setOpen(false);
        }}
        className="h-9 w-full rounded-lg border border-border bg-surface pl-9 pr-3 text-sm text-fg shadow-sm transition-colors placeholder:text-muted focus-visible:border-brand focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
      />
      {showDropdown ? (
        <div
          role="listbox"
          aria-label={t("search.resultsAria")}
          className="absolute left-0 right-0 top-[calc(100%+4px)] z-20 max-h-80 overflow-y-auto rounded-lg border border-border bg-overlay shadow-overlay animate-fade-in"
        >
          {loading ? (
            <div className="px-3 py-2 text-xs text-muted">{t("search.searching")}</div>
          ) : null}
          {error ? (
            <div role="alert" className="px-3 py-2 text-xs text-danger">
              {error}
            </div>
          ) : null}
          {hits.map((hit) => (
            <button
              key={`${hit.type}:${hit.id}`}
              onClick={() => pick(hit)}
              role="option"
              className="grid w-full grid-cols-[auto_1fr] gap-x-2 border-b border-border px-3 py-2 text-left transition-colors last:border-b-0 hover:bg-surface-2 focus-visible:bg-surface-2 focus-visible:outline-none"
            >
              <span
                className={cn(
                  "row-span-2 inline-flex h-5 w-12 items-center justify-center self-center rounded-full text-[10px] font-semibold uppercase tracking-wide",
                  hit.type === "folder"
                    ? "bg-warning/15 text-warning"
                    : "bg-brand/10 text-brand",
                )}
              >
                {hit.type === "folder" ? t("search.typeFolder") : t("search.typeFile")}
              </span>
              <span className="truncate text-sm font-medium text-fg">{hit.name}</span>
              <span className="truncate text-xs text-muted">{hit.path}</span>
            </button>
          ))}
          {!loading && !error && hits.length === 0 && searched ? (
            <div className="px-3 py-2 text-xs text-muted">{t("search.noResults")}</div>
          ) : null}
        </div>
      ) : null}
    </div>
  );
}

import { useEffect, useMemo, useRef, useState } from "react";
import * as Dialog from "@radix-ui/react-dialog";
import { useNavigate } from "react-router-dom";
import {
  Search as SearchIcon,
  File as FileIcon,
  Folder as FolderIcon,
  Clock,
  CornerDownLeft,
} from "lucide-react";
import { searchFiles, type SearchHit } from "../api/client";
import { cn } from "../lib/cn";

const RECENTS_KEY = "zkdrive.recentSearches";
const MAX_RECENTS = 6;
const DEBOUNCE_MS = 180;

type TypeFilter = "all" | "file" | "folder";

function readRecents(): string[] {
  try {
    const raw = localStorage.getItem(RECENTS_KEY);
    const parsed = raw ? (JSON.parse(raw) as unknown) : [];
    return Array.isArray(parsed)
      ? parsed.filter((x): x is string => typeof x === "string")
      : [];
  } catch {
    return [];
  }
}

function pushRecent(term: string): string[] {
  const trimmed = term.trim();
  if (!trimmed) return readRecents();
  const next = [trimmed, ...readRecents().filter((r) => r !== trimmed)].slice(0, MAX_RECENTS);
  try {
    localStorage.setItem(RECENTS_KEY, JSON.stringify(next));
  } catch {
    /* non-fatal */
  }
  return next;
}

export interface CommandPaletteDialogProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
}

// CommandPaletteDialog is the lazily-loaded body of the global palette.
// Search is debounced and runs against GET /search; results are
// keyboard-navigable (arrows + Enter) and selecting one navigates to the
// file/folder. Recent searches persist in localStorage.
export default function CommandPaletteDialog({
  open,
  onOpenChange,
}: CommandPaletteDialogProps) {
  const nav = useNavigate();
  const [query, setQuery] = useState("");
  const [filter, setFilter] = useState<TypeFilter>("all");
  const [hits, setHits] = useState<SearchHit[]>([]);
  const [loading, setLoading] = useState(false);
  const [recents, setRecents] = useState<string[]>(() => readRecents());
  const [activeIndex, setActiveIndex] = useState(0);
  const reqSeq = useRef(0);

  // Reset transient state each time the palette opens so every invocation is
  // a clean slate — including the type filter, which would otherwise persist
  // a previous "file"/"folder" selection and silently scope the next search.
  useEffect(() => {
    if (open) {
      setQuery("");
      setHits([]);
      setActiveIndex(0);
      setFilter("all");
      setRecents(readRecents());
    }
  }, [open]);

  // Debounced search.
  useEffect(() => {
    const q = query.trim();
    if (!q) {
      setHits([]);
      setLoading(false);
      return;
    }
    setLoading(true);
    const seq = ++reqSeq.current;
    const handle = setTimeout(async () => {
      try {
        const resp = await searchFiles(q, { limit: 20 });
        if (seq !== reqSeq.current) return;
        setHits(resp.hits);
        setActiveIndex(0);
      } catch {
        if (seq === reqSeq.current) setHits([]);
      } finally {
        if (seq === reqSeq.current) setLoading(false);
      }
    }, DEBOUNCE_MS);
    return () => clearTimeout(handle);
  }, [query]);

  const filteredHits = useMemo(
    () => (filter === "all" ? hits : hits.filter((h) => h.type === filter)),
    [hits, filter],
  );

  // Switching the type filter changes the visible list, so move the active
  // highlight back to the first row rather than leaving it on a now-hidden
  // (or out-of-range) index.
  useEffect(() => {
    setActiveIndex(0);
  }, [filter]);

  const goToHit = (hit: SearchHit) => {
    setRecents(pushRecent(query));
    onOpenChange(false);
    if (hit.type === "folder") {
      nav(`/drive/folder/${hit.id}`);
    } else if (hit.folder_id) {
      nav(`/drive/folder/${hit.folder_id}`);
    } else {
      nav("/drive");
    }
  };

  const onKeyDown = (e: React.KeyboardEvent) => {
    if (e.key === "ArrowDown") {
      e.preventDefault();
      setActiveIndex((i) => Math.min(i + 1, Math.max(filteredHits.length - 1, 0)));
    } else if (e.key === "ArrowUp") {
      e.preventDefault();
      setActiveIndex((i) => Math.max(i - 1, 0));
    } else if (e.key === "Enter") {
      e.preventDefault();
      const hit = filteredHits[activeIndex];
      if (hit) goToHit(hit);
    }
  };

  return (
    <Dialog.Root open={open} onOpenChange={onOpenChange}>
      <Dialog.Portal>
        <Dialog.Overlay className="fixed inset-0 z-[120] bg-black/40 backdrop-blur-sm animate-fade-in" />
        <Dialog.Content
          aria-label="Command palette"
          onKeyDown={onKeyDown}
          className="fixed left-1/2 top-[12vh] z-[120] w-[calc(100vw-2rem)] max-w-xl -translate-x-1/2 overflow-hidden rounded-card border border-border bg-overlay shadow-overlay animate-scale-in focus:outline-none"
        >
          <Dialog.Title className="sr-only">Search files and folders</Dialog.Title>
          <div className="flex items-center gap-3 border-b border-border px-4">
            <SearchIcon className="h-5 w-5 shrink-0 text-muted" aria-hidden="true" />
            {/* eslint-disable-next-line jsx-a11y/no-autofocus */}
            <input
              autoFocus
              value={query}
              onChange={(e) => setQuery(e.target.value)}
              placeholder="Search files and folders…"
              aria-label="Search files and folders"
              className="h-12 w-full bg-transparent text-fg outline-none placeholder:text-muted"
            />
          </div>

          <div className="flex items-center gap-1 border-b border-border px-3 py-2">
            {(["all", "file", "folder"] as const).map((f) => (
              <button
                key={f}
                type="button"
                onClick={() => setFilter(f)}
                aria-pressed={filter === f}
                className={cn(
                  "rounded-md px-2.5 py-1 text-xs font-medium capitalize",
                  filter === f ? "bg-brand text-brand-fg" : "text-muted hover:bg-surface-2",
                )}
              >
                {f === "all" ? "All" : `${f}s`}
              </button>
            ))}
          </div>

          <div className="max-h-[50vh] overflow-y-auto p-2">
            {!query.trim() && recents.length > 0 && (
              <div>
                <div className="px-2 py-1 text-xs font-medium uppercase tracking-wide text-muted">
                  Recent
                </div>
                {recents.map((r) => (
                  <button
                    key={r}
                    type="button"
                    onClick={() => setQuery(r)}
                    className="flex w-full items-center gap-3 rounded-md px-2 py-2 text-left text-sm text-fg hover:bg-surface-2"
                  >
                    <Clock className="h-4 w-4 text-muted" aria-hidden="true" />
                    {r}
                  </button>
                ))}
              </div>
            )}

            {query.trim() && loading && (
              <div className="px-3 py-6 text-center text-sm text-muted">Searching…</div>
            )}

            {query.trim() && !loading && filteredHits.length === 0 && (
              <div className="px-3 py-6 text-center text-sm text-muted">
                No results for &ldquo;{query.trim()}&rdquo;
              </div>
            )}

            {filteredHits.length > 0 && (
              <ul role="listbox" aria-label="Search results">
                {filteredHits.map((hit, i) => (
                  <li key={`${hit.type}-${hit.id}`} role="option" aria-selected={i === activeIndex}>
                    <button
                      type="button"
                      onMouseEnter={() => setActiveIndex(i)}
                      onClick={() => goToHit(hit)}
                      className={cn(
                        "flex w-full items-center gap-3 rounded-md px-2 py-2 text-left",
                        i === activeIndex ? "bg-surface-2" : "",
                      )}
                    >
                      {hit.type === "folder" ? (
                        <FolderIcon className="h-4 w-4 shrink-0 text-brand" aria-hidden="true" />
                      ) : (
                        <FileIcon className="h-4 w-4 shrink-0 text-muted" aria-hidden="true" />
                      )}
                      <span className="min-w-0 flex-1">
                        <span className="block truncate text-sm text-fg">{hit.name}</span>
                        {hit.path && (
                          <span className="block truncate text-xs text-muted">{hit.path}</span>
                        )}
                      </span>
                      {i === activeIndex && (
                        <CornerDownLeft className="h-4 w-4 shrink-0 text-muted" aria-hidden="true" />
                      )}
                    </button>
                  </li>
                ))}
              </ul>
            )}
          </div>
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog.Root>
  );
}

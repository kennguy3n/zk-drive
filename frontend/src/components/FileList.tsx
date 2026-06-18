import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import { useVirtualizer } from "@tanstack/react-virtual";
import {
  Download,
  Pencil,
  Share2,
  Trash2,
  FileEdit,
  ArrowUp,
  ArrowDown,
} from "lucide-react";
import { getDownloadURL, type FileItem } from "../api/client";
import { isOfficeDocument } from "../collab/office";
import FilePreview from "./FilePreview";
import { EmptyState } from "./ui/EmptyState";
import { cn } from "../lib/cn";

export interface FileListProps {
  files: FileItem[];
  // The page owns the rename flow (it prompts for the new name via the
  // design-system usePrompt dialog) so this list stays presentational and
  // free of native window.prompt. It receives the whole file so the
  // prompt can seed the current name.
  onRename: (file: FileItem) => void;
  onDelete: (id: string) => void;
  // onShare is optional so callers that don't wire ShareDialog yet
  // keep working unchanged — the Share button is hidden when omitted.
  onShare?: (file: FileItem) => void;
  // onEdit is optional so callers that haven't wired the office editor
  // keep working unchanged. When provided, an "Edit" button appears
  // for office document types (see collab/office.ts) and invokes it
  // with the target file; the parent decides how to present the
  // editor (FileBrowserPage opens the OnlyOffice editor overlay).
  onEdit?: (file: FileItem) => void;
  // selectedIDs + onToggleSelect power the bulk-operations toolbar
  // rendered by the parent page. When omitted, selection checkboxes
  // are hidden (keeps the legacy single-file UX for callers that
  // haven't opted in yet).
  selectedIDs?: Set<string>;
  onToggleSelect?: (id: string) => void;
}

type SortKey = "name" | "size_bytes" | "updated_at";
type SortDir = "asc" | "desc";

// Above this many rows we window the list with @tanstack/react-virtual so
// a 1000+ file folder only mounts the ~visible rows (4.7). Below it, the
// overhead isn't worth the lost native find-in-page, so we render all rows.
const VIRTUAL_THRESHOLD = 80;
const ROW_HEIGHT = 52;

// formatBytes renders a byte count as a human-friendly string.
function formatBytes(n: number): string {
  if (n < 1024) return `${n} B`;
  const units = ["KB", "MB", "GB", "TB"];
  let v = n / 1024;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v.toFixed(1)} ${units[i]}`;
}

// handleDownload fetches a presigned URL and opens it. We don't force an
// <a download> click because the user may want the browser's default
// behaviour (view PDFs, play media, etc.).
async function handleDownload(id: string): Promise<void> {
  const url = await getDownloadURL(id);
  window.open(url, "_blank", "noopener");
}

const actionBtnCls =
  "inline-flex h-7 w-7 items-center justify-center rounded-md text-muted " +
  "hover:bg-surface-2 hover:text-fg focus-visible:outline-none " +
  "focus-visible:ring-2 focus-visible:ring-ring";

interface FileRowProps {
  file: FileItem;
  active: boolean;
  showSelection: boolean;
  selected: boolean;
  onToggleSelect?: (id: string) => void;
  onRename: (file: FileItem) => void;
  onDelete: (id: string) => void;
  onShare?: (file: FileItem) => void;
  onEdit?: (file: FileItem) => void;
  onFocusRow: () => void;
}

// FileRow is a single grid row. Extracted so the virtualized and plain
// render paths share identical markup/behaviour.
function FileRow({
  file: f,
  active,
  showSelection,
  selected,
  onToggleSelect,
  onRename,
  onDelete,
  onShare,
  onEdit,
  onFocusRow,
}: FileRowProps) {
  const { t } = useTranslation();
  return (
    <div
      role="row"
      aria-selected={showSelection ? selected : undefined}
      tabIndex={active ? 0 : -1}
      onFocus={onFocusRow}
      className={cn(
        "flex h-[52px] items-center gap-3 border-b border-border px-3 text-sm outline-none",
        "focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-inset",
        active && "bg-surface-2",
      )}
    >
      {showSelection && (
        <div role="cell" className="flex w-6 shrink-0 items-center">
          <input
            type="checkbox"
            checked={selected}
            onChange={() => onToggleSelect?.(f.id)}
            aria-label={t("drive.selectAria", { name: f.name })}
            className="h-4 w-4 accent-brand"
          />
        </div>
      )}
      <div role="cell" className="flex min-w-0 flex-1 items-center gap-2.5">
        <FilePreview fileID={f.id} mimeType={f.mime_type} size="thumb" alt={f.name} />
        <span className="truncate text-fg">{f.name}</span>
      </div>
      <div role="cell" className="hidden w-24 shrink-0 text-right text-muted sm:block">
        {formatBytes(f.size_bytes)}
      </div>
      <div role="cell" className="hidden w-44 shrink-0 text-muted md:block">
        {new Date(f.updated_at).toLocaleString()}
      </div>
      <div role="cell" className="flex shrink-0 items-center gap-0.5">
        <button
          type="button"
          onClick={() => void handleDownload(f.id)}
          className={actionBtnCls}
          aria-label={t("common.download")}
          title={t("common.download")}
        >
          <Download className="h-4 w-4" aria-hidden="true" />
        </button>
        {onEdit && isOfficeDocument(f.name) && (
          <button
            type="button"
            onClick={() => onEdit(f)}
            className={actionBtnCls}
            aria-label={t("onlyoffice.editAria", { name: f.name })}
            title={t("common.edit")}
          >
            <FileEdit className="h-4 w-4" aria-hidden="true" />
          </button>
        )}
        <button
          type="button"
          onClick={() => onRename(f)}
          className={actionBtnCls}
          aria-label={t("common.rename")}
          title={t("common.rename")}
        >
          <Pencil className="h-4 w-4" aria-hidden="true" />
        </button>
        {onShare && (
          <button
            type="button"
            onClick={() => onShare(f)}
            className={actionBtnCls}
            aria-label={t("common.share")}
            title={t("common.share")}
          >
            <Share2 className="h-4 w-4" aria-hidden="true" />
          </button>
        )}
        <button
          type="button"
          onClick={() => onDelete(f.id)}
          className={cn(actionBtnCls, "hover:text-danger")}
          aria-label={t("common.delete")}
          title={t("common.delete")}
        >
          <Trash2 className="h-4 w-4" aria-hidden="true" />
        </button>
      </div>
    </div>
  );
}

// FileList is the polished file browser table: sortable columns, optional
// multi-select, full keyboard navigation (arrow keys move the active row,
// Enter downloads, Delete trashes), and virtual scrolling for large
// folders. Rendered with ARIA grid roles so screen readers announce it as
// a table without relying on a native <table> (which can't be virtualized
// cleanly). Styling uses semantic tokens so it adapts to dark mode.
export default function FileList({
  files,
  onRename,
  onDelete,
  onShare,
  onEdit,
  selectedIDs,
  onToggleSelect,
}: FileListProps) {
  const { t } = useTranslation();
  const [sortKey, setSortKey] = useState<SortKey>("name");
  const [sortDir, setSortDir] = useState<SortDir>("asc");
  const [activeIndex, setActiveIndex] = useState(0);
  const scrollRef = useRef<HTMLDivElement>(null);

  const showSelection = !!onToggleSelect;

  const sorted = useMemo(() => {
    const copy = [...files];
    copy.sort((a, b) => {
      let cmp: number;
      if (sortKey === "name") cmp = a.name.localeCompare(b.name);
      else if (sortKey === "size_bytes") cmp = a.size_bytes - b.size_bytes;
      else cmp = a.updated_at.localeCompare(b.updated_at);
      return sortDir === "asc" ? cmp : -cmp;
    });
    return copy;
  }, [files, sortKey, sortDir]);

  // Keep the active row in bounds when the file set changes (e.g. navigating
  // to a smaller folder). A stale out-of-range index would leave no row with
  // tabIndex=0, making the grid unreachable by keyboard Tab.
  useEffect(() => {
    setActiveIndex((prev) => Math.min(prev, Math.max(sorted.length - 1, 0)));
  }, [sorted.length]);

  const virtualize = sorted.length > VIRTUAL_THRESHOLD;
  const virtualizer = useVirtualizer({
    count: sorted.length,
    getScrollElement: () => scrollRef.current,
    estimateSize: () => ROW_HEIGHT,
    overscan: 12,
    enabled: virtualize,
  });

  const toggleSort = (key: SortKey) => {
    if (key === sortKey) {
      setSortDir((d) => (d === "asc" ? "desc" : "asc"));
    } else {
      setSortKey(key);
      setSortDir("asc");
    }
  };

  const moveActive = useCallback(
    (next: number) => {
      const clamped = Math.max(0, Math.min(next, sorted.length - 1));
      setActiveIndex(clamped);
      if (virtualize) virtualizer.scrollToIndex(clamped, { align: "auto" });
    },
    [sorted.length, virtualize, virtualizer],
  );

  const onKeyDown = (e: React.KeyboardEvent) => {
    // Grid-level navigation/shortcuts apply only when focus is on a row
    // itself. If focus has moved into an interactive child (action button,
    // selection checkbox), let that control own the key — otherwise
    // Enter/Delete/Space would fire both the grid action (download/delete/
    // toggle) and the control's native click, causing a double action.
    const target = e.target as HTMLElement;
    if (target.closest("button, input, a, [role='button']")) return;

    if (e.key === "ArrowDown") {
      e.preventDefault();
      moveActive(activeIndex + 1);
    } else if (e.key === "ArrowUp") {
      e.preventDefault();
      moveActive(activeIndex - 1);
    } else if (e.key === "Home") {
      e.preventDefault();
      moveActive(0);
    } else if (e.key === "End") {
      e.preventDefault();
      moveActive(sorted.length - 1);
    } else if (e.key === "Enter") {
      const f = sorted[activeIndex];
      if (f) void handleDownload(f.id);
    } else if (e.key === "Delete") {
      const f = sorted[activeIndex];
      if (f) onDelete(f.id);
    } else if (e.key === " " && showSelection) {
      e.preventDefault();
      const f = sorted[activeIndex];
      if (f) onToggleSelect?.(f.id);
    }
  };

  if (files.length === 0) {
    return <EmptyState title={t("drive.noFilesInFolder")} />;
  }

  const rowProps = (f: FileItem, index: number): FileRowProps => ({
    file: f,
    active: index === activeIndex,
    showSelection,
    selected: selectedIDs?.has(f.id) ?? false,
    onToggleSelect,
    onRename,
    onDelete,
    onShare,
    onEdit,
    onFocusRow: () => setActiveIndex(index),
  });

  const sortIndicator = (key: SortKey) =>
    sortKey === key ? (
      sortDir === "asc" ? (
        <ArrowUp className="h-3 w-3" aria-hidden="true" />
      ) : (
        <ArrowDown className="h-3 w-3" aria-hidden="true" />
      )
    ) : null;

  return (
    <div
      role="grid"
      aria-label={t("common.name")}
      aria-rowcount={sorted.length + 1}
      onKeyDown={onKeyDown}
    >
      {/* Header */}
      <div
        role="row"
        className="flex items-center gap-3 border-b border-border px-3 py-2 text-xs font-medium text-muted"
      >
        {showSelection && <div role="columnheader" className="w-6 shrink-0" />}
        <button
          type="button"
          role="columnheader"
          aria-sort={sortKey === "name" ? (sortDir === "asc" ? "ascending" : "descending") : "none"}
          onClick={() => toggleSort("name")}
          className="flex min-w-0 flex-1 items-center gap-1 text-left hover:text-fg"
        >
          {t("common.name")} {sortIndicator("name")}
        </button>
        <button
          type="button"
          role="columnheader"
          aria-sort={sortKey === "size_bytes" ? (sortDir === "asc" ? "ascending" : "descending") : "none"}
          onClick={() => toggleSort("size_bytes")}
          className="hidden w-24 shrink-0 items-center justify-end gap-1 hover:text-fg sm:flex"
        >
          {t("common.size")} {sortIndicator("size_bytes")}
        </button>
        <button
          type="button"
          role="columnheader"
          aria-sort={sortKey === "updated_at" ? (sortDir === "asc" ? "ascending" : "descending") : "none"}
          onClick={() => toggleSort("updated_at")}
          className="hidden w-44 shrink-0 items-center gap-1 hover:text-fg md:flex"
        >
          {t("common.modified")} {sortIndicator("updated_at")}
        </button>
        <div role="columnheader" className="shrink-0 text-right">
          {t("common.actions")}
        </div>
      </div>

      {/* Body */}
      {virtualize ? (
        <div
          ref={scrollRef}
          className="max-h-[60vh] overflow-y-auto"
          // `contain: paint` confines repaints to the scroll box without
          // applying size containment (which would collapse a max-height
          // container to zero). Keeps scrolling cheap for 1000+ rows.
          style={{ contain: "paint" }}
        >
          <div style={{ height: virtualizer.getTotalSize(), position: "relative", width: "100%" }}>
            {virtualizer.getVirtualItems().map((vi) => {
              const f = sorted[vi.index];
              return (
                <div
                  key={f.id}
                  data-index={vi.index}
                  ref={virtualizer.measureElement}
                  style={{
                    position: "absolute",
                    top: 0,
                    left: 0,
                    width: "100%",
                    transform: `translateY(${vi.start}px)`,
                  }}
                >
                  <FileRow {...rowProps(f, vi.index)} />
                </div>
              );
            })}
          </div>
        </div>
      ) : (
        <div>
          {sorted.map((f, i) => (
            <FileRow key={f.id} {...rowProps(f, i)} />
          ))}
        </div>
      )}
    </div>
  );
}

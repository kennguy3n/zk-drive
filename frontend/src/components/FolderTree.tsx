import { useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { useTranslation } from "react-i18next";
import { Folder as FolderIcon, Home } from "lucide-react";
import { listFolders, type Folder } from "../api/client";
import { translateApiError } from "../api/errors";
import EncryptionBadge from "./EncryptionBadge";
import { FolderTreeSkeleton } from "./ui";
import { cn } from "../lib/cn";

// FolderTree is a one-level tree: it shows the workspace root plus
// direct children of the current folder. A full recursive tree is a
// follow-up enhancement.
export default function FolderTree({
  currentFolderID,
  reloadKey = 0,
}: {
  currentFolderID: string | null;
  // Incremented by the parent after a root-level folder mutation so the
  // tree refetches even when currentFolderID is unchanged (e.g. creating
  // or deleting a folder without navigating away from the current view).
  reloadKey?: number;
}) {
  const { t } = useTranslation();
  const [rootFolders, setRootFolders] = useState<Folder[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    let cancelled = false;
    setLoading(true);
    // Clear any prior error at the start of the (re-)fetch so a stale
    // failure message can't render alongside the loading skeleton during
    // a navigation- or reloadKey-driven refetch.
    setError(null);
    listFolders(null)
      .then((list) => {
        if (!cancelled) {
          setRootFolders(list);
          setError(null);
        }
      })
      .catch((err) => {
        if (!cancelled) setError(translateApiError(err, t));
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [currentFolderID, reloadKey, t]);

  return (
    <aside
      aria-label={t("nav.workspace")}
      className="w-60 shrink-0 border-r border-border bg-surface p-4"
    >
      <div className="mb-3 px-2 text-xs font-semibold uppercase tracking-wide text-muted">
        {t("nav.workspace")}
      </div>
      <div className="relative">
        {currentFolderID === null && (
          <span
            aria-hidden="true"
            className="absolute left-0 top-1.5 bottom-1.5 w-0.5 rounded-full bg-brand"
          />
        )}
        <Link
          to="/drive"
          aria-current={currentFolderID === null ? "page" : undefined}
          className={cn(
            "group flex items-center gap-2 rounded-lg px-2 py-1.5 text-sm transition-colors",
            currentFolderID === null
              ? "bg-brand/10 font-medium text-brand"
              : "text-fg hover:bg-surface-2",
          )}
        >
          <Home
            className={cn(
              "h-4 w-4 shrink-0",
              currentFolderID === null
                ? "text-brand"
                : "text-muted group-hover:text-fg",
            )}
            aria-hidden="true"
          />
          <span className="truncate">{t("drive.rootFolder")}</span>
        </Link>
      </div>
      {/*
        Stale-while-revalidate: only show the skeleton on the initial load
        (when there's no prior data to display). On reloadKey/navigation
        refetches we keep the previously fetched list visible instead of
        replacing it with a skeleton, so the sidebar never renders a
        skeleton and a populated list at the same time.
      */}
      {loading && rootFolders.length === 0 ? (
        <div className="mt-2">
          <FolderTreeSkeleton />
        </div>
      ) : null}
      {error ? (
        <div role="alert" className="mt-2 px-2 text-xs text-danger">
          {error}
        </div>
      ) : null}
      <ul className="mt-1 list-none space-y-0.5 p-0">
        {rootFolders.map((f) => {
          const active = currentFolderID === f.id;
          return (
            <li
              key={f.id}
              className={cn(
                "group relative flex items-center rounded-lg pr-2 transition-colors",
                active ? "bg-brand/10" : "hover:bg-surface-2",
              )}
            >
              {/*
                Active rows wear a brand-tinted fill plus a left accent
                bar — the KChat "current section" treatment — so the
                selected folder reads at a glance instead of relying on
                a near-invisible flat highlight.
              */}
              {active && (
                <span
                  aria-hidden="true"
                  className="absolute left-0 top-1.5 bottom-1.5 w-0.5 rounded-full bg-brand"
                />
              )}
              {/*
                The folder-name `<Link>` and the privacy indicator are
                siblings (not parent/child) so the indicator can be its
                own `<Link to="/drive/privacy">` without nesting `<a>`
                inside `<a>` (invalid HTML; suppresses clicks).

                The folder icon, name, and the row's horizontal padding
                all live INSIDE the name `<Link>` (flex-1) so the whole
                left region is one navigation target with no dead zones.
                The privacy indicator is now icon-only (see
                `size="icon"`) so the name keeps layout priority and
                truncates with an ellipsis instead of being squeezed to
                an unreadable "Legal Con…".
              */}
              <Link
                to={`/drive/folder/${f.id}`}
                aria-current={active ? "page" : undefined}
                className={cn(
                  "flex min-w-0 flex-1 items-center gap-2 px-2 py-1.5 text-sm",
                  active ? "font-medium text-brand" : "text-fg",
                )}
              >
                <FolderIcon
                  className={cn(
                    "h-4 w-4 shrink-0",
                    active ? "text-brand" : "text-muted group-hover:text-fg",
                  )}
                  aria-hidden="true"
                />
                <span className="truncate">{f.name}</span>
              </Link>
              <EncryptionBadge mode={f.encryption_mode} size="icon" tabbable={false} />
            </li>
          );
        })}
        {!loading && !error && rootFolders.length === 0 ? (
          <li className="px-2 py-1.5 text-xs text-muted">{t("drive.noFolders")}</li>
        ) : null}
      </ul>
    </aside>
  );
}

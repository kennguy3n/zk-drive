import { useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { useTranslation } from "react-i18next";
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

  const rowBase =
    "flex items-center rounded-lg text-sm transition-colors hover:bg-surface-2";

  return (
    <aside
      aria-label={t("nav.workspace")}
      className="w-60 shrink-0 border-r border-border bg-surface p-4"
    >
      <div className="mb-2 px-2 text-xs font-semibold uppercase tracking-wide text-muted">
        {t("nav.workspace")}
      </div>
      <Link
        to="/drive"
        aria-current={currentFolderID === null ? "page" : undefined}
        className={cn(
          "block rounded-lg px-2 py-1.5 text-sm transition-colors hover:bg-surface-2",
          currentFolderID === null ? "bg-brand/10 font-medium text-brand" : "text-fg",
        )}
      >
        {t("drive.rootFolder")}
      </Link>
      {loading ? (
        <div className="mt-2">
          <FolderTreeSkeleton />
        </div>
      ) : null}
      {error ? (
        <div role="alert" className="mt-2 px-2 text-xs text-danger">
          {error}
        </div>
      ) : null}
      <ul className="mt-2 list-none space-y-0.5 p-0">
        {rootFolders.map((f) => (
          <li
            key={f.id}
            className={cn(
              rowBase,
              "pr-2",
              currentFolderID === f.id ? "bg-brand/10" : "",
            )}
          >
            {/*
              The folder-name `<Link>` and the privacy-mode badge are
              siblings here (not parent/child) so the badge can render
              as its own clickable `<Link to="/drive/privacy">` without
              nesting `<a>` inside `<a>` — which is invalid HTML and
              suppresses badge click events in most browsers.

              To preserve the "click anywhere on the row to open the
              folder" UX we had before the refactor, the row padding
              and the bulk of the horizontal space live INSIDE the
              folder-name `<Link>` (padding + flex: 1). The badge
              keeps its own intrinsic width at the right edge with a
              small `paddingRight` on the `<li>` for visual breathing
              room. There is no `gap` between the link and the badge
              — any gap would be an unclickable dead zone for folder
              navigation, which was the Devin Review finding on the
              first cut of this refactor.
            */}
            <Link
              to={`/drive/folder/${f.id}`}
              aria-current={currentFolderID === f.id ? "page" : undefined}
              className={cn(
                "mr-1.5 flex-1 truncate px-2 py-1.5",
                currentFolderID === f.id ? "font-medium text-brand" : "text-fg",
              )}
            >
              {f.name}
            </Link>
            {/*
              Privacy-mode badge sits at the end of each sidebar row
              so users can see at a glance which folders are strict-
              ZK (server-blind) without having to open them. This is
              the PROPOSAL §3.3 "surface the mode everywhere a folder
              is rendered" contract: file list + breadcrumb + sidebar.
              EncryptionBadge falls back to the confidential rendering
              for folders missing the field (pre-Phase-4 rows), so the
              tree still renders cleanly.
            */}
            <EncryptionBadge mode={f.encryption_mode} tabbable={false} />
          </li>
        ))}
        {!loading && !error && rootFolders.length === 0 ? (
          <li className="px-2 py-1.5 text-xs text-muted">{t("drive.noFolders")}</li>
        ) : null}
      </ul>
    </aside>
  );
}

import { useCallback, useEffect, useRef, useState } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import {
  Search as SearchIcon,
  FolderPlus,
  LayoutTemplate,
  Shield,
  FileText,
  Settings,
  CreditCard,
  LogOut,
  Folder as FolderIcon,
  Share2,
  Pencil,
  Trash2,
  Move,
  Copy,
  Download,
  X,
} from "lucide-react";
import { Trans, useTranslation } from "react-i18next";
import FolderTree from "../components/FolderTree";
import FileList from "../components/FileList";
import UploadButton from "../components/UploadButton";
import SearchBar from "../components/SearchBar";
import ShareDialog from "../components/ShareDialog";
import OnlyOfficeEditor from "../components/OnlyOfficeEditor";
import EncryptionBadge, { type EncryptionMode } from "../components/EncryptionBadge";
import EnableNotificationsButton from "../components/EnableNotificationsButton";
import { translateApiError } from "../api/errors";
import {
  bulkCopy,
  bulkDelete,
  bulkDownload,
  bulkMove,
  createClientRoomFromTemplate,
  createFolder,
  deleteFile,
  deleteFolder,
  fetchClientRoomTemplates,
  getFolderContents,
  getOnlyOfficeStatus,
  listFolders,
  renameFile,
  renameFolder,
  type BulkResponse,
  type ClientRoomTemplate,
  type FileItem,
  type Folder,
} from "../api/client";
import { collectAllFolders } from "../api/folders";
import { useAuth } from "../hooks/useAuth";
import { useFeatures } from "../hooks/useFeatures";
import { Feature } from "../features/featureKeys";
import { ThemeToggle } from "../components/ThemeToggle";
import { useCommandPalette } from "../components/CommandPalette";
import { OnboardingEmptyState } from "../components/OnboardingEmptyState";
import {
  Button,
  Field,
  Input,
  Modal,
  FileListSkeleton,
  useConfirm,
  usePrompt,
  useResourcePicker,
  useToast,
  type PickerItem,
} from "../components/ui";
import { cn } from "../lib/cn";

// shareTarget is the resource currently being shared via ShareDialog.
// Kept discriminated-union so the dialog can render the right noun
// ("Share folder" vs "Share file") without a second prop.
type ShareTarget =
  | { type: "folder"; value: Folder }
  | { type: "file"; value: FileItem };

// How long a collected folder list stays usable before the next move/copy
// re-walks the tree. The list is also invalidated eagerly on local folder
// create/delete; the TTL is the backstop for changes made elsewhere (another
// tab or user) so a stale destination can't linger indefinitely.
const FOLDER_CACHE_TTL_MS = 30_000;

// FileBrowserPage is the main "drive" surface: breadcrumb + folder tree +
// file table + upload/create controls. The selected folder is stored in
// the URL so refreshes keep context.
export default function FileBrowserPage() {
  const { folderId } = useParams<{ folderId?: string }>();
  const currentFolderID = folderId ?? null;
  const nav = useNavigate();
  const { t } = useTranslation();
  const { logout, isAdmin } = useAuth();
  const { isEnabled } = useFeatures();
  const palette = useCommandPalette();
  const confirm = useConfirm();
  const prompt = usePrompt();
  const pickResource = useResourcePicker();
  const toast = useToast();
  // openRef lets the onboarding "Upload your first file" card trigger the
  // UploadButton's hidden file picker without duplicating upload logic.
  const uploadOpenRef = useRef<(() => void) | null>(null);
  // Monotonic sequence used by refresh() to discard superseded responses
  // (out-of-order folder-navigation / mutation refetch races).
  const refreshSeq = useRef(0);
  // Cached destination-folder list for the move/copy picker. Walking the whole
  // tree is O(folders) API calls, so we collect once and reuse it across
  // operations (see pickTargetFolder), invalidating on local folder
  // create/delete and after FOLDER_CACHE_TTL_MS.
  const folderCacheRef = useRef<{ folders: Folder[]; at: number } | null>(null);
  // Controls the in-flight tree walk so it can be aborted if the user
  // navigates away mid-collection on a large workspace, or superseded by a
  // newer walk.
  const collectAbortRef = useRef<AbortController | null>(null);

  const [folder, setFolder] = useState<Folder | null>(null);
  const [subfolders, setSubfolders] = useState<Folder[]>([]);
  const [files, setFiles] = useState<FileItem[]>([]);
  const [error, setError] = useState<string | null>(null);
  // True while the listing for the current view is loading (initial mount
  // and folder navigation). Gates the onboarding cards and the file/folder
  // sections so neither flashes before real data arrives: the arrays start
  // empty, which would otherwise read as "empty workspace" on the first
  // render. Only the navigation effect toggles this — in-place refetches
  // after a mutation (rename/delete/move) keep the current list visible
  // rather than blinking it back to a skeleton.
  const [loading, setLoading] = useState(true);
  const [shareTarget, setShareTarget] = useState<ShareTarget | null>(null);
  const [selectedFiles, setSelectedFiles] = useState<Set<string>>(new Set());
  const [createFolderOpen, setCreateFolderOpen] = useState(false);
  const [templateDialogOpen, setTemplateDialogOpen] = useState(false);
  // Office editing: feature flag (from the backend) plus the file
  // currently open in the OnlyOffice editor overlay.
  const [onlyOfficeEnabled, setOnlyOfficeEnabled] = useState(false);
  const [editorFile, setEditorFile] = useState<FileItem | null>(null);
  // Bumped after a root-level folder create/delete so the sidebar
  // (which lists only root folders) refetches; navigation alone won't
  // change its dependency otherwise.
  const [treeReloadKey, setTreeReloadKey] = useState(0);
  // True while a bulk move/copy/delete/download is in flight; disables the
  // bulk-action buttons so a fast double-click can't fire overlapping
  // mutations against the same selection.
  const [bulkBusy, setBulkBusy] = useState(false);

  const toggleSelect = useCallback((id: string) => {
    setSelectedFiles((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  }, []);

  const refresh = useCallback(async () => {
    // Supersession guard: rapid folder navigation (or a mutation refetch
    // racing a navigation) can leave two requests in flight. Without this,
    // an out-of-order earlier response would write stale contents to state
    // (the skeleton hides the flash, but the stale data still lands). Bump a
    // sequence per call and drop any response that a newer call has
    // superseded so only the latest view's data is ever committed.
    const seq = ++refreshSeq.current;
    setError(null);
    try {
      if (currentFolderID) {
        const { folder: f, children, files: f2 } = await getFolderContents(currentFolderID);
        if (seq !== refreshSeq.current) return;
        setFolder(f);
        setSubfolders(children);
        setFiles(f2);
      } else {
        const roots = await listFolders(null);
        if (seq !== refreshSeq.current) return;
        setFolder(null);
        setSubfolders(roots);
        // Root view: backend doesn't expose a file listing for the
        // null folder, so we show an empty table and nudge the user
        // to open a subfolder.
        setFiles([]);
      }
    } catch (err) {
      if (seq !== refreshSeq.current) return;
      setError(translateApiError(err, t));
    }
  }, [currentFolderID, t]);

  // Initial load + folder navigation: drive the loading flag here (not in
  // refresh) so post-mutation refetches don't toggle it and blink the list.
  useEffect(() => {
    let cancelled = false;
    setLoading(true);
    refresh().finally(() => {
      if (!cancelled) setLoading(false);
    });
    return () => {
      cancelled = true;
    };
  }, [refresh]);

  // Probe the office-editing feature flag once on mount. A failure
  // (endpoint missing / network) leaves it disabled so the Edit
  // affordance simply stays hidden.
  useEffect(() => {
    let cancelled = false;
    getOnlyOfficeStatus()
      .then((enabled) => {
        if (!cancelled) setOnlyOfficeEnabled(enabled);
      })
      .catch(() => {
        if (!cancelled) setOnlyOfficeEnabled(false);
      });
    return () => {
      cancelled = true;
    };
  }, []);

  // Selections are folder-scoped; clearing them on navigation prevents
  // stale IDs from a previous folder leaking into bulk operations.
  useEffect(() => {
    setSelectedFiles(new Set());
  }, [currentFolderID]);

  // Abort any in-flight folder-tree walk when the page unmounts so a long
  // collection on a large workspace doesn't keep firing requests after the
  // user has navigated away.
  useEffect(() => {
    return () => {
      collectAbortRef.current?.abort();
    };
  }, []);

  // Drops the cached destination-folder list so the next move/copy re-walks
  // the tree. Called whenever this page changes the folder set.
  const invalidateFolderCache = useCallback(() => {
    folderCacheRef.current = null;
  }, []);

  const handleCreateFolder = () => {
    setCreateFolderOpen(true);
  };

  const handleDeleteFolder = async (target: Folder) => {
    const ok = await confirm({
      title: t("drive.deleteFolderTitle"),
      description: t("drive.deleteFolderConfirmNamed", { name: target.name }),
      confirmLabel: t("common.delete"),
      cancelLabel: t("common.cancel"),
      tone: "danger",
    });
    if (!ok) return;
    try {
      await deleteFolder(target.id);
      invalidateFolderCache();
      await refresh();
      // The deleted folder is a child of the current view, so the sidebar
      // (root folders only) is affected only when deleting at the root.
      if (currentFolderID === null) setTreeReloadKey((k) => k + 1);
      toast.success(t("drive.folderDeleted", { name: target.name }));
    } catch (e) {
      toast.error(translateApiError(e, t));
    }
  };

  // A rename changes a folder's name/path, so the cached destination list the
  // move/copy picker serves would show stale labels until the TTL — invalidate
  // it here alongside the create/delete hooks.
  const handleRenameFolder = async (target: Folder) => {
    const name = await prompt({
      title: t("drive.renameFolderTitle"),
      label: t("common.name"),
      defaultValue: target.name,
      confirmLabel: t("common.rename"),
      required: true,
    });
    const next = name?.trim();
    if (!next || next === target.name) return;
    try {
      await renameFolder(target.id, next);
      invalidateFolderCache();
      await refresh();
      // Root folders are mirrored in the sidebar, so reload it when renaming
      // at the root.
      if (currentFolderID === null) setTreeReloadKey((k) => k + 1);
      toast.success(t("drive.folderRenamed", { name: next }));
    } catch (e) {
      toast.error(translateApiError(e, t));
    }
  };

  // Surfaces the outcome of a bulk operation. The API reports per-item
  // success/failure (BulkResponse), so a partial failure (some files moved,
  // some not) is reported honestly instead of looking identical to success.
  const reportBulk = useCallback(
    (res: BulkResponse, action: "move" | "copy" | "delete") => {
      if (res.failed.length === 0) {
        const key =
          action === "move"
            ? "drive.moveSuccess"
            : action === "copy"
              ? "drive.copySuccess"
              : "drive.deleteSuccess";
        toast.success(t(key, { count: res.succeeded.length }));
      } else {
        toast.error(
          t("drive.bulkSomeFailed", {
            ok: res.succeeded.length,
            failed: res.failed.length,
          }),
        );
      }
    },
    [t, toast],
  );

  // Opens the searchable folder picker fed by the live folder tree and
  // resolves to the chosen folder id (or null if cancelled). Replaces the
  // old "type a folder UUID" prompt — the single most user-hostile flow.
  const pickTargetFolder = useCallback(
    async (count: number, action: "move" | "copy"): Promise<string | null> => {
      let folders: Folder[];
      const cached = folderCacheRef.current;
      if (cached && Date.now() - cached.at < FOLDER_CACHE_TTL_MS) {
        folders = cached.folders;
      } else {
        // Supersede any previous walk, then collect with a fresh controller so
        // an unmount (or another move/copy) can abort this one.
        collectAbortRef.current?.abort();
        const controller = new AbortController();
        collectAbortRef.current = controller;
        try {
          folders = await collectAllFolders(controller.signal);
          folderCacheRef.current = { folders, at: Date.now() };
        } catch (e) {
          // An aborted walk (unmount / superseded) is not a user-facing error.
          if (controller.signal.aborted) return null;
          toast.error(translateApiError(e, t));
          return null;
        } finally {
          if (collectAbortRef.current === controller) collectAbortRef.current = null;
        }
      }
      const items: PickerItem[] = folders
        // Can't move/copy a file into the folder it already lives in.
        .filter((f) => f.id !== currentFolderID)
        .sort((a, b) => (a.path || a.name).localeCompare(b.path || b.name))
        .map((f) => ({
          id: f.id,
          label: f.name,
          description: f.path,
          searchText: `${f.name} ${f.path}`,
        }));
      const picked = await pickResource({
        title: action === "move" ? t("drive.movePickerTitle") : t("drive.copyPickerTitle"),
        description:
          action === "move"
            ? t("drive.movePickerDescription", { count })
            : t("drive.copyPickerDescription", { count }),
        items,
        searchable: true,
        searchPlaceholder: t("drive.movePickerSearchPlaceholder"),
        emptyMessage: t("drive.movePickerEmpty"),
        confirmLabel: action === "move" ? t("drive.move") : t("drive.copy"),
      });
      return picked?.id ?? null;
    },
    [currentFolderID, pickResource, t, toast],
  );

  const onBulkMove = async () => {
    if (selectedFiles.size === 0) return;
    setBulkBusy(true);
    try {
      const target = await pickTargetFolder(selectedFiles.size, "move");
      if (!target) return;
      const res = await bulkMove({ file_ids: [...selectedFiles], target_folder_id: target });
      setSelectedFiles(new Set());
      await refresh();
      reportBulk(res, "move");
    } catch (e) {
      toast.error(translateApiError(e, t));
    } finally {
      setBulkBusy(false);
    }
  };

  const onBulkCopy = async () => {
    if (selectedFiles.size === 0) return;
    setBulkBusy(true);
    try {
      const target = await pickTargetFolder(selectedFiles.size, "copy");
      if (!target) return;
      const res = await bulkCopy({ file_ids: [...selectedFiles], target_folder_id: target });
      setSelectedFiles(new Set());
      await refresh();
      reportBulk(res, "copy");
    } catch (e) {
      toast.error(translateApiError(e, t));
    } finally {
      setBulkBusy(false);
    }
  };

  const onBulkDelete = async () => {
    if (selectedFiles.size === 0) return;
    const ok = await confirm({
      title: t("drive.bulkDeleteTitle"),
      description: t("drive.bulkDeleteConfirm", { count: selectedFiles.size }),
      confirmLabel: t("common.delete"),
      cancelLabel: t("common.cancel"),
      tone: "danger",
    });
    if (!ok) return;
    setBulkBusy(true);
    try {
      const res = await bulkDelete({ file_ids: [...selectedFiles] });
      setSelectedFiles(new Set());
      await refresh();
      reportBulk(res, "delete");
    } catch (e) {
      toast.error(translateApiError(e, t));
    } finally {
      setBulkBusy(false);
    }
  };

  const onBulkDownload = async () => {
    if (selectedFiles.size === 0) return;
    setBulkBusy(true);
    try {
      const blob = await bulkDownload([...selectedFiles]);
      const url = URL.createObjectURL(blob);
      const a = document.createElement("a");
      a.href = url;
      a.download = "download.zip";
      a.click();
      URL.revokeObjectURL(url);
    } catch (e) {
      toast.error(translateApiError(e, t));
    } finally {
      setBulkBusy(false);
    }
  };

  // FileList owns the rename/delete confirmation dialogs (design-system
  // useConfirm/usePrompt fired inside the row); the page just performs the
  // mutation and refreshes once the list has collected a valid value.
  const handleRenameFile = async (id: string, name: string) => {
    try {
      await renameFile(id, name);
      await refresh();
      toast.success(t("drive.fileRenamed"));
    } catch (e) {
      toast.error(translateApiError(e, t));
    }
  };

  const handleDeleteFile = async (id: string) => {
    try {
      await deleteFile(id);
      await refresh();
      toast.success(t("drive.fileDeleted"));
    } catch (e) {
      toast.error(translateApiError(e, t));
    }
  };

  // First-run experience: at the workspace root with nothing in it yet, show
  // the onboarding action cards instead of empty folder/file sections. The
  // explicit files.length check keeps this correct if root-level file listing
  // is ever added (today files is [] at root). Gated on !loading so a
  // workspace that already has content never flashes the cards before its
  // first listing resolves.
  const showOnboarding =
    !loading &&
    !currentFolderID &&
    subfolders.length === 0 &&
    files.length === 0 &&
    !error;

  return (
    <div className="flex min-h-screen bg-bg text-fg">
      <FolderTree currentFolderID={currentFolderID} reloadKey={treeReloadKey} />
      <main className="flex min-w-0 flex-1 flex-col">
        {/* Top utility bar: location (breadcrumb) + search + navigation. */}
        <header className="sticky top-0 z-20 flex flex-wrap items-center gap-x-3 gap-y-2 border-b border-border bg-bg/85 px-4 py-3 backdrop-blur-md sm:px-6">
          <div className="min-w-0 flex-1">
            <Breadcrumb folder={folder} />
          </div>
          <div className="flex items-center gap-2">
            <div className="hidden lg:block">
              <SearchBar />
            </div>
            <button
              type="button"
              onClick={() => palette.open()}
              aria-label={t("search.commandPaletteAria", { defaultValue: "Search (Ctrl+K)" })}
              title="Ctrl+K"
              className="inline-flex h-9 items-center gap-2 rounded-lg border border-border bg-surface px-3 text-sm text-muted transition-colors hover:bg-surface-2 hover:text-fg"
            >
              <SearchIcon className="h-4 w-4" aria-hidden="true" />
              <kbd className="hidden rounded border border-border px-1.5 text-xs sm:inline">⌘K</kbd>
            </button>
            <ThemeToggle />
            {/* empty:hidden collapses this wrapper when EnableNotificationsButton
                renders null (the common granted/denied/unsupported case) so it
                doesn't leave an empty flex item that adds a stray gap. */}
            <div className="empty:hidden [&>button]:inline-flex [&>button]:h-9 [&>button]:items-center [&>button]:gap-2 [&>button]:rounded-lg [&>button]:border [&>button]:border-border [&>button]:bg-surface [&>button]:px-3 [&>button]:text-sm [&>button]:text-fg [&>button]:transition-colors [&>button]:hover:bg-surface-2 [&>button]:disabled:opacity-60">
              <EnableNotificationsButton />
            </div>
            <span className="mx-1 hidden h-6 w-px bg-border sm:block" aria-hidden="true" />
            <nav className="flex items-center gap-0.5">
              {currentFolderID ? (
                <NavPill
                  to={`/drive/folder/${currentFolderID}/documents`}
                  icon={<FileText className="h-4 w-4" aria-hidden="true" />}
                  title={t("drive.documentsTooltip")}
                >
                  {t("nav.documents")}
                </NavPill>
              ) : null}
              <NavPill
                to="/drive/privacy"
                icon={<Shield className="h-4 w-4" aria-hidden="true" />}
                title={t("drive.privacyTooltip")}
              >
                {t("nav.privacy")}
              </NavPill>
              {isAdmin ? (
                <>
                  <NavPill to="/admin" icon={<Settings className="h-4 w-4" aria-hidden="true" />}>
                    {t("nav.admin")}
                  </NavPill>
                  <NavPill
                    to="/billing"
                    icon={<CreditCard className="h-4 w-4" aria-hidden="true" />}
                  >
                    {t("nav.billing")}
                  </NavPill>
                </>
              ) : null}
            </nav>
            <Button
              variant="ghost"
              size="sm"
              onClick={() => {
                logout();
                nav("/login", { replace: true });
              }}
            >
              <LogOut className="h-4 w-4" aria-hidden="true" />
              <span className="sr-only sm:not-sr-only sm:inline">{t("auth.logout")}</span>
            </Button>
          </div>
        </header>

        <div className="flex-1 px-4 py-6 sm:px-6">
          {/* Action bar: page context + primary create/upload actions. */}
          <div className="mb-6 flex flex-wrap items-center justify-between gap-3">
            <div className="flex min-w-0 items-center gap-2">
              <h1 className="truncate text-xl font-semibold tracking-tight text-fg">
                {folder ? folder.name : t("nav.drive")}
              </h1>
            </div>
            <div className="flex flex-wrap items-center gap-2">
              <Button variant="secondary" size="md" onClick={handleCreateFolder}>
                <FolderPlus className="h-4 w-4" aria-hidden="true" />
                {t("drive.newFolder")}
              </Button>
              {isAdmin && isEnabled(Feature.ClientRooms) ? (
                <Button variant="secondary" size="md" onClick={() => setTemplateDialogOpen(true)}>
                  <LayoutTemplate className="h-4 w-4" aria-hidden="true" />
                  {t("drive.createFromTemplate")}
                </Button>
              ) : null}
              <UploadButton
                folderID={currentFolderID}
                onUploaded={() => refresh()}
                openRef={uploadOpenRef}
              />
            </div>
          </div>

          {error ? (
            <div
              role="alert"
              className="mb-4 rounded-card border border-danger/30 bg-danger/5 px-4 py-3 text-sm text-danger"
            >
              {error}
            </div>
          ) : null}

          {/*
            First listing in flight: show a skeleton instead of any content
            branch. Without this, the !loading guard on showOnboarding means
            the files section would briefly render FileList's "No files"
            empty state before the onboarding cards (or real rows) take its
            place — trading one flash for another. The skeleton also keeps
            layout stable (CLS 0) on slower connections.
          */}
          {loading && !error ? <FileListSkeleton /> : null}

          {!loading && showOnboarding ? (
            <OnboardingEmptyState
              onUpload={() => uploadOpenRef.current?.()}
              onCreateFolder={handleCreateFolder}
              onInvite={isAdmin ? () => nav("/admin") : undefined}
            />
          ) : null}

          {!loading && !showOnboarding && subfolders.length > 0 ? (
            <section className="mb-8">
              <h2 className="mb-3 text-xs font-semibold uppercase tracking-wide text-muted">
                {t("drive.folders")}
              </h2>
              <ul className="grid list-none grid-cols-1 gap-3 p-0 sm:grid-cols-2 xl:grid-cols-3">
                {subfolders.map((f) => (
                  <li
                    key={f.id}
                    className="group relative flex items-center gap-3 rounded-card border border-border bg-surface p-3.5 transition-all hover:border-brand/40 hover:bg-surface-2 hover:shadow-glow"
                  >
                    {/*
                      Stretched-link card: only the name is a real <Link>,
                      but its ::after overlay covers the whole card so the
                      entire surface navigates. The action buttons and the
                      encryption badge sit above the overlay (relative z-10) so
                      they stay clickable / hoverable. The badge is itself a
                      <Link to="/drive/privacy"> (a sibling of the name anchor,
                      not nested — valid HTML), so floating it above the overlay
                      both fires its privacy-mode tooltip on hover and keeps it a
                      live link to the privacy explainer; w-fit confines the
                      raised z-index to the badge so the rest of the card still
                      navigates to the folder. tabbable={false} keeps the keyboard
                      tab order clean across the N identical-destination badges.
                    */}
                    <span className="flex h-11 w-11 shrink-0 items-center justify-center rounded-xl bg-brand/10 text-brand transition-colors group-hover:bg-brand group-hover:text-brand-fg">
                      <FolderIcon className="h-5 w-5" aria-hidden="true" />
                    </span>
                    <div className="flex min-w-0 flex-1 flex-col gap-1">
                      <Link
                        to={`/drive/folder/${f.id}`}
                        className="truncate font-medium text-fg no-underline outline-none after:absolute after:inset-0 after:rounded-card after:content-[''] focus-visible:after:ring-2 focus-visible:after:ring-ring"
                      >
                        {f.name}
                      </Link>
                      <span className="relative z-10 w-fit">
                        <EncryptionBadge
                          mode={f.encryption_mode}
                          linkToHelp
                          tabbable={false}
                        />
                      </span>
                    </div>
                    <div className="relative z-10 flex shrink-0 items-center gap-0.5">
                      <button
                        type="button"
                        onClick={() => setShareTarget({ type: "folder", value: f })}
                        className="inline-flex h-8 w-8 items-center justify-center rounded-md text-muted transition-colors hover:bg-surface hover:text-fg focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
                        aria-label={t("common.share")}
                        title={t("common.share")}
                      >
                        <Share2 className="h-4 w-4" aria-hidden="true" />
                      </button>
                      <button
                        type="button"
                        onClick={() => handleRenameFolder(f)}
                        className="inline-flex h-8 w-8 items-center justify-center rounded-md text-muted transition-colors hover:bg-surface hover:text-fg focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
                        aria-label={t("common.rename")}
                        title={t("common.rename")}
                      >
                        <Pencil className="h-4 w-4" aria-hidden="true" />
                      </button>
                      <button
                        type="button"
                        onClick={() => handleDeleteFolder(f)}
                        className="inline-flex h-8 w-8 items-center justify-center rounded-md text-muted transition-colors hover:bg-danger/10 hover:text-danger focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
                        aria-label={t("common.delete")}
                        title={t("common.delete")}
                      >
                        <Trash2 className="h-4 w-4" aria-hidden="true" />
                      </button>
                    </div>
                  </li>
                ))}
              </ul>
            </section>
          ) : null}

          {!loading && !showOnboarding ? (
            <section>
              <h2 className="mb-3 text-xs font-semibold uppercase tracking-wide text-muted">
                {t("drive.files")}
              </h2>
              {selectedFiles.size > 0 ? (
                <div
                  role="toolbar"
                  aria-label={t("drive.bulkActionsAria")}
                  className="mb-3 flex flex-wrap items-center gap-2 rounded-card border border-brand/30 bg-brand/5 px-3 py-2"
                >
                  <span className="text-sm font-medium text-fg">
                    {t("drive.selectedCount", { count: selectedFiles.size })}
                  </span>
                  {/* ml-auto pushes the action group to the right so the bar reads
                      "N selected" on the left and the operations on the right. */}
                  <div className="ml-auto flex flex-wrap items-center gap-2">
                    <Button variant="ghost" size="sm" onClick={onBulkMove} loading={bulkBusy}>
                      <Move className="h-4 w-4" aria-hidden="true" />
                      {t("drive.move")}
                    </Button>
                    <Button variant="ghost" size="sm" onClick={onBulkCopy} loading={bulkBusy}>
                      <Copy className="h-4 w-4" aria-hidden="true" />
                      {t("drive.copy")}
                    </Button>
                    <Button variant="ghost" size="sm" onClick={onBulkDownload} loading={bulkBusy}>
                      <Download className="h-4 w-4" aria-hidden="true" />
                      {t("drive.downloadZip")}
                    </Button>
                    <Button variant="danger" size="sm" onClick={onBulkDelete} loading={bulkBusy}>
                      <Trash2 className="h-4 w-4" aria-hidden="true" />
                      {t("common.delete")}
                    </Button>
                    <Button
                      variant="ghost"
                      size="sm"
                      onClick={() => setSelectedFiles(new Set())}
                    >
                      <X className="h-4 w-4" aria-hidden="true" />
                      {t("drive.clear")}
                    </Button>
                  </div>
                </div>
              ) : null}
              <FileList
                files={files}
                onRename={handleRenameFile}
                onDelete={handleDeleteFile}
                onShare={(f) => setShareTarget({ type: "file", value: f })}
                onEdit={
                  onlyOfficeEnabled && isEnabled(Feature.OnlyOffice)
                    ? (f) => setEditorFile(f)
                    : undefined
                }
                selectedIDs={selectedFiles}
                onToggleSelect={toggleSelect}
              />
            </section>
          ) : null}
        </div>
      </main>
      {shareTarget ? (
        <ShareDialog resource={shareTarget} onClose={() => setShareTarget(null)} />
      ) : null}
      {editorFile ? (
        <div
          className="fixed inset-0 z-[1000] flex items-center justify-center bg-black/50 p-6"
          role="dialog"
          aria-modal="true"
          aria-label={t("onlyoffice.title")}
        >
          <div className="flex h-[min(860px,92vh)] w-[min(1200px,96vw)] flex-col overflow-hidden rounded-card bg-surface shadow-overlay">
            <OnlyOfficeEditor
              fileID={editorFile.id}
              mode="edit"
              onClose={() => {
                setEditorFile(null);
                // The save callback writes a new version server-side;
                // refresh so the listing reflects the latest size /
                // modified time once the editor closes.
                refresh();
              }}
            />
          </div>
        </div>
      ) : null}
      {createFolderOpen ? (
        <CreateFolderDialog
          parentID={currentFolderID}
          onClose={() => setCreateFolderOpen(false)}
          onCreated={() => {
            setCreateFolderOpen(false);
            invalidateFolderCache();
            refresh();
            // The sidebar lists only root folders, so it only needs to
            // refetch when the newly created folder is itself root-level.
            if (currentFolderID === null) setTreeReloadKey((k) => k + 1);
          }}
        />
      ) : null}
      {templateDialogOpen ? (
        <TemplateDialog
          onClose={() => setTemplateDialogOpen(false)}
          onCreated={(folderID) => {
            setTemplateDialogOpen(false);
            invalidateFolderCache();
            nav(`/drive/folder/${folderID}`);
          }}
        />
      ) : null}
    </div>
  );
}

// NavPill is a tokenised header navigation link with an icon. The
// accessible name is exactly the child text (the icon is aria-hidden) so
// keyboard / screen-reader users — and the e2e suite — match it by role.
function NavPill({
  to,
  icon,
  title,
  children,
}: {
  to: string;
  icon: React.ReactNode;
  title?: string;
  children: React.ReactNode;
}) {
  return (
    <Link
      to={to}
      title={title}
      className="inline-flex h-9 items-center gap-2 rounded-lg px-3 text-sm font-medium text-muted no-underline transition-colors hover:bg-surface-2 hover:text-fg focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
    >
      {icon}
      {/* sr-only (not `hidden`) below md so the link keeps an accessible
          name when the icon-only label is the only visible content. */}
      <span className="sr-only md:not-sr-only md:inline">{children}</span>
    </Link>
  );
}

function CreateFolderDialog({
  parentID,
  onClose,
  onCreated,
}: {
  parentID: string | null;
  onClose: () => void;
  onCreated: () => void;
}) {
  const { t } = useTranslation();
  const [name, setName] = useState("");
  // Reuses the EncryptionMode union exported by EncryptionBadge so the
  // dialog state, the badge prop, and the createFolder request body all
  // share a single source of truth. A typo like setMode("strict_z") is
  // a TS error here, not a silent server-side rejection.
  const [mode, setMode] = useState<EncryptionMode>("managed_encrypted");
  const [nameError, setNameError] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const submit = async () => {
    setError(null);
    // `required` on the Input only rejects the empty string; a whitespace-only
    // name passes native validation, so guard it explicitly and tell the user.
    if (!name.trim()) {
      setNameError(t("folder.nameRequired"));
      return;
    }
    setBusy(true);
    try {
      await createFolder({
        name: name.trim(),
        parent_folder_id: parentID,
        encryption_mode: mode,
      });
      onCreated();
    } catch (e) {
      setError(translateApiError(e, t));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Modal
      open
      onOpenChange={(o) => {
        if (!o) onClose();
      }}
      title={t("folder.createTitle")}
      size="lg"
      className="max-h-[88vh] overflow-y-auto"
      footer={
        <>
          <Button variant="ghost" onClick={onClose} type="button">
            {t("common.cancel")}
          </Button>
          <Button variant="primary" form="create-folder-form" type="submit" loading={busy}>
            {t("common.create")}
          </Button>
        </>
      }
    >
      <form
        id="create-folder-form"
        onSubmit={(e) => {
          e.preventDefault();
          submit();
        }}
        className="grid gap-4"
      >
        <Field label={t("common.name")} error={nameError ?? undefined}>
          {(p) => (
            <Input
              {...p}
              value={name}
              onChange={(e) => {
                setName(e.target.value);
                if (nameError) setNameError(null);
              }}
              placeholder={t("drive.folderNamePrompt")}
              autoFocus
              required
            />
          )}
        </Field>

        <fieldset className="grid gap-2">
          <legend className="mb-1 text-sm font-medium text-fg">{t("folder.privacyMode")}</legend>
          {/*
            Native radios are retained (not the RadioCard primitive) so the
            value is a real input[name="encmode"] — the privacy / documents
            / demo e2e suites check() these directly. The surrounding label
            is styled as a KChat selection card.
          */}
          <div role="radiogroup" aria-label={t("folder.privacyMode")} className="grid gap-2">
            <label
              className={cn(
                "flex cursor-pointer items-start gap-3 rounded-card border p-3 transition-colors",
                mode === "managed_encrypted"
                  ? "border-brand bg-brand/5"
                  : "border-border hover:bg-surface-2",
              )}
            >
              <input
                type="radio"
                name="encmode"
                value="managed_encrypted"
                checked={mode === "managed_encrypted"}
                onChange={() => setMode("managed_encrypted")}
                className="mt-0.5 h-4 w-4 shrink-0 accent-brand"
              />
              <span className="text-sm leading-relaxed text-fg">
                <Trans
                  i18nKey="folder.managedDescription"
                  components={{ strong: <strong />, em: <em /> }}
                />
              </span>
            </label>
            <label
              className={cn(
                "flex cursor-pointer items-start gap-3 rounded-card border p-3 transition-colors",
                mode === "strict_zk"
                  ? "border-brand bg-brand/5"
                  : "border-border hover:bg-surface-2",
              )}
            >
              <input
                type="radio"
                name="encmode"
                value="strict_zk"
                checked={mode === "strict_zk"}
                onChange={() => setMode("strict_zk")}
                className="mt-0.5 h-4 w-4 shrink-0 accent-brand"
              />
              <span className="text-sm leading-relaxed text-fg">
                <Trans i18nKey="folder.strictDescription" components={{ strong: <strong /> }} />
              </span>
            </label>
          </div>

          {/*
            Side-by-side comparison table so the user can see the exact
            trade-offs each mode entails before committing. The row order
            matches docs/PRODUCT.md §3.3 and PrivacyPage so the
            customer-facing story is consistent across every surface
            ("be honest about what 'ZK' means" — docs/BRAND.md).
          */}
          <table
            aria-label={t("folder.compareAria")}
            className="mt-2 w-full border-collapse text-xs"
          >
            <thead>
              <tr>
                <th className={cmpThCls} scope="col">
                  &nbsp;
                </th>
                <th className={cmpThCls} scope="col">
                  {t("folder.cmpHeaderConfidential")}
                </th>
                <th className={cmpThCls} scope="col">
                  {t("folder.cmpHeaderZk")}
                </th>
              </tr>
            </thead>
            <tbody>
              <tr>
                <th className={cmpRowThCls} scope="row">
                  {t("folder.cmpRowPreviews")}
                </th>
                <td className={cmpTdYesCls}>{t("common.yes")}</td>
                <td className={cmpTdNoCls}>{t("common.no")}</td>
              </tr>
              <tr>
                <th className={cmpRowThCls} scope="row">
                  {t("folder.cmpRowSearch")}
                </th>
                <td className={cmpTdYesCls}>{t("common.yes")}</td>
                <td className={cmpTdMutedCls}>{t("folder.cmpMetadataOnly")}</td>
              </tr>
              <tr>
                <th className={cmpRowThCls} scope="row">
                  {t("folder.cmpRowVirus")}
                </th>
                <td className={cmpTdYesCls}>{t("common.yes")}</td>
                <td className={cmpTdNoCls}>{t("common.no")}</td>
              </tr>
              <tr>
                <th className={cmpRowThCls} scope="row">
                  {t("folder.cmpRowRecovery")}
                </th>
                <td className={cmpTdYesCls}>{t("common.yes")}</td>
                <td className={cmpTdMutedCls}>{t("folder.cmpNoYouHoldKeys")}</td>
              </tr>
              <tr>
                <th className={cmpRowThCls} scope="row">
                  {t("folder.cmpRowServerRead")}
                </th>
                <td className={cmpTdNoCls}>{t("folder.cmpInMemoryOnly")}</td>
                <td className={cmpTdYesCls}>{t("folder.cmpNever")}</td>
              </tr>
            </tbody>
          </table>

          {mode === "strict_zk" ? (
            <div
              role="alert"
              className="mt-2 rounded-card border border-warning/30 bg-warning/10 px-3 py-2 text-xs text-warning"
            >
              {t("folder.strictWarning")}
            </div>
          ) : null}

          <p className="mt-2 text-xs text-muted">
            {/*
              Opens in a new tab so the in-flight folder name + mode radio
              selection in this dialog survive the click. A react-router
              <Link> would unmount FileBrowserPage and wipe the dialog
              state; a plain <a target="_blank"> leaves the dialog intact,
              and the new tab still hits the SPA's /drive/privacy route.
            */}
            <a
              href="/drive/privacy"
              target="_blank"
              rel="noopener noreferrer"
              className="font-medium text-brand hover:underline"
            >
              {t("folder.learnMoreArrow")}
            </a>
          </p>
        </fieldset>

        {error ? (
          <p role="alert" className="text-sm text-danger">
            {error}
          </p>
        ) : null}
      </form>
    </Modal>
  );
}

function TemplateDialog({
  onClose,
  onCreated,
}: {
  onClose: () => void;
  onCreated: (folderID: string) => void;
}) {
  const { t } = useTranslation();
  const prompt = usePrompt();
  const [templates, setTemplates] = useState<ClientRoomTemplate[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    (async () => {
      try {
        setTemplates(await fetchClientRoomTemplates());
      } catch (e) {
        setError(translateApiError(e, t));
      } finally {
        setLoading(false);
      }
    })();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const pick = async (name: string) => {
    const clientName = await prompt({
      title: t("drive.createFromTemplate"),
      label: t("drive.clientNamePrompt"),
      required: true,
      confirmLabel: t("common.create"),
    });
    if (!clientName || !clientName.trim()) return;
    try {
      const r = await createClientRoomFromTemplate(name, clientName.trim());
      onCreated(r.folder_id);
    } catch (e) {
      setError(translateApiError(e, t));
    }
  };

  return (
    <Modal
      open
      onOpenChange={(o) => {
        if (!o) onClose();
      }}
      title={t("drive.createFromTemplate")}
      size="lg"
      className="max-h-[88vh] overflow-y-auto"
    >
      {loading ? <p className="text-sm text-muted">{t("common.loading")}</p> : null}
      {error ? (
        <p role="alert" className="mb-3 text-sm text-danger">
          {error}
        </p>
      ) : null}
      <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
        {templates.map((tpl) => (
          <button
            key={tpl.name}
            type="button"
            onClick={() => pick(tpl.name)}
            className="rounded-card border border-border bg-surface p-4 text-left transition-all hover:border-brand/40 hover:bg-surface-2 hover:shadow-glow focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
          >
            <div className="flex items-center gap-2 font-semibold capitalize text-fg">
              <span className="flex h-8 w-8 shrink-0 items-center justify-center rounded-lg bg-brand/10 text-brand">
                <LayoutTemplate className="h-4 w-4" aria-hidden="true" />
              </span>
              {tpl.name}
            </div>
            <ul className="mt-3 grid gap-1 pl-1 text-xs text-muted">
              {tpl.sub_folders.map((s) => (
                <li key={s} className="flex items-center gap-1.5">
                  <FolderIcon className="h-3 w-3 shrink-0" aria-hidden="true" />
                  {s}
                </li>
              ))}
            </ul>
          </button>
        ))}
      </div>
    </Modal>
  );
}

function Breadcrumb({ folder }: { folder: Folder | null }) {
  const { t } = useTranslation();
  // Split the materialized path into clickable segments. The API stores
  // paths like "/Engineering/Backend/" so trimming empty parts keeps the
  // display clean.
  //
  // EncryptionBadge sits at the end of the breadcrumb so users always
  // know what privacy mode the current folder is in — not just when
  // they scan a parent's subfolder list. This is the same trade-off
  // matrix as docs/PRODUCT.md "Per-folder privacy modes" (managed =
  // server-readable, strict = server-blind), surfaced at the point
  // of action.
  const parts = folder?.path?.split("/").filter(Boolean) ?? [];
  return (
    <nav
      aria-label={t("nav.breadcrumb")}
      className="flex min-w-0 items-center gap-1.5 text-sm"
    >
      <Link
        to="/drive"
        className="shrink-0 font-medium text-muted no-underline transition-colors hover:text-fg"
      >
        {t("drive.rootBreadcrumb")}
      </Link>
      {parts.map((p, i) => (
        <span key={i} className="flex min-w-0 items-center gap-1.5">
          <span className="shrink-0 text-muted/60" aria-hidden="true">
            /
          </span>
          <span
            className={cn(
              "truncate",
              i === parts.length - 1 ? "font-medium text-fg" : "text-muted",
            )}
          >
            {p}
          </span>
        </span>
      ))}
      {folder ? (
        <span className="ml-1 shrink-0">
          <EncryptionBadge mode={folder.encryption_mode} size="header" />
        </span>
      ) : null}
    </nav>
  );
}

// Privacy-mode comparison-table classes used by CreateFolderDialog. The
// table mirrors the docs/PRODUCT.md §3.3 row order so a customer who reads
// the docs and then opens the dialog sees the same trade-off matrix; "Yes"
// tones are success green to match the confidential badge and "No" tones
// are danger red, keeping EncryptionBadge the single source of colour
// vocabulary.
const cmpThCls = "border-b border-border px-2 py-1 text-left font-medium text-fg";
const cmpRowThCls = "border-b border-border/60 px-2 py-1 text-left font-normal text-muted";
const cmpTdYesCls = "border-b border-border/60 bg-success/10 px-2 py-1 text-center text-success";
const cmpTdNoCls = "border-b border-border/60 bg-danger/10 px-2 py-1 text-center text-danger";
// Neutral tier for nuanced cells that are a trade-off, not a hard negative —
// e.g. zero-knowledge "Metadata only" search and "No (you hold the keys)"
// recovery, which are deliberate properties of the mode rather than red-flag
// deficiencies. Keeping them muted avoids framing a privacy feature as a warning.
const cmpTdMutedCls = "border-b border-border/60 bg-surface-2 px-2 py-1 text-center text-muted";

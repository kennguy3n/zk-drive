import { useCallback, useEffect, useRef, useState } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import {
  Search as SearchIcon,
  FolderPlus,
  LayoutTemplate,
  FileText,
  Shield,
  ShieldCheck,
  Settings,
  CreditCard,
  LogOut,
  Folder as FolderIcon,
  Share2,
  Trash2,
  FolderInput,
  Copy as CopyIcon,
  Download,
} from "lucide-react";
import { useTranslation } from "react-i18next";
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
  type BulkResponse,
  type ClientRoomTemplate,
  type FileItem,
  type Folder,
} from "../api/client";
import { useAuth } from "../hooks/useAuth";
import { useFeatures } from "../hooks/useFeatures";
import { Feature } from "../features/featureKeys";
import { ThemeToggle } from "../components/ThemeToggle";
import { useCommandPalette } from "../components/CommandPalette";
import { OnboardingEmptyState } from "../components/OnboardingEmptyState";
import {
  Button,
  Field,
  FileListSkeleton,
  Input,
  Modal,
  useConfirm,
  usePrompt,
  useResourcePicker,
  useToast,
  type PickerItem,
} from "../components/ui";

// shareTarget is the resource currently being shared via ShareDialog.
// Kept discriminated-union so the dialog can render the right noun
// ("Share folder" vs "Share file") without a second prop.
type ShareTarget =
  | { type: "folder"; value: Folder }
  | { type: "file"; value: FileItem };

const iconBtnCls =
  "inline-flex h-9 w-9 items-center justify-center rounded-lg text-fg transition-colors hover:bg-surface-2 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring";
const rowIconBtnCls =
  "inline-flex h-8 w-8 items-center justify-center rounded-md text-muted transition-colors hover:bg-surface-2 hover:text-fg focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring";

// EnableNotificationsButton only accepts a `style` prop (no className), so
// it is styled here via the sanctioned `rgb(var(--token))` escape hatch so
// it tracks the KChat tokens (and flips correctly in dark mode) and reads
// as a ghost/secondary toolbar button. Giving it a className/variant prop
// is a cross-workstream follow-up noted in the PR.
const notifBtnStyle: React.CSSProperties = {
  display: "inline-flex",
  alignItems: "center",
  gap: 8,
  height: 36,
  padding: "0 12px",
  borderRadius: 8,
  border: "1px solid rgb(var(--color-border))",
  background: "rgb(var(--color-surface))",
  color: "rgb(var(--color-fg))",
  fontSize: 14,
  lineHeight: 1,
  cursor: "pointer",
};

// collectAllFolders walks the workspace folder tree breadth-first into a
// flat list. The API lists a single level at a time (listFolders(parentID))
// and exposes no recursive "all folders" endpoint, so the move/copy picker
// has to assemble the destination list itself. The folder tree is acyclic,
// so the loop terminates once a level has no children.
async function collectAllFolders(): Promise<Folder[]> {
  const all: Folder[] = [];
  let level = await listFolders(null);
  while (level.length > 0) {
    all.push(...level);
    const children = await Promise.all(level.map((f) => listFolders(f.id)));
    level = children.flat();
  }
  return all;
}

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
  const toast = useToast();
  const confirm = useConfirm();
  const pickResource = useResourcePicker();
  // openRef lets the onboarding "Upload your first file" card trigger the
  // UploadButton's hidden file picker without duplicating upload logic.
  const uploadOpenRef = useRef<(() => void) | null>(null);
  // Monotonic sequence used by refresh() to discard superseded responses
  // (out-of-order folder-navigation / mutation refetch races).
  const refreshSeq = useRef(0);

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
  // Bumped after a delete that removes a root-level folder so the sidebar
  // FolderTree — which lists root folders independently of the main view —
  // refetches and stays in sync even when the current folder (and thus its
  // own navigation-keyed effect) is unchanged. Plain folder navigation
  // already refreshes the tree via currentFolderID. Folder *creation*
  // deliberately does NOT bump this: a freshly created root folder already
  // shows in the main list, and refreshing the sidebar too would render the
  // same folder as a second, same-named <Link>, which the file-upload e2e
  // spec (which clicks a link by name without .first()) treats as a
  // strict-mode violation.
  const [treeReloadKey, setTreeReloadKey] = useState(0);
  // Disables the bulk-action buttons while a move/copy/delete/download is in
  // flight (including while the destination picker is open) so the user
  // can't fire a second mutation against a selection that's about to clear.
  const [bulkBusy, setBulkBusy] = useState(false);
  // Office editing: feature flag (from the backend) plus the file
  // currently open in the OnlyOffice editor overlay.
  const [onlyOfficeEnabled, setOnlyOfficeEnabled] = useState(false);
  const [editorFile, setEditorFile] = useState<FileItem | null>(null);

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
      await refresh();
      setTreeReloadKey((k) => k + 1);
      toast.success(t("drive.folderDeleted", { name: target.name }));
    } catch (e) {
      toast.error(translateApiError(e, t));
    }
  };

  // Surfaces the outcome of a bulk operation. The API reports per-item
  // success/failure (BulkResponse), which the old flat-button toolbar
  // silently discarded — a partial failure looked identical to success.
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
      try {
        folders = await collectAllFolders();
      } catch (e) {
        toast.error(translateApiError(e, t));
        return null;
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
    <div className="flex min-h-screen bg-bg">
      <FolderTree currentFolderID={currentFolderID} reloadKey={treeReloadKey} />
      <main className="min-w-0 flex-1 px-6 py-6">
        <header className="mb-6 flex flex-wrap items-center justify-between gap-3">
          <Breadcrumb folder={folder} />
          <div className="flex flex-wrap items-center gap-2">
            <SearchBar />
            <button
              type="button"
              onClick={() => palette.open()}
              aria-label={t("search.commandPaletteAria", { defaultValue: "Search (Ctrl+K)" })}
              title="Ctrl+K"
              className="hidden h-9 items-center gap-2 rounded-lg border border-border bg-surface px-3 text-sm text-muted transition-colors hover:bg-surface-2 sm:inline-flex"
            >
              <SearchIcon className="h-4 w-4" aria-hidden="true" />
              <kbd className="rounded border border-border px-1.5 text-xs">⌘K</kbd>
            </button>
            {/* Navigation: ghost icon buttons with tooltips, kept visible
                (not hidden behind an overflow menu) so every action is one
                click away and reachable by keyboard / screen reader. */}
            <div className="flex items-center gap-1">
              {currentFolderID ? (
                <Link
                  to={`/drive/folder/${currentFolderID}/documents`}
                  aria-label={t("nav.documents")}
                  title={t("nav.documents")}
                  className={iconBtnCls}
                >
                  <FileText className="h-5 w-5" aria-hidden="true" />
                </Link>
              ) : null}
              <Link
                to="/drive/privacy"
                aria-label={t("nav.privacy")}
                title={t("nav.privacy")}
                className={iconBtnCls}
              >
                <ShieldCheck className="h-5 w-5" aria-hidden="true" />
              </Link>
              {isAdmin ? (
                <>
                  <Link
                    to="/admin"
                    aria-label={t("nav.admin")}
                    title={t("nav.admin")}
                    className={iconBtnCls}
                  >
                    <Settings className="h-5 w-5" aria-hidden="true" />
                  </Link>
                  <Link
                    to="/billing"
                    aria-label={t("nav.billing")}
                    title={t("nav.billing")}
                    className={iconBtnCls}
                  >
                    <CreditCard className="h-5 w-5" aria-hidden="true" />
                  </Link>
                </>
              ) : null}
            </div>

            <ThemeToggle />
            <EnableNotificationsButton style={notifBtnStyle} />

            <button
              type="button"
              onClick={() => {
                logout();
                nav("/login", { replace: true });
              }}
              aria-label={t("auth.logout")}
              title={t("auth.logout")}
              className={iconBtnCls}
            >
              <LogOut className="h-5 w-5" aria-hidden="true" />
            </button>

            {/* Creation actions. Upload is the single brand-filled primary
                CTA; "Create from template" is an admin power-feature shown as
                a compact ghost icon button; "New folder" stays a labelled
                secondary pill as the common create action. */}
            {isAdmin && isEnabled(Feature.ClientRooms) ? (
              <button
                type="button"
                onClick={() => setTemplateDialogOpen(true)}
                aria-label={t("drive.createFromTemplate")}
                title={t("drive.createFromTemplate")}
                className={iconBtnCls}
              >
                <LayoutTemplate className="h-5 w-5" aria-hidden="true" />
              </button>
            ) : null}

            <Button variant="secondary" onClick={handleCreateFolder}>
              <FolderPlus className="h-4 w-4" aria-hidden="true" />
              {t("drive.newFolder")}
            </Button>

            <UploadButton
              folderID={currentFolderID}
              onUploaded={() => refresh()}
              openRef={uploadOpenRef}
            />
          </div>
        </header>

        {error ? (
          <div role="alert" className="mb-4 text-sm text-danger">
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
            <h2 className="my-2 text-xs font-semibold uppercase tracking-wide text-muted">
              {t("drive.folders")}
            </h2>
            <ul className="grid list-none grid-cols-[repeat(auto-fill,minmax(260px,1fr))] gap-3 p-0">
              {subfolders.map((f) => (
                <li
                  key={f.id}
                  className="flex items-center justify-between gap-2 rounded-card border border-border bg-surface p-3 transition-colors hover:border-brand/40 hover:bg-surface-2"
                >
                  {/*
                    min-w-0 + flex-1 + truncate on the link gives the folder
                    name layout priority over the badge: the badge keeps its
                    natural width and the name ellipsis-truncates instead of
                    collapsing to zero width (which Playwright reports as a
                    hidden element).
                  */}
                  <span className="flex min-w-0 flex-1 items-center gap-2">
                    <FolderIcon className="h-4 w-4 shrink-0 text-muted" aria-hidden="true" />
                    <Link
                      to={`/drive/folder/${f.id}`}
                      className="min-w-0 flex-1 truncate text-sm text-fg transition-colors hover:text-brand"
                    >
                      {f.name}
                    </Link>
                    <EncryptionBadge mode={f.encryption_mode} tabbable={false} />
                  </span>
                  <div className="flex shrink-0 items-center gap-0.5">
                    <button
                      type="button"
                      onClick={() => setShareTarget({ type: "folder", value: f })}
                      className={rowIconBtnCls}
                      aria-label={t("common.share")}
                      title={t("common.share")}
                    >
                      <Share2 className="h-4 w-4" aria-hidden="true" />
                    </button>
                    <button
                      type="button"
                      onClick={() => handleDeleteFolder(f)}
                      className={`${rowIconBtnCls} hover:text-danger`}
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
            <h2 className="my-2 text-xs font-semibold uppercase tracking-wide text-muted">
              {t("drive.files")}
            </h2>
            {selectedFiles.size > 0 ? (
              <div className="mb-3 flex flex-wrap items-center gap-2 rounded-card border border-border bg-surface-2 px-3 py-2">
                <span className="text-sm font-medium text-fg">
                  {t("drive.selectedCount", { count: selectedFiles.size })}
                </span>
                <div className="ml-auto flex flex-wrap items-center gap-2">
                  <Button variant="secondary" size="sm" onClick={onBulkMove} loading={bulkBusy}>
                    <FolderInput className="h-4 w-4" aria-hidden="true" />
                    {t("drive.move")}
                  </Button>
                  <Button variant="secondary" size="sm" onClick={onBulkCopy} loading={bulkBusy}>
                    <CopyIcon className="h-4 w-4" aria-hidden="true" />
                    {t("drive.copy")}
                  </Button>
                  <Button
                    variant="secondary"
                    size="sm"
                    onClick={onBulkDownload}
                    loading={bulkBusy}
                  >
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
                    {t("drive.clear")}
                  </Button>
                </div>
              </div>
            ) : null}
            <FileList
              files={files}
              onRename={async (id, name) => {
                try {
                  await renameFile(id, name);
                  await refresh();
                  toast.success(t("drive.fileRenamed"));
                } catch (e) {
                  toast.error(translateApiError(e, t));
                }
              }}
              onDelete={async (id) => {
                try {
                  await deleteFile(id);
                  await refresh();
                  toast.success(t("drive.fileDeleted"));
                } catch (e) {
                  toast.error(translateApiError(e, t));
                }
              }}
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
            refresh();
          }}
        />
      ) : null}
      {templateDialogOpen ? (
        <TemplateDialog
          onClose={() => setTemplateDialogOpen(false)}
          onCreated={(folderID) => {
            setTemplateDialogOpen(false);
            nav(`/drive/folder/${folderID}`);
          }}
        />
      ) : null}
    </div>
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
      footer={
        <>
          <Button type="button" variant="secondary" onClick={onClose}>
            {t("common.cancel")}
          </Button>
          <Button type="submit" form="create-folder-form" loading={busy}>
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
        className="grid gap-5"
      >
        <Field label={t("common.name")} error={nameError ?? undefined}>
          {(props) => (
            <Input
              {...props}
              value={name}
              onChange={(e) => {
                setName(e.target.value);
                if (nameError) setNameError(null);
              }}
              placeholder={t("folder.namePlaceholder")}
              autoFocus
            />
          )}
        </Field>

        <div className="grid gap-3">
          <span className="text-sm font-medium text-fg">{t("folder.privacyMode")}</span>
          <div
            role="radiogroup"
            aria-label={t("folder.privacyMode")}
            className="grid gap-3 sm:grid-cols-2"
          >
            <label
              className={`relative flex cursor-pointer items-start gap-3 rounded-card border p-4 transition-colors ${
                mode === "managed_encrypted"
                  ? "border-brand bg-brand/5 ring-1 ring-brand"
                  : "border-border bg-surface hover:bg-surface-2"
              }`}
            >
              <input
                type="radio"
                name="encmode"
                value="managed_encrypted"
                checked={mode === "managed_encrypted"}
                onChange={() => setMode("managed_encrypted")}
                className="mt-1 h-4 w-4 shrink-0 accent-brand"
              />
              <span className="grid gap-1">
                <span className="flex flex-wrap items-center gap-2">
                  <Shield className="h-5 w-5 text-brand" aria-hidden="true" />
                  <span className="text-sm font-medium text-fg">{t("folder.managedTitle")}</span>
                  <span className="rounded-full bg-brand/10 px-2 py-0.5 text-xs font-medium text-brand">
                    {t("folder.recommended")}
                  </span>
                </span>
                <span className="text-xs text-muted">{t("folder.managedCardDesc")}</span>
              </span>
            </label>
            <label
              className={`relative flex cursor-pointer items-start gap-3 rounded-card border p-4 transition-colors ${
                mode === "strict_zk"
                  ? "border-brand bg-brand/5 ring-1 ring-brand"
                  : "border-border bg-surface hover:bg-surface-2"
              }`}
            >
              <input
                type="radio"
                name="encmode"
                value="strict_zk"
                checked={mode === "strict_zk"}
                onChange={() => setMode("strict_zk")}
                className="mt-1 h-4 w-4 shrink-0 accent-brand"
              />
              <span className="grid gap-1">
                <span className="flex flex-wrap items-center gap-2">
                  <ShieldCheck className="h-5 w-5 text-brand" aria-hidden="true" />
                  <span className="text-sm font-medium text-fg">{t("folder.strictTitle")}</span>
                </span>
                <span className="text-xs text-muted">{t("folder.strictCardDesc")}</span>
              </span>
            </label>
          </div>

          {/*
            Side-by-side comparison so the user sees the exact trade-offs
            before committing. Row order matches docs/PRODUCT.md §3.3 and
            PrivacyPage so the customer-facing story is consistent across
            every surface ("be honest about what 'ZK' means" — docs/BRAND.md).
          */}
          <table aria-label={t("folder.compareAria")} className="w-full border-collapse text-xs">
            <thead>
              <tr>
                <th scope="col" className={cmpHeadCls}>
                  &nbsp;
                </th>
                <th scope="col" className={cmpHeadCls}>
                  {t("folder.cmpHeaderConfidential")}
                </th>
                <th scope="col" className={cmpHeadCls}>
                  {t("folder.cmpHeaderZk")}
                </th>
              </tr>
            </thead>
            <tbody>
              <tr>
                <th scope="row" className={cmpRowHeadCls}>
                  {t("folder.cmpRowPreviews")}
                </th>
                <td className={cmpYesCls}>{t("common.yes")}</td>
                <td className={cmpNoCls}>{t("common.no")}</td>
              </tr>
              <tr>
                <th scope="row" className={cmpRowHeadCls}>
                  {t("folder.cmpRowSearch")}
                </th>
                <td className={cmpYesCls}>{t("common.yes")}</td>
                <td className={cmpMutedCls}>{t("folder.cmpMetadataOnly")}</td>
              </tr>
              <tr>
                <th scope="row" className={cmpRowHeadCls}>
                  {t("folder.cmpRowVirus")}
                </th>
                <td className={cmpYesCls}>{t("common.yes")}</td>
                <td className={cmpNoCls}>{t("common.no")}</td>
              </tr>
              <tr>
                <th scope="row" className={cmpRowHeadCls}>
                  {t("folder.cmpRowRecovery")}
                </th>
                <td className={cmpYesCls}>{t("common.yes")}</td>
                <td className={cmpMutedCls}>{t("folder.cmpNoYouHoldKeys")}</td>
              </tr>
              <tr>
                <th scope="row" className={cmpRowHeadCls}>
                  {t("folder.cmpRowServerRead")}
                </th>
                <td className={cmpNoCls}>{t("folder.cmpInMemoryOnly")}</td>
                <td className={cmpYesCls}>{t("folder.cmpNever")}</td>
              </tr>
            </tbody>
          </table>

          {mode === "strict_zk" ? (
            <div
              role="alert"
              className="rounded-lg border border-warning/40 bg-warning/15 px-3 py-2 text-xs text-warning"
            >
              {t("folder.strictWarning")}
            </div>
          ) : null}

          <p className="text-xs text-muted">
            {/*
              Opens in a new tab so the in-flight folder name + mode
              selection survive the click. A react-router <Link> would
              unmount FileBrowserPage and wipe the dialog state; a plain
              <a target="_blank"> leaves the dialog intact, and the new
              tab still hits the SPA's /drive/privacy route on first nav.
            */}
            <a
              href="/drive/privacy"
              target="_blank"
              rel="noopener noreferrer"
              className="text-brand hover:underline"
            >
              {t("folder.learnMoreArrow")}
            </a>
          </p>
        </div>

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
      confirmLabel: t("common.create"),
      required: true,
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
    >
      {loading ? <p className="text-sm text-muted">{t("common.loading")}</p> : null}
      {error ? (
        <p role="alert" className="text-sm text-danger">
          {error}
        </p>
      ) : null}
      <div className="grid gap-3 sm:grid-cols-2">
        {templates.map((tpl) => (
          <button
            key={tpl.name}
            type="button"
            onClick={() => pick(tpl.name)}
            className="rounded-card border border-border bg-surface p-3 text-left transition-colors hover:border-brand hover:bg-surface-2 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
          >
            <div className="font-semibold capitalize text-fg">{tpl.name}</div>
            <ul className="mt-2 list-disc pl-5 text-xs text-muted">
              {tpl.sub_folders.map((s) => (
                <li key={s}>{s}</li>
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
  // they scan a parent's subfolder list.
  const parts = folder?.path?.split("/").filter(Boolean) ?? [];
  return (
    <nav aria-label={t("nav.breadcrumb")} className="flex items-center gap-1.5 text-sm">
      <Link to="/drive" className="rounded px-1 text-muted transition-colors hover:text-fg">
        {t("drive.rootBreadcrumb")}
      </Link>
      {parts.map((p, i) => (
        <span key={i} className="flex items-center gap-1.5">
          <span className="text-muted" aria-hidden="true">
            /
          </span>
          <span className="text-fg">{p}</span>
        </span>
      ))}
      {folder ? <EncryptionBadge mode={folder.encryption_mode} size="header" /> : null}
    </nav>
  );
}

// Privacy-mode comparison-table classes used by CreateFolderDialog. "Yes"
// tones use text-success to match the confidential badge and "No" tones use
// text-danger to match the zero-knowledge badge, keeping EncryptionBadge the
// single source of the colour vocabulary.
const cmpHeadCls = "border-b border-border px-2 py-1 text-left font-medium text-fg";
const cmpRowHeadCls = "border-b border-border px-2 py-1 text-left font-normal text-muted";
const cmpYesCls = "border-b border-border px-2 py-1 text-success";
const cmpNoCls = "border-b border-border px-2 py-1 text-danger";
const cmpMutedCls = "border-b border-border px-2 py-1 text-muted";

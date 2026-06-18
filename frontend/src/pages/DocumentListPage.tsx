// DocumentListPage — lists the documents inside a folder and lets
// the user create new ones with a capability-gated collab-mode
// selector. This page is the entrypoint to the collab editor:
// each row links to /drive/document/:id and renders the doc's
// (encryption mode, collab mode) badges so the user knows what
// experience they'll get before they click in.
//
// The page sits as a tab alongside the files table on
// FileBrowserPage; selecting "Documents" in the folder header
// navigates to this page with the same folder id in the path.

import { useCallback, useEffect, useId, useState } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import { useTranslation } from "react-i18next";
import type { TFunction } from "i18next";
import { ArrowLeft, FileText, Plus, Trash2 } from "lucide-react";
import {
  createDocument,
  deleteDocument,
  getFolder,
  listFolderDocuments,
  type CollabMode,
  type Document,
  type Folder,
} from "../api/client";
import { translateApiError } from "../api/errors";
import { resolveAllowedCollabModes } from "../collab/capability";
import EncryptionBadge from "../components/EncryptionBadge";
import CollabModeSelector from "../components/CollabModeSelector";
import {
  AppShell,
  Badge,
  Button,
  EmptyState,
  Field,
  FileListSkeleton,
  Input,
  Modal,
  PageHeader,
  Table,
  TBody,
  Td,
  Th,
  THead,
  Tr,
  useConfirm,
  useToast,
} from "../components/ui";

type BadgeTone = "neutral" | "brand" | "success" | "danger" | "warning";

export default function DocumentListPage() {
  const { folderId } = useParams<{ folderId: string }>();
  const nav = useNavigate();
  const { t } = useTranslation();
  const confirm = useConfirm();
  const toast = useToast();

  const [folder, setFolder] = useState<Folder | null>(null);
  const [docs, setDocs] = useState<Document[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);
  const [createOpen, setCreateOpen] = useState(false);

  // refresh owns the data fetch. The useEffect below provides a per-effect
  // cancellation token that flips on cleanup, so a stale folder-A response
  // can't clobber folder-B's data when the user navigates between folders
  // quickly. Matches the DocumentEditorPage `cancelled` pattern.
  const refresh = useCallback(
    async (isCancelled?: () => boolean) => {
      if (!folderId) return;
      setError(null);
      try {
        const [{ folder: f }, list] = await Promise.all([
          getFolder(folderId),
          listFolderDocuments(folderId),
        ]);
        if (isCancelled?.()) return;
        setFolder(f);
        setDocs(list);
      } catch (e) {
        if (isCancelled?.()) return;
        setError(translateApiError(e, t));
      }
    },
    [folderId, t],
  );

  useEffect(() => {
    let cancelled = false;
    setLoading(true);
    void refresh(() => cancelled).finally(() => {
      if (!cancelled) setLoading(false);
    });
    return () => {
      cancelled = true;
    };
  }, [refresh]);

  const onDelete = useCallback(
    async (d: Document) => {
      const ok = await confirm({
        title: t("docs.deleteTitle"),
        description: t("docs.deleteConfirm", { name: d.name }),
        confirmLabel: t("common.delete"),
        cancelLabel: t("common.cancel"),
        tone: "danger",
      });
      if (!ok) return;
      try {
        await deleteDocument(d.id);
        setDocs((prev) => prev.filter((x) => x.id !== d.id));
        toast.success(t("docs.deleted", { name: d.name }));
      } catch (e) {
        const msg = translateApiError(e, t);
        setError(msg);
        toast.error(msg);
      }
    },
    [confirm, t, toast],
  );

  if (!folderId) {
    return (
      <AppShell maxWidth="lg">
        <EmptyState
          icon={<FileText className="h-6 w-6" aria-hidden="true" />}
          title={t("docs.missingFolderId")}
          action={
            <Button onClick={() => nav("/drive")}>{t("admin.backToDrive")}</Button>
          }
        />
      </AppShell>
    );
  }

  return (
    <AppShell
      maxWidth="lg"
      nav={
        <Link
          to={`/drive/folder/${folderId}`}
          aria-label={t("docs.backToFolderAria")}
          className="inline-flex items-center gap-1.5 rounded-full px-3 py-1.5 text-sm font-medium text-muted transition-colors hover:bg-surface-2 hover:text-fg focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
        >
          <ArrowLeft className="h-4 w-4" aria-hidden="true" />
          {t("docs.backToFolder")}
        </Link>
      }
    >
      <PageHeader
        eyebrow={
          <span className="inline-flex items-center gap-2">
            {t("docs.eyebrow")}
            {folder && <EncryptionBadge mode={folder.encryption_mode} size="row" />}
          </span>
        }
        title={folder ? folder.name : t("docs.folderFallback")}
        description={t("docs.pageDescription")}
        actions={
          <Button onClick={() => setCreateOpen(true)} disabled={!folder}>
            <Plus className="h-4 w-4" aria-hidden="true" />
            {t("docs.newDocument")}
          </Button>
        }
      />

      {error && (
        <div
          role="alert"
          className="mb-4 rounded-card border border-danger/30 bg-danger/10 px-4 py-3 text-sm text-danger"
        >
          {error}
        </div>
      )}

      {loading ? (
        <div className="rounded-card border border-border bg-surface">
          <FileListSkeleton rows={6} />
        </div>
      ) : docs.length === 0 ? (
        <EmptyState
          icon={<FileText className="h-6 w-6" aria-hidden="true" />}
          title={t("docs.emptyTitle")}
          description={t("docs.emptyDescription")}
          action={
            <Button onClick={() => setCreateOpen(true)} disabled={!folder}>
              <Plus className="h-4 w-4" aria-hidden="true" />
              {t("docs.newDocument")}
            </Button>
          }
        />
      ) : (
        <Table>
          <THead>
            <Tr>
              <Th>{t("common.name")}</Th>
              <Th>{t("docs.mode")}</Th>
              <Th>{t("docs.updated")}</Th>
              <Th className="text-right">
                <span className="sr-only">{t("common.actions")}</span>
              </Th>
            </Tr>
          </THead>
          <TBody>
            {docs.map((d) => (
              <Tr key={d.id}>
                <Td>
                  <Link
                    to={`/drive/document/${d.id}`}
                    className="inline-flex items-center gap-2 font-medium text-fg transition-colors hover:text-brand focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2 focus-visible:ring-offset-bg"
                  >
                    <FileText className="h-4 w-4 shrink-0 text-muted" aria-hidden="true" />
                    {d.name || t("docs.untitled")}
                  </Link>
                </Td>
                <Td>
                  <Badge tone={collabModeTone(d.collab_mode)}>
                    {collabModeLabel(t, d.collab_mode)}
                  </Badge>
                </Td>
                <Td className="text-muted">
                  {new Date(d.updated_at).toLocaleString()}
                </Td>
                <Td className="text-right">
                  <Button
                    variant="ghost"
                    size="sm"
                    onClick={() => onDelete(d)}
                    aria-label={t("docs.deleteAria", { name: d.name })}
                    className="text-muted hover:bg-danger/10 hover:text-danger"
                  >
                    <Trash2 className="h-4 w-4" aria-hidden="true" />
                  </Button>
                </Td>
              </Tr>
            ))}
          </TBody>
        </Table>
      )}

      {folder && (
        <CreateDocumentDialog
          open={createOpen}
          folder={folder}
          onOpenChange={setCreateOpen}
          onCreated={(d) => {
            setCreateOpen(false);
            nav(`/drive/document/${d.id}`);
          }}
        />
      )}
    </AppShell>
  );
}

interface CreateDocumentDialogProps {
  open: boolean;
  folder: Folder;
  onOpenChange: (open: boolean) => void;
  onCreated: (doc: Document) => void;
}

function CreateDocumentDialog({
  open,
  folder,
  onOpenChange,
  onCreated,
}: CreateDocumentDialogProps) {
  const { t } = useTranslation();
  const formId = useId();
  // The folder's encryption_mode bounds the allowed collab modes.
  // We compute the allowed list client-side from the SAME resolver
  // the server uses (mirrored into src/collab/capability.ts) so the
  // dialog can disable invalid options before the user submits.
  const allowed = resolveAllowedCollabModes(folder.encryption_mode);
  // Default to the richest allowed mode — matches the server's
  // DefaultCollabModeFor and the Google-Docs-style expectation.
  const [mode, setMode] = useState<CollabMode>(
    allowed[allowed.length - 1] ?? "markdown",
  );
  const [name, setName] = useState(t("docs.untitled"));
  const [submitting, setSubmitting] = useState(false);
  const [dialogError, setDialogError] = useState<string | null>(null);

  // Reset the form every time the dialog opens. The parent mounts this
  // dialog persistently (so the Modal can animate its exit), which means
  // name / mode / error would otherwise survive a close→reopen and show
  // the previous attempt's values. Keying the reset on `open` gives each
  // open a clean slate without remounting (remounting would cut the exit
  // animation). `allowed` is recomputed each render but is stable for a
  // given folder.encryption_mode, so depending only on `open` is correct.
  useEffect(() => {
    if (!open) return;
    setName(t("docs.untitled"));
    setMode(allowed[allowed.length - 1] ?? "markdown");
    setDialogError(null);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open]);

  const submit = useCallback(
    async (e: React.FormEvent) => {
      e.preventDefault();
      const trimmed = name.trim();
      if (!trimmed) {
        setDialogError(t("docs.nameRequired"));
        return;
      }
      setSubmitting(true);
      setDialogError(null);
      try {
        const doc = await createDocument({
          folder_id: folder.id,
          name: trimmed,
          collab_mode: mode,
        });
        onCreated(doc);
      } catch (e2) {
        setDialogError(translateApiError(e2, t));
      } finally {
        setSubmitting(false);
      }
    },
    [folder.id, mode, name, onCreated, t],
  );

  return (
    <Modal
      open={open}
      onOpenChange={(next) => {
        if (submitting) return;
        onOpenChange(next);
      }}
      title={t("docs.newInFolder", { name: folder.name })}
      size="lg"
      footer={
        <>
          <Button
            variant="secondary"
            onClick={() => onOpenChange(false)}
            disabled={submitting}
          >
            {t("common.cancel")}
          </Button>
          <Button type="submit" form={formId} loading={submitting}>
            {t("common.create")}
          </Button>
        </>
      }
    >
      <form id={formId} onSubmit={submit} className="flex flex-col gap-4">
        <Field label={t("common.name")} error={dialogError ?? undefined}>
          {(props) => (
            <Input
              {...props}
              autoFocus
              value={name}
              onChange={(e) => setName(e.target.value)}
              disabled={submitting}
              placeholder={t("docs.namePlaceholder")}
            />
          )}
        </Field>
        <CollabModeSelector
          value={mode}
          onChange={setMode}
          allowedModes={allowed}
          encryptionMode={folder.encryption_mode}
          disabled={submitting}
          busyLabel={t("docs.creating")}
        />
      </form>
    </Modal>
  );
}

function collabModeLabel(t: TFunction, m: CollabMode): string {
  switch (m) {
    case "markdown":
      return t("collab.markdown");
    case "rich":
      return t("collab.rich");
    case "rich_presence":
      return t("collab.richPresence");
    case "disabled":
      return t("collab.disabled");
  }
}

function collabModeTone(m: CollabMode): BadgeTone {
  switch (m) {
    case "markdown":
      return "neutral";
    case "rich":
    case "rich_presence":
      return "brand";
    case "disabled":
      return "warning";
  }
}

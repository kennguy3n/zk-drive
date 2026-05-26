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

import { useCallback, useEffect, useState } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import { useTranslation } from "react-i18next";
import type { TFunction } from "i18next";
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

export default function DocumentListPage() {
  const { folderId } = useParams<{ folderId: string }>();
  const nav = useNavigate();
  const { t } = useTranslation();

  const [folder, setFolder] = useState<Folder | null>(null);
  const [docs, setDocs] = useState<Document[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [createOpen, setCreateOpen] = useState(false);

  // refresh is also called from the mutation paths (onDelete, onCreated)
  // so its cancellation guard is checked inline by passing an optional
  // cancelled-flag ref. The useEffect below owns the initial-load + folder-
  // change reload and provides a per-effect cancellation token that flips
  // on cleanup, so a stale folder-A response can't clobber folder-B's data
  // when the user navigates between folders quickly. Matches the
  // DocumentEditorPage `cancelled` pattern.
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
    void refresh(() => cancelled);
    return () => {
      cancelled = true;
    };
  }, [refresh]);

  const onDelete = useCallback(
    async (d: Document) => {
      if (!confirm(t("docs.deleteConfirm", { name: d.name }))) return;
      try {
        await deleteDocument(d.id);
        setDocs((prev) => prev.filter((x) => x.id !== d.id));
      } catch (e) {
        setError(translateApiError(e, t));
      }
    },
    [t],
  );

  if (!folderId) {
    return (
      <div style={pageStyle}>
        {t("docs.missingFolderId")} <Link to="/drive">{t("admin.backToDrive")}</Link>
      </div>
    );
  }

  return (
    <div style={pageStyle}>
      <header style={headerStyle}>
        <div style={{ display: "flex", alignItems: "center", gap: 12 }}>
          <Link to={`/drive/folder/${folderId}`} style={backBtnStyle} aria-label={t("docs.backToFolderAria")}>
            ←
          </Link>
          <h1 style={{ margin: 0, fontSize: 20, fontWeight: 600 }}>
            {t("docs.headerTitle", { name: folder ? folder.name : t("docs.folderFallback") })}
          </h1>
          {folder && <EncryptionBadge mode={folder.encryption_mode} size="row" />}
        </div>
        <button onClick={() => setCreateOpen(true)} style={primaryBtn}>
          {t("docs.newDocument")}
        </button>
      </header>

      <main style={{ padding: 24 }}>
        {error && (
          <div style={errorBanner}>{error}</div>
        )}
        {docs.length === 0 ? (
          <p style={{ color: "#6b7280" }}>{t("docs.empty")}</p>
        ) : (
          <table style={tableStyle}>
            <thead>
              <tr>
                <th style={thStyle}>{t("common.name")}</th>
                <th style={thStyle}>{t("docs.mode")}</th>
                <th style={thStyle}>{t("docs.updated")}</th>
                <th style={thStyle} aria-label={t("common.actions")} />
              </tr>
            </thead>
            <tbody>
              {docs.map((d) => (
                <tr key={d.id} style={trStyle}>
                  <td style={tdStyle}>
                    <Link to={`/drive/document/${d.id}`} style={linkStyle}>
                      {d.name}
                    </Link>
                  </td>
                  <td style={tdStyle}>{collabModeLabel(t, d.collab_mode)}</td>
                  <td style={tdStyle}>{new Date(d.updated_at).toLocaleString()}</td>
                  <td style={{ ...tdStyle, textAlign: "right" }}>
                    <button onClick={() => onDelete(d)} style={dangerBtn}>
                      {t("common.delete")}
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </main>

      {createOpen && folder && (
        <CreateDocumentDialog
          folder={folder}
          onClose={() => setCreateOpen(false)}
          onCreated={(d) => {
            setCreateOpen(false);
            nav(`/drive/document/${d.id}`);
          }}
        />
      )}
    </div>
  );
}

interface CreateDocumentDialogProps {
  folder: Folder;
  onClose: () => void;
  onCreated: (doc: Document) => void;
}

function CreateDocumentDialog({ folder, onClose, onCreated }: CreateDocumentDialogProps) {
  const { t } = useTranslation();
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
    <div
      role="dialog"
      aria-modal="true"
      style={modalBackdrop}
      onClick={() => !submitting && onClose()}
    >
      <form
        onSubmit={submit}
        style={modalCard}
        onClick={(e) => e.stopPropagation()}
      >
        <h2 style={{ margin: 0, fontSize: 18 }}>{t("docs.newInFolder", { name: folder.name })}</h2>
        <label style={{ display: "flex", flexDirection: "column", gap: 4 }}>
          <span style={{ fontSize: 13, color: "#374151" }}>{t("common.name")}</span>
          <input
            autoFocus
            value={name}
            onChange={(e) => setName(e.target.value)}
            disabled={submitting}
            style={inputStyle}
          />
        </label>
        <CollabModeSelector
          value={mode}
          onChange={setMode}
          allowedModes={allowed}
          encryptionMode={folder.encryption_mode}
        />
        {dialogError && <p style={{ color: "#991b1b", fontSize: 13 }}>{dialogError}</p>}
        <div style={{ display: "flex", justifyContent: "flex-end", gap: 8 }}>
          <button type="button" onClick={onClose} disabled={submitting} style={btnStyle}>
            {t("common.cancel")}
          </button>
          <button type="submit" disabled={submitting} style={primaryBtn}>
            {submitting ? t("docs.creating") : t("common.create")}
          </button>
        </div>
      </form>
    </div>
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

const pageStyle: React.CSSProperties = {
  minHeight: "100vh",
  background: "#f9fafb",
};

const headerStyle: React.CSSProperties = {
  display: "flex",
  alignItems: "center",
  justifyContent: "space-between",
  padding: "16px 24px",
  borderBottom: "1px solid #e5e7eb",
  gap: 12,
  background: "white",
};

const backBtnStyle: React.CSSProperties = {
  display: "inline-flex",
  alignItems: "center",
  justifyContent: "center",
  width: 32,
  height: 32,
  background: "white",
  border: "1px solid #d1d5db",
  borderRadius: 4,
  textDecoration: "none",
  color: "#111827",
  fontSize: 16,
};

const tableStyle: React.CSSProperties = {
  width: "100%",
  borderCollapse: "collapse",
  background: "white",
  border: "1px solid #e5e7eb",
  borderRadius: 4,
};

const thStyle: React.CSSProperties = {
  textAlign: "left",
  padding: "8px 12px",
  borderBottom: "1px solid #e5e7eb",
  background: "#f9fafb",
  fontSize: 13,
  fontWeight: 600,
  color: "#374151",
};

const trStyle: React.CSSProperties = {
  borderBottom: "1px solid #f3f4f6",
};

const tdStyle: React.CSSProperties = {
  padding: "8px 12px",
  fontSize: 14,
};

const linkStyle: React.CSSProperties = {
  color: "#1d4ed8",
  textDecoration: "none",
  fontWeight: 500,
};

const errorBanner: React.CSSProperties = {
  padding: 12,
  background: "#fee2e2",
  border: "1px solid #fecaca",
  color: "#991b1b",
  borderRadius: 4,
  marginBottom: 16,
};

const primaryBtn: React.CSSProperties = {
  padding: "8px 14px",
  background: "#1d4ed8",
  color: "white",
  border: "1px solid #1d4ed8",
  borderRadius: 4,
  fontSize: 13,
  fontWeight: 500,
  cursor: "pointer",
};

const btnStyle: React.CSSProperties = {
  padding: "8px 14px",
  background: "white",
  border: "1px solid #d1d5db",
  borderRadius: 4,
  fontSize: 13,
  cursor: "pointer",
};

const dangerBtn: React.CSSProperties = {
  padding: "4px 10px",
  background: "white",
  border: "1px solid #fecaca",
  color: "#991b1b",
  borderRadius: 4,
  fontSize: 12,
  cursor: "pointer",
};

const inputStyle: React.CSSProperties = {
  padding: "6px 10px",
  border: "1px solid #d1d5db",
  borderRadius: 4,
  fontSize: 14,
};

const modalBackdrop: React.CSSProperties = {
  position: "fixed",
  inset: 0,
  background: "rgba(0,0,0,0.4)",
  display: "flex",
  alignItems: "center",
  justifyContent: "center",
  zIndex: 100,
};

const modalCard: React.CSSProperties = {
  background: "white",
  borderRadius: 8,
  padding: 24,
  width: "min(480px, 90vw)",
  display: "flex",
  flexDirection: "column",
  gap: 12,
};

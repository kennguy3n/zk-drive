import { useCallback, useEffect, useState } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import FolderTree from "../components/FolderTree";
import FileList from "../components/FileList";
import UploadButton from "../components/UploadButton";
import SearchBar from "../components/SearchBar";
import ShareDialog from "../components/ShareDialog";
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
  listFolders,
  renameFile,
  type ClientRoomTemplate,
  type FileItem,
  type Folder,
} from "../api/client";
import { useAuth } from "../hooks/useAuth";

// shareTarget is the resource currently being shared via ShareDialog.
// Kept discriminated-union so the dialog can render the right noun
// ("Share folder" vs "Share file") without a second prop.
type ShareTarget =
  | { type: "folder"; value: Folder }
  | { type: "file"; value: FileItem };

// FileBrowserPage is the main "drive" surface: breadcrumb + folder tree +
// file table + upload/create controls. The selected folder is stored in
// the URL so refreshes keep context.
export default function FileBrowserPage() {
  const { folderId } = useParams<{ folderId?: string }>();
  const currentFolderID = folderId ?? null;
  const nav = useNavigate();
  const { logout, isAdmin } = useAuth();

  const [folder, setFolder] = useState<Folder | null>(null);
  const [subfolders, setSubfolders] = useState<Folder[]>([]);
  const [files, setFiles] = useState<FileItem[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [shareTarget, setShareTarget] = useState<ShareTarget | null>(null);
  const [selectedFiles, setSelectedFiles] = useState<Set<string>>(new Set());
  const [createFolderOpen, setCreateFolderOpen] = useState(false);
  const [templateDialogOpen, setTemplateDialogOpen] = useState(false);

  const toggleSelect = useCallback((id: string) => {
    setSelectedFiles((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  }, []);

  const refresh = useCallback(async () => {
    setError(null);
    try {
      if (currentFolderID) {
        const { folder: f, children, files: f2 } = await getFolderContents(currentFolderID);
        setFolder(f);
        setSubfolders(children);
        setFiles(f2);
      } else {
        setFolder(null);
        setSubfolders(await listFolders(null));
        // Root view: backend doesn't expose a file listing for null folder
        // in Phase 1, so we show an empty table and nudge the user to
        // open a subfolder.
        setFiles([]);
      }
    } catch (err) {
      setError(String((err as Error)?.message ?? err));
    }
  }, [currentFolderID]);

  useEffect(() => {
    refresh();
  }, [refresh]);

  // Selections are folder-scoped; clearing them on navigation prevents
  // stale IDs from a previous folder leaking into bulk operations.
  useEffect(() => {
    setSelectedFiles(new Set());
  }, [currentFolderID]);

  const handleCreateFolder = () => {
    setCreateFolderOpen(true);
  };

  const handleDeleteFolder = async (id: string) => {
    if (!confirm("Delete folder and all contents?")) return;
    await deleteFolder(id);
    refresh();
  };

  return (
    <div style={{ display: "flex", minHeight: "100vh" }}>
      <FolderTree currentFolderID={currentFolderID} />
      <main style={{ flex: 1, padding: 24 }}>
        <header
          style={{
            display: "flex",
            alignItems: "center",
            justifyContent: "space-between",
            marginBottom: 16,
          }}
        >
          <Breadcrumb folder={folder} />
          <div style={{ display: "flex", alignItems: "center", gap: 12 }}>
            <SearchBar />
            <button onClick={handleCreateFolder} style={btn}>New folder</button>
            {isAdmin ? (
              <button onClick={() => setTemplateDialogOpen(true)} style={btn}>
                Create from template
              </button>
            ) : null}
            <UploadButton folderID={currentFolderID} onUploaded={() => refresh()} />
            {isAdmin ? (
              <>
                <Link to="/admin" style={{ ...btn, textDecoration: "none", color: "#111827" }}>
                  Admin
                </Link>
                <Link to="/billing" style={{ ...btn, textDecoration: "none", color: "#111827" }}>
                  Billing
                </Link>
              </>
            ) : null}
            <button
              onClick={() => {
                logout();
                nav("/login", { replace: true });
              }}
              style={{ ...btn, marginLeft: 12 }}
            >
              Log out
            </button>
          </div>
        </header>

        {error ? (
          <div style={{ color: "#b91c1c", marginBottom: 16, fontSize: 13 }}>{error}</div>
        ) : null}

        {subfolders.length > 0 ? (
          <section style={{ marginBottom: 24 }}>
            <h2 style={{ fontSize: 14, color: "#6b7280", textTransform: "uppercase", margin: "8px 0" }}>
              Folders
            </h2>
            <ul style={{ listStyle: "none", padding: 0, display: "grid", gap: 8, gridTemplateColumns: "repeat(auto-fill, minmax(200px, 1fr))" }}>
              {subfolders.map((f) => (
                <li
                  key={f.id}
                  style={{
                    padding: 12,
                    border: "1px solid #e5e7eb",
                    borderRadius: 6,
                    background: "white",
                    display: "flex",
                    justifyContent: "space-between",
                    alignItems: "center",
                  }}
                >
                  <span style={{ display: "flex", alignItems: "center", gap: 6, minWidth: 0 }}>
                    <Link to={`/drive/folder/${f.id}`} style={{ overflow: "hidden", textOverflow: "ellipsis" }}>
                      {f.name}
                    </Link>
                    <EncryptionBadge mode={f.encryption_mode} />
                  </span>
                  <div style={{ display: "flex", gap: 6 }}>
                    <button
                      onClick={() => setShareTarget({ type: "folder", value: f })}
                      style={btn}
                    >
                      Share
                    </button>
                    <button onClick={() => handleDeleteFolder(f.id)} style={{ ...btn, color: "#b91c1c" }}>
                      Delete
                    </button>
                  </div>
                </li>
              ))}
            </ul>
          </section>
        ) : null}

        <section>
          <h2 style={{ fontSize: 14, color: "#6b7280", textTransform: "uppercase", margin: "8px 0" }}>
            Files
          </h2>
          {selectedFiles.size > 0 ? (
            <div
              style={{
                display: "flex",
                gap: 8,
                padding: "8px 12px",
                marginBottom: 8,
                background: "#eff6ff",
                border: "1px solid #bfdbfe",
                borderRadius: 4,
              }}
            >
              <span style={{ fontSize: 13 }}>{selectedFiles.size} selected</span>
              <button
                style={btn}
                onClick={async () => {
                  const target = prompt("Target folder id:");
                  if (!target) return;
                  await bulkMove({ file_ids: [...selectedFiles], target_folder_id: target });
                  setSelectedFiles(new Set());
                  refresh();
                }}
              >
                Move
              </button>
              <button
                style={btn}
                onClick={async () => {
                  const target = prompt("Target folder id:");
                  if (!target) return;
                  await bulkCopy({ file_ids: [...selectedFiles], target_folder_id: target });
                  setSelectedFiles(new Set());
                  refresh();
                }}
              >
                Copy
              </button>
              <button
                style={{ ...btn, color: "#b91c1c" }}
                onClick={async () => {
                  if (!confirm(`Delete ${selectedFiles.size} files?`)) return;
                  await bulkDelete({ file_ids: [...selectedFiles] });
                  setSelectedFiles(new Set());
                  refresh();
                }}
              >
                Delete
              </button>
              <button
                style={btn}
                onClick={async () => {
                  const blob = await bulkDownload([...selectedFiles]);
                  const url = URL.createObjectURL(blob);
                  const a = document.createElement("a");
                  a.href = url;
                  a.download = "download.zip";
                  a.click();
                  URL.revokeObjectURL(url);
                }}
              >
                Download zip
              </button>
              <button style={btn} onClick={() => setSelectedFiles(new Set())}>
                Clear
              </button>
            </div>
          ) : null}
          <FileList
            files={files}
            onRename={async (id, name) => {
              await renameFile(id, name);
              refresh();
            }}
            onDelete={async (id) => {
              await deleteFile(id);
              refresh();
            }}
            onShare={(f) => setShareTarget({ type: "file", value: f })}
            selectedIDs={selectedFiles}
            onToggleSelect={toggleSelect}
          />
        </section>
      </main>
      {shareTarget ? (
        <ShareDialog resource={shareTarget} onClose={() => setShareTarget(null)} />
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

// EncryptionBadge displays a small pill next to the folder name
// indicating whether the folder is server-processable (managed) or
// strict zero-knowledge. Unknown / missing modes fall back to the
// neutral "managed" rendering so pre-Phase 4 rows stay clean.
function EncryptionBadge({ mode }: { mode?: string }) {
  const isStrict = mode === "strict_zk";
  return (
    <span
      title={
        isStrict
          ? "Strict zero-knowledge: end-to-end encrypted, no server-side processing."
          : "Managed encrypted: server-side preview, search, and virus scanning enabled."
      }
      style={{
        fontSize: 10,
        padding: "1px 6px",
        borderRadius: 999,
        background: isStrict ? "#fee2e2" : "#dcfce7",
        color: isStrict ? "#991b1b" : "#166534",
        whiteSpace: "nowrap",
      }}
    >
      {isStrict ? "strict-ZK" : "managed"}
    </span>
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
  const [name, setName] = useState("");
  const [mode, setMode] = useState<"managed_encrypted" | "strict_zk">("managed_encrypted");
  const [error, setError] = useState<string | null>(null);

  const submit = async () => {
    setError(null);
    if (!name.trim()) return;
    try {
      await createFolder({
        name: name.trim(),
        parent_folder_id: parentID,
        encryption_mode: mode,
      });
      onCreated();
    } catch (e) {
      setError(String((e as Error)?.message ?? e));
    }
  };

  return (
    <Modal onClose={onClose} title="New folder">
      <form
        onSubmit={(e) => {
          e.preventDefault();
          submit();
        }}
        style={{ display: "grid", gap: 12 }}
      >
        <label style={{ display: "grid", gap: 4 }}>
          <span>Name</span>
          <input value={name} onChange={(e) => setName(e.target.value)} autoFocus required />
        </label>
        <fieldset style={{ border: "1px solid #e5e7eb", borderRadius: 6, padding: 12 }}>
          <legend style={{ fontSize: 13, color: "#6b7280" }}>Encryption mode</legend>
          <label style={{ display: "block", marginBottom: 8 }}>
            <input
              type="radio"
              name="encmode"
              value="managed_encrypted"
              checked={mode === "managed_encrypted"}
              onChange={() => setMode("managed_encrypted")}
            />{" "}
            Managed Encrypted (default)
          </label>
          <label style={{ display: "block" }}>
            <input
              type="radio"
              name="encmode"
              value="strict_zk"
              checked={mode === "strict_zk"}
              onChange={() => setMode("strict_zk")}
            />{" "}
            Strict Zero-Knowledge
          </label>
          {mode === "strict_zk" ? (
            <div
              style={{
                marginTop: 8,
                padding: 8,
                background: "#fef3c7",
                border: "1px solid #fde68a",
                fontSize: 12,
                color: "#92400e",
                borderRadius: 4,
              }}
            >
              Strict-ZK disables server-side previews, full-text search, and virus
              scanning. Files are end-to-end encrypted.
            </div>
          ) : null}
        </fieldset>
        {error ? <p style={{ color: "#b91c1c" }}>{error}</p> : null}
        <div style={{ display: "flex", justifyContent: "flex-end", gap: 8 }}>
          <button type="button" onClick={onClose}>
            Cancel
          </button>
          <button type="submit">Create</button>
        </div>
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
  const [templates, setTemplates] = useState<ClientRoomTemplate[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    (async () => {
      try {
        setTemplates(await fetchClientRoomTemplates());
      } catch (e) {
        setError(String((e as Error)?.message ?? e));
      } finally {
        setLoading(false);
      }
    })();
  }, []);

  const pick = async (name: string) => {
    const clientName = prompt("Client name for this room:");
    if (!clientName || !clientName.trim()) return;
    try {
      const r = await createClientRoomFromTemplate(name, clientName.trim());
      onCreated(r.folder_id);
    } catch (e) {
      setError(String((e as Error)?.message ?? e));
    }
  };

  return (
    <Modal onClose={onClose} title="Create from template">
      {loading ? <p>Loading…</p> : null}
      {error ? <p style={{ color: "#b91c1c" }}>{error}</p> : null}
      <div
        style={{
          display: "grid",
          gap: 12,
          gridTemplateColumns: "repeat(auto-fill, minmax(200px, 1fr))",
        }}
      >
        {templates.map((t) => (
          <button
            key={t.name}
            onClick={() => pick(t.name)}
            style={{
              textAlign: "left",
              padding: 12,
              border: "1px solid #e5e7eb",
              background: "white",
              borderRadius: 6,
              cursor: "pointer",
            }}
          >
            <div style={{ fontWeight: 600, textTransform: "capitalize" }}>{t.name}</div>
            <ul style={{ margin: "8px 0 0 16px", padding: 0, fontSize: 12, color: "#4b5563" }}>
              {t.sub_folders.map((s) => (
                <li key={s}>{s}</li>
              ))}
            </ul>
          </button>
        ))}
      </div>
    </Modal>
  );
}

function Modal({
  onClose,
  title,
  children,
}: {
  onClose: () => void;
  title: string;
  children: React.ReactNode;
}) {
  return (
    <div
      style={{
        position: "fixed",
        inset: 0,
        background: "rgba(15, 23, 42, 0.35)",
        display: "flex",
        alignItems: "center",
        justifyContent: "center",
        zIndex: 30,
      }}
      onClick={onClose}
    >
      <div
        onClick={(e) => e.stopPropagation()}
        style={{
          background: "white",
          padding: 20,
          borderRadius: 8,
          minWidth: 420,
          maxWidth: 640,
        }}
      >
        <h3 style={{ marginTop: 0 }}>{title}</h3>
        {children}
      </div>
    </div>
  );
}

function Breadcrumb({ folder }: { folder: Folder | null }) {
  // Split the materialized path into clickable segments. The API stores
  // paths like "/Engineering/Backend/" so trimming empty parts keeps the
  // display clean.
  const parts = folder?.path?.split("/").filter(Boolean) ?? [];
  return (
    <nav style={{ fontSize: 14 }}>
      <Link to="/drive">Drive</Link>
      {parts.map((p, i) => (
        <span key={i}>
          <span style={{ margin: "0 6px", color: "#9ca3af" }}>/</span>
          <span>{p}</span>
        </span>
      ))}
    </nav>
  );
}

const btn: React.CSSProperties = {
  padding: "8px 12px",
  marginRight: 8,
  background: "white",
  border: "1px solid #d1d5db",
  borderRadius: 4,
  fontSize: 13,
};

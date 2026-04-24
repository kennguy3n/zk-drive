import { useCallback, useEffect, useState } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import FolderTree from "../components/FolderTree";
import FileList from "../components/FileList";
import UploadButton from "../components/UploadButton";
import SearchBar from "../components/SearchBar";
import ShareDialog from "../components/ShareDialog";
import {
  createFolder,
  deleteFile,
  deleteFolder,
  getFolderContents,
  listFolders,
  renameFile,
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
  const { logout } = useAuth();

  const [folder, setFolder] = useState<Folder | null>(null);
  const [subfolders, setSubfolders] = useState<Folder[]>([]);
  const [files, setFiles] = useState<FileItem[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [shareTarget, setShareTarget] = useState<ShareTarget | null>(null);

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

  const handleCreateFolder = async () => {
    const name = prompt("New folder name:");
    if (!name || !name.trim()) return;
    await createFolder({ name: name.trim(), parent_folder_id: currentFolderID });
    refresh();
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
            <UploadButton folderID={currentFolderID} onUploaded={() => refresh()} />
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
                  <Link to={`/drive/folder/${f.id}`}>{f.name}</Link>
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
          />
        </section>
      </main>
      {shareTarget ? (
        <ShareDialog resource={shareTarget} onClose={() => setShareTarget(null)} />
      ) : null}
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

import { getDownloadURL, type FileItem } from "../api/client";
import FilePreview from "./FilePreview";

export interface FileListProps {
  files: FileItem[];
  onRename: (id: string, name: string) => void;
  onDelete: (id: string) => void;
  // onShare is optional so callers that don't wire ShareDialog yet
  // keep working unchanged — the Share button is hidden when omitted.
  onShare?: (file: FileItem) => void;
  // selectedIDs + onToggleSelect power the bulk-operations toolbar
  // rendered by the parent page. When omitted, selection checkboxes
  // are hidden (keeps the legacy single-file UX for callers that
  // haven't opted in yet).
  selectedIDs?: Set<string>;
  onToggleSelect?: (id: string) => void;
}

// formatBytes renders a byte count as a human-friendly string. Kept
// inline because it's tiny and only used here.
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

// handleDownload fetches a presigned URL and navigates the browser to it.
// We don't trigger an <a download> click because the user might want the
// browser's default behaviour (view PDFs, play media, etc.).
async function handleDownload(id: string): Promise<void> {
  const url = await getDownloadURL(id);
  window.open(url, "_blank", "noopener");
}

export default function FileList({
  files,
  onRename,
  onDelete,
  onShare,
  selectedIDs,
  onToggleSelect,
}: FileListProps) {
  if (files.length === 0) {
    return (
      <div style={{ padding: 32, color: "#6b7280" }}>No files in this folder.</div>
    );
  }
  const showSelection = !!onToggleSelect;
  return (
    <table style={{ width: "100%", borderCollapse: "collapse" }}>
      <thead>
        <tr style={{ textAlign: "left", borderBottom: "1px solid #e5e7eb" }}>
          {showSelection ? <th style={{ padding: "8px 12px", width: 32 }}></th> : null}
          <th style={{ padding: "8px 12px", fontSize: 12, color: "#6b7280" }}>Name</th>
          <th style={{ padding: "8px 12px", fontSize: 12, color: "#6b7280" }}>Size</th>
          <th style={{ padding: "8px 12px", fontSize: 12, color: "#6b7280" }}>Updated</th>
          <th style={{ padding: "8px 12px", fontSize: 12, color: "#6b7280" }}>Actions</th>
        </tr>
      </thead>
      <tbody>
        {files.map((f) => (
          <tr key={f.id} style={{ borderBottom: "1px solid #f3f4f6" }}>
            {showSelection ? (
              <td style={{ padding: "8px 12px" }}>
                <input
                  type="checkbox"
                  checked={selectedIDs?.has(f.id) ?? false}
                  onChange={() => onToggleSelect?.(f.id)}
                  aria-label={`select ${f.name}`}
                />
              </td>
            ) : null}
            <td style={{ padding: "8px 12px" }}>
              <div style={{ display: "flex", alignItems: "center", gap: 8 }}>
                <FilePreview fileID={f.id} mimeType={f.mime_type} size="thumb" alt={f.name} />
                <span>{f.name}</span>
              </div>
            </td>
            <td style={{ padding: "8px 12px", fontSize: 13, color: "#374151" }}>
              {formatBytes(f.size_bytes)}
            </td>
            <td style={{ padding: "8px 12px", fontSize: 13, color: "#374151" }}>
              {new Date(f.updated_at).toLocaleString()}
            </td>
            <td style={{ padding: "8px 12px" }}>
              <button onClick={() => handleDownload(f.id)} style={actionBtn}>
                Download
              </button>
              <button
                onClick={() => {
                  const name = prompt("Rename file to:", f.name);
                  if (name && name.trim()) onRename(f.id, name.trim());
                }}
                style={actionBtn}
              >
                Rename
              </button>
              {onShare ? (
                <button onClick={() => onShare(f)} style={actionBtn}>
                  Share
                </button>
              ) : null}
              <button
                onClick={() => {
                  if (confirm(`Delete "${f.name}"?`)) onDelete(f.id);
                }}
                style={{ ...actionBtn, color: "#b91c1c" }}
              >
                Delete
              </button>
            </td>
          </tr>
        ))}
      </tbody>
    </table>
  );
}

const actionBtn: React.CSSProperties = {
  padding: "4px 10px",
  marginRight: 4,
  background: "transparent",
  border: "1px solid #d1d5db",
  borderRadius: 4,
  fontSize: 12,
};

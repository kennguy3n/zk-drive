import { useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { listFolders, type Folder } from "../api/client";

// FolderTree is a one-level tree for Phase 1: it shows the workspace root
// plus direct children of the current folder. Full recursive tree is a
// Phase 2 feature once sharing UI lands.
export default function FolderTree({ currentFolderID }: { currentFolderID: string | null }) {
  const [rootFolders, setRootFolders] = useState<Folder[]>([]);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    listFolders(null)
      .then((list) => {
        if (!cancelled) setRootFolders(list);
      })
      .catch((err) => {
        if (!cancelled) setError(String(err?.message ?? err));
      });
    return () => {
      cancelled = true;
    };
  }, [currentFolderID]);

  return (
    <aside
      style={{
        width: 240,
        padding: 16,
        borderRight: "1px solid #e5e7eb",
        background: "white",
        minHeight: "100vh",
      }}
    >
      <div style={{ fontSize: 12, textTransform: "uppercase", color: "#6b7280", marginBottom: 8 }}>
        Workspace
      </div>
      <Link
        to="/drive"
        style={{
          display: "block",
          padding: "6px 8px",
          borderRadius: 4,
          background: currentFolderID === null ? "#eef2ff" : "transparent",
        }}
      >
        Root
      </Link>
      {error ? (
        <div style={{ color: "#b91c1c", fontSize: 12, marginTop: 8 }}>{error}</div>
      ) : null}
      <ul style={{ listStyle: "none", padding: 0, margin: "8px 0 0" }}>
        {rootFolders.map((f) => (
          <li key={f.id}>
            <Link
              to={`/drive/folder/${f.id}`}
              style={{
                display: "block",
                padding: "6px 8px",
                borderRadius: 4,
                background: currentFolderID === f.id ? "#eef2ff" : "transparent",
              }}
            >
              {f.name}
            </Link>
          </li>
        ))}
      </ul>
    </aside>
  );
}

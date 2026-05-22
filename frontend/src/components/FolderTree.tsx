import { useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { listFolders, type Folder } from "../api/client";
import EncryptionBadge from "./EncryptionBadge";

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
                display: "flex",
                alignItems: "center",
                gap: 6,
                padding: "6px 8px",
                borderRadius: 4,
                background: currentFolderID === f.id ? "#eef2ff" : "transparent",
              }}
            >
              <span
                style={{
                  flex: 1,
                  overflow: "hidden",
                  textOverflow: "ellipsis",
                  whiteSpace: "nowrap",
                }}
              >
                {f.name}
              </span>
              {/*
                Privacy-mode badge sits at the end of each sidebar row
                so users can see at a glance which folders are strict-
                ZK (server-blind) without having to open them. This is
                the PROPOSAL §3.3 "surface the mode everywhere a
                folder is rendered" contract: file list + breadcrumb
                + sidebar. EncryptionBadge falls back to the managed
                rendering for folders missing the field (pre-Phase-4
                rows), so the tree still renders cleanly.
              */}
              <EncryptionBadge mode={f.encryption_mode} />
            </Link>
          </li>
        ))}
      </ul>
    </aside>
  );
}

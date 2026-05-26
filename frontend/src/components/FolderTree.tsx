import { useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { useTranslation } from "react-i18next";
import { listFolders, type Folder } from "../api/client";
import EncryptionBadge from "./EncryptionBadge";

// FolderTree is a one-level tree: it shows the workspace root plus
// direct children of the current folder. A full recursive tree is a
// follow-up enhancement.
export default function FolderTree({ currentFolderID }: { currentFolderID: string | null }) {
  const { t } = useTranslation();
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
        {t("nav.workspace")}
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
        {t("drive.rootFolder")}
      </Link>
      {error ? (
        <div style={{ color: "#b91c1c", fontSize: 12, marginTop: 8 }}>{error}</div>
      ) : null}
      <ul style={{ listStyle: "none", padding: 0, margin: "8px 0 0" }}>
        {rootFolders.map((f) => (
          <li
            key={f.id}
            style={{
              display: "flex",
              alignItems: "center",
              borderRadius: 4,
              background: currentFolderID === f.id ? "#eef2ff" : "transparent",
              paddingRight: 8,
            }}
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
              style={{
                flex: 1,
                padding: "6px 8px",
                marginRight: 6,
                overflow: "hidden",
                textOverflow: "ellipsis",
                whiteSpace: "nowrap",
              }}
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
      </ul>
    </aside>
  );
}

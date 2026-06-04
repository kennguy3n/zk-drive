import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import { getFilePreviewURL } from "../api/client";
import { isOfficeDocument } from "../collab/office";

// FilePreview renders the server-generated thumbnail for a file.
// The preview endpoint 404s for unsupported types or versions the
// preview worker hasn't processed yet; in that case we fall back to a
// mime-type-aware placeholder so the UI never shows a broken image.
//
// Sizes:
//   - "thumb" (default): 48 px square, used inline in file list rows
//   - "panel": 256 px square, used in the preview panel / modal
//
// The underlying URL is a presigned GET that expires in 15 min, so
// we refetch on every mount rather than caching across route changes.
export interface FilePreviewProps {
  fileID: string;
  mimeType: string | null;
  size?: "thumb" | "panel";
  alt?: string;
  // fileName powers the office-document detection for the optional
  // "Open in editor" button. When omitted the button never shows.
  fileName?: string;
  // onlyOfficeEnabled + onOpenEditor wire the office-editor affordance
  // in the panel layout. The button only renders when office editing
  // is configured, the file is an office document, and a handler is
  // provided — so thumb usage in list rows is unaffected.
  onlyOfficeEnabled?: boolean;
  onOpenEditor?: () => void;
}

export default function FilePreview({
  fileID,
  mimeType,
  size = "thumb",
  alt,
  fileName,
  onlyOfficeEnabled,
  onOpenEditor,
}: FilePreviewProps) {
  const { t } = useTranslation();
  const [url, setUrl] = useState<string | null>(null);
  const [failed, setFailed] = useState(false);

  useEffect(() => {
    let cancelled = false;
    setUrl(null);
    setFailed(false);
    getFilePreviewURL(fileID)
      .then((u) => {
        if (!cancelled) setUrl(u);
      })
      .catch(() => {
        if (!cancelled) setFailed(true);
      });
    return () => {
      cancelled = true;
    };
  }, [fileID]);

  const px = size === "panel" ? 256 : 48;
  const wrapper: React.CSSProperties = {
    width: px,
    height: px,
    display: "inline-flex",
    alignItems: "center",
    justifyContent: "center",
    background: "#f3f4f6",
    border: "1px solid #e5e7eb",
    borderRadius: 4,
    overflow: "hidden",
    flexShrink: 0,
  };

  const previewNode =
    url && !failed ? (
      <div style={wrapper}>
        <img
          src={url}
          alt={alt ?? t("preview.alt")}
          style={{ maxWidth: "100%", maxHeight: "100%", objectFit: "contain" }}
          onError={() => setFailed(true)}
        />
      </div>
    ) : (
      <div style={wrapper} aria-label={alt ?? t("preview.unavailable")}>
        <span style={{ fontSize: size === "panel" ? 14 : 11, color: "#6b7280" }}>
          {placeholderLabel(mimeType)}
        </span>
      </div>
    );

  // The "Open in editor" affordance only appears in the panel layout
  // when office editing is configured, a handler is wired, and the
  // file is an office document. Inline thumbnails (list rows) never
  // render it.
  const showEditorButton =
    size === "panel" && !!onOpenEditor && !!onlyOfficeEnabled && isOfficeDocument(fileName);
  if (!showEditorButton) {
    return previewNode;
  }
  return (
    <div style={{ display: "inline-flex", flexDirection: "column", gap: 8, alignItems: "center" }}>
      {previewNode}
      <button onClick={onOpenEditor} style={openEditorBtnStyle}>
        {t("onlyoffice.openInEditor")}
      </button>
    </div>
  );
}

const openEditorBtnStyle: React.CSSProperties = {
  padding: "6px 12px",
  background: "white",
  border: "1px solid #d1d5db",
  borderRadius: 4,
  fontSize: 13,
  cursor: "pointer",
};

// placeholderLabel returns a short hint based on the mime type so the
// placeholder at least communicates "this is a PDF" vs a generic file
// icon. Keeping this static (no icon font) matches the rest of the
// minimal UI style.
function placeholderLabel(mime: string | null): string {
  if (!mime) return "FILE";
  const m = mime.toLowerCase();
  if (m.startsWith("image/")) return "IMG";
  if (m === "application/pdf") return "PDF";
  if (m.startsWith("video/")) return "VID";
  if (m.startsWith("audio/")) return "AUD";
  if (m.startsWith("text/")) return "TXT";
  return "FILE";
}

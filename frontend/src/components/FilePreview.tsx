import { useEffect, useState } from "react";
import { getFilePreviewURL } from "../api/client";

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
}

export default function FilePreview({
  fileID,
  mimeType,
  size = "thumb",
  alt,
}: FilePreviewProps) {
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

  if (url && !failed) {
    return (
      <div style={wrapper}>
        <img
          src={url}
          alt={alt ?? "preview"}
          style={{ maxWidth: "100%", maxHeight: "100%", objectFit: "contain" }}
          onError={() => setFailed(true)}
        />
      </div>
    );
  }

  return (
    <div style={wrapper} aria-label={alt ?? "no preview available"}>
      <span style={{ fontSize: size === "panel" ? 14 : 11, color: "#6b7280" }}>
        {placeholderLabel(mimeType)}
      </span>
    </div>
  );
}

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

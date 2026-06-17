import { useEffect, useState, type ComponentType } from "react";
import { useTranslation } from "react-i18next";
import {
  Download,
  File as FileIcon,
  FileText,
  FileVideo,
  Image as ImageIcon,
  Music,
  Share2,
} from "lucide-react";
import { getDownloadURL, getFilePreviewURL } from "../api/client";
import { isOfficeDocument } from "../collab/office";
import { Button, PagePreviewSkeleton, Skeleton, useToast } from "./ui";
import { cn } from "../lib/cn";

// FilePreview renders the server-generated preview for a file. The preview
// endpoint 404s for unsupported types or versions the preview worker
// hasn't processed yet; in that case we fall back to a mime-type-aware
// placeholder so the UI never shows a broken image.
//
// Two layouts share the same fetch + fallback logic:
//   - "thumb" (default): a compact 48 px tile used inline in file-list
//     rows. Shimmers while the presigned URL loads, then swaps in the
//     image (or a mime icon when no preview exists).
//   - "panel": a full-bleed, tokenised viewer with a top bar (file name +
//     a primary download / share / open-in-editor action group), a
//     skeleton loading state and a graceful unsupported-type fallback.
//
// Everything is token-based (bg-surface / border-border / text-muted …) so
// it re-themes with the app and flips correctly in dark mode.
//
// The underlying URL is a presigned GET that expires in 15 min, so we
// refetch on every mount rather than caching across route changes.
export interface FilePreviewProps {
  fileID: string;
  mimeType: string | null;
  size?: "thumb" | "panel";
  alt?: string;
  // fileName powers the panel top-bar label and the office-document
  // detection for the optional "Open in editor" button. When omitted the
  // button never shows and the bar falls back to a generic title.
  fileName?: string;
  // onlyOfficeEnabled + onOpenEditor wire the office-editor affordance in
  // the panel layout. The button only renders when office editing is
  // configured, the file is an office document, and a handler is provided
  // — so thumb usage in list rows is unaffected.
  onlyOfficeEnabled?: boolean;
  onOpenEditor?: () => void;
  // onShare, when provided, renders a Share action in the panel top bar
  // (typically the parent opens its ShareDialog). Omitted → no button.
  onShare?: () => void;
}

export default function FilePreview(props: FilePreviewProps) {
  const { fileID, size = "thumb" } = props;
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

  const onFail = () => setFailed(true);

  // The panel viewer pulls in useToast (download errors); keeping it in a
  // dedicated component means the lightweight thumb path — rendered in
  // file-list rows that may sit outside a ToastProvider — never touches
  // that hook.
  if (size === "panel") {
    return <PanelViewer {...props} url={url} failed={failed} onFail={onFail} />;
  }
  return <ThumbPreview {...props} url={url} failed={failed} onFail={onFail} />;
}

interface ViewProps extends FilePreviewProps {
  url: string | null;
  failed: boolean;
  onFail: () => void;
}

function ThumbPreview({ mimeType, alt, url, failed, onFail }: ViewProps) {
  const { t } = useTranslation();
  const tile = "h-12 w-12 shrink-0 rounded-lg";

  if (failed) {
    return (
      <div
        role="img"
        aria-label={alt ?? t("preview.unavailable")}
        className={cn(
          tile,
          "flex items-center justify-center border border-border bg-surface-2 text-muted",
        )}
      >
        <MimeIcon mime={mimeType} className="h-5 w-5" />
      </div>
    );
  }
  if (!url) {
    return <Skeleton className={tile} />;
  }
  return (
    <div
      className={cn(
        tile,
        "flex items-center justify-center overflow-hidden border border-border bg-surface-2",
      )}
    >
      <img
        src={url}
        alt={alt ?? t("preview.alt")}
        className="max-h-full max-w-full object-contain"
        onError={onFail}
        loading="lazy"
      />
    </div>
  );
}

function PanelViewer({
  fileID,
  mimeType,
  alt,
  fileName,
  onlyOfficeEnabled,
  onOpenEditor,
  onShare,
  url,
  failed,
  onFail,
}: ViewProps) {
  const { t } = useTranslation();
  const toast = useToast();
  const [downloading, setDownloading] = useState(false);

  const showEditorButton =
    !!onOpenEditor && !!onlyOfficeEnabled && isOfficeDocument(fileName);

  const handleDownload = async () => {
    setDownloading(true);
    try {
      const downloadURL = await getDownloadURL(fileID);
      const a = document.createElement("a");
      a.href = downloadURL;
      a.rel = "noopener";
      if (fileName) a.download = fileName;
      document.body.appendChild(a);
      a.click();
      a.remove();
    } catch {
      toast.error(t("preview.downloadFailed"));
    } finally {
      setDownloading(false);
    }
  };

  let body;
  if (failed) {
    body = (
      <div
        role="img"
        aria-label={alt ?? t("preview.unavailable")}
        className="flex flex-col items-center gap-3 text-muted"
      >
        <MimeIcon mime={mimeType} className="h-12 w-12" />
        <span className="text-xs font-semibold uppercase tracking-wide">
          {placeholderLabel(mimeType)}
        </span>
        <span className="text-sm">{t("preview.unavailable")}</span>
      </div>
    );
  } else if (url) {
    body = (
      <img
        src={url}
        alt={alt ?? t("preview.alt")}
        className="max-h-full max-w-full rounded-lg object-contain shadow-sm"
        onError={onFail}
      />
    );
  } else {
    body = (
      <div className="w-full" role="status" aria-label={t("preview.loading")}>
        <PagePreviewSkeleton />
      </div>
    );
  }

  return (
    <div className="flex w-full flex-col overflow-hidden rounded-card border border-border bg-surface">
      <div className="flex items-center justify-between gap-3 border-b border-border px-4 py-3">
        <div className="flex min-w-0 items-center gap-2.5">
          <span className="flex h-9 w-9 shrink-0 items-center justify-center rounded-lg bg-surface-2 text-muted">
            <MimeIcon mime={mimeType} className="h-5 w-5" />
          </span>
          <div className="min-w-0">
            <p className="truncate text-sm font-semibold text-fg">
              {fileName ?? alt ?? t("preview.untitled")}
            </p>
            {mimeType && <p className="truncate text-xs text-muted">{mimeType}</p>}
          </div>
        </div>
        <div className="flex shrink-0 items-center gap-2">
          {onShare && (
            <Button variant="secondary" size="sm" onClick={onShare}>
              <Share2 className="h-4 w-4" aria-hidden="true" />
              {t("preview.share")}
            </Button>
          )}
          {showEditorButton && (
            <Button variant="secondary" size="sm" onClick={onOpenEditor}>
              {t("onlyoffice.openInEditor")}
            </Button>
          )}
          <Button
            variant="primary"
            size="sm"
            onClick={handleDownload}
            loading={downloading}
          >
            <Download className="h-4 w-4" aria-hidden="true" />
            {t("preview.download")}
          </Button>
        </div>
      </div>
      <div className="flex min-h-[60vh] items-center justify-center bg-surface-2 p-4">
        {body}
      </div>
    </div>
  );
}

// MimeIcon renders a lucide glyph chosen from the mime type so the
// placeholder/top-bar communicates "this is a PDF" vs a generic file.
function MimeIcon({ mime, className }: { mime: string | null; className?: string }) {
  const Icon = pickIcon(mime);
  return <Icon className={className} aria-hidden="true" />;
}

function pickIcon(mime: string | null): ComponentType<{ className?: string }> {
  if (!mime) return FileIcon;
  const m = mime.toLowerCase();
  if (m.startsWith("image/")) return ImageIcon;
  if (m === "application/pdf") return FileText;
  if (m.startsWith("video/")) return FileVideo;
  if (m.startsWith("audio/")) return Music;
  if (m.startsWith("text/")) return FileText;
  return FileIcon;
}

// placeholderLabel returns a short hint based on the mime type, shown
// under the icon in the panel fallback.
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

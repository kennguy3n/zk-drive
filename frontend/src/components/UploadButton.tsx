import { useEffect, useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import { Upload } from "lucide-react";
import { uploadFile, type FileItem } from "../api/client";
import { translateApiError } from "../api/errors";
import { Button } from "./ui";

export interface UploadButtonProps {
  folderID: string | null;
  onUploaded: (file: FileItem) => void;
  // openRef lets a parent trigger the hidden file picker imperatively
  // (e.g. the onboarding "Upload your first file" card). Optional so
  // existing callers are unaffected.
  openRef?: React.MutableRefObject<(() => void) | null>;
  /** Button variant — defaults to the brand gradient primary CTA. */
  variant?: "primary" | "gradient" | "secondary" | "ghost";
  size?: "sm" | "md" | "lg";
}

// UploadButton hides the file input behind a styled button and runs the
// three-step presigned-URL flow defined in api/client.ts. Errors bubble
// up through an inline message so the user isn't left guessing.
export default function UploadButton({
  folderID,
  onUploaded,
  openRef,
  variant = "gradient",
  size = "md",
}: UploadButtonProps) {
  const { t } = useTranslation();
  const inputRef = useRef<HTMLInputElement>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  // Expose an imperative open() to the parent via openRef so the
  // onboarding "Upload your first file" card can trigger the picker.
  useEffect(() => {
    if (!openRef) return;
    openRef.current = () => inputRef.current?.click();
    return () => {
      openRef.current = null;
    };
  }, [openRef]);

  const handleChange = async (e: React.ChangeEvent<HTMLInputElement>) => {
    const file = e.target.files?.[0];
    if (!file) return;
    setBusy(true);
    setError(null);
    try {
      const uploaded = await uploadFile(file, folderID);
      onUploaded(uploaded);
    } catch (err) {
      setError(translateApiError(err, t));
    } finally {
      setBusy(false);
      if (inputRef.current) inputRef.current.value = "";
    }
  };

  return (
    <div className="inline-flex items-center gap-2">
      <Button
        type="button"
        variant={variant}
        size={size}
        loading={busy}
        onClick={() => inputRef.current?.click()}
      >
        {!busy && <Upload className="h-4 w-4" aria-hidden="true" />}
        {busy ? t("drive.uploading") : t("drive.uploadFile")}
      </Button>
      <input ref={inputRef} type="file" onChange={handleChange} className="hidden" />
      {error ? (
        <span role="alert" className="text-xs text-danger">
          {error}
        </span>
      ) : null}
    </div>
  );
}

import { useRef, useState } from "react";
import { uploadFile, type FileItem } from "../api/client";

export interface UploadButtonProps {
  folderID: string | null;
  onUploaded: (file: FileItem) => void;
}

// UploadButton hides the file input behind a styled button and runs the
// three-step presigned-URL flow defined in api/client.ts. Errors bubble
// up through an inline message so the user isn't left guessing.
export default function UploadButton({ folderID, onUploaded }: UploadButtonProps) {
  const inputRef = useRef<HTMLInputElement>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const handleChange = async (e: React.ChangeEvent<HTMLInputElement>) => {
    const file = e.target.files?.[0];
    if (!file) return;
    setBusy(true);
    setError(null);
    try {
      const uploaded = await uploadFile(file, folderID);
      onUploaded(uploaded);
    } catch (err) {
      setError(String((err as Error)?.message ?? err));
    } finally {
      setBusy(false);
      if (inputRef.current) inputRef.current.value = "";
    }
  };

  return (
    <div style={{ display: "inline-block" }}>
      <button
        type="button"
        onClick={() => inputRef.current?.click()}
        disabled={busy}
        style={{
          padding: "8px 14px",
          background: "#2563eb",
          color: "white",
          border: "none",
          borderRadius: 4,
          fontSize: 13,
          opacity: busy ? 0.6 : 1,
        }}
      >
        {busy ? "Uploading..." : "Upload file"}
      </button>
      <input
        ref={inputRef}
        type="file"
        onChange={handleChange}
        style={{ display: "none" }}
      />
      {error ? (
        <span style={{ marginLeft: 12, color: "#b91c1c", fontSize: 12 }}>{error}</span>
      ) : null}
    </div>
  );
}

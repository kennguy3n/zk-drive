import { useCallback, useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { fetchCMK, updateCMK } from "../api/client";
import { useAuth } from "../hooks/useAuth";

// EncryptionPage lets workspace admins set the customer-managed key
// (CMK) URI used by zk-object-fabric to wrap per-object DEKs.
// Accepted schemes are documented inline so an admin pasting an ARN
// from AWS KMS knows the field is correct without reading the Go
// source.
export default function EncryptionPage() {
  const { isAdmin } = useAuth();
  const [initialURI, setInitialURI] = useState("");
  const [uri, setURI] = useState("");
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [message, setMessage] = useState<string | null>(null);

  const load = useCallback(async () => {
    setError(null);
    setLoading(true);
    try {
      const r = await fetchCMK();
      setInitialURI(r.cmk_uri ?? "");
      setURI(r.cmk_uri ?? "");
    } catch (e) {
      setError(errMessage(e));
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    if (isAdmin) load();
  }, [isAdmin, load]);

  if (!isAdmin) {
    return (
      <div style={{ padding: 32 }}>
        <h2>Admin only</h2>
        <p>
          This page is restricted to workspace administrators.{" "}
          <Link to="/drive">Back to drive</Link>
        </p>
      </div>
    );
  }

  const save = async () => {
    setError(null);
    setMessage(null);
    try {
      await updateCMK(uri.trim());
      setMessage("CMK saved.");
      await load();
    } catch (e) {
      setError(errMessage(e));
    }
  };

  return (
    <div style={{ padding: 24 }}>
      <header
        style={{
          display: "flex",
          alignItems: "center",
          justifyContent: "space-between",
          marginBottom: 16,
        }}
      >
        <h1 style={{ margin: 0 }}>Encryption (CMK)</h1>
        <Link to="/admin">Back to admin</Link>
      </header>
      {loading ? <p>Loading…</p> : null}
      {error ? <p style={{ color: "#b91c1c" }}>{error}</p> : null}
      {message ? <p style={{ color: "#047857" }}>{message}</p> : null}

      <div
        style={{
          border: "1px solid #fde68a",
          background: "#fffbeb",
          padding: 12,
          borderRadius: 6,
          marginBottom: 16,
          fontSize: 13,
          color: "#92400e",
          maxWidth: 640,
        }}
      >
        <strong>Managed-encrypted vs strict zero-knowledge:</strong> A CMK
        enables managed-encrypted folders where the gateway wraps per-object
        DEKs with the customer key — the server can still stream plaintext
        for preview, virus scan, and search. Strict-zero-knowledge folders
        remain end-to-end encrypted regardless of this setting; the server
        never sees plaintext there and all server-side processing is
        disabled.
      </div>

      <form
        onSubmit={(e) => {
          e.preventDefault();
          save();
        }}
        style={{ display: "grid", gap: 12, maxWidth: 640 }}
      >
        <label style={{ display: "grid", gap: 4 }}>
          <span>CMK URI</span>
          <input
            value={uri}
            onChange={(e) => setURI(e.target.value)}
            placeholder="arn:aws:kms:us-east-1:123456789012:key/..."
            style={{ fontFamily: "monospace" }}
          />
          <small style={{ color: "#6b7280" }}>
            Accepted schemes: <code>arn:aws:kms:...</code>, <code>kms://</code>,{" "}
            <code>vault://</code>, <code>transit://</code>. Leave empty for
            the gateway default key.
          </small>
        </label>
        <div style={{ display: "flex", gap: 8 }}>
          <button type="submit">Save</button>
          <button type="button" onClick={() => setURI(initialURI)}>
            Reset
          </button>
        </div>
      </form>
    </div>
  );
}

function errMessage(e: unknown): string {
  if (e && typeof e === "object" && "message" in e) {
    return String((e as { message: unknown }).message);
  }
  return String(e);
}

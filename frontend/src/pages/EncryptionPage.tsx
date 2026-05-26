import { useCallback, useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { useTranslation } from "react-i18next";
import { fetchCMK, updateCMK } from "../api/client";
import { translateApiError } from "../api/errors";
import { useAuth } from "../hooks/useAuth";

// EncryptionPage lets workspace admins set the customer-managed key
// (CMK) URI used by zk-object-fabric to wrap per-object DEKs.
// Accepted schemes are documented inline so an admin pasting an ARN
// from AWS KMS knows the field is correct without reading the Go
// source.
export default function EncryptionPage() {
  const { isAdmin } = useAuth();
  const { t } = useTranslation();
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
      setError(translateApiError(e, t));
    } finally {
      setLoading(false);
    }
    // t identity is stable per i18next mount; intentionally excluded
    // to avoid re-running load on a language change — the user is on
    // admin-only English copy anyway.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  useEffect(() => {
    if (isAdmin) load();
  }, [isAdmin, load]);

  if (!isAdmin) {
    return (
      <div style={{ padding: 32 }}>
        <h2>{t("admin.adminOnly")}</h2>
        <p>
          {t("admin.adminOnlyDescription")} <Link to="/drive">{t("admin.backToDrive")}</Link>
        </p>
      </div>
    );
  }

  const save = async () => {
    setError(null);
    setMessage(null);
    try {
      await updateCMK(uri.trim());
      setMessage(t("encryption.cmkSaved"));
      await load();
    } catch (e) {
      setError(translateApiError(e, t));
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
        <h1 style={{ margin: 0 }}>{t("encryption.pageTitle")}</h1>
        <Link to="/admin">{t("admin.backToAdmin")}</Link>
      </header>
      {loading ? <p>{t("common.loading")}</p> : null}
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
        <strong>{t("encryption.modesHeading")}</strong> {t("encryption.modesExplanation")}
      </div>

      <form
        onSubmit={(e) => {
          e.preventDefault();
          save();
        }}
        style={{ display: "grid", gap: 12, maxWidth: 640 }}
      >
        <label style={{ display: "grid", gap: 4 }}>
          <span>{t("encryption.cmkUri")}</span>
          <input
            value={uri}
            onChange={(e) => setURI(e.target.value)}
            placeholder="arn:aws:kms:us-east-1:123456789012:key/..."
            style={{ fontFamily: "monospace" }}
          />
          <small style={{ color: "#6b7280" }}>
            {t("encryption.cmkSchemesHint")}
          </small>
        </label>
        <div style={{ display: "flex", gap: 8 }}>
          <button type="submit">{t("common.save")}</button>
          <button type="button" onClick={() => setURI(initialURI)}>
            {t("common.reset")}
          </button>
        </div>
      </form>
    </div>
  );
}



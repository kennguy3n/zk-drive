import { useCallback, useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { useTranslation } from "react-i18next";
import {
  fetchCMK,
  updateCMK,
  getDefaultEncryptionMode,
  updateDefaultEncryptionMode,
  type EncryptionMode,
} from "../api/client";
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

      <DefaultEncryptionModeSection />
    </div>
  );
}

// DefaultEncryptionModeSection lets a Secure Business admin pick the
// encryption mode applied to new top-level folders by default
// (managed-encrypted vs strict zero-knowledge). It is its own
// component with isolated state so a save here never disturbs the CMK
// form above. The supported set is sourced from the server response so
// the picker can't drift from the backend's allow-list.
function DefaultEncryptionModeSection() {
  const { t } = useTranslation();
  const [mode, setMode] = useState<EncryptionMode | null>(null);
  const [saving, setSaving] = useState(false);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [message, setMessage] = useState<string | null>(null);

  const load = useCallback(async () => {
    setError(null);
    setLoading(true);
    try {
      const r = await getDefaultEncryptionMode();
      setMode(r.mode);
    } catch (e) {
      setError(translateApiError(e, t));
    } finally {
      setLoading(false);
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  useEffect(() => {
    load();
  }, [load]);

  const choose = async (next: EncryptionMode) => {
    if (next === mode || saving) return;
    setError(null);
    setMessage(null);
    setSaving(true);
    try {
      const r = await updateDefaultEncryptionMode(next);
      setMode(r.mode);
      setMessage(t("encryption.defaultModeSaved"));
    } catch (e) {
      setError(translateApiError(e, t));
    } finally {
      setSaving(false);
    }
  };

  const options: {
    value: EncryptionMode;
    label: string;
    hint: string;
    badge?: string;
  }[] = [
    {
      value: "managed_encrypted",
      label: t("encryption.defaultModeManagedLabel"),
      hint: t("encryption.defaultModeManagedHint"),
    },
    {
      value: "strict_zk",
      label: t("encryption.defaultModeStrictLabel"),
      hint: t("encryption.defaultModeStrictHint"),
      badge: t("encryption.defaultModeSecureBusinessBadge"),
    },
  ];

  return (
    <section style={{ marginTop: 32, maxWidth: 640 }}>
      <h2 style={{ fontSize: 18, marginBottom: 4 }}>
        {t("encryption.defaultModeHeading")}
      </h2>
      <p style={{ color: "#6b7280", fontSize: 13, marginTop: 0 }}>
        {t("encryption.defaultModeDescription")}
      </p>
      {loading ? <p>{t("common.loading")}</p> : null}
      {error ? <p style={{ color: "#b91c1c" }}>{error}</p> : null}
      {message ? <p style={{ color: "#047857" }}>{message}</p> : null}

      <div style={{ display: "grid", gap: 12 }}>
        {options.map((opt) => {
          const selected = mode === opt.value;
          return (
            <label
              key={opt.value}
              style={{
                display: "grid",
                gridTemplateColumns: "auto 1fr",
                gap: 10,
                alignItems: "start",
                border: selected ? "2px solid #2563eb" : "1px solid #d1d5db",
                background: selected ? "#eff6ff" : "#fff",
                borderRadius: 8,
                padding: 12,
                cursor: saving ? "wait" : "pointer",
              }}
            >
              <input
                type="radio"
                name="default-encryption-mode"
                value={opt.value}
                checked={selected}
                disabled={saving || loading}
                onChange={() => choose(opt.value)}
                style={{ marginTop: 3 }}
              />
              <span>
                <span style={{ fontWeight: 600 }}>
                  {opt.label}
                  {opt.badge ? (
                    <span
                      style={{
                        marginLeft: 8,
                        fontSize: 11,
                        fontWeight: 700,
                        color: "#1e3a8a",
                        background: "#dbeafe",
                        borderRadius: 4,
                        padding: "1px 6px",
                        verticalAlign: "middle",
                      }}
                    >
                      {opt.badge}
                    </span>
                  ) : null}
                </span>
                <span
                  style={{
                    display: "block",
                    color: "#6b7280",
                    fontSize: 13,
                    marginTop: 2,
                  }}
                >
                  {opt.hint}
                </span>
              </span>
            </label>
          );
        })}
      </div>
    </section>
  );
}



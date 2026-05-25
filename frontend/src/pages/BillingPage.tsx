import { useEffect, useMemo, useState } from "react";
import { Link, useSearchParams } from "react-router-dom";
import { useTranslation } from "react-i18next";
import type { TFunction } from "i18next";
import {
  createCheckoutSession,
  createPortalSession,
  fetchBillingUsage,
  type BillingUsageSummary,
} from "../api/client";
import { translateApiError } from "../api/errors";
import { useAuth } from "../hooks/useAuth";

// Tier metadata mirrors the canonical strings in
// internal/billing/billing.go. Translated label/blurb live in en.json
// so non-English locales can customise the plan card copy without
// changing component code.
const TIER_IDS = ["starter", "business", "secure_business"] as const;
type TierID = (typeof TIER_IDS)[number];

// BillingPage shows the workspace's current plan tier and usage-vs-limit
// bars. Admin-only because the underlying /api/admin/billing/usage
// endpoint is admin-only.
export default function BillingPage() {
  const { isAdmin } = useAuth();
  const { t } = useTranslation();
  const [usage, setUsage] = useState<BillingUsageSummary | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [selectedTier, setSelectedTier] = useState<TierID>("business");
  const [busy, setBusy] = useState<"checkout" | "portal" | null>(null);
  const [searchParams, setSearchParams] = useSearchParams();

  const banner = useMemo(() => bannerFromSearch(searchParams, t), [searchParams, t]);

  useEffect(() => {
    (async () => {
      try {
        setUsage(await fetchBillingUsage());
      } catch (e) {
        setError(translateApiError(e, t));
      }
    })();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  async function handleUpgrade() {
    setError(null);
    setBusy("checkout");
    try {
      const here = window.location.origin + window.location.pathname;
      const { url } = await createCheckoutSession({
        tier: selectedTier,
        success_url: `${here}?stripe=success`,
        cancel_url: `${here}?stripe=cancel`,
      });
      window.location.assign(url);
    } catch (e) {
      setBusy(null);
      setError(translateApiError(e, t) || t("billing.checkoutFailed"));
    }
  }

  async function handleManage() {
    setError(null);
    setBusy("portal");
    try {
      const here = window.location.origin + window.location.pathname;
      const { url } = await createPortalSession({ return_url: here });
      window.location.assign(url);
    } catch (e) {
      setBusy(null);
      setError(translateApiError(e, t) || t("billing.portalFailed"));
    }
  }

  function dismissBanner() {
    const next = new URLSearchParams(searchParams);
    next.delete("stripe");
    setSearchParams(next, { replace: true });
  }

  if (!isAdmin) {
    return (
      <div style={{ padding: 32 }}>
        <h2>{t("admin.adminOnly")}</h2>
        <p>
          {t("billing.adminOnlyDescription")} <Link to="/drive">{t("admin.backToDrive")}</Link>
        </p>
      </div>
    );
  }
  return (
    <div style={{ padding: 24 }}>
      <header style={{ display: "flex", justifyContent: "space-between", alignItems: "center", marginBottom: 16 }}>
        <h1 style={{ margin: 0 }}>{t("billing.title")}</h1>
        <div>
          <Link to="/admin" style={{ marginRight: 16 }}>
            {t("nav.admin")}
          </Link>
          <Link to="/drive">{t("admin.backToDrive")}</Link>
        </div>
      </header>
      {banner && (
        <div
          role="status"
          style={{
            background: banner.tone === "success" ? "#ecfdf5" : "#fef3c7",
            color: banner.tone === "success" ? "#065f46" : "#92400e",
            border: `1px solid ${banner.tone === "success" ? "#a7f3d0" : "#fde68a"}`,
            padding: "10px 14px",
            borderRadius: 6,
            marginBottom: 16,
            display: "flex",
            justifyContent: "space-between",
            alignItems: "center",
          }}
        >
          <span>{banner.message}</span>
          <button
            type="button"
            onClick={dismissBanner}
            style={{ background: "transparent", border: "none", cursor: "pointer", color: "inherit" }}
          >
            {t("common.dismiss")}
          </button>
        </div>
      )}
      {error && <p style={{ color: "#b91c1c" }}>{error}</p>}
      {usage && (
        <div>
          <h2 style={{ marginBottom: 4 }}>{t("billing.planLabel", { tier: usage.tier })}</h2>
          {!usage.plan_configured && (
            <p style={{ color: "#6b7280", marginTop: 0 }}>
              {t("billing.noPlanConfigured")}
            </p>
          )}

          <section style={{ marginTop: 24, marginBottom: 24 }}>
            <h3 style={{ marginBottom: 8 }}>{t("billing.manageSubscription")}</h3>
            <p style={{ color: "#6b7280", marginTop: 0 }}>
              {t("billing.manageDescription")}
            </p>
            <fieldset
              style={{
                border: "1px solid #e5e7eb",
                borderRadius: 8,
                padding: 12,
                display: "grid",
                gridTemplateColumns: "repeat(auto-fit, minmax(220px, 1fr))",
                gap: 12,
              }}
            >
              <legend style={{ padding: "0 6px", color: "#374151" }}>{t("billing.chooseTier")}</legend>
              {TIER_IDS.map((id) => {
                const active = selectedTier === id;
                const current = usage.tier === id;
                return (
                  <label
                    key={id}
                    style={{
                      border: `1px solid ${active ? "#2563eb" : "#e5e7eb"}`,
                      borderRadius: 6,
                      padding: 10,
                      cursor: "pointer",
                      background: active ? "#eff6ff" : "white",
                    }}
                  >
                    <div style={{ display: "flex", alignItems: "center", gap: 8 }}>
                      <input
                        type="radio"
                        name="tier"
                        value={id}
                        checked={active}
                        onChange={() => setSelectedTier(id)}
                      />
                      <strong>{t(`billing.tier.${id}.label`)}</strong>
                      {current && <span style={{ color: "#059669", fontSize: 12 }}>{t("billing.currentBadge")}</span>}
                    </div>
                    <div style={{ color: "#6b7280", fontSize: 13, marginTop: 6 }}>{t(`billing.tier.${id}.blurb`)}</div>
                  </label>
                );
              })}
            </fieldset>
            <div style={{ display: "flex", gap: 12, marginTop: 16 }}>
              <button
                type="button"
                onClick={handleUpgrade}
                disabled={busy !== null}
                style={primaryButtonStyle(busy === "checkout")}
              >
                {busy === "checkout" ? t("billing.redirecting") : t("billing.upgradePlan")}
              </button>
              {usage.plan_configured && (
                <button
                  type="button"
                  onClick={handleManage}
                  disabled={busy !== null}
                  style={secondaryButtonStyle(busy === "portal")}
                >
                  {busy === "portal" ? t("billing.redirecting") : t("billing.manageSubscription")}
                </button>
              )}
            </div>
          </section>

          <UsageBar label={t("billing.usageStorage")} used={usage.storage_used_bytes} limit={usage.storage_limit_bytes} bytes />
          <UsageBar
            label={t("billing.usageBandwidth")}
            used={usage.bandwidth_used_bytes_month}
            limit={usage.bandwidth_limit_bytes_month}
            bytes
          />
          <UsageBar label={t("billing.usageUsers")} used={usage.user_count} limit={usage.user_limit} />
        </div>
      )}
    </div>
  );
}

function UsageBar({
  label,
  used,
  limit,
  bytes = false,
}: {
  label: string;
  used: number;
  limit: number;
  bytes?: boolean;
}) {
  const pct = limit > 0 ? Math.min(100, (used / limit) * 100) : 0;
  const over = limit > 0 && used > limit;
  return (
    <div style={{ marginBottom: 20 }}>
      <div style={{ display: "flex", justifyContent: "space-between", marginBottom: 4 }}>
        <strong>{label}</strong>
        <span>
          {bytes ? formatBytes(used) : used} / {bytes ? formatBytes(limit) : limit}
        </span>
      </div>
      <div style={{ background: "#e5e7eb", height: 10, borderRadius: 4 }}>
        <div
          style={{
            background: over ? "#b91c1c" : "#2563eb",
            width: `${pct}%`,
            height: 10,
            borderRadius: 4,
          }}
        />
      </div>
    </div>
  );
}

function formatBytes(n: number): string {
  if (n < 1024) return `${n} B`;
  const units = ["KB", "MB", "GB", "TB"];
  let v = n / 1024;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v.toFixed(1)} ${units[i]}`;
}

function bannerFromSearch(
  params: URLSearchParams,
  t: TFunction,
): { tone: "success" | "warning"; message: string } | null {
  switch (params.get("stripe")) {
    case "success":
      return { tone: "success", message: t("billing.bannerCheckoutSuccess") };
    case "cancel":
      return { tone: "warning", message: t("billing.bannerCheckoutCancel") };
    default:
      return null;
  }
}

function primaryButtonStyle(busy: boolean): React.CSSProperties {
  return {
    background: "#2563eb",
    color: "white",
    border: "none",
    borderRadius: 6,
    padding: "8px 16px",
    cursor: busy ? "wait" : "pointer",
    opacity: busy ? 0.7 : 1,
  };
}

function secondaryButtonStyle(busy: boolean): React.CSSProperties {
  return {
    background: "white",
    color: "#2563eb",
    border: "1px solid #2563eb",
    borderRadius: 6,
    padding: "8px 16px",
    cursor: busy ? "wait" : "pointer",
    opacity: busy ? 0.7 : 1,
  };
}



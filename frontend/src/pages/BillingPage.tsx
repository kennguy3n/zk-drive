import { useEffect, useMemo, useState } from "react";
import { Link, useSearchParams } from "react-router-dom";
import {
  createCheckoutSession,
  createPortalSession,
  fetchBillingUsage,
  type BillingUsageSummary,
} from "../api/client";
import { useAuth } from "../hooks/useAuth";

// Tier metadata mirrors the canonical strings in
// internal/billing/billing.go. The blurbs are kept short — full
// pricing lives on the marketing site, not in the admin shell.
const TIER_OPTIONS = [
  {
    id: "starter",
    label: "Starter",
    blurb: "10 GB / user pooled, 25 users, 100 GB bandwidth / month.",
  },
  {
    id: "business",
    label: "Business",
    blurb: "1 TB pooled, 250 users, 1 TB bandwidth / month, audit log.",
  },
  {
    id: "secure_business",
    label: "Secure Business",
    blurb: "Customer-managed keys, regional placement, larger limits.",
  },
] as const;

type TierID = (typeof TIER_OPTIONS)[number]["id"];

// BillingPage shows the workspace's current plan tier and usage-vs-limit
// bars. Admin-only because the underlying /api/admin/billing/usage
// endpoint is admin-only.
export default function BillingPage() {
  const { isAdmin } = useAuth();
  const [usage, setUsage] = useState<BillingUsageSummary | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [selectedTier, setSelectedTier] = useState<TierID>("business");
  const [busy, setBusy] = useState<"checkout" | "portal" | null>(null);
  const [searchParams, setSearchParams] = useSearchParams();

  const banner = useMemo(() => bannerFromSearch(searchParams), [searchParams]);

  useEffect(() => {
    (async () => {
      try {
        setUsage(await fetchBillingUsage());
      } catch (e) {
        setError(e instanceof Error ? e.message : String(e));
      }
    })();
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
      setError(formatApiError(e, "Could not start checkout"));
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
      setError(formatApiError(e, "Could not open the customer portal"));
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
        <h2>Admin only</h2>
        <p>
          Billing is restricted to workspace administrators.{" "}
          <Link to="/drive">Back to drive</Link>
        </p>
      </div>
    );
  }
  return (
    <div style={{ padding: 24 }}>
      <header style={{ display: "flex", justifyContent: "space-between", alignItems: "center", marginBottom: 16 }}>
        <h1 style={{ margin: 0 }}>Billing</h1>
        <div>
          <Link to="/admin" style={{ marginRight: 16 }}>
            Admin
          </Link>
          <Link to="/drive">Back to drive</Link>
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
            Dismiss
          </button>
        </div>
      )}
      {error && <p style={{ color: "#b91c1c" }}>{error}</p>}
      {usage && (
        <div>
          <h2 style={{ marginBottom: 4 }}>Plan: {usage.tier}</h2>
          {!usage.plan_configured && (
            <p style={{ color: "#6b7280", marginTop: 0 }}>
              No plan row configured — showing free-tier defaults.
            </p>
          )}

          <section style={{ marginTop: 24, marginBottom: 24 }}>
            <h3 style={{ marginBottom: 8 }}>Manage subscription</h3>
            <p style={{ color: "#6b7280", marginTop: 0 }}>
              Pick a tier and start a Stripe Checkout flow, or open the
              customer portal to update payment details, swap tiers, or
              cancel.
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
              <legend style={{ padding: "0 6px", color: "#374151" }}>Choose a tier</legend>
              {TIER_OPTIONS.map((opt) => {
                const active = selectedTier === opt.id;
                const current = usage.tier === opt.id;
                return (
                  <label
                    key={opt.id}
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
                        value={opt.id}
                        checked={active}
                        onChange={() => setSelectedTier(opt.id)}
                      />
                      <strong>{opt.label}</strong>
                      {current && <span style={{ color: "#059669", fontSize: 12 }}>(current)</span>}
                    </div>
                    <div style={{ color: "#6b7280", fontSize: 13, marginTop: 6 }}>{opt.blurb}</div>
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
                {busy === "checkout" ? "Redirecting…" : "Upgrade plan"}
              </button>
              {usage.plan_configured && (
                <button
                  type="button"
                  onClick={handleManage}
                  disabled={busy !== null}
                  style={secondaryButtonStyle(busy === "portal")}
                >
                  {busy === "portal" ? "Redirecting…" : "Manage subscription"}
                </button>
              )}
            </div>
          </section>

          <UsageBar label="Storage" used={usage.storage_used_bytes} limit={usage.storage_limit_bytes} bytes />
          <UsageBar
            label="Bandwidth (this month)"
            used={usage.bandwidth_used_bytes_month}
            limit={usage.bandwidth_limit_bytes_month}
            bytes
          />
          <UsageBar label="Users" used={usage.user_count} limit={usage.user_limit} />
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

function bannerFromSearch(params: URLSearchParams): { tone: "success" | "warning"; message: string } | null {
  switch (params.get("stripe")) {
    case "success":
      return {
        tone: "success",
        message:
          "Checkout complete — your plan will update once Stripe delivers the webhook (usually within a few seconds).",
      };
    case "cancel":
      return {
        tone: "warning",
        message: "Checkout was cancelled. No changes were made to your plan.",
      };
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

function formatApiError(e: unknown, fallback: string): string {
  if (e && typeof e === "object" && "response" in e) {
    const resp = (e as { response?: { data?: unknown; status?: number } }).response;
    if (resp) {
      if (typeof resp.data === "string" && resp.data.trim() !== "") {
        return `${fallback}: ${resp.data.trim()}`;
      }
      if (resp.status === 501) {
        return `${fallback}: Stripe is not configured on this server.`;
      }
      if (resp.status === 412) {
        return `${fallback}: this workspace has no active Stripe subscription yet.`;
      }
      if (resp.status) {
        return `${fallback}: HTTP ${resp.status}`;
      }
    }
  }
  return e instanceof Error ? `${fallback}: ${e.message}` : `${fallback}: ${String(e)}`;
}

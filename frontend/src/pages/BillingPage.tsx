import { useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { fetchBillingUsage, type BillingUsageSummary } from "../api/client";
import { useAuth } from "../hooks/useAuth";

// BillingPage shows the workspace's current plan tier and usage-vs-limit
// bars. Admin-only because the underlying /api/admin/billing/usage
// endpoint is admin-only.
export default function BillingPage() {
  const { isAdmin } = useAuth();
  const [usage, setUsage] = useState<BillingUsageSummary | null>(null);
  const [error, setError] = useState<string | null>(null);
  useEffect(() => {
    (async () => {
      try {
        setUsage(await fetchBillingUsage());
      } catch (e) {
        setError(e instanceof Error ? e.message : String(e));
      }
    })();
  }, []);
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
      {error && <p style={{ color: "#b91c1c" }}>{error}</p>}
      {usage && (
        <div>
          <h2>Plan: {usage.tier}</h2>
          {!usage.plan_configured && (
            <p style={{ color: "#6b7280" }}>
              No plan row configured — showing free-tier defaults.
            </p>
          )}
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

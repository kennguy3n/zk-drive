import { useCallback, useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { useTranslation } from "react-i18next";
import {
  deactivateUser,
  deleteRetentionPolicy,
  fetchAuditLog,
  fetchHealthDashboard,
  fetchRetentionPolicies,
  fetchStorageUsage,
  fetchUsers,
  inviteUser,
  updateUserRole,
  upsertRetentionPolicy,
  type AdminUser,
  type AuditEntry,
  type HealthColor,
  type HealthReport,
  type HealthSubsystem,
  type RetentionPolicy,
  type StorageUsage,
} from "../api/client";
import { translateApiError } from "../api/errors";
import { useAuth } from "../hooks/useAuth";
import { useFeatures } from "../hooks/useFeatures";
import { Feature } from "../features/featureKeys";
import { Skeleton } from "../components/ui/Skeleton";

type Tab = "users" | "audit" | "retention" | "storage" | "health";

// Admin tabs that require a feature flag. "users" and "storage" are baseline
// admin surfaces (always available to an admin); the rest are progressive
// disclosure tied to billing tier.
const TAB_FEATURE: Partial<Record<Tab, string>> = {
  audit: Feature.AuditLog,
  retention: Feature.RetentionPolicies,
};

// AdminPage is the single-page admin console. Sub-views are tab-switched
// inline (rather than separate routes) because the underlying data sets
// are small and the user almost always wants to flip between them. All
// user-facing copy is sourced from the "admin" i18n namespace so the
// console localizes alongside the rest of the SPA.
export default function AdminPage() {
  const { isAdmin, logout } = useAuth();
  const { isEnabled, loaded: featuresLoaded } = useFeatures();
  const { t } = useTranslation();
  const [tab, setTab] = useState<Tab>("users");

  const visibleTabs = (["users", "audit", "retention", "storage", "health"] as Tab[]).filter(
    (id) => {
      const feature = TAB_FEATURE[id];
      return !feature || isEnabled(feature);
    },
  );
  // If the active tab gets gated out (e.g. tier downgrade, or it was selected
  // before features resolved), fall back to the first visible tab so we never
  // render a hidden sub-view.
  const activeTab = visibleTabs.includes(tab) ? tab : visibleTabs[0];

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
        <h1 style={{ margin: 0 }}>{t("nav.admin")}</h1>
        <div>
          <Link to="/admin/placement" style={{ marginRight: 16 }}>
            {t("admin.placement")}
          </Link>
          <Link to="/admin/encryption" style={{ marginRight: 16 }}>
            {t("admin.encryption")}
          </Link>
          {isEnabled(Feature.KChat) ? (
            <Link to="/admin/kchat" style={{ marginRight: 16 }}>
              {t("nav.kchatRooms")}
            </Link>
          ) : null}
          <Link to="/billing" style={{ marginRight: 16 }}>
            {t("nav.billing")}
          </Link>
          <Link to="/drive" style={{ marginRight: 16 }}>
            {t("admin.backToDrive")}
          </Link>
          <button onClick={logout}>{t("auth.logout")}</button>
        </div>
      </header>
      <nav
        style={{
          display: "flex",
          gap: 4,
          borderBottom: "1px solid #e5e7eb",
          marginBottom: 16,
        }}
        role={featuresLoaded ? undefined : "status"}
        aria-label={featuresLoaded ? undefined : t("common.loading")}
      >
        {featuresLoaded
          ? visibleTabs.map((id) => (
              <button
                key={id}
                onClick={() => setTab(id)}
                style={{
                  padding: "8px 12px",
                  background: activeTab === id ? "#eff6ff" : "transparent",
                  border: "none",
                  borderBottom:
                    activeTab === id
                      ? "2px solid #2563eb"
                      : "2px solid transparent",
                  cursor: "pointer",
                }}
              >
                {t(`admin.tab.${id}`)}
              </button>
            ))
          : // Until /api/features resolves, isEnabled() is fail-closed (false)
            // for every gated tab, so rendering the real strip now would show
            // only the baseline tabs and then visibly pop the audit/retention
            // tabs in once features load. Show a same-height skeleton strip
            // instead so the final tabs replace placeholders rather than
            // appearing from nothing. Size it to visibleTabs.length — which,
            // while loading, is exactly the always-on baseline tabs (users,
            // storage, health) — so a Free workspace settles with zero layout
            // shift; a higher tier reveals its extra tabs additively rather
            // than the strip shrinking from a hardcoded count.
            Array.from({ length: visibleTabs.length }).map((_, i) => (
              <Skeleton
                key={i}
                style={{ height: 34, width: 84, borderRadius: 6 }}
              />
            ))}
      </nav>
      {activeTab === "users" && <UsersTab />}
      {activeTab === "audit" && <AuditTab />}
      {activeTab === "retention" && <RetentionTab />}
      {activeTab === "storage" && <StorageTab />}
      {activeTab === "health" && <HealthTab />}
    </div>
  );
}

// healthColors maps each traffic-light status to its pill colours.
// Kept module-level so it isn't reallocated per render.
const healthColors: Record<HealthColor, { bg: string; fg: string; dot: string }> = {
  green: { bg: "#dcfce7", fg: "#166534", dot: "#22c55e" },
  yellow: { bg: "#fef9c3", fg: "#854d0e", dot: "#eab308" },
  red: { bg: "#fee2e2", fg: "#991b1b", dot: "#ef4444" },
  unknown: { bg: "#f1f5f9", fg: "#475569", dot: "#94a3b8" },
};

function HealthPill({ status }: { status: HealthColor }) {
  const { t } = useTranslation();
  const c = healthColors[status] ?? healthColors.unknown;
  const labelKey = `admin.health.status${status.charAt(0).toUpperCase()}${status.slice(1)}`;
  return (
    <span
      style={{
        display: "inline-flex",
        alignItems: "center",
        gap: 6,
        padding: "2px 10px",
        borderRadius: 999,
        background: c.bg,
        color: c.fg,
        fontSize: 13,
        fontWeight: 600,
      }}
    >
      <span
        style={{
          width: 8,
          height: 8,
          borderRadius: "50%",
          background: c.dot,
          display: "inline-block",
        }}
      />
      {t(labelKey)}
    </span>
  );
}

// formatDetailValue renders a single opaque detail value. Objects and
// arrays are JSON-stringified compactly; primitives pass through. This
// keeps the renderer robust against the per-subsystem detail bags
// evolving server-side without a frontend change.
function formatDetailValue(value: unknown): string {
  if (value === null || value === undefined) return "—";
  if (typeof value === "object") {
    try {
      return JSON.stringify(value);
    } catch {
      return String(value);
    }
  }
  return String(value);
}

function SubsystemCard({ sub }: { sub: HealthSubsystem }) {
  const details = sub.detail ? Object.entries(sub.detail) : [];
  return (
    <div
      style={{
        border: "1px solid #e5e7eb",
        borderRadius: 8,
        padding: 16,
        background: "#fff",
        display: "flex",
        flexDirection: "column",
        gap: 10,
      }}
    >
      <div style={{ display: "flex", alignItems: "center", justifyContent: "space-between", gap: 8 }}>
        <span style={{ fontWeight: 600, textTransform: "capitalize" }}>{sub.name}</span>
        <HealthPill status={sub.status} />
      </div>
      {sub.error && <div style={{ color: "#b91c1c", fontSize: 13 }}>{sub.error}</div>}
      {details.length > 0 && (
        <dl style={{ margin: 0, display: "grid", gridTemplateColumns: "auto 1fr", gap: "2px 12px", fontSize: 13 }}>
          {details.map(([k, v]) => (
            <div key={k} style={{ display: "contents" }}>
              <dt style={{ color: "#6b7280" }}>{k}</dt>
              <dd style={{ margin: 0, fontFamily: "monospace", wordBreak: "break-word" }}>{formatDetailValue(v)}</dd>
            </div>
          ))}
        </dl>
      )}
    </div>
  );
}

// HealthTab renders the traffic-light health dashboard (WS8 8.1). It
// polls GET /api/admin/health-dashboard on mount and on a 15s interval
// (auto-refresh, toggleable) so an operator watching the page sees a
// subsystem recover/degrade without manual reloads. The endpoint always
// returns 200 with the report in the body, so a degraded subsystem is
// rendered, not surfaced as a request error.
function HealthTab() {
  const { t } = useTranslation();
  const [report, setReport] = useState<HealthReport | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);
  const [auto, setAuto] = useState(true);

  const refresh = useCallback(async () => {
    setLoading(true);
    try {
      setReport(await fetchHealthDashboard());
      setError(null);
    } catch (e) {
      setError(translateApiError(e, t));
    } finally {
      setLoading(false);
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  useEffect(() => {
    refresh();
  }, [refresh]);

  useEffect(() => {
    if (!auto) return;
    const id = window.setInterval(refresh, 15000);
    return () => window.clearInterval(id);
  }, [auto, refresh]);

  return (
    <section>
      <div style={{ display: "flex", alignItems: "center", justifyContent: "space-between", gap: 12, flexWrap: "wrap" }}>
        <div>
          <h2 style={{ marginBottom: 4 }}>{t("admin.health.title")}</h2>
          <p style={{ margin: 0, color: "#6b7280", fontSize: 14, maxWidth: 640 }}>{t("admin.health.subtitle")}</p>
        </div>
        <div style={{ display: "flex", alignItems: "center", gap: 12 }}>
          {report && (
            <span style={{ display: "flex", alignItems: "center", gap: 8 }}>
              <span style={{ color: "#6b7280", fontSize: 13 }}>{t("admin.health.overall")}</span>
              <HealthPill status={report.status} />
            </span>
          )}
          <label style={{ display: "flex", alignItems: "center", gap: 6, fontSize: 13, color: "#374151" }}>
            <input type="checkbox" checked={auto} onChange={(e) => setAuto(e.target.checked)} />
            {t("admin.health.autoRefresh")}
          </label>
          <button onClick={refresh} disabled={loading}>
            {t("admin.health.refresh")}
          </button>
        </div>
      </div>

      {error && <p style={{ color: "#b91c1c" }}>{error}</p>}

      {report && report.subsystems.length === 0 && <p>{t("admin.health.noSubsystems")}</p>}

      {report && report.subsystems.length > 0 && (
        <div
          style={{
            display: "grid",
            gridTemplateColumns: "repeat(auto-fill, minmax(280px, 1fr))",
            gap: 12,
            marginTop: 16,
          }}
        >
          {report.subsystems.map((sub) => (
            <SubsystemCard key={sub.name} sub={sub} />
          ))}
        </div>
      )}

      {report && (
        <p style={{ color: "#9ca3af", fontSize: 12, marginTop: 16 }}>
          {t("admin.health.lastUpdated", { time: new Date(report.generated_at).toLocaleTimeString() })}
        </p>
      )}
    </section>
  );
}

function UsersTab() {
  const { t } = useTranslation();
  const [users, setUsers] = useState<AdminUser[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [form, setForm] = useState({ email: "", name: "", password: "", role: "member" });
  const refresh = useCallback(async () => {
    try {
      setUsers(await fetchUsers());
    } catch (e) {
      setError(translateApiError(e, t));
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);
  useEffect(() => {
    refresh();
  }, [refresh]);
  return (
    <section>
      <h2>{t("admin.users")}</h2>
      {error && <p style={{ color: "#b91c1c" }}>{error}</p>}
      <form
        onSubmit={async (e) => {
          e.preventDefault();
          try {
            await inviteUser(form);
            setForm({ email: "", name: "", password: "", role: "member" });
            await refresh();
          } catch (err) {
            setError(translateApiError(err, t));
          }
        }}
        style={{ display: "flex", gap: 8, marginBottom: 16 }}
      >
        <input
          placeholder={t("admin.emailPlaceholder")}
          value={form.email}
          onChange={(e) => setForm({ ...form, email: e.target.value })}
          required
        />
        <input
          placeholder={t("admin.namePlaceholder")}
          value={form.name}
          onChange={(e) => setForm({ ...form, name: e.target.value })}
          required
        />
        <input
          placeholder={t("admin.tempPasswordPlaceholder")}
          type="password"
          value={form.password}
          onChange={(e) => setForm({ ...form, password: e.target.value })}
          required
        />
        <select value={form.role} onChange={(e) => setForm({ ...form, role: e.target.value })}>
          <option value="member">{t("admin.roleMember")}</option>
          <option value="admin">{t("admin.roleAdmin")}</option>
        </select>
        <button type="submit">{t("admin.invite")}</button>
      </form>
      <table style={{ width: "100%", borderCollapse: "collapse" }}>
        <thead>
          <tr style={{ textAlign: "left", borderBottom: "1px solid #e5e7eb" }}>
            <th style={th}>{t("admin.colEmail")}</th>
            <th style={th}>{t("admin.colName")}</th>
            <th style={th}>{t("admin.colRole")}</th>
            <th style={th}>{t("admin.colStatus")}</th>
            <th style={th}>{t("common.actions")}</th>
          </tr>
        </thead>
        <tbody>
          {users.map((u) => (
            <tr key={u.id} style={{ borderBottom: "1px solid #f3f4f6" }}>
              <td style={td}>{u.email}</td>
              <td style={td}>{u.name}</td>
              <td style={td}>
                <select
                  value={u.role}
                  onChange={async (e) => {
                    try {
                      await updateUserRole(u.id, e.target.value);
                      await refresh();
                    } catch (err) {
                      setError(translateApiError(err, t));
                    }
                  }}
                >
                  <option value="member">{t("admin.roleMember")}</option>
                  <option value="admin">{t("admin.roleAdmin")}</option>
                </select>
              </td>
              <td style={td}>{u.deactivated_at ? t("admin.statusDeactivated") : t("admin.statusActive")}</td>
              <td style={td}>
                {!u.deactivated_at && (
                  <button
                    onClick={async () => {
                      if (!confirm(t("admin.deactivateConfirm", { email: u.email }))) return;
                      try {
                        await deactivateUser(u.id);
                        await refresh();
                      } catch (err) {
                        setError(translateApiError(err, t));
                      }
                    }}
                  >
                    {t("admin.deactivate")}
                  </button>
                )}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </section>
  );
}

function AuditTab() {
  const { t } = useTranslation();
  const [entries, setEntries] = useState<AuditEntry[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [action, setAction] = useState<string>("");
  const [offset, setOffset] = useState(0);
  const limit = 50;

  useEffect(() => {
    (async () => {
      try {
        setEntries(await fetchAuditLog({ action: action || undefined, offset, limit }));
      } catch (e) {
        setError(translateApiError(e, t));
      }
    })();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [action, offset]);

  return (
    <section>
      <h2>{t("admin.auditLog")}</h2>
      {error && <p style={{ color: "#b91c1c" }}>{error}</p>}
      <div style={{ marginBottom: 12 }}>
        <label>
          {t("admin.filterAction")}{" "}
          <input
            value={action}
            onChange={(e) => setAction(e.target.value)}
            placeholder={t("admin.filterActionPlaceholder")}
          />
        </label>
      </div>
      <table style={{ width: "100%", borderCollapse: "collapse" }}>
        <thead>
          <tr style={{ textAlign: "left", borderBottom: "1px solid #e5e7eb" }}>
            <th style={th}>{t("admin.colTime")}</th>
            <th style={th}>{t("admin.colActor")}</th>
            <th style={th}>{t("admin.colAction")}</th>
            <th style={th}>{t("admin.colResource")}</th>
          </tr>
        </thead>
        <tbody>
          {entries.map((e) => (
            <tr key={e.id} style={{ borderBottom: "1px solid #f3f4f6" }}>
              <td style={td}>{new Date(e.created_at).toLocaleString()}</td>
              <td style={td}>{e.actor_id ?? "-"}</td>
              <td style={td}>{e.action}</td>
              <td style={td}>
                {e.resource_type ?? "-"} {e.resource_id ?? ""}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
      <div style={{ marginTop: 12 }}>
        <button onClick={() => setOffset(Math.max(0, offset - limit))} disabled={offset === 0}>
          {t("common.prev")}
        </button>{" "}
        <button onClick={() => setOffset(offset + limit)} disabled={entries.length < limit}>
          {t("common.next")}
        </button>
      </div>
    </section>
  );
}

function RetentionTab() {
  const { t } = useTranslation();
  const [policies, setPolicies] = useState<RetentionPolicy[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [form, setForm] = useState<{
    max_versions: string;
    max_age_days: string;
    archive_after_days: string;
  }>({ max_versions: "", max_age_days: "30", archive_after_days: "" });
  const refresh = useCallback(async () => {
    try {
      setPolicies(await fetchRetentionPolicies());
    } catch (e) {
      setError(translateApiError(e, t));
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);
  useEffect(() => {
    refresh();
  }, [refresh]);
  const parseOpt = (s: string): number | null => {
    const n = Number(s);
    return s === "" || Number.isNaN(n) ? null : n;
  };
  const summarise = (p: RetentionPolicy): string => {
    const parts: string[] = [];
    if (p.max_versions != null) parts.push(t("admin.retentionVersions", { count: p.max_versions }));
    if (p.max_age_days != null) parts.push(t("admin.retentionMaxAge", { count: p.max_age_days }));
    if (p.archive_after_days != null)
      parts.push(t("admin.retentionArchiveAfter", { count: p.archive_after_days }));
    return parts.length === 0 ? t("admin.retentionNoLimits") : parts.join(", ");
  };
  return (
    <section>
      <h2>{t("admin.retentionPolicies")}</h2>
      {error && <p style={{ color: "#b91c1c" }}>{error}</p>}
      <form
        onSubmit={async (e) => {
          e.preventDefault();
          try {
            await upsertRetentionPolicy({
              max_versions: parseOpt(form.max_versions),
              max_age_days: parseOpt(form.max_age_days),
              archive_after_days: parseOpt(form.archive_after_days),
            });
            await refresh();
          } catch (err) {
            setError(translateApiError(err, t));
          }
        }}
        style={{ display: "flex", gap: 8, marginBottom: 16, flexWrap: "wrap" }}
      >
        <label>
          {t("admin.maxVersions")}{" "}
          <input
            type="number"
            min={1}
            placeholder={t("admin.unsetPlaceholder")}
            value={form.max_versions}
            onChange={(e) => setForm({ ...form, max_versions: e.target.value })}
          />
        </label>
        <label>
          {t("admin.maxAgeDays")}{" "}
          <input
            type="number"
            min={1}
            placeholder={t("admin.unsetPlaceholder")}
            value={form.max_age_days}
            onChange={(e) => setForm({ ...form, max_age_days: e.target.value })}
          />
        </label>
        <label>
          {t("admin.archiveAfterDays")}{" "}
          <input
            type="number"
            min={1}
            placeholder={t("admin.unsetPlaceholder")}
            value={form.archive_after_days}
            onChange={(e) => setForm({ ...form, archive_after_days: e.target.value })}
          />
        </label>
        <button type="submit">{t("admin.savePolicy")}</button>
      </form>
      <ul>
        {policies.map((p) => (
          <li key={p.id} style={{ marginBottom: 8 }}>
            {p.folder_id
              ? t("admin.retentionFolderLabel", { id: p.folder_id })
              : t("admin.retentionWorkspaceDefault")}{" "}
            — {summarise(p)}{" "}
            <button
              onClick={async () => {
                if (!confirm(t("admin.deletePolicyConfirm"))) return;
                try {
                  await deleteRetentionPolicy(p.id);
                  await refresh();
                } catch (err) {
                  setError(translateApiError(err, t));
                }
              }}
            >
              {t("common.delete")}
            </button>
          </li>
        ))}
      </ul>
    </section>
  );
}

function StorageTab() {
  const { t } = useTranslation();
  const [usage, setUsage] = useState<StorageUsage | null>(null);
  const [error, setError] = useState<string | null>(null);
  useEffect(() => {
    (async () => {
      try {
        setUsage(await fetchStorageUsage());
      } catch (e) {
        setError(translateApiError(e, t));
      }
    })();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);
  return (
    <section>
      <h2>{t("admin.storage")}</h2>
      {error && <p style={{ color: "#b91c1c" }}>{error}</p>}
      {usage && (
        <div>
          <p>{t("admin.storageTotal", { bytes: formatBytes(usage.total_bytes) })}</p>
          <table style={{ width: "100%", borderCollapse: "collapse" }}>
            <thead>
              <tr style={{ textAlign: "left", borderBottom: "1px solid #e5e7eb" }}>
                <th style={th}>{t("admin.colUser")}</th>
                <th style={th}>{t("admin.colBytes")}</th>
                <th style={th}>{t("admin.colShare")}</th>
              </tr>
            </thead>
            <tbody>
              {(usage.per_user ?? []).map((u) => {
                const pct = usage.total_bytes > 0 ? (u.total_bytes / usage.total_bytes) * 100 : 0;
                return (
                  <tr key={u.user_id} style={{ borderBottom: "1px solid #f3f4f6" }}>
                    <td style={td}>{u.email}</td>
                    <td style={td}>{formatBytes(u.total_bytes)}</td>
                    <td style={td}>
                      <div style={{ background: "#e5e7eb", width: 200, height: 8, borderRadius: 4 }}>
                        <div
                          style={{
                            background: "#2563eb",
                            width: `${pct}%`,
                            height: 8,
                            borderRadius: 4,
                          }}
                        />
                      </div>
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      )}
    </section>
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

const th: React.CSSProperties = { padding: "8px 12px", fontSize: 12, color: "#6b7280" };
const td: React.CSSProperties = { padding: "8px 12px", fontSize: 13, color: "#374151" };

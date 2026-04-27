import { useCallback, useEffect, useState } from "react";
import { Link } from "react-router-dom";
import {
  deactivateUser,
  deleteRetentionPolicy,
  fetchAuditLog,
  fetchRetentionPolicies,
  fetchStorageUsage,
  fetchUsers,
  inviteUser,
  updateUserRole,
  upsertRetentionPolicy,
  type AdminUser,
  type AuditEntry,
  type RetentionPolicy,
  type StorageUsage,
} from "../api/client";
import { useAuth } from "../hooks/useAuth";

type Tab = "users" | "audit" | "retention" | "storage";

// AdminPage is the single-page admin console. Sub-views are tab-switched
// inline (rather than separate routes) because the underlying data sets
// are small and the user almost always wants to flip between them.
export default function AdminPage() {
  const { isAdmin, logout } = useAuth();
  const [tab, setTab] = useState<Tab>("users");

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
        <h1 style={{ margin: 0 }}>Admin</h1>
        <div>
          <Link to="/admin/placement" style={{ marginRight: 16 }}>
            Placement
          </Link>
          <Link to="/admin/encryption" style={{ marginRight: 16 }}>
            Encryption
          </Link>
          <Link to="/billing" style={{ marginRight: 16 }}>
            Billing
          </Link>
          <Link to="/drive" style={{ marginRight: 16 }}>
            Back to drive
          </Link>
          <button onClick={logout}>Log out</button>
        </div>
      </header>
      <nav
        style={{
          display: "flex",
          gap: 4,
          borderBottom: "1px solid #e5e7eb",
          marginBottom: 16,
        }}
      >
        {(["users", "audit", "retention", "storage"] as Tab[]).map((t) => (
          <button
            key={t}
            onClick={() => setTab(t)}
            style={{
              padding: "8px 12px",
              background: tab === t ? "#eff6ff" : "transparent",
              border: "none",
              borderBottom: tab === t ? "2px solid #2563eb" : "2px solid transparent",
              cursor: "pointer",
            }}
          >
            {t.charAt(0).toUpperCase() + t.slice(1)}
          </button>
        ))}
      </nav>
      {tab === "users" && <UsersTab />}
      {tab === "audit" && <AuditTab />}
      {tab === "retention" && <RetentionTab />}
      {tab === "storage" && <StorageTab />}
    </div>
  );
}

function UsersTab() {
  const [users, setUsers] = useState<AdminUser[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [form, setForm] = useState({ email: "", name: "", password: "", role: "member" });
  const refresh = useCallback(async () => {
    try {
      setUsers(await fetchUsers());
    } catch (e) {
      setError(errMessage(e));
    }
  }, []);
  useEffect(() => {
    refresh();
  }, [refresh]);
  return (
    <section>
      <h2>Users</h2>
      {error && <p style={{ color: "#b91c1c" }}>{error}</p>}
      <form
        onSubmit={async (e) => {
          e.preventDefault();
          try {
            await inviteUser(form);
            setForm({ email: "", name: "", password: "", role: "member" });
            await refresh();
          } catch (err) {
            setError(errMessage(err));
          }
        }}
        style={{ display: "flex", gap: 8, marginBottom: 16 }}
      >
        <input
          placeholder="email"
          value={form.email}
          onChange={(e) => setForm({ ...form, email: e.target.value })}
          required
        />
        <input
          placeholder="name"
          value={form.name}
          onChange={(e) => setForm({ ...form, name: e.target.value })}
          required
        />
        <input
          placeholder="temp password"
          type="password"
          value={form.password}
          onChange={(e) => setForm({ ...form, password: e.target.value })}
          required
        />
        <select value={form.role} onChange={(e) => setForm({ ...form, role: e.target.value })}>
          <option value="member">member</option>
          <option value="admin">admin</option>
        </select>
        <button type="submit">Invite</button>
      </form>
      <table style={{ width: "100%", borderCollapse: "collapse" }}>
        <thead>
          <tr style={{ textAlign: "left", borderBottom: "1px solid #e5e7eb" }}>
            <th style={th}>Email</th>
            <th style={th}>Name</th>
            <th style={th}>Role</th>
            <th style={th}>Status</th>
            <th style={th}>Actions</th>
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
                      setError(errMessage(err));
                    }
                  }}
                >
                  <option value="member">member</option>
                  <option value="admin">admin</option>
                </select>
              </td>
              <td style={td}>{u.deactivated_at ? "deactivated" : "active"}</td>
              <td style={td}>
                {!u.deactivated_at && (
                  <button
                    onClick={async () => {
                      if (!confirm(`Deactivate ${u.email}?`)) return;
                      try {
                        await deactivateUser(u.id);
                        await refresh();
                      } catch (err) {
                        setError(errMessage(err));
                      }
                    }}
                  >
                    Deactivate
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
        setError(errMessage(e));
      }
    })();
  }, [action, offset]);

  return (
    <section>
      <h2>Audit log</h2>
      {error && <p style={{ color: "#b91c1c" }}>{error}</p>}
      <div style={{ marginBottom: 12 }}>
        <label>
          Filter action:{" "}
          <input value={action} onChange={(e) => setAction(e.target.value)} placeholder="e.g. file.upload" />
        </label>
      </div>
      <table style={{ width: "100%", borderCollapse: "collapse" }}>
        <thead>
          <tr style={{ textAlign: "left", borderBottom: "1px solid #e5e7eb" }}>
            <th style={th}>Time</th>
            <th style={th}>Actor</th>
            <th style={th}>Action</th>
            <th style={th}>Resource</th>
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
          Prev
        </button>{" "}
        <button onClick={() => setOffset(offset + limit)} disabled={entries.length < limit}>
          Next
        </button>
      </div>
    </section>
  );
}

function RetentionTab() {
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
      setError(errMessage(e));
    }
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
    if (p.max_versions != null) parts.push(`${p.max_versions} versions`);
    if (p.max_age_days != null) parts.push(`${p.max_age_days}d max age`);
    if (p.archive_after_days != null) parts.push(`archive after ${p.archive_after_days}d`);
    return parts.length === 0 ? "no limits configured" : parts.join(", ");
  };
  return (
    <section>
      <h2>Retention policies</h2>
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
            setError(errMessage(err));
          }
        }}
        style={{ display: "flex", gap: 8, marginBottom: 16, flexWrap: "wrap" }}
      >
        <label>
          Max versions{" "}
          <input
            type="number"
            min={1}
            placeholder="unset"
            value={form.max_versions}
            onChange={(e) => setForm({ ...form, max_versions: e.target.value })}
          />
        </label>
        <label>
          Max age (days){" "}
          <input
            type="number"
            min={1}
            placeholder="unset"
            value={form.max_age_days}
            onChange={(e) => setForm({ ...form, max_age_days: e.target.value })}
          />
        </label>
        <label>
          Archive after (days){" "}
          <input
            type="number"
            min={1}
            placeholder="unset"
            value={form.archive_after_days}
            onChange={(e) => setForm({ ...form, archive_after_days: e.target.value })}
          />
        </label>
        <button type="submit">Save policy</button>
      </form>
      <ul>
        {policies.map((p) => (
          <li key={p.id} style={{ marginBottom: 8 }}>
            {p.folder_id ? `folder ${p.folder_id}` : "workspace default"} — {summarise(p)}{" "}
            <button
              onClick={async () => {
                if (!confirm("Delete policy?")) return;
                try {
                  await deleteRetentionPolicy(p.id);
                  await refresh();
                } catch (err) {
                  setError(errMessage(err));
                }
              }}
            >
              Delete
            </button>
          </li>
        ))}
      </ul>
    </section>
  );
}

function StorageTab() {
  const [usage, setUsage] = useState<StorageUsage | null>(null);
  const [error, setError] = useState<string | null>(null);
  useEffect(() => {
    (async () => {
      try {
        setUsage(await fetchStorageUsage());
      } catch (e) {
        setError(errMessage(e));
      }
    })();
  }, []);
  return (
    <section>
      <h2>Storage</h2>
      {error && <p style={{ color: "#b91c1c" }}>{error}</p>}
      {usage && (
        <div>
          <p>Total: {formatBytes(usage.total_bytes)}</p>
          <table style={{ width: "100%", borderCollapse: "collapse" }}>
            <thead>
              <tr style={{ textAlign: "left", borderBottom: "1px solid #e5e7eb" }}>
                <th style={th}>User</th>
                <th style={th}>Bytes</th>
                <th style={th}>Share</th>
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

function errMessage(e: unknown): string {
  if (e && typeof e === "object" && "message" in e) {
    return String((e as { message: unknown }).message);
  }
  return String(e);
}

const th: React.CSSProperties = { padding: "8px 12px", fontSize: 12, color: "#6b7280" };
const td: React.CSSProperties = { padding: "8px 12px", fontSize: 13, color: "#374151" };

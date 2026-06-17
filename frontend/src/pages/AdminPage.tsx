import { useCallback, useEffect, useMemo, useState, type ReactNode } from "react";
import { Link, NavLink, useNavigate } from "react-router-dom";
import { useTranslation } from "react-i18next";
import {
  Activity,
  Archive,
  CreditCard,
  HardDrive,
  KeyRound,
  LogOut,
  MapPin,
  MessagesSquare,
  Plus,
  RefreshCw,
  ScrollText,
  Search,
  Trash2,
  Users as UsersIcon,
  type LucideIcon,
} from "lucide-react";
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
import {
  AppShell,
  Badge,
  Button,
  EmptyState,
  Field,
  Input,
  Select,
  PageHeader,
  Skeleton,
  Table,
  TBody,
  THead,
  Td,
  Th,
  Tr,
  Tabs,
  useConfirm,
  useToast,
  type TabItem,
  type BadgeProps,
} from "../components/ui";
import { ThemeToggle } from "../components/ThemeToggle";
import { cn } from "../lib/cn";

type Tab = "users" | "audit" | "retention" | "storage" | "health";

// Admin tabs that require a feature flag. "users", "storage" and "health" are
// baseline admin surfaces (always available to an admin); the rest are
// progressive disclosure tied to billing tier.
const TAB_FEATURE: Partial<Record<Tab, string>> = {
  audit: Feature.AuditLog,
  retention: Feature.RetentionPolicies,
};

const TAB_ICON: Record<Tab, LucideIcon> = {
  users: UsersIcon,
  audit: ScrollText,
  retention: Archive,
  storage: HardDrive,
  health: Activity,
};

// AdminPage is the single-page admin console. Sub-views are tab-switched
// inline (rather than separate routes) because the underlying data sets
// are small and the user almost always wants to flip between them. All
// user-facing copy is sourced from the "admin" i18n namespace so the
// console localizes alongside the rest of the SPA.
export default function AdminPage() {
  const { isAdmin } = useAuth();
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
    return <AdminAccessDenied />;
  }

  const tabItems: TabItem<Tab>[] = visibleTabs.map((id) => ({
    value: id,
    label: t(`admin.tab.${id}`),
    icon: <TabIcon id={id} />,
  }));

  return (
    <AdminShell active="admin">
      <PageHeader title={t("nav.admin")} description={t("admin.consoleDescription")} />

      {featuresLoaded ? (
        <div className="mb-6">
          <Tabs
            tabs={tabItems}
            value={activeTab}
            onChange={setTab}
            aria-label={t("nav.admin")}
          />
        </div>
      ) : (
        // Until /api/features resolves, isEnabled() is fail-closed (false) for
        // every gated tab, so rendering the real strip now would show only the
        // baseline tabs and then visibly pop the audit/retention tabs in once
        // features load. Show a same-height skeleton strip instead so the final
        // tabs replace placeholders rather than appearing from nothing.
        <div
          className="mb-6 flex items-center gap-1 border-b border-border pb-2"
          role="status"
          aria-label={t("common.loading")}
        >
          {Array.from({ length: visibleTabs.length }).map((_, i) => (
            <Skeleton key={i} className="h-9 w-24 rounded-lg" />
          ))}
        </div>
      )}

      <div role="tabpanel" aria-label={t(`admin.tab.${activeTab}`)}>
        {activeTab === "users" && <UsersTab />}
        {activeTab === "audit" && <AuditTab />}
        {activeTab === "retention" && <RetentionTab />}
        {activeTab === "storage" && <StorageTab />}
        {activeTab === "health" && <HealthTab />}
      </div>
    </AdminShell>
  );
}

function TabIcon({ id }: { id: Tab }) {
  const Icon = TAB_ICON[id];
  return <Icon className="h-4 w-4" aria-hidden />;
}

// --- Shared admin chrome ------------------------------------------------

// AdminShell wraps every Admin-suite page in the shared AppShell with a
// consistent brand mark, section nav and theme/logout actions. It is kept
// local to this file per the workstream's "build new primitives locally"
// rule; the other admin pages render their own equivalent.
function AdminShell({ active, children }: { active: AdminSection; children: ReactNode }) {
  const { t } = useTranslation();
  const { logout } = useAuth();
  const { isEnabled } = useFeatures();

  return (
    <AppShell
      brand={<AdminBrand />}
      nav={<AdminNav active={active} kchatEnabled={isEnabled(Feature.KChat)} />}
      actions={
        <>
          <Link
            to="/drive"
            className="hidden rounded-full px-3 py-1.5 text-sm font-medium text-muted transition-colors hover:bg-surface-2 hover:text-fg sm:inline-flex"
          >
            {t("admin.backToDrive")}
          </Link>
          <ThemeToggle />
          <Button variant="secondary" size="sm" onClick={logout}>
            <LogOut className="h-4 w-4" aria-hidden />
            <span className="hidden sm:inline">{t("auth.logout")}</span>
          </Button>
        </>
      }
    >
      {children}
    </AppShell>
  );
}

function AdminBrand() {
  const { t } = useTranslation();
  return (
    <Link
      to="/admin"
      className="flex items-center gap-2 rounded-lg focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
    >
      <span className="flex h-7 w-7 items-center justify-center rounded-lg bg-brand-gradient text-xs font-bold text-white">
        zk
      </span>
      <span className="text-sm font-semibold text-fg">{t("nav.admin")}</span>
    </Link>
  );
}

type AdminSection = "admin" | "placement" | "encryption" | "kchat" | "billing";

function AdminNav({ active, kchatEnabled }: { active: AdminSection; kchatEnabled: boolean }) {
  const { t } = useTranslation();
  const items: { id: AdminSection; to: string; icon: LucideIcon; label: string; show: boolean }[] = [
    { id: "admin", to: "/admin", icon: UsersIcon, label: t("nav.admin"), show: true },
    { id: "placement", to: "/admin/placement", icon: MapPin, label: t("admin.placement"), show: true },
    { id: "encryption", to: "/admin/encryption", icon: KeyRound, label: t("admin.encryption"), show: true },
    { id: "kchat", to: "/admin/kchat", icon: MessagesSquare, label: t("nav.kchatRooms"), show: kchatEnabled },
    { id: "billing", to: "/billing", icon: CreditCard, label: t("nav.billing"), show: true },
  ];
  return (
    <div className="flex min-w-0 items-center gap-1 overflow-x-auto">
      {items
        .filter((i) => i.show)
        .map((i) => (
          <NavLink
            key={i.id}
            to={i.to}
            aria-current={active === i.id ? "page" : undefined}
            className={cn(
              "inline-flex shrink-0 items-center gap-1.5 rounded-full px-3 py-1.5 text-sm font-medium transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring",
              active === i.id
                ? "bg-brand/10 text-brand"
                : "text-muted hover:bg-surface-2 hover:text-fg",
            )}
          >
            <i.icon className="h-4 w-4" aria-hidden />
            <span className="hidden md:inline">{i.label}</span>
          </NavLink>
        ))}
    </div>
  );
}

function AdminAccessDenied() {
  const { t } = useTranslation();
  const navigate = useNavigate();
  return (
    <AppShell brand={<AdminBrand />} actions={<ThemeToggle />} maxWidth="md">
      <div className="mx-auto mt-10 max-w-md">
        <EmptyState
          icon={<KeyRound className="h-6 w-6" aria-hidden />}
          title={t("admin.adminOnly")}
          description={t("admin.adminOnlyDescription")}
          action={
            <Button variant="secondary" size="sm" onClick={() => navigate("/drive")}>
              {t("admin.backToDrive")}
            </Button>
          }
        />
      </div>
    </AppShell>
  );
}

// --- Shared building blocks --------------------------------------------

function Panel({ children, className }: { children: ReactNode; className?: string }) {
  return (
    <section className={cn("rounded-card border border-border bg-surface p-5 sm:p-6", className)}>
      {children}
    </section>
  );
}

function SectionHeading({
  title,
  description,
  actions,
}: {
  title: string;
  description?: string;
  actions?: ReactNode;
}) {
  return (
    <div className="mb-5 flex flex-col gap-2 sm:flex-row sm:items-start sm:justify-between">
      <div className="min-w-0">
        <h2 className="text-lg font-semibold text-fg">{title}</h2>
        {description && <p className="mt-1 max-w-2xl text-sm text-muted">{description}</p>}
      </div>
      {actions && <div className="flex shrink-0 flex-wrap items-center gap-2">{actions}</div>}
    </div>
  );
}

function ErrorBanner({ message }: { message: string }) {
  return (
    <div
      role="alert"
      className="mb-4 rounded-lg border border-danger/30 bg-danger/10 px-4 py-3 text-sm text-danger"
    >
      {message}
    </div>
  );
}

function RefreshButton({ onClick, loading }: { onClick: () => void; loading?: boolean }) {
  const { t } = useTranslation();
  return (
    <Button variant="ghost" size="sm" onClick={onClick} disabled={loading} aria-label={t("admin.refresh")}>
      <RefreshCw className={cn("h-4 w-4", loading && "animate-spin")} aria-hidden />
      <span className="hidden sm:inline">{t("admin.refresh")}</span>
    </Button>
  );
}

const EMAIL_RE = /^[^\s@]+@[^\s@]+\.[^\s@]+$/;
const MIN_PASSWORD = 8;

// --- Users -------------------------------------------------------------

function UsersTab() {
  const { t } = useTranslation();
  const toast = useToast();
  const confirm = useConfirm();
  const [users, setUsers] = useState<AdminUser[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);
  const [query, setQuery] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const [savingRole, setSavingRole] = useState<string | null>(null);
  const [deactivating, setDeactivating] = useState<string | null>(null);
  const [form, setForm] = useState({ email: "", name: "", password: "", role: "member" });
  const [fieldErrors, setFieldErrors] = useState<{ email?: string; name?: string; password?: string }>({});

  const refresh = useCallback(async () => {
    setLoading(true);
    try {
      setUsers(await fetchUsers());
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

  const filtered = useMemo(() => {
    const q = query.trim().toLowerCase();
    if (!q) return users;
    return users.filter(
      (u) => u.email.toLowerCase().includes(q) || u.name.toLowerCase().includes(q),
    );
  }, [users, query]);

  const validate = (): boolean => {
    const errs: { email?: string; name?: string; password?: string } = {};
    if (!form.email.trim()) errs.email = t("admin.emailRequired");
    else if (!EMAIL_RE.test(form.email.trim())) errs.email = t("admin.emailInvalid");
    if (!form.name.trim()) errs.name = t("admin.nameRequired");
    if (form.password.length < MIN_PASSWORD) errs.password = t("admin.passwordTooShort");
    setFieldErrors(errs);
    return Object.keys(errs).length === 0;
  };

  const submitInvite = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!validate()) return;
    setSubmitting(true);
    try {
      await inviteUser({
        email: form.email.trim(),
        name: form.name.trim(),
        password: form.password,
        role: form.role,
      });
      toast.success(t("admin.inviteSuccess", { email: form.email.trim() }));
      setForm({ email: "", name: "", password: "", role: "member" });
      setFieldErrors({});
      await refresh();
    } catch (err) {
      toast.error(translateApiError(err, t, { fallback: t("admin.saveError") }));
    } finally {
      setSubmitting(false);
    }
  };

  const changeRole = async (u: AdminUser, role: string) => {
    if (role === u.role) return;
    setSavingRole(u.id);
    try {
      await updateUserRole(u.id, role);
      toast.success(t("admin.roleUpdated", { email: u.email }));
      await refresh();
    } catch (err) {
      toast.error(translateApiError(err, t, { fallback: t("admin.saveError") }));
    } finally {
      setSavingRole(null);
    }
  };

  const deactivate = async (u: AdminUser) => {
    const ok = await confirm({
      title: t("admin.deactivateTitle"),
      description: t("admin.deactivateDescription", { email: u.email }),
      confirmLabel: t("admin.deactivate"),
      cancelLabel: t("common.cancel"),
      tone: "danger",
    });
    if (!ok) return;
    setDeactivating(u.id);
    try {
      await deactivateUser(u.id);
      toast.success(t("admin.deactivateSuccess", { email: u.email }));
      await refresh();
    } catch (err) {
      toast.error(translateApiError(err, t, { fallback: t("admin.saveError") }));
    } finally {
      setDeactivating(null);
    }
  };

  return (
    <div className="flex flex-col gap-6">
      {error && <ErrorBanner message={error} />}

      <Panel>
        <SectionHeading title={t("admin.inviteHeading")} description={t("admin.inviteDescription")} />
        <form onSubmit={submitInvite} noValidate className="flex flex-col gap-4">
          <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
            <Field
              label={t("admin.fieldEmail")}
              required
              error={fieldErrors.email}
              className="lg:col-span-1"
            >
              {(p) => (
                <Input
                  {...p}
                  type="email"
                  autoComplete="off"
                  placeholder={t("admin.fieldEmailPlaceholder")}
                  value={form.email}
                  onChange={(e) => setForm({ ...form, email: e.target.value })}
                />
              )}
            </Field>
            <Field label={t("admin.fieldName")} required error={fieldErrors.name}>
              {(p) => (
                <Input
                  {...p}
                  placeholder={t("admin.fieldNamePlaceholder")}
                  value={form.name}
                  onChange={(e) => setForm({ ...form, name: e.target.value })}
                />
              )}
            </Field>
            <Field
              label={t("admin.fieldTempPassword")}
              required
              error={fieldErrors.password}
              hint={t("admin.fieldTempPasswordHint")}
            >
              {(p) => (
                <Input
                  {...p}
                  type="password"
                  autoComplete="new-password"
                  placeholder={t("admin.fieldTempPasswordPlaceholder")}
                  value={form.password}
                  onChange={(e) => setForm({ ...form, password: e.target.value })}
                />
              )}
            </Field>
            <Field label={t("admin.fieldRole")}>
              {(p) => (
                <Select
                  {...p}
                  value={form.role}
                  onChange={(e) => setForm({ ...form, role: e.target.value })}
                >
                  <option value="member">{t("admin.roleMember")}</option>
                  <option value="admin">{t("admin.roleAdmin")}</option>
                </Select>
              )}
            </Field>
          </div>
          <div className="flex justify-end">
            <Button type="submit" loading={submitting}>
              <Plus className="h-4 w-4" aria-hidden />
              {t("admin.sendInvite")}
            </Button>
          </div>
        </form>
      </Panel>

      <Panel>
        <SectionHeading
          title={t("admin.users")}
          description={t("admin.usersDescription")}
          actions={<RefreshButton onClick={refresh} loading={loading} />}
        />

        <div className="mb-4 max-w-xs">
          <Field label={t("admin.usersSearchLabel")} hideLabel>
            {(p) => (
              <Input
                {...p}
                type="search"
                placeholder={t("admin.usersSearchPlaceholder")}
                value={query}
                onChange={(e) => setQuery(e.target.value)}
              />
            )}
          </Field>
        </div>

        {loading ? (
          <UsersTableSkeleton />
        ) : users.length === 0 ? (
          <EmptyState
            icon={<UsersIcon className="h-6 w-6" aria-hidden />}
            title={t("admin.noUsersTitle")}
            description={t("admin.noUsersDescription")}
          />
        ) : filtered.length === 0 ? (
          <EmptyState
            icon={<Search className="h-6 w-6" aria-hidden />}
            title={t("admin.noUsersMatchTitle")}
            description={t("admin.noUsersMatchDescription", { query: query.trim() })}
          />
        ) : (
          <Table>
            <THead>
              <Tr>
                <Th>{t("admin.colEmail")}</Th>
                <Th>{t("admin.colName")}</Th>
                <Th>{t("admin.colRole")}</Th>
                <Th>{t("admin.colStatus")}</Th>
                <Th>
                  <span className="block text-right">{t("common.actions")}</span>
                </Th>
              </Tr>
            </THead>
            <TBody>
              {filtered.map((u) => {
                const deactivated = Boolean(u.deactivated_at);
                return (
                  <Tr key={u.id}>
                    <Td className="font-medium">{u.email}</Td>
                    <Td>
                      <span className="text-muted">{u.name || "—"}</span>
                    </Td>
                    <Td>
                      <label className="sr-only" htmlFor={`role-${u.id}`}>
                        {t("admin.colRole")}
                      </label>
                      <Select
                        id={`role-${u.id}`}
                        className="w-36"
                        value={u.role}
                        disabled={savingRole === u.id || deactivated}
                        onChange={(e) => changeRole(u, e.target.value)}
                      >
                        <option value="member">{t("admin.roleMember")}</option>
                        <option value="admin">{t("admin.roleAdmin")}</option>
                      </Select>
                    </Td>
                    <Td>
                      <Badge tone={deactivated ? "neutral" : "success"} dot>
                        {deactivated ? t("admin.badgeDeactivated") : t("admin.badgeActive")}
                      </Badge>
                    </Td>
                    <Td className="text-right">
                      {!deactivated && (
                        <Button
                          variant="ghost"
                          size="sm"
                          loading={deactivating === u.id}
                          onClick={() => deactivate(u)}
                        >
                          <span className="inline-flex items-center gap-1.5 text-danger">
                            <Trash2 className="h-4 w-4" aria-hidden />
                            {t("admin.deactivate")}
                          </span>
                        </Button>
                      )}
                    </Td>
                  </Tr>
                );
              })}
            </TBody>
          </Table>
        )}
      </Panel>
    </div>
  );
}

function UsersTableSkeleton() {
  const { t } = useTranslation();
  return (
    <div className="space-y-2" role="status" aria-label={t("common.loading")}>
      {Array.from({ length: 5 }).map((_, i) => (
        <div key={i} className="flex items-center gap-4 rounded-lg border border-border p-3">
          <Skeleton className="h-4 w-48" />
          <Skeleton className="h-4 w-32" />
          <Skeleton className="ml-auto h-8 w-24 rounded-lg" />
          <Skeleton className="h-6 w-20 rounded-full" />
        </div>
      ))}
    </div>
  );
}

// --- Audit -------------------------------------------------------------

function AuditTab() {
  const { t } = useTranslation();
  const [entries, setEntries] = useState<AuditEntry[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);
  const [action, setAction] = useState<string>("");
  const [offset, setOffset] = useState(0);
  const limit = 50;

  const load = useCallback(async () => {
    setLoading(true);
    try {
      setEntries(await fetchAuditLog({ action: action || undefined, offset, limit }));
      setError(null);
    } catch (e) {
      setError(translateApiError(e, t));
    } finally {
      setLoading(false);
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [action, offset]);

  useEffect(() => {
    load();
  }, [load]);

  return (
    <Panel>
      <SectionHeading
        title={t("admin.auditLog")}
        description={t("admin.auditDescription")}
        actions={<RefreshButton onClick={load} loading={loading} />}
      />
      {error && <ErrorBanner message={error} />}

      <div className="mb-4 max-w-xs">
        <Field label={t("admin.filterAction")}>
          {(p) => (
            <Input
              {...p}
              value={action}
              onChange={(e) => {
                setOffset(0);
                setAction(e.target.value);
              }}
              placeholder={t("admin.filterActionPlaceholder")}
            />
          )}
        </Field>
      </div>

      {loading ? (
        <ListSkeleton rows={6} />
      ) : entries.length === 0 ? (
        <EmptyState
          icon={<ScrollText className="h-6 w-6" aria-hidden />}
          title={t("admin.auditEmptyTitle")}
          description={t("admin.auditLogEmpty")}
        />
      ) : (
        <>
          <Table>
            <THead>
              <Tr>
                <Th>{t("admin.colTime")}</Th>
                <Th>{t("admin.colActor")}</Th>
                <Th>{t("admin.colAction")}</Th>
                <Th>{t("admin.colResource")}</Th>
              </Tr>
            </THead>
            <TBody>
              {entries.map((e) => (
                <Tr key={e.id}>
                  <Td className="whitespace-nowrap">
                    <span className="text-muted">{new Date(e.created_at).toLocaleString()}</span>
                  </Td>
                  <Td>
                    <span className="text-muted">{e.actor_id ?? "—"}</span>
                  </Td>
                  <Td>
                    <span className="font-mono text-xs">{e.action}</span>
                  </Td>
                  <Td>
                    <span className="text-muted">
                      {e.resource_type ? (
                        <>
                          {e.resource_type}{" "}
                          {e.resource_id && (
                            <span className="font-mono text-xs">{e.resource_id}</span>
                          )}
                        </>
                      ) : (
                        "—"
                      )}
                    </span>
                  </Td>
                </Tr>
              ))}
            </TBody>
          </Table>
          <div className="mt-4 flex items-center justify-end gap-2">
            <Button
              variant="secondary"
              size="sm"
              onClick={() => setOffset(Math.max(0, offset - limit))}
              disabled={offset === 0 || loading}
            >
              {t("common.prev")}
            </Button>
            <Button
              variant="secondary"
              size="sm"
              onClick={() => setOffset(offset + limit)}
              disabled={entries.length < limit || loading}
            >
              {t("common.next")}
            </Button>
          </div>
        </>
      )}
    </Panel>
  );
}

// --- Retention ---------------------------------------------------------

function RetentionTab() {
  const { t } = useTranslation();
  const toast = useToast();
  const confirm = useConfirm();
  const [policies, setPolicies] = useState<RetentionPolicy[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [deletingId, setDeletingId] = useState<string | null>(null);
  const [form, setForm] = useState<{
    max_versions: string;
    max_age_days: string;
    archive_after_days: string;
  }>({ max_versions: "", max_age_days: "30", archive_after_days: "" });

  const refresh = useCallback(async () => {
    setLoading(true);
    try {
      setPolicies(await fetchRetentionPolicies());
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

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    setSaving(true);
    try {
      await upsertRetentionPolicy({
        max_versions: parseOpt(form.max_versions),
        max_age_days: parseOpt(form.max_age_days),
        archive_after_days: parseOpt(form.archive_after_days),
      });
      toast.success(t("admin.policySaved"));
      await refresh();
    } catch (err) {
      toast.error(translateApiError(err, t, { fallback: t("admin.saveError") }));
    } finally {
      setSaving(false);
    }
  };

  const remove = async (p: RetentionPolicy) => {
    const ok = await confirm({
      title: t("admin.deletePolicyTitle"),
      description: t("admin.deletePolicyDescription"),
      confirmLabel: t("common.delete"),
      cancelLabel: t("common.cancel"),
      tone: "danger",
    });
    if (!ok) return;
    setDeletingId(p.id);
    try {
      await deleteRetentionPolicy(p.id);
      toast.success(t("admin.policyDeleted"));
      await refresh();
    } catch (err) {
      toast.error(translateApiError(err, t, { fallback: t("admin.saveError") }));
    } finally {
      setDeletingId(null);
    }
  };

  return (
    <div className="flex flex-col gap-6">
      {error && <ErrorBanner message={error} />}

      <Panel>
        <SectionHeading
          title={t("admin.retentionFormHeading")}
          description={t("admin.retentionFormDescription")}
        />
        <form onSubmit={submit} className="flex flex-col gap-4">
          <div className="grid gap-4 sm:grid-cols-3">
            <Field label={t("admin.maxVersions")}>
              {(p) => (
                <Input
                  {...p}
                  type="number"
                  min={1}
                  placeholder={t("admin.unsetPlaceholder")}
                  value={form.max_versions}
                  onChange={(e) => setForm({ ...form, max_versions: e.target.value })}
                />
              )}
            </Field>
            <Field label={t("admin.maxAgeDays")}>
              {(p) => (
                <Input
                  {...p}
                  type="number"
                  min={1}
                  placeholder={t("admin.unsetPlaceholder")}
                  value={form.max_age_days}
                  onChange={(e) => setForm({ ...form, max_age_days: e.target.value })}
                />
              )}
            </Field>
            <Field label={t("admin.archiveAfterDays")}>
              {(p) => (
                <Input
                  {...p}
                  type="number"
                  min={1}
                  placeholder={t("admin.unsetPlaceholder")}
                  value={form.archive_after_days}
                  onChange={(e) => setForm({ ...form, archive_after_days: e.target.value })}
                />
              )}
            </Field>
          </div>
          <div className="flex justify-end">
            <Button type="submit" loading={saving}>
              {t("admin.savePolicy")}
            </Button>
          </div>
        </form>
      </Panel>

      <Panel>
        <SectionHeading
          title={t("admin.retentionPolicies")}
          actions={<RefreshButton onClick={refresh} loading={loading} />}
        />
        {loading ? (
          <ListSkeleton rows={3} />
        ) : policies.length === 0 ? (
          <EmptyState
            icon={<Archive className="h-6 w-6" aria-hidden />}
            title={t("admin.noPoliciesTitle")}
            description={t("admin.noPoliciesDescription")}
          />
        ) : (
          <Table>
            <THead>
              <Tr>
                <Th>{t("admin.colScope")}</Th>
                <Th>{t("admin.colLimits")}</Th>
                <Th>
                  <span className="block text-right">{t("common.actions")}</span>
                </Th>
              </Tr>
            </THead>
            <TBody>
              {policies.map((p) => (
                <Tr key={p.id}>
                  <Td className="font-medium">
                    {p.folder_id ? (
                      <span className="inline-flex items-center gap-2">
                        <Badge tone="neutral">{t("admin.retentionScopeFolder")}</Badge>
                        <span className="font-mono text-xs text-muted">{p.folder_id}</span>
                      </span>
                    ) : (
                      <Badge tone="brand">{t("admin.retentionWorkspaceDefault")}</Badge>
                    )}
                  </Td>
                  <Td>
                    <span className="text-muted">{summarise(p)}</span>
                  </Td>
                  <Td className="text-right">
                    <Button
                      variant="ghost"
                      size="sm"
                      loading={deletingId === p.id}
                      onClick={() => remove(p)}
                    >
                      <span className="inline-flex items-center gap-1.5 text-danger">
                        <Trash2 className="h-4 w-4" aria-hidden />
                        {t("common.delete")}
                      </span>
                    </Button>
                  </Td>
                </Tr>
              ))}
            </TBody>
          </Table>
        )}
      </Panel>
    </div>
  );
}

// --- Storage -----------------------------------------------------------

function StorageTab() {
  const { t } = useTranslation();
  const [usage, setUsage] = useState<StorageUsage | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);

  const refresh = useCallback(async () => {
    setLoading(true);
    try {
      setUsage(await fetchStorageUsage());
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

  const perUser = usage?.per_user ?? [];

  return (
    <Panel>
      <SectionHeading
        title={t("admin.storage")}
        description={t("admin.storageDescription")}
        actions={<RefreshButton onClick={refresh} loading={loading} />}
      />
      {error && <ErrorBanner message={error} />}

      {loading ? (
        <ListSkeleton rows={5} />
      ) : usage ? (
        <>
          <div className="mb-5 flex items-baseline gap-2">
            <span className="text-sm text-muted">{t("admin.storageTotalLabel")}</span>
            <span className="text-2xl font-semibold tracking-tight text-fg">
              {formatBytes(usage.total_bytes)}
            </span>
          </div>
          {perUser.length === 0 ? (
            <EmptyState
              icon={<HardDrive className="h-6 w-6" aria-hidden />}
              title={t("admin.noStorageTitle")}
              description={t("admin.noStorageDescription")}
            />
          ) : (
            <Table>
              <THead>
                <Tr>
                  <Th>{t("admin.colUser")}</Th>
                  <Th>{t("admin.colBytes")}</Th>
                  <Th className="w-1/3">{t("admin.colShare")}</Th>
                </Tr>
              </THead>
              <TBody>
                {perUser.map((u) => {
                  const pct =
                    usage.total_bytes > 0 ? (u.total_bytes / usage.total_bytes) * 100 : 0;
                  return (
                    <Tr key={u.user_id}>
                      <Td className="font-medium">{u.email}</Td>
                      <Td className="whitespace-nowrap">
                        <span className="text-muted">{formatBytes(u.total_bytes)}</span>
                      </Td>
                      <Td>
                        <div className="flex items-center gap-2">
                          <div
                            className="h-2 w-full max-w-[200px] overflow-hidden rounded-full bg-surface-2"
                            role="progressbar"
                            aria-valuenow={Math.round(pct)}
                            aria-valuemin={0}
                            aria-valuemax={100}
                          >
                            <div
                              className="h-full rounded-full bg-brand transition-[width]"
                              style={{ width: `${pct}%` }}
                            />
                          </div>
                          <span className="w-10 shrink-0 text-right text-xs text-muted">
                            {pct.toFixed(0)}%
                          </span>
                        </div>
                      </Td>
                    </Tr>
                  );
                })}
              </TBody>
            </Table>
          )}
        </>
      ) : null}
    </Panel>
  );
}

// --- Health ------------------------------------------------------------

// healthTone maps each traffic-light status to a shared Badge tone.
const healthTone: Record<HealthColor, BadgeProps["tone"]> = {
  green: "success",
  yellow: "warning",
  red: "danger",
  unknown: "neutral",
};

function HealthPill({ status }: { status: HealthColor }) {
  const { t } = useTranslation();
  const labelKey = `admin.health.status${status.charAt(0).toUpperCase()}${status.slice(1)}`;
  return (
    <Badge tone={healthTone[status] ?? "neutral"} dot>
      {t(labelKey)}
    </Badge>
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
    <div className="flex flex-col gap-3 rounded-card border border-border bg-surface p-4">
      <div className="flex items-center justify-between gap-2">
        <span className="font-semibold capitalize text-fg">{sub.name}</span>
        <HealthPill status={sub.status} />
      </div>
      {sub.error && <div className="text-sm text-danger">{sub.error}</div>}
      {details.length > 0 && (
        <dl className="grid grid-cols-[auto_1fr] gap-x-3 gap-y-0.5 text-sm">
          {details.map(([k, v]) => (
            <div key={k} className="contents">
              <dt className="text-muted">{k}</dt>
              <dd className="m-0 break-words font-mono text-xs text-fg">{formatDetailValue(v)}</dd>
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
      setError(translateApiError(e, t, { fallback: t("admin.health.loadError") }));
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
    <Panel>
      <SectionHeading
        title={t("admin.health.title")}
        description={t("admin.health.subtitle")}
        actions={
          <div className="flex items-center gap-3">
            {report && (
              <span className="flex items-center gap-2">
                <span className="text-sm text-muted">{t("admin.health.overall")}</span>
                <HealthPill status={report.status} />
              </span>
            )}
            <label className="flex items-center gap-2 text-sm text-fg">
              <input
                type="checkbox"
                className="h-4 w-4 rounded border-border text-brand focus-visible:ring-2 focus-visible:ring-ring"
                checked={auto}
                onChange={(e) => setAuto(e.target.checked)}
              />
              {t("admin.health.autoRefresh")}
            </label>
            <RefreshButton onClick={refresh} loading={loading} />
          </div>
        }
      />

      {error && <ErrorBanner message={error} />}

      {!report && loading && (
        <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-3">
          {Array.from({ length: 6 }).map((_, i) => (
            <Skeleton key={i} className="h-28 rounded-card" />
          ))}
        </div>
      )}

      {report && report.subsystems.length === 0 && (
        <EmptyState
          icon={<Activity className="h-6 w-6" aria-hidden />}
          title={t("admin.health.noSubsystems")}
        />
      )}

      {report && report.subsystems.length > 0 && (
        <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-3">
          {report.subsystems.map((sub) => (
            <SubsystemCard key={sub.name} sub={sub} />
          ))}
        </div>
      )}

      {report && (
        <p className="mt-4 text-xs text-muted">
          {t("admin.health.lastUpdated", {
            time: new Date(report.generated_at).toLocaleTimeString(),
          })}
        </p>
      )}
    </Panel>
  );
}

// --- helpers -----------------------------------------------------------

function ListSkeleton({ rows = 5 }: { rows?: number }) {
  const { t } = useTranslation();
  return (
    <div className="space-y-2" role="status" aria-label={t("common.loading")}>
      {Array.from({ length: rows }).map((_, i) => (
        <Skeleton key={i} className="h-12 rounded-lg" />
      ))}
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

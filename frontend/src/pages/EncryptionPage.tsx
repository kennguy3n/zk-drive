import { useCallback, useEffect, useState, type ReactNode } from "react";
import { Link, NavLink, useNavigate } from "react-router-dom";
import { useTranslation } from "react-i18next";
import {
  CreditCard,
  Eye,
  KeyRound,
  LogOut,
  MapPin,
  MessagesSquare,
  ShieldCheck,
  Users as UsersIcon,
  type LucideIcon,
} from "lucide-react";
import {
  fetchCMK,
  getDefaultEncryptionMode,
  updateCMK,
  updateDefaultEncryptionMode,
  type EncryptionMode,
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
  PageHeader,
  RadioCard,
  Skeleton,
  useToast,
} from "../components/ui";
import { ThemeToggle } from "../components/ThemeToggle";
import { cn } from "../lib/cn";

// EncryptionPage lets workspace admins choose the default encryption mode
// for new folders and configure the customer-managed key (CMK) that
// zk-object-fabric uses to wrap per-object DEKs. Both settings are wired to
// the real admin API; all copy lives in the "encryption" i18n namespace.
export default function EncryptionPage() {
  const { isAdmin } = useAuth();
  const { t } = useTranslation();

  if (!isAdmin) {
    return <AccessDenied />;
  }

  return (
    <AdminShell active="encryption">
      <PageHeader
        title={t("encryption.pageTitle")}
        description={t("encryption.pageDescription")}
      />
      <div className="flex flex-col gap-6">
        <DefaultEncryptionModeSection />
        <CmkSection />
      </div>
    </AdminShell>
  );
}

// DefaultEncryptionModeSection lets a Secure Business admin pick the
// encryption mode applied to new top-level folders by default. The
// supported set is sourced from the server response so the picker can't
// drift from the backend's allow-list.
function DefaultEncryptionModeSection() {
  const { t } = useTranslation();
  const toast = useToast();
  const [mode, setMode] = useState<EncryptionMode | null>(null);
  const [supported, setSupported] = useState<EncryptionMode[]>([]);
  const [saving, setSaving] = useState<EncryptionMode | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  const load = useCallback(async () => {
    setError(null);
    setLoading(true);
    try {
      const r = await getDefaultEncryptionMode();
      setMode(r.mode);
      setSupported(r.supported);
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
    setSaving(next);
    try {
      const r = await updateDefaultEncryptionMode(next);
      setMode(r.mode);
      toast.success(t("encryption.defaultModeSaved"));
    } catch (e) {
      toast.error(translateApiError(e, t));
    } finally {
      setSaving(null);
    }
  };

  const options: {
    value: EncryptionMode;
    title: string;
    description: string;
    icon: ReactNode;
    badge?: string;
  }[] = [
    {
      value: "managed_encrypted",
      title: t("encryption.defaultModeManagedLabel"),
      description: t("encryption.defaultModeManagedHint"),
      icon: <Eye className="h-5 w-5" aria-hidden />,
    },
    {
      value: "strict_zk",
      title: t("encryption.defaultModeStrictLabel"),
      description: t("encryption.defaultModeStrictHint"),
      icon: <ShieldCheck className="h-5 w-5" aria-hidden />,
      badge: t("encryption.defaultModeSecureBusinessBadge"),
    },
  ];
  const visibleOptions =
    supported.length > 0 ? options.filter((o) => supported.includes(o.value)) : options;

  return (
    <Panel>
      <SectionHeading
        title={t("encryption.defaultModeHeading")}
        description={t("encryption.defaultModeDescription")}
        actions={
          mode ? (
            <span className="inline-flex items-center gap-2 text-sm text-muted">
              {t("encryption.currentDefault")}
              <Badge tone="brand">
                {t(
                  mode === "strict_zk"
                    ? "encryption.defaultModeStrictLabel"
                    : "encryption.defaultModeManagedLabel",
                )}
              </Badge>
            </span>
          ) : undefined
        }
      />
      {error && <ErrorBanner message={error} />}
      {loading ? (
        <div className="grid gap-3 sm:grid-cols-2" aria-hidden>
          <Skeleton className="h-28 rounded-card" />
          <Skeleton className="h-28 rounded-card" />
        </div>
      ) : (
        <div
          role="radiogroup"
          aria-label={t("encryption.defaultModeHeading")}
          className="grid gap-3 sm:grid-cols-2"
        >
          {visibleOptions.map((opt) => (
            <RadioCard
              key={opt.value}
              selected={mode === opt.value}
              onSelect={() => choose(opt.value)}
              disabled={saving !== null}
              title={opt.title}
              description={opt.description}
              icon={opt.icon}
              badge={opt.badge}
            />
          ))}
        </div>
      )}
    </Panel>
  );
}

// CmkSection configures the customer-managed key URI used to wrap
// per-object DEKs for managed-encrypted folders. Strict zero-knowledge
// folders are unaffected — the server never sees their plaintext.
function CmkSection() {
  const { t } = useTranslation();
  const toast = useToast();
  const [initialURI, setInitialURI] = useState("");
  const [uri, setURI] = useState("");
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);

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
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  useEffect(() => {
    load();
  }, [load]);

  const save = async (e: React.FormEvent) => {
    e.preventDefault();
    setError(null);
    setSaving(true);
    try {
      await updateCMK(uri.trim());
      toast.success(t("encryption.cmkSaved"));
      await load();
    } catch (err) {
      toast.error(translateApiError(err, t));
    } finally {
      setSaving(false);
    }
  };

  const dirty = uri !== initialURI;

  return (
    <Panel>
      <SectionHeading title={t("encryption.cmkHeading")} description={t("encryption.cmkDescription")} />
      {error && <ErrorBanner message={error} />}

      <div className="mb-5 rounded-lg border border-border bg-surface-2 p-4 text-sm text-muted">
        <p className="font-medium text-fg">{t("encryption.modesHeading")}</p>
        <p className="mt-1">{t("encryption.modesExplanation")}</p>
      </div>

      {loading ? (
        <Skeleton className="h-10 max-w-xl rounded-lg" />
      ) : (
        <form onSubmit={save} className="flex max-w-xl flex-col gap-4">
          <Field label={t("encryption.cmkUri")} hint={t("encryption.cmkSchemesHint")}>
            {(p) => (
              <Input
                {...p}
                className="font-mono"
                value={uri}
                onChange={(e) => setURI(e.target.value)}
                placeholder="arn:aws:kms:us-east-1:123456789012:key/..."
                autoComplete="off"
                spellCheck={false}
              />
            )}
          </Field>
          <div className="flex flex-wrap items-center gap-3">
            <Button type="submit" loading={saving} disabled={!dirty}>
              {t("common.save")}
            </Button>
            <Button
              type="button"
              variant="ghost"
              onClick={() => setURI(initialURI)}
              disabled={!dirty || saving}
            >
              {t("common.reset")}
            </Button>
          </div>
        </form>
      )}
    </Panel>
  );
}

// --- Shared admin chrome ------------------------------------------------
// Kept local to this file per the workstream's "build new primitives
// locally" rule; AdminPage renders an equivalent shell. A coordinator can
// later extract a shared <AdminShell> primitive into components/ui.

type AdminSection = "admin" | "placement" | "encryption" | "kchat" | "billing";

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

function AccessDenied() {
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

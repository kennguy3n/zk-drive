import { useCallback, useEffect, useMemo, useState, type ReactNode } from "react";
import { Link, NavLink, useNavigate } from "react-router-dom";
import { useTranslation } from "react-i18next";
import {
  CreditCard,
  Globe,
  KeyRound,
  LogOut,
  MapPin,
  MessagesSquare,
  Users as UsersIcon,
  type LucideIcon,
} from "lucide-react";
import { fetchPlacement, updatePlacement, type PlacementPolicy } from "../api/client";
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
  Select,
  Skeleton,
  useToast,
} from "../components/ui";
import { ThemeToggle } from "../components/ThemeToggle";
import { cn } from "../lib/cn";

// PlacementPage lets workspace admins view and edit the data-residency
// placement policy that zk-object-fabric uses to route per-workspace
// storage. The form exposes the subset of fabric.Policy the UI cares about;
// other fields (tenant, encryption, cache location) round-trip unchanged
// because we PUT the entire payload we received on GET.

const PROVIDERS = ["wasabi", "b2", "s3"] as const;

interface PlacementForm {
  provider: string;
  region: string;
  country: string;
  storageClass: string;
}

function formFromPolicy(p: PlacementPolicy): PlacementForm {
  const pl = p.policy.placement;
  return {
    provider: pl.provider?.[0] ?? "wasabi",
    region: pl.region?.[0] ?? "",
    country: pl.country?.[0] ?? "",
    storageClass: pl.storage_class?.[0] ?? "",
  };
}

export default function PlacementPage() {
  const { isAdmin } = useAuth();
  const { t } = useTranslation();
  const toast = useToast();
  const [policy, setPolicy] = useState<PlacementPolicy | null>(null);
  const [initial, setInitial] = useState<PlacementForm | null>(null);
  const [form, setForm] = useState<PlacementForm>({
    provider: "wasabi",
    region: "",
    country: "",
    storageClass: "",
  });
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);

  const load = useCallback(async () => {
    setError(null);
    setLoading(true);
    try {
      const p = await fetchPlacement();
      setPolicy(p);
      const f = formFromPolicy(p);
      setInitial(f);
      setForm(f);
    } catch (e) {
      setError(translateApiError(e, t));
    } finally {
      setLoading(false);
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  useEffect(() => {
    if (isAdmin) load();
  }, [isAdmin, load]);

  const dirty = useMemo(
    () => (initial ? JSON.stringify(initial) !== JSON.stringify(form) : false),
    [initial, form],
  );

  if (!isAdmin) {
    return <AccessDenied />;
  }

  const providerLabel = (value: string): string => {
    const key = `placement.provider_${value}`;
    const label = t(key);
    return label === key ? value : label;
  };

  const save = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!policy) return;
    setError(null);
    setSaving(true);
    // Preserve every field from the GET payload; only replace the slices the
    // form edits so tenant / encryption / cache location stay stable.
    const next: PlacementPolicy = {
      ...policy,
      policy: {
        ...policy.policy,
        placement: {
          ...policy.policy.placement,
          provider: form.provider ? [form.provider] : [],
          region: form.region.trim() ? [form.region.trim()] : [],
          country: form.country.trim() ? [form.country.trim().toUpperCase()] : [],
          storage_class: form.storageClass.trim() ? [form.storageClass.trim()] : [],
        },
      },
    };
    try {
      await updatePlacement(next);
      toast.success(t("placement.savedConfirm"));
      await load();
    } catch (err) {
      toast.error(translateApiError(err, t));
    } finally {
      setSaving(false);
    }
  };

  const set = (patch: Partial<PlacementForm>) => setForm((prev) => ({ ...prev, ...patch }));

  return (
    <AdminShell active="placement">
      <PageHeader title={t("placement.title")} description={t("placement.pageDescription")} />

      {error && <ErrorBanner message={error} />}

      <Panel>
        <SectionHeading
          title={t("placement.formHeading")}
          description={t("placement.formDescription")}
          actions={
            !loading && initial ? (
              <div className="flex flex-wrap items-center gap-2">
                <span className="text-sm text-muted">{t("placement.currentLabel")}</span>
                <Badge tone="brand">{providerLabel(initial.provider)}</Badge>
                {initial.region && <Badge tone="neutral">{initial.region}</Badge>}
                {initial.country && <Badge tone="neutral">{initial.country}</Badge>}
                {initial.storageClass && <Badge tone="neutral">{initial.storageClass}</Badge>}
              </div>
            ) : undefined
          }
        />

        {loading ? (
          <div className="flex max-w-xl flex-col gap-4">
            <Skeleton className="h-10 rounded-lg" />
            <Skeleton className="h-10 rounded-lg" />
            <Skeleton className="h-10 rounded-lg" />
            <Skeleton className="h-10 rounded-lg" />
          </div>
        ) : (
          <form onSubmit={save} className="flex max-w-xl flex-col gap-5">
            <Field label={t("placement.provider")} hint={t("placement.providerHint")}>
              {(p) => (
                <Select
                  {...p}
                  value={form.provider}
                  onChange={(e) => set({ provider: e.target.value })}
                >
                  {PROVIDERS.map((value) => (
                    <option key={value} value={value}>
                      {providerLabel(value)}
                    </option>
                  ))}
                </Select>
              )}
            </Field>

            <Field label={t("placement.region")} hint={t("placement.regionHint")}>
              {(p) => (
                <Input
                  {...p}
                  value={form.region}
                  onChange={(e) => set({ region: e.target.value })}
                  placeholder={t("placement.regionPlaceholder")}
                  autoComplete="off"
                />
              )}
            </Field>

            <Field label={t("placement.country")} hint={t("placement.countryHint")}>
              {(p) => (
                <Input
                  {...p}
                  value={form.country}
                  onChange={(e) => set({ country: e.target.value.toUpperCase() })}
                  placeholder={t("placement.countryPlaceholder")}
                  maxLength={2}
                  autoComplete="off"
                  className="uppercase"
                />
              )}
            </Field>

            <Field label={t("placement.storageClass")} hint={t("placement.storageClassHint")}>
              {(p) => (
                <Input
                  {...p}
                  value={form.storageClass}
                  onChange={(e) => set({ storageClass: e.target.value })}
                  placeholder={t("placement.storageClassPlaceholder")}
                  autoComplete="off"
                />
              )}
            </Field>

            <div className="flex flex-wrap items-center gap-3">
              <Button type="submit" loading={saving} disabled={!dirty || !policy}>
                {t("common.save")}
              </Button>
              <Button
                type="button"
                variant="ghost"
                onClick={() => initial && setForm(initial)}
                disabled={!dirty || saving}
              >
                {t("common.reset")}
              </Button>
            </div>
          </form>
        )}
      </Panel>
    </AdminShell>
  );
}

// --- Shared admin chrome ------------------------------------------------
// Kept local to this file per the workstream's "build new primitives
// locally" rule; AdminPage renders an equivalent shell.

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
          icon={<Globe className="h-6 w-6" aria-hidden />}
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

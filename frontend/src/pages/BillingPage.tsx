import { useCallback, useEffect, useRef, useState, type ReactNode } from "react";
import { Link, useSearchParams } from "react-router-dom";
import { useTranslation } from "react-i18next";
import type { TFunction } from "i18next";
import { ArrowLeft, Database, Gauge, Lock, AlertTriangle, Users } from "lucide-react";
import {
  createCheckoutSession,
  createPortalSession,
  fetchBillingUsage,
  type BillingUsageSummary,
} from "../api/client";
import { translateApiError } from "../api/errors";
import { useAuth } from "../hooks/useAuth";
import {
  AppShell,
  Badge,
  Button,
  EmptyState,
  PageHeader,
  RadioCard,
  Skeleton,
  useToast,
} from "../components/ui";
import { cn } from "../lib/cn";

// Tier metadata mirrors the canonical strings in
// internal/billing/billing.go. Translated label/blurb live in en.json
// so non-English locales can customise the plan card copy without
// changing component code.
const TIER_IDS = ["starter", "business", "secure_business"] as const;
type TierID = (typeof TIER_IDS)[number];

function isTierId(value: string): value is TierID {
  return (TIER_IDS as readonly string[]).includes(value);
}

// BillingPage shows the workspace's current plan, usage-vs-limit meters and
// a KChat RadioCard tier picker that starts a Stripe Checkout flow. Admin-only
// because the underlying /api/admin/billing/usage endpoint is admin-only.
export default function BillingPage() {
  const { isAdmin } = useAuth();
  const { t } = useTranslation();
  const toast = useToast();
  const [usage, setUsage] = useState<BillingUsageSummary | null>(null);
  const [loading, setLoading] = useState(true);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [selectedTier, setSelectedTier] = useState<TierID>("business");
  const [busy, setBusy] = useState<"checkout" | "portal" | null>(null);
  const [searchParams, setSearchParams] = useSearchParams();

  const loadUsage = useCallback(async () => {
    setLoading(true);
    setLoadError(null);
    try {
      const data = await fetchBillingUsage();
      setUsage(data);
      if (isTierId(data.tier)) setSelectedTier(data.tier);
    } catch (e) {
      const message = translateApiError(e, t);
      setLoadError(message);
      toast.error(t("billing.loadFailed"), message);
    } finally {
      setLoading(false);
    }
  }, [t, toast]);

  useEffect(() => {
    void loadUsage();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  // Surface the Stripe Checkout return state as a toast instead of an inline
  // banner, then strip the ?stripe= param so a refresh doesn't replay it. The
  // ref guards against double-firing under React StrictMode's double-invoke.
  const handledStripe = useRef<string | null>(null);
  useEffect(() => {
    const status = searchParams.get("stripe");
    if (!status || handledStripe.current === status) return;
    handledStripe.current = status;
    if (status === "success") {
      toast.success(t("billing.checkoutSuccessTitle"), t("billing.bannerCheckoutSuccess"));
    } else if (status === "cancel") {
      toast.info(t("billing.checkoutCancelTitle"), t("billing.bannerCheckoutCancel"));
    }
    const next = new URLSearchParams(searchParams);
    next.delete("stripe");
    setSearchParams(next, { replace: true });
  }, [searchParams, setSearchParams, toast, t]);

  async function handleUpgrade() {
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
      toast.error(
        t("billing.checkoutFailed"),
        translateApiError(e, t, { fallback: t("billing.checkoutFailedDesc") }),
      );
    }
  }

  async function handleManage() {
    setBusy("portal");
    try {
      const here = window.location.origin + window.location.pathname;
      const { url } = await createPortalSession({ return_url: here });
      window.location.assign(url);
    } catch (e) {
      setBusy(null);
      toast.error(
        t("billing.portalFailed"),
        translateApiError(e, t, { fallback: t("billing.portalFailedDesc") }),
      );
    }
  }

  const brand = (
    <Link
      to="/drive"
      className="rounded-full px-1 text-base font-semibold tracking-tight text-fg focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2 focus-visible:ring-offset-surface"
    >
      <span className="text-brand">ZK</span> Drive
    </Link>
  );
  const backToDrive = (
    <NavPill to="/drive" icon={<ArrowLeft className="h-4 w-4" aria-hidden="true" />}>
      {t("admin.backToDrive")}
    </NavPill>
  );

  if (!isAdmin) {
    return (
      <AppShell brand={brand} actions={backToDrive} maxWidth="lg">
        <PageHeader title={t("billing.title")} />
        <div className="rounded-card border border-border bg-surface">
          <EmptyState
            icon={<Lock className="h-6 w-6" />}
            title={t("admin.adminOnly")}
            description={t("billing.adminOnlyDescription")}
            action={
              <NavPill to="/drive" className="border border-border bg-surface-2 text-fg">
                {t("admin.backToDrive")}
              </NavPill>
            }
          />
        </div>
      </AppShell>
    );
  }

  const nav = (
    <>
      <NavPill to="/admin">{t("nav.admin")}</NavPill>
      <NavPill to="/billing" active>
        {t("nav.billing")}
      </NavPill>
    </>
  );

  return (
    <AppShell brand={brand} nav={nav} actions={backToDrive} maxWidth="lg">
      <PageHeader title={t("billing.title")} description={t("billing.pageDescription")} />

      {loading ? (
        <BillingSkeleton label={t("common.loading")} />
      ) : loadError && !usage ? (
        <div className="rounded-card border border-border bg-surface">
          <EmptyState
            icon={<AlertTriangle className="h-6 w-6" />}
            title={t("billing.loadFailed")}
            description={loadError}
            action={
              <Button variant="secondary" onClick={() => void loadUsage()}>
                {t("common.retry")}
              </Button>
            }
          />
        </div>
      ) : usage ? (
        <div className="flex flex-col gap-6">
          <PlanSummary
            usage={usage}
            busy={busy}
            onManage={handleManage}
            t={t}
          />

          <section
            className="rounded-card border border-border bg-surface p-6"
            aria-labelledby="billing-usage-heading"
          >
            <h2 id="billing-usage-heading" className="text-lg font-semibold text-fg">
              {t("billing.usageHeading")}
            </h2>
            <p className="mt-1 text-sm text-muted">{t("billing.usageDescription")}</p>
            <div className="mt-5 flex flex-col gap-5">
              <UsageMeter
                label={t("billing.usageStorage")}
                icon={<Database className="h-4 w-4" />}
                used={usage.storage_used_bytes}
                limit={usage.storage_limit_bytes}
                bytes
                t={t}
              />
              <UsageMeter
                label={t("billing.usageBandwidth")}
                icon={<Gauge className="h-4 w-4" />}
                used={usage.bandwidth_used_bytes_month}
                limit={usage.bandwidth_limit_bytes_month}
                bytes
                t={t}
              />
              <UsageMeter
                label={t("billing.usageUsers")}
                icon={<Users className="h-4 w-4" />}
                used={usage.user_count}
                limit={usage.user_limit}
                t={t}
              />
            </div>
          </section>

          <section
            className="rounded-card border border-border bg-surface p-6"
            aria-labelledby="billing-change-heading"
          >
            <h2 id="billing-change-heading" className="text-lg font-semibold text-fg">
              {t("billing.upgradeHeading")}
            </h2>
            <p className="mt-1 text-sm text-muted">{t("billing.manageDescription")}</p>
            <div
              role="radiogroup"
              aria-label={t("billing.chooseTier")}
              className="mt-5 grid gap-3 sm:grid-cols-3"
            >
              {TIER_IDS.map((id) => (
                <RadioCard
                  key={id}
                  selected={selectedTier === id}
                  onSelect={() => setSelectedTier(id)}
                  title={t(`billing.tier.${id}.label`)}
                  description={t(`billing.tier.${id}.blurb`)}
                  badge={usage.tier === id ? t("billing.currentShort") : undefined}
                />
              ))}
            </div>
            <div className="mt-6 flex flex-wrap items-center gap-3">
              <Button
                variant="gradient"
                size="lg"
                loading={busy === "checkout"}
                disabled={busy !== null}
                onClick={handleUpgrade}
              >
                {busy === "checkout" ? t("billing.redirecting") : t("billing.upgradePlan")}
              </Button>
              <p className="text-xs text-muted">{t("billing.checkoutHint")}</p>
            </div>
          </section>
        </div>
      ) : null}
    </AppShell>
  );
}

// PlanSummary is the "what you have / what you pay" block: current tier badge,
// the entitlements included in the plan, and the Stripe-managed payment row.
function PlanSummary({
  usage,
  busy,
  onManage,
  t,
}: {
  usage: BillingUsageSummary;
  busy: "checkout" | "portal" | null;
  onManage: () => void;
  t: TFunction;
}) {
  const tierLabel = isTierId(usage.tier) ? t(`billing.tier.${usage.tier}.label`) : usage.tier;
  return (
    <section
      className="rounded-card border border-border bg-surface p-6"
      aria-labelledby="billing-plan-heading"
    >
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div className="flex flex-wrap items-center gap-3">
          <h2 id="billing-plan-heading" className="text-lg font-semibold text-fg">
            {t("billing.summaryHeading")}
          </h2>
          <Badge tone="brand" dot>
            {tierLabel}
          </Badge>
        </div>
        {!usage.plan_configured && (
          <span className="text-sm text-muted">{t("billing.noPlanConfigured")}</span>
        )}
      </div>

      <div className="mt-4 grid gap-3 sm:grid-cols-3">
        <StatTile
          icon={<Database className="h-5 w-5" />}
          label={t("billing.usageStorage")}
          value={limitDisplay(usage.storage_limit_bytes, true, t)}
        />
        <StatTile
          icon={<Users className="h-5 w-5" />}
          label={t("billing.usageUsers")}
          value={limitDisplay(usage.user_limit, false, t)}
        />
        <StatTile
          icon={<Gauge className="h-5 w-5" />}
          label={t("billing.usageBandwidth")}
          value={limitDisplay(usage.bandwidth_limit_bytes_month, true, t)}
        />
      </div>

      <div className="mt-5 rounded-card border border-border bg-surface-2/40 p-4">
        <h3 className="text-sm font-semibold text-fg">{t("billing.paymentHeading")}</h3>
        <p className="mt-1 text-sm text-muted">
          {usage.plan_configured ? t("billing.paymentManaged") : t("billing.freeTierNote")}
        </p>
        {usage.plan_configured && (
          <div className="mt-3">
            <Button
              variant="secondary"
              loading={busy === "portal"}
              disabled={busy !== null}
              onClick={onManage}
            >
              {busy === "portal" ? t("billing.redirecting") : t("billing.manageSubscription")}
            </Button>
          </div>
        )}
      </div>
    </section>
  );
}

function StatTile({
  icon,
  label,
  value,
}: {
  icon: ReactNode;
  label: string;
  value: string;
}) {
  return (
    <div className="flex items-center gap-3 rounded-card border border-border bg-surface-2/40 p-4">
      <span
        className="flex h-10 w-10 shrink-0 items-center justify-center rounded-lg bg-brand/10 text-brand"
        aria-hidden="true"
      >
        {icon}
      </span>
      <div className="min-w-0">
        <div className="text-xs font-medium uppercase tracking-wide text-muted">{label}</div>
        <div className="truncate text-sm font-semibold text-fg">{value}</div>
      </div>
    </div>
  );
}

// UsageMeter renders a tokenised progress bar that turns warning/danger as the
// workspace approaches or exceeds a limit. The fill width is the only inline
// style (a runtime percentage Tailwind can't express); every colour is a token.
function UsageMeter({
  label,
  icon,
  used,
  limit,
  bytes = false,
  t,
}: {
  label: string;
  icon: ReactNode;
  used: number;
  limit: number;
  bytes?: boolean;
  t: TFunction;
}) {
  const hasLimit = limit > 0;
  const pct = hasLimit ? Math.min(100, (used / limit) * 100) : 0;
  const over = hasLimit && used > limit;
  const near = hasLimit && !over && pct >= 90;
  const fmt = (n: number) => (bytes ? formatBytes(n) : String(n));
  const barColor = over ? "bg-danger" : near ? "bg-warning" : "bg-brand";

  return (
    <div>
      <div className="mb-1.5 flex items-center justify-between gap-3">
        <span className="flex items-center gap-2 text-sm font-medium text-fg">
          <span className="text-muted" aria-hidden="true">
            {icon}
          </span>
          {label}
        </span>
        <span className="font-mono text-xs text-muted">
          {fmt(used)} / {hasLimit ? fmt(limit) : t("billing.unlimited")}
        </span>
      </div>
      <div
        role="progressbar"
        aria-valuemin={0}
        aria-valuemax={100}
        aria-valuenow={Math.round(pct)}
        aria-label={label}
        className="h-2.5 w-full overflow-hidden rounded-full bg-surface-2"
      >
        <div
          className={cn("h-full rounded-full transition-[width] duration-500", barColor)}
          style={{ width: `${pct}%` }}
        />
      </div>
      <div className="mt-1 min-h-[1.25rem]">
        {over ? (
          <Badge tone="danger">{t("billing.overLimit")}</Badge>
        ) : near ? (
          <Badge tone="warning">{t("billing.nearLimit")}</Badge>
        ) : hasLimit ? (
          <span className="text-xs text-muted">
            {t("billing.percentUsed", { pct: Math.round(pct) })}
          </span>
        ) : null}
      </div>
    </div>
  );
}

function BillingSkeleton({ label }: { label: string }) {
  return (
    <div className="flex flex-col gap-6" role="status" aria-label={label}>
      <Skeleton className="h-44 rounded-card" />
      <Skeleton className="h-52 rounded-card" />
      <Skeleton className="h-56 rounded-card" />
    </div>
  );
}

// NavPill is a tokenised pill-shaped navigation link for the AppShell top bar.
// It stays a real <a> (keyboard/Tab, open-in-new-tab) rather than a Button so
// navigation keeps anchor semantics; the indigo focus ring matches the system.
function NavPill({
  to,
  active,
  icon,
  className,
  children,
}: {
  to: string;
  active?: boolean;
  icon?: ReactNode;
  className?: string;
  children: ReactNode;
}) {
  return (
    <Link
      to={to}
      aria-current={active ? "page" : undefined}
      className={cn(
        "inline-flex items-center gap-1.5 rounded-full px-3 py-1.5 text-sm font-medium transition-colors",
        "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2 focus-visible:ring-offset-surface",
        active ? "bg-surface-2 text-fg" : "text-muted hover:bg-surface-2 hover:text-fg",
        className,
      )}
    >
      {icon}
      {children}
    </Link>
  );
}

function limitDisplay(limit: number, bytes: boolean, t: TFunction): string {
  if (limit <= 0) return t("billing.unlimited");
  return bytes ? formatBytes(limit) : String(limit);
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

import { type ReactNode } from "react";
import { Link } from "react-router-dom";
import { Trans, useTranslation } from "react-i18next";
import { ArrowLeft, Check, Eye, ShieldCheck, X } from "lucide-react";
import EncryptionBadge from "../components/EncryptionBadge";
import { AppShell, Badge, PageHeader, Table, THead, TBody, Tr, Th, Td } from "../components/ui";

// PrivacyPage is the customer-facing explainer for ZK Drive's two
// per-folder privacy modes. All long-form copy is sourced from the
// i18n "privacy" namespace so translators can localize without
// touching component JSX. Inline elements that must remain literal
// (the privacy badges, the github link) are rendered via <Trans>
// with named slot placeholders for translator-safe interpolation.
export default function PrivacyPage() {
  const { t } = useTranslation();

  const strong = <strong className="font-semibold text-fg" />;
  const em = <em />;

  // One symmetric comparison matrix instead of two stacked tables. The icon
  // tone is the semantic signal: a working capability reads success, a
  // disabled one reads danger, and "the server can read plaintext" reads as a
  // caution (the honest managed-mode trade-off) rather than a value judgement.
  const rows: Array<{
    capability: string;
    managed: { icon: ReactNode; text: string; dim?: boolean };
    strict: { icon: ReactNode; text: string; dim?: boolean };
  }> = [
    {
      capability: t("privacy.tableServerRead"),
      managed: { icon: <Eye className="h-4 w-4 text-warning" />, text: t("privacy.managedServerRead") },
      strict: { icon: <ShieldCheck className="h-4 w-4 text-success" />, text: t("privacy.strictServerRead") },
    },
    {
      capability: t("privacy.tablePreviews"),
      managed: { icon: <Check className="h-4 w-4 text-success" />, text: t("privacy.available") },
      strict: { icon: <X className="h-4 w-4 text-danger" />, text: t("privacy.strictPreviews"), dim: true },
    },
    {
      capability: t("privacy.tableSearch"),
      managed: { icon: <Check className="h-4 w-4 text-success" />, text: t("privacy.available") },
      strict: { icon: <X className="h-4 w-4 text-danger" />, text: t("privacy.strictSearch"), dim: true },
    },
    {
      capability: t("privacy.tableVirus"),
      managed: { icon: <Check className="h-4 w-4 text-success" />, text: t("privacy.available") },
      strict: { icon: <X className="h-4 w-4 text-danger" />, text: t("privacy.strictVirus"), dim: true },
    },
    {
      capability: t("privacy.tableAdminRecovery"),
      managed: { icon: <Check className="h-4 w-4 text-success" />, text: t("privacy.available") },
      strict: { icon: <X className="h-4 w-4 text-danger" />, text: t("privacy.strictAdminRecovery"), dim: true },
    },
    {
      capability: t("privacy.tableAtRest"),
      managed: { icon: <Check className="h-4 w-4 text-success" />, text: t("privacy.managedAtRest") },
      strict: { icon: <Check className="h-4 w-4 text-success" />, text: t("privacy.strictAtRest") },
    },
  ];

  const brand = (
    <Link
      to="/drive"
      className="rounded-full px-1 text-base font-semibold tracking-tight text-fg focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2 focus-visible:ring-offset-surface"
    >
      <span className="text-brand">ZK</span> Drive
    </Link>
  );
  const backToDrive = (
    <Link
      to="/drive"
      className="inline-flex items-center gap-1.5 rounded-full px-3 py-1.5 text-sm font-medium text-muted transition-colors hover:bg-surface-2 hover:text-fg focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2 focus-visible:ring-offset-surface"
    >
      <ArrowLeft className="h-4 w-4" aria-hidden="true" />
      {t("privacy.backToDrive")}
    </Link>
  );

  return (
    <AppShell brand={brand} actions={backToDrive} maxWidth="lg">
      <PageHeader
        eyebrow={t("privacy.eyebrow")}
        title={t("privacy.pageTitle")}
        description={t("privacy.subtitle")}
      />

      <div className="flex flex-col gap-6">
        <section className="rounded-card border border-border bg-surface p-6">
          <p className="text-base leading-relaxed text-fg">
            <Trans
              i18nKey="privacy.intro"
              components={{
                badgeManaged: <EncryptionBadge mode="managed_encrypted" linkToHelp={false} />,
                badgeStrict: <EncryptionBadge mode="strict_zk" linkToHelp={false} />,
              }}
            />
          </p>
        </section>

        <section className="rounded-card border border-border bg-surface p-6" aria-labelledby="privacy-brand-heading">
          <h2 id="privacy-brand-heading" className="text-lg font-semibold text-fg">
            {t("privacy.brandHeading")}
          </h2>
          <p className="mt-2 text-sm leading-relaxed text-muted">{t("privacy.brandBody")}</p>
          <ul className="mt-3 space-y-2 text-sm leading-relaxed text-muted">
            <li className="flex gap-2">
              <span className="mt-2 h-1.5 w-1.5 shrink-0 rounded-full bg-brand" aria-hidden="true" />
              <span>
                <Trans i18nKey="privacy.brandPointManaged" components={{ strong, em }} />
              </span>
            </li>
            <li className="flex gap-2">
              <span className="mt-2 h-1.5 w-1.5 shrink-0 rounded-full bg-brand" aria-hidden="true" />
              <span>
                <Trans i18nKey="privacy.brandPointStrict" components={{ strong }} />
              </span>
            </li>
          </ul>
        </section>

        <div className="grid gap-4 lg:grid-cols-2">
          <section
            className="flex flex-col rounded-card border border-border bg-surface p-6"
            aria-labelledby="privacy-managed-heading"
          >
            <div className="flex flex-wrap items-center gap-2">
              <EncryptionBadge mode="managed_encrypted" size="header" linkToHelp={false} />
              <h2 id="privacy-managed-heading" className="text-lg font-semibold text-fg">
                {t("privacy.managedHeading")}
              </h2>
              <Badge tone="brand">{t("privacy.theDefault")}</Badge>
            </div>
            <p className="mt-3 text-sm leading-relaxed text-muted">
              <Trans i18nKey="privacy.managedBody" components={{ strong, em }} />
            </p>
            <div className="mt-4 rounded-card border border-border bg-surface-2/40 p-3 text-sm text-muted">
              <span className="font-medium text-fg">{t("privacy.tableHonestName")}: </span>
              <Trans i18nKey="privacy.managedHonestName" components={{ strong }} />
            </div>
          </section>

          <section
            className="flex flex-col rounded-card border border-border bg-surface p-6"
            aria-labelledby="privacy-strict-heading"
          >
            <div className="flex flex-wrap items-center gap-2">
              <EncryptionBadge mode="strict_zk" size="header" linkToHelp={false} />
              <h2 id="privacy-strict-heading" className="text-lg font-semibold text-fg">
                {t("privacy.strictHeading")}
              </h2>
              <Badge tone="neutral">{t("privacy.optInPerFolder")}</Badge>
            </div>
            <p className="mt-3 text-sm leading-relaxed text-muted">
              <Trans i18nKey="privacy.strictBody" components={{ strong }} />
            </p>
            <div className="mt-4 rounded-card border border-border bg-surface-2/40 p-3 text-sm text-muted">
              <span className="font-medium text-fg">{t("privacy.tableReversibility")}: </span>
              {t("privacy.strictReversibility")}
            </div>
          </section>
        </div>

        <section className="rounded-card border border-border bg-surface p-6" aria-labelledby="privacy-comparison-heading">
          <h2 id="privacy-comparison-heading" className="text-lg font-semibold text-fg">
            {t("privacy.comparisonHeading")}
          </h2>
          <div className="mt-4">
            <Table>
              <caption className="sr-only">{t("privacy.comparisonCaption")}</caption>
              <THead>
                <Tr>
                  <Th scope="col">{t("privacy.colCapability")}</Th>
                  <Th scope="col">{t("privacy.managedHeading")}</Th>
                  <Th scope="col">{t("privacy.strictHeading")}</Th>
                </Tr>
              </THead>
              <TBody>
                {rows.map((row) => (
                  <Tr key={row.capability}>
                    <Th scope="row" className="font-medium normal-case tracking-normal text-fg">
                      {row.capability}
                    </Th>
                    <Td>
                      <ComparisonCell icon={row.managed.icon} dim={row.managed.dim}>
                        {row.managed.text}
                      </ComparisonCell>
                    </Td>
                    <Td>
                      <ComparisonCell icon={row.strict.icon} dim={row.strict.dim}>
                        {row.strict.text}
                      </ComparisonCell>
                    </Td>
                  </Tr>
                ))}
              </TBody>
            </Table>
          </div>
        </section>

        <section className="rounded-card border border-border bg-surface p-6" aria-labelledby="privacy-picking-heading">
          <h2 id="privacy-picking-heading" className="text-lg font-semibold text-fg">
            {t("privacy.pickingHeading")}
          </h2>
          <ul className="mt-3 space-y-2 text-sm leading-relaxed text-muted">
            <li className="flex gap-2">
              <span className="mt-2 h-1.5 w-1.5 shrink-0 rounded-full bg-brand" aria-hidden="true" />
              <span>
                <Trans i18nKey="privacy.pickingDefault" components={{ strong }} />
              </span>
            </li>
            <li className="flex gap-2">
              <span className="mt-2 h-1.5 w-1.5 shrink-0 rounded-full bg-brand" aria-hidden="true" />
              <span>
                <Trans i18nKey="privacy.pickingStrict" components={{ strong }} />
              </span>
            </li>
            <li className="flex gap-2">
              <span className="mt-2 h-1.5 w-1.5 shrink-0 rounded-full bg-brand" aria-hidden="true" />
              <span>
                <Trans i18nKey="privacy.pickingPerFolder" components={{ strong }} />
              </span>
            </li>
          </ul>
        </section>

        <section className="rounded-card border border-border bg-surface p-6" aria-labelledby="privacy-audit-heading">
          <h2 id="privacy-audit-heading" className="text-lg font-semibold text-fg">
            {t("privacy.auditHeading")}
          </h2>
          <p className="mt-2 text-sm leading-relaxed text-muted">
            <Trans i18nKey="privacy.auditBody" components={{ em }} />
          </p>
        </section>

        <p className="text-xs leading-relaxed text-muted">
          <Trans
            i18nKey="privacy.operatorNote"
            components={{
              fabricLink: (
                <a
                  href="https://github.com/kennguy3n/zk-object-fabric"
                  rel="noreferrer"
                  className="text-brand underline underline-offset-2 hover:text-brand-hover"
                />
              ),
              code: <code className="rounded bg-surface-2 px-1 py-0.5 font-mono text-[0.85em] text-fg" />,
            }}
          />
        </p>
      </div>
    </AppShell>
  );
}

// ComparisonCell pairs a tokenised status icon with its descriptive text so
// the managed/strict matrix is scannable at a glance and still readable to
// screen readers (the icon is decorative; the text carries the meaning).
function ComparisonCell({
  icon,
  dim,
  children,
}: {
  icon: ReactNode;
  dim?: boolean;
  children: ReactNode;
}) {
  return (
    <span className="flex items-start gap-2">
      <span className="mt-0.5 shrink-0" aria-hidden="true">
        {icon}
      </span>
      <span className={dim ? "text-muted" : "text-fg"}>{children}</span>
    </span>
  );
}

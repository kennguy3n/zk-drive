import { Link } from "react-router-dom";
import { Trans, useTranslation } from "react-i18next";
import EncryptionBadge from "../components/EncryptionBadge";

// PrivacyPage is the customer-facing explainer for ZK Drive's two
// per-folder privacy modes. All long-form copy is sourced from the
// i18n "privacy" namespace so translators can localize without
// touching component JSX. Inline elements that must remain literal
// (the privacy badges, the github link) are rendered via <Trans>
// with named slot placeholders for translator-safe interpolation.
export default function PrivacyPage() {
  const { t } = useTranslation();
  return (
    <div style={{ maxWidth: 880, margin: "0 auto", padding: 24, fontSize: 15, lineHeight: 1.55 }}>
      <header style={{ marginBottom: 16, display: "flex", justifyContent: "space-between", alignItems: "center" }}>
        <h1 style={{ fontSize: 24, margin: 0 }}>{t("privacy.pageTitle")}</h1>
        <Link to="/drive" style={backLink}>&larr; {t("privacy.backToDrive")}</Link>
      </header>

      <p style={{ color: "#374151" }}>
        <Trans
          i18nKey="privacy.intro"
          components={{
            badgeManaged: <EncryptionBadge mode="managed_encrypted" linkToHelp={false} />,
            badgeStrict: <EncryptionBadge mode="strict_zk" linkToHelp={false} />,
          }}
        />
      </p>

      <section style={section} aria-labelledby="brand-heading">
        <h2 id="brand-heading" style={h2}>
          {t("privacy.brandHeading")}
        </h2>
        <p>{t("privacy.brandBody")}</p>
        <ul style={{ paddingLeft: 20, margin: "8px 0 0" }}>
          <li>
            <Trans i18nKey="privacy.brandPointManaged" components={{ strong: <strong />, em: <em /> }} />
          </li>
          <li>
            <Trans i18nKey="privacy.brandPointStrict" components={{ strong: <strong /> }} />
          </li>
        </ul>
      </section>

      <section style={section} aria-labelledby="managed-heading">
        <h2 id="managed-heading" style={h2}>
          <EncryptionBadge mode="managed_encrypted" size="header" linkToHelp={false} /> {t("privacy.managedHeading")}
          <span style={muted}> &nbsp;&middot;&nbsp; {t("privacy.theDefault")}</span>
        </h2>
        <p>
          <Trans i18nKey="privacy.managedBody" components={{ strong: <strong />, em: <em /> }} />
        </p>
        <table style={table}>
          <tbody>
            <tr><th style={th}>{t("privacy.tableServerRead")}</th><td style={td}>{t("privacy.managedServerRead")}</td></tr>
            <tr><th style={th}>{t("privacy.tablePreviews")}</th><td style={td}>{t("privacy.available")}</td></tr>
            <tr><th style={th}>{t("privacy.tableSearch")}</th><td style={td}>{t("privacy.available")}</td></tr>
            <tr><th style={th}>{t("privacy.tableVirus")}</th><td style={td}>{t("privacy.available")}</td></tr>
            <tr><th style={th}>{t("privacy.tableAdminRecovery")}</th><td style={td}>{t("privacy.available")}</td></tr>
            <tr><th style={th}>{t("privacy.tableAtRest")}</th><td style={td}>{t("privacy.managedAtRest")}</td></tr>
            <tr><th style={th}>{t("privacy.tableHonestName")}</th><td style={td}>
              <Trans i18nKey="privacy.managedHonestName" components={{ strong: <strong /> }} />
            </td></tr>
          </tbody>
        </table>
      </section>

      <section style={section} aria-labelledby="strict-heading">
        <h2 id="strict-heading" style={h2}>
          <EncryptionBadge mode="strict_zk" size="header" linkToHelp={false} /> {t("privacy.strictHeading")}
          <span style={muted}> &nbsp;&middot;&nbsp; {t("privacy.optInPerFolder")}</span>
        </h2>
        <p>
          <Trans i18nKey="privacy.strictBody" components={{ strong: <strong /> }} />
        </p>
        <table style={table}>
          <tbody>
            <tr><th style={th}>{t("privacy.tableServerRead")}</th><td style={td}>{t("privacy.strictServerRead")}</td></tr>
            <tr><th style={th}>{t("privacy.tablePreviews")}</th><td style={td}>{t("privacy.strictPreviews")}</td></tr>
            <tr><th style={th}>{t("privacy.tableSearch")}</th><td style={td}>{t("privacy.strictSearch")}</td></tr>
            <tr><th style={th}>{t("privacy.tableVirus")}</th><td style={td}>{t("privacy.strictVirus")}</td></tr>
            <tr><th style={th}>{t("privacy.tableAdminRecovery")}</th><td style={td}>{t("privacy.strictAdminRecovery")}</td></tr>
            <tr><th style={th}>{t("privacy.tableAtRest")}</th><td style={td}>{t("privacy.strictAtRest")}</td></tr>
            <tr><th style={th}>{t("privacy.tableReversibility")}</th><td style={td}>{t("privacy.strictReversibility")}</td></tr>
          </tbody>
        </table>
      </section>

      <section style={section} aria-labelledby="picking-heading">
        <h2 id="picking-heading" style={h2}>{t("privacy.pickingHeading")}</h2>
        <ul style={{ paddingLeft: 20 }}>
          <li>
            <Trans i18nKey="privacy.pickingDefault" components={{ strong: <strong /> }} />
          </li>
          <li>
            <Trans i18nKey="privacy.pickingStrict" components={{ strong: <strong /> }} />
          </li>
          <li>
            <Trans i18nKey="privacy.pickingPerFolder" components={{ strong: <strong /> }} />
          </li>
        </ul>
      </section>

      <section style={section} aria-labelledby="audit-heading">
        <h2 id="audit-heading" style={h2}>{t("privacy.auditHeading")}</h2>
        <p>
          <Trans i18nKey="privacy.auditBody" components={{ em: <em /> }} />
        </p>
      </section>

      <p style={{ color: "#6b7280", fontSize: 13, marginTop: 24 }}>
        <Trans
          i18nKey="privacy.operatorNote"
          components={{
            fabricLink: (
              <a href="https://github.com/kennguy3n/zk-object-fabric" rel="noreferrer" />
            ),
            code: <code />,
          }}
        />
      </p>
    </div>
  );
}

const backLink: React.CSSProperties = {
  fontSize: 13,
  color: "#374151",
  textDecoration: "none",
};

const section: React.CSSProperties = {
  border: "1px solid #e5e7eb",
  borderRadius: 8,
  padding: 16,
  marginTop: 16,
  background: "white",
};

const h2: React.CSSProperties = {
  margin: "0 0 8px",
  fontSize: 18,
  display: "flex",
  alignItems: "center",
  gap: 8,
};

const muted: React.CSSProperties = {
  fontSize: 13,
  fontWeight: 400,
  color: "#6b7280",
};

const table: React.CSSProperties = {
  width: "100%",
  borderCollapse: "collapse",
  fontSize: 14,
  marginTop: 8,
};

const th: React.CSSProperties = {
  textAlign: "left",
  padding: "6px 8px",
  borderTop: "1px solid #f3f4f6",
  color: "#374151",
  width: "40%",
  verticalAlign: "top",
  fontWeight: 500,
};

const td: React.CSSProperties = {
  padding: "6px 8px",
  borderTop: "1px solid #f3f4f6",
  verticalAlign: "top",
  color: "#1f2937",
};

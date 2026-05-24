import { Link } from "react-router-dom";
import EncryptionBadge from "../components/EncryptionBadge";

// PrivacyPage is the customer-facing explainer for ZK Drive's two
// per-folder privacy modes. It is deliberately written to avoid the
// "zero-knowledge by default" framing that docs/PRODUCT.md
// calls out as misleading:
//
//   > Business Secure (managed) is **not** strict zero-knowledge. The
//   > zk-object-fabric gateway can read plaintext in memory during
//   > request handling. This is the right default for most SME use
//   > cases ... but it **must be called "confidential managed
//   > storage," not "zero-knowledge,"** in customer-facing UI.
//
// The page lives at /drive/privacy, behind RequireAuth, so we can link
// to it from the CreateFolderDialog and the FileBrowserPage header —
// the two points where a customer is actually making (or living with)
// a privacy-mode choice. The content is intentionally static (no API
// calls): no plaintext leaves the SPA, and the page works offline as
// part of the PWA shell.
export default function PrivacyPage() {
  return (
    <div style={{ maxWidth: 880, margin: "0 auto", padding: 24, fontSize: 15, lineHeight: 1.55 }}>
      <header style={{ marginBottom: 16, display: "flex", justifyContent: "space-between", alignItems: "center" }}>
        <h1 style={{ fontSize: 24, margin: 0 }}>How your data is protected</h1>
        <Link to="/drive" style={backLink}>&larr; Back to Drive</Link>
      </header>

      <p style={{ color: "#374151" }}>
        ZK Drive lets each folder pick one of two privacy modes. The
        choice is made when the folder is created and is surfaced
        everywhere a folder is shown (sidebar, file list, breadcrumb)
        via a coloured badge:
        {" "}
        <EncryptionBadge mode="managed_encrypted" />
        {" "}or{" "}
        <EncryptionBadge mode="strict_zk" />.
        We try to be honest about the trade-offs rather than market
        both modes as "zero-knowledge" — most providers do that and it
        is not accurate for either one.
      </p>

      <section style={section} aria-labelledby="managed-heading">
        <h2 id="managed-heading" style={h2}>
          <EncryptionBadge mode="managed_encrypted" size="header" /> Confidential managed
          <span style={muted}> &nbsp;&middot;&nbsp; the default</span>
        </h2>
        <p>
          Files are encrypted at rest by the zk-object-fabric storage
          gateway. The encryption keys live on the gateway, not on
          customer devices, so the server <strong>can</strong> read
          plaintext in memory during a request. That is what enables
          previews, full-text search, virus scanning, and admin recovery
          paths. This is the right default for most SMEs &mdash; but it
          is <em>not</em> zero-knowledge, and we say so plainly.
        </p>
        <table style={table}>
          <tbody>
            <tr><th style={th}>Server can read plaintext</th><td style={td}>Yes, in memory during request handling</td></tr>
            <tr><th style={th}>Previews / thumbnails</th><td style={td}>Available</td></tr>
            <tr><th style={th}>Full-text search</th><td style={td}>Available</td></tr>
            <tr><th style={th}>Virus / malware scanning</th><td style={td}>Available</td></tr>
            <tr><th style={th}>Admin recovery if a user loses access</th><td style={td}>Available</td></tr>
            <tr><th style={th}>At-rest encryption</th><td style={td}>Yes, gateway-side (zk-object-fabric ManagedEncrypted)</td></tr>
            <tr><th style={th}>Honest name</th><td style={td}>Confidential managed storage. <strong>Not</strong> zero-knowledge.</td></tr>
          </tbody>
        </table>
      </section>

      <section style={section} aria-labelledby="strict-heading">
        <h2 id="strict-heading" style={h2}>
          <EncryptionBadge mode="strict_zk" size="header" /> Strict zero-knowledge
          <span style={muted}> &nbsp;&middot;&nbsp; opt-in, per folder</span>
        </h2>
        <p>
          Files are encrypted on your device with keys the server never
          sees. The gateway only ever stores opaque ciphertext for these
          folders. Because the server cannot decrypt the contents,
          previews, search, virus scanning, and admin password-reset
          recovery are all <strong>disabled</strong> for content in
          these folders &mdash; that is the honest cost of the
          guarantee. Choose this mode only for content where that
          trade-off is worth it.
        </p>
        <table style={table}>
          <tbody>
            <tr><th style={th}>Server can read plaintext</th><td style={td}>No, ever</td></tr>
            <tr><th style={th}>Previews / thumbnails</th><td style={td}>Disabled (server has no plaintext)</td></tr>
            <tr><th style={th}>Full-text search</th><td style={td}>Disabled (metadata-only search still works)</td></tr>
            <tr><th style={th}>Virus / malware scanning</th><td style={td}>Disabled (cannot scan what we cannot read)</td></tr>
            <tr><th style={th}>Admin recovery if a user loses access</th><td style={td}>Not possible &mdash; keys live on the client only</td></tr>
            <tr><th style={th}>At-rest encryption</th><td style={td}>Yes, end-to-end via the client SDK</td></tr>
            <tr><th style={th}>Reversibility</th><td style={td}>One-way: a folder cannot be downgraded back to managed once it has strict-ZK content, because the server never had the plaintext</td></tr>
          </tbody>
        </table>
      </section>

      <section style={section} aria-labelledby="picking-heading">
        <h2 id="picking-heading" style={h2}>Which one should this folder be?</h2>
        <ul style={{ paddingLeft: 20 }}>
          <li>
            <strong>Default to confidential managed</strong> for everyday
            documents, project workspaces, client deliverables, and
            anything you want to find by typing into search.
          </li>
          <li>
            <strong>Use strict zero-knowledge</strong> for legal hold,
            HR-investigation evidence, M&amp;A diligence rooms, source
            code repos under NDA, customer credentials &mdash; anywhere
            losing the trade-off (previews, search, virus scan) is the
            right price for keeping the server out of the loop.
          </li>
          <li>
            <strong>Pick per folder, not per workspace.</strong> The
            choice is at the folder level so you can mix sensitive and
            non-sensitive content in the same workspace without giving
            up either mode.
          </li>
        </ul>
      </section>

      <section style={section} aria-labelledby="audit-heading">
        <h2 id="audit-heading" style={h2}>What the server logs</h2>
        <p>
          Both modes record the same audit metadata: who created the
          folder, when, parent folder, and the chosen privacy mode.
          File-level metadata (name, size, type, version, modification
          time, who shared it with whom) is also recorded in both modes
          &mdash; this is what lets share-links, retention policies,
          guest invitations, and billing work. For strict-zero-knowledge
          folders <em>nothing else</em> is recorded: no thumbnails, no
          extracted text, no scan verdict, no preview cache.
        </p>
      </section>

      <p style={{ color: "#6b7280", fontSize: 13, marginTop: 24 }}>
        Operator note: the underlying storage layer is{" "}
        <a href="https://github.com/kennguy3n/zk-object-fabric" rel="noreferrer">
          zk-object-fabric
        </a>
        . Mode mapping: confidential managed = <code>ManagedEncrypted</code>;
        strict zero-knowledge = <code>StrictZK</code>. See the README
        and PROPOSAL §3.3 for the full threat model.
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
  fontSize: 18,
  margin: "0 0 12px",
  display: "flex",
  alignItems: "center",
  gap: 8,
  flexWrap: "wrap",
};

const muted: React.CSSProperties = {
  fontSize: 13,
  color: "#6b7280",
  fontWeight: "normal",
};

const table: React.CSSProperties = {
  width: "100%",
  borderCollapse: "collapse",
  fontSize: 14,
};

const th: React.CSSProperties = {
  textAlign: "left",
  padding: "6px 10px",
  borderBottom: "1px solid #f3f4f6",
  width: "40%",
  color: "#374151",
  fontWeight: 500,
  verticalAlign: "top",
};

const td: React.CSSProperties = {
  padding: "6px 10px",
  borderBottom: "1px solid #f3f4f6",
  color: "#111827",
  verticalAlign: "top",
};

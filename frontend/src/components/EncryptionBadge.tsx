import { Link } from "react-router-dom";

// EncryptionBadge renders the privacy mode of a folder as a small pill
// next to its name. Mode strings come from the API (`encryption_mode`
// field on Folder) and map to two product-named modes per
// docs/PRODUCT.md "Per-folder privacy modes":
//
//   - "strict_zk"          -> "zero-knowledge"
//                             (opt-in; server never sees plaintext;
//                              no preview / no search / no virus scan)
//   - "managed_encrypted"  -> "confidential" (the default)
//                             (gateway-side encryption at rest; server
//                              can read plaintext in memory during
//                              request handling, which is what enables
//                              preview / search / virus scanning)
//   - anything else / missing -> default to the confidential rendering,
//                                so legacy rows without the field stay
//                                clean
//
// The badge label is deliberately short — "confidential" /
// "zero-knowledge" — to match the customer-facing brand vocabulary
// (docs/BRAND.md): the product is *not* zero-knowledge by default, and
// the badge must never claim it is. The server-readable trade-off is
// surfaced through the badge tooltip (below) and the PrivacyPage table.
// Per docs/PRODUCT.md, ManagedEncrypted must be called "confidential"
// in customer-facing UI — never "zero-knowledge", "encrypted-at-rest
// only", or anything that implies the server cannot read the bytes.
//
// EncryptionMode is defined in api/client.ts (the wire-level types
// module) so the API request/response shape and the UI share a single
// source of truth for the union. Re-exported here for ergonomic imports
// from UI code (`import EncryptionBadge, { type EncryptionMode } from
// ".../EncryptionBadge"`).
export type { EncryptionMode } from "../api/client";

export interface EncryptionBadgeProps {
  // The badge takes a plain `string | undefined` (not `EncryptionMode`)
  // at the type level so it can render legacy folder rows that don't
  // carry the field at all, OR a hypothetical future mode the server
  // starts emitting before the frontend has been re-deployed. The
  // runtime branch (`mode === "strict_zk"`) is the single source of
  // truth — unknown values fall through to the confidential rendering,
  // matching the documented "missing -> confidential" contract above.
  // Callers that DO know the mode at compile time should still declare
  // their own state as `EncryptionMode` for autocomplete; the
  // assignment widens to string here at the prop boundary, which is
  // fine.
  mode?: string;
  // size lets callers pick "row" (small, alongside file/folder names)
  // or "header" (larger, alongside the current folder in the breadcrumb).
  // Both render the same colour / label; only padding + font scale up.
  size?: "row" | "header";
  // linkToHelp controls whether the badge renders as a clickable
  // `<Link>` to /drive/privacy (the brand-aligned customer explainer
  // page) or as a plain `<span>`. Default true so every appearance of
  // the badge is also a discovery affordance for the privacy modes
  // documentation — a small but load-bearing piece of the "be honest
  // about what 'ZK' means" rebrand (see docs/BRAND.md). Two situations
  // require linkToHelp={false}:
  //
  //   1. The badge is rendered INSIDE another `<a>` / `<Link>` (e.g.
  //      a folder row whose entire surface is the navigation target).
  //      Nesting `<a>` is invalid HTML; callers must hoist the badge
  //      out of the outer anchor or pass linkToHelp={false}.
  //   2. The badge is rendered ON the privacy page itself, where the
  //      same-page link would do nothing useful.
  linkToHelp?: boolean;
}

export default function EncryptionBadge({
  mode,
  size = "row",
  linkToHelp = true,
}: EncryptionBadgeProps) {
  const isStrict = mode === "strict_zk";
  const label = isStrict ? "zero-knowledge" : "confidential";
  // The tooltip retains the longer, technically-precise framing so a
  // user who hovers gets the full "server can read plaintext in
  // memory" disclosure. The short pill label is for at-a-glance
  // recognition; the tooltip is for the trade-off.
  //
  // The "Click to learn more" hint is only appended when the badge is
  // actually clickable (linkToHelp=true). On the privacy page itself,
  // and anywhere else that opts out of the help link, the badge is a
  // plain `<span>` and the hint would be a lie — so we suppress it.
  const body = isStrict
    ? "Zero-knowledge: end-to-end encrypted. The server cannot decrypt this folder, so previews, full-text search, and virus scanning are disabled here."
    : "Confidential (managed encrypted storage): encrypted at rest, but the server can read plaintext in memory during request handling — which is what enables previews, full-text search, and virus scanning. NOT zero-knowledge.";
  const title = linkToHelp
    ? `${body} Click to learn more about privacy modes.`
    : body;
  const padding = size === "header" ? "2px 10px" : "1px 6px";
  const fontSize = size === "header" ? 12 : 10;
  const style = {
    fontSize,
    padding,
    borderRadius: 999,
    background: isStrict ? "#fee2e2" : "#dcfce7",
    color: isStrict ? "#991b1b" : "#166534",
    whiteSpace: "nowrap" as const,
    // border on the badge makes both variants legible against the
    // page background even in high-contrast / forced-colors mode.
    border: `1px solid ${isStrict ? "#fca5a5" : "#86efac"}`,
    textDecoration: "none",
    display: "inline-block",
  };
  const dataMode = isStrict ? "strict_zk" : "managed_encrypted";

  if (linkToHelp) {
    return (
      <Link
        to="/drive/privacy"
        title={title}
        data-testid="encryption-badge"
        data-mode={dataMode}
        // The badge link must not steal focus when keyboard users tab
        // through file rows — the parent row navigation should still be
        // the primary affordance. We keep it focusable (default) so
        // screen reader users can reach it, but the visual outline is
        // suppressed in favour of the existing pill background; assistive
        // tech still announces the link role + accessible name.
        aria-label={`${label} — learn about privacy modes`}
        style={style}
      >
        {label}
      </Link>
    );
  }

  return (
    <span
      title={title}
      data-testid="encryption-badge"
      data-mode={dataMode}
      style={style}
    >
      {label}
    </span>
  );
}

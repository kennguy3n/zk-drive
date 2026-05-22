// EncryptionBadge renders the privacy mode of a folder as a small pill
// next to its name. Mode strings come from the API (`encryption_mode`
// field on Folder) and map to two product-named modes per PROPOSAL.md
// §3.3:
//
//   - "strict_zk"          -> "strict zero-knowledge"
//                             (opt-in; server never sees plaintext;
//                              no preview / no search / no virus scan)
//   - "managed_encrypted"  -> "managed" (the default)
//                             (gateway-side encryption at rest; server
//                              can read plaintext in memory during
//                              request handling, which is what enables
//                              preview / search / virus scanning)
//   - anything else / missing -> default to the managed rendering, so
//                                pre-Phase-4 rows stay clean
//
// The badge label is deliberately kept to the short canonical mode
// name ("managed" / "strict zero-knowledge") so it matches the
// dialog's "Confidential managed (default)" / "Strict zero-knowledge"
// radio labels and the PrivacyPage section headings — customers see
// the same vocabulary everywhere a folder is rendered. The
// server-readable trade-off is surfaced through the badge tooltip
// (below) and the PrivacyPage table, not through the badge text
// itself, to avoid two different customer-facing labels for the same
// mode. PROPOSAL.md §3.3 is explicit only about NOT calling the
// managed mode "zero-knowledge" — "managed" satisfies that
// constraint and matches the brand vocabulary used elsewhere.
export type EncryptionMode = "strict_zk" | "managed_encrypted" | string;

export interface EncryptionBadgeProps {
  mode?: EncryptionMode;
  // size lets callers pick "row" (small, alongside file/folder names)
  // or "header" (larger, alongside the current folder in the breadcrumb).
  // Both render the same colour / label; only padding + font scale up.
  size?: "row" | "header";
}

export default function EncryptionBadge({ mode, size = "row" }: EncryptionBadgeProps) {
  const isStrict = mode === "strict_zk";
  const label = isStrict ? "strict zero-knowledge" : "managed";
  const title = isStrict
    ? "Strict zero-knowledge: end-to-end encrypted. The server cannot decrypt this folder, so previews, full-text search, and virus scanning are disabled here."
    : "Server-readable (confidential managed storage): encrypted at rest, but the server can read plaintext in memory during request handling — which is what enables previews, full-text search, and virus scanning.";
  const padding = size === "header" ? "2px 10px" : "1px 6px";
  const fontSize = size === "header" ? 12 : 10;
  return (
    <span
      title={title}
      data-testid="encryption-badge"
      data-mode={isStrict ? "strict_zk" : "managed_encrypted"}
      style={{
        fontSize,
        padding,
        borderRadius: 999,
        background: isStrict ? "#fee2e2" : "#dcfce7",
        color: isStrict ? "#991b1b" : "#166534",
        whiteSpace: "nowrap",
        // border on the badge makes both variants legible against the
        // page background even in high-contrast / forced-colors mode.
        border: `1px solid ${isStrict ? "#fca5a5" : "#86efac"}`,
      }}
    >
      {label}
    </span>
  );
}

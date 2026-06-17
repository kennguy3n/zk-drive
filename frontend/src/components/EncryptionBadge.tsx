import { Link } from "react-router-dom";
import { useTranslation } from "react-i18next";
import { Badge } from "./ui";
import { cn } from "../lib/cn";

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
  // tabbable controls whether the badge participates in the keyboard
  // tab order. Default true so a solo badge (e.g. breadcrumb header,
  // dialog comparison) is reachable by Tab like any other link. Pass
  // tabbable={false} when the badge is rendered in a repeated list
  // (sidebar tree, subfolder card grid) so the user does not have to
  // tab through N identical-destination links — the dedicated
  // "Privacy" link in the app header is the primary keyboard
  // discovery affordance in that case. Screen-reader users still
  // reach skip-tabbed badges via arrow-key / virtual-cursor
  // navigation and hear the link role + accessible name. Ignored
  // when linkToHelp={false} (a `<span>` has no tab order anyway).
  tabbable?: boolean;
}

export default function EncryptionBadge({
  mode,
  size = "row",
  linkToHelp = true,
  tabbable = true,
}: EncryptionBadgeProps) {
  const { t } = useTranslation();
  const isStrict = mode === "strict_zk";
  const label = isStrict ? t("encryption.zeroKnowledge") : t("encryption.confidential");
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
    ? t("encryption.strictZkTooltip")
    : t("encryption.confidentialTooltip");
  const title = linkToHelp
    ? `${body} ${t("encryption.clickToLearnMore")}`
    : body;
  const dataMode = isStrict ? "strict_zk" : "managed_encrypted";

  // Tokenised tones (no hard-coded hex): strict zero-knowledge wears the
  // KChat brand indigo — it is the premium, server-blind mode and the
  // colour signals "this is the strong one" rather than "danger".
  // Confidential (the managed default) wears success green: encrypted at
  // rest, server-readable. The leading dot makes the two modes scannable
  // at a glance in dense file lists. Both tones re-theme with the app and
  // flip correctly in dark mode because they resolve from CSS variables.
  const tone = isStrict ? "brand" : "success";
  const badge = (
    <Badge
      tone={tone}
      dot
      className={cn(
        "whitespace-nowrap",
        // The "header" size sits next to the breadcrumb folder name, so it
        // scales up a notch. Tailwind orders utilities by ascending scale,
        // so these larger values reliably win over Badge's defaults.
        size === "header" && "px-3 py-1 text-sm",
      )}
    >
      {label}
    </Badge>
  );

  if (linkToHelp) {
    return (
      <Link
        to="/drive/privacy"
        title={title}
        data-testid="encryption-badge"
        data-mode={dataMode}
        // tabIndex={-1} removes the badge from the keyboard tab order
        // without removing the link role from assistive tech (screen
        // reader users still reach it via arrow keys / virtual cursor
        // and hear the link + accessible name). Used in list contexts
        // where the parent row navigation is the primary affordance
        // and tabbing through N identical-destination badges would be
        // noise. See the `tabbable` prop comment above.
        tabIndex={tabbable ? undefined : -1}
        aria-label={t("encryption.badgeAria", { label })}
        className="inline-flex rounded-full no-underline focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2 focus-visible:ring-offset-bg"
      >
        {badge}
      </Link>
    );
  }

  return (
    <span
      title={title}
      data-testid="encryption-badge"
      data-mode={dataMode}
      className="inline-flex"
    >
      {badge}
    </span>
  );
}

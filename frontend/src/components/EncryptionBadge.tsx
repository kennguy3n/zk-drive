import { Link } from "react-router-dom";
import { useTranslation } from "react-i18next";
import { Lock, ShieldCheck } from "lucide-react";
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
  // size lets callers pick the density:
  //   - "row"    small text pill alongside file/folder names
  //   - "header" larger pill (leading dot) for the breadcrumb
  //   - "icon"   icon-only indicator for dense lists (the sidebar tree),
  //              where even the compact pill would crowd the name out of
  //              its slot. The mode reads from the icon + colour (lock /
  //              brand for zero-knowledge, shield / success for
  //              confidential) and the full explanation stays in the
  //              tooltip + aria-label.
  // All densities share the same colour vocabulary and accessible name.
  size?: "row" | "header" | "icon";
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
  // rest, server-readable. Both tones re-theme with the app and flip
  // correctly in dark mode because they resolve from CSS variables.
  //
  // The pill is sized for its context. The dense "row" variant sits next
  // to file/folder names in tight, multi-column lists — e.g. the two-up
  // subfolder cards in the drive, where a card budgets only ~100px for
  // name + badge after the Share/Delete actions. It therefore stays
  // deliberately compact (and `shrink-0`, below) so the sibling name
  // keeps layout priority and truncates with an ellipsis instead of
  // collapsing to zero width. The "header" variant sits in the breadcrumb
  // where there is room to breathe, so it scales up and adds a leading
  // dot for extra scannability. The shared `Badge` primitive's fixed
  // footprint (text-xs / px-2.5 / leading dot) overflows the row slot and
  // can't be trimmed via className (cn is clsx-only, so the larger scale
  // wins), so the pill is composed locally here from the same tone tokens
  // — see the PR description's coordinator follow-up proposing a Badge
  // `size="sm"` variant.
  const toneClasses = isStrict
    ? "bg-brand/10 text-brand"
    : "bg-success/10 text-success";
  const dotClass = isStrict ? "bg-brand" : "bg-success";
  const isHeader = size === "header";
  const isIcon = size === "icon";
  const textTone = isStrict ? "text-brand" : "text-success";
  const Icon = isStrict ? Lock : ShieldCheck;
  const badge = isIcon ? (
    // The icon-only density has no visible text, so it needs its own
    // accessible name when it is NOT wrapped in the help <Link> (which already
    // carries aria-label below). Giving the span role="img" + aria-label only
    // in the linkToHelp={false} case keeps it announced for screen readers
    // without double-labelling the link; the glyph itself stays aria-hidden.
    <span
      role={linkToHelp ? undefined : "img"}
      aria-label={linkToHelp ? undefined : t("encryption.badgeAria", { label })}
      className={cn("inline-flex items-center justify-center", textTone)}
    >
      <Icon className="h-4 w-4 shrink-0" aria-hidden="true" />
    </span>
  ) : (
    <span
      className={cn(
        "inline-flex items-center rounded-full font-medium leading-none whitespace-nowrap",
        toneClasses,
        isHeader ? "gap-1.5 px-2.5 py-1 text-xs" : "px-1.5 py-0.5 text-[10px]",
      )}
    >
      {isHeader && (
        <span
          className={cn("h-1.5 w-1.5 shrink-0 rounded-full", dotClass)}
          aria-hidden="true"
        />
      )}
      {label}
    </span>
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
        className="inline-flex shrink-0 rounded-full no-underline focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2 focus-visible:ring-offset-bg"
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
      className="inline-flex shrink-0"
    >
      {badge}
    </span>
  );
}

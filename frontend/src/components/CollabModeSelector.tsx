// CollabModeSelector renders a radio-list of collab modes
// (markdown / rich / rich_presence) gated by the parent folder's
// encryption_mode. The set of allowed modes is computed server-side
// (api/drive/document.go newDocumentResponse → AllowedModes) and
// passed in here verbatim; modes outside that set are greyed-out
// and carry an explanation of WHY they're unavailable.
//
// The component is used by:
//   1. New-document dialog on DocumentListPage (initial choice)
//   2. Document settings dropdown on DocumentEditorPage
//      (change-collab-mode action)
//
// Each option is a KChat `RadioCard`; the set is wrapped in a
// `role="radiogroup"` with an accessible name so assistive tech
// announces it as a single radio group (RadioCard renders one
// `role="radio"` cell and explicitly does NOT provide its own group).

import { useId, type ReactNode } from "react";
import { useTranslation } from "react-i18next";
import type { TFunction } from "i18next";
import { FileText, Table2, Users } from "lucide-react";
import { RadioCard } from "./ui";
import type { CollabMode, EncryptionMode } from "../api/client";

// MODES is the public ordering: least → most feature-rich. We
// intentionally do NOT include "disabled" in the user-facing list —
// the tombstone state is server-set on privacy regression (which is
// currently impossible since folder mode is immutable) and is not
// a thing the user picks.
const MODES: {
  value: CollabMode;
  titleKey: string;
  descriptionKey: string;
  icon: ReactNode;
}[] = [
  {
    value: "markdown",
    titleKey: "collab.markdown",
    descriptionKey: "collab.markdownDesc",
    icon: <FileText className="h-5 w-5" aria-hidden="true" />,
  },
  {
    value: "rich",
    titleKey: "collab.rich",
    descriptionKey: "collab.richDesc",
    icon: <Table2 className="h-5 w-5" aria-hidden="true" />,
  },
  {
    value: "rich_presence",
    titleKey: "collab.richPresence",
    descriptionKey: "collab.richPresenceDesc",
    icon: <Users className="h-5 w-5" aria-hidden="true" />,
  },
];

export interface CollabModeSelectorProps {
  value: CollabMode;
  onChange: (m: CollabMode) => void;
  // allowedModes mirrors the server's allowed_collab_modes field
  // — drives both the disabled state of each radio AND the
  // explanation shown when a mode is unavailable.
  allowedModes: CollabMode[];
  // encryptionMode is shown in the disabled-mode explanation so the
  // user understands WHY rich isn't available ("this folder is
  // strict_zk"). Falls back to a generic message when missing.
  encryptionMode?: EncryptionMode;
  // id is forwarded to the underlying radio group for ARIA wiring.
  id?: string;
  // disabled forces every radio off, regardless of allowedModes. The
  // editor's settings dropdown sets this while a setCollabMode PATCH
  // is in flight so the user can't rapid-click a second mode-change
  // before the first one resolves (Devin Review
  // ANALYSIS_pr-review-job-d387c.._0004). Defaults to false so
  // existing call sites (new-doc dialog) are unaffected.
  disabled?: boolean;
}

// disabledExplanation returns the text shown when a mode is greyed
// out. The encryptionMode determines the specific rationale
// (strict_zk has no server-side merge → no rich).
function disabledExplanation(
  t: TFunction,
  mode: CollabMode,
  encryptionMode?: EncryptionMode,
): string {
  const modeName = mode === "rich" ? t("collab.rich") : t("collab.richPresence");
  if (encryptionMode === "strict_zk") {
    return t("collab.disabledStrictZk", { mode: modeName });
  }
  return t("collab.disabledOther", { mode: modeName });
}

export default function CollabModeSelector({
  value,
  onChange,
  allowedModes,
  encryptionMode,
  id,
  disabled = false,
}: CollabModeSelectorProps) {
  const { t } = useTranslation();
  const labelId = useId();
  // The richest allowed mode (last in MODES order) is the recommended
  // default — surface a "Recommended" badge so non-technical users
  // pick the best collaborative experience without reading every line.
  const recommended = [...MODES]
    .reverse()
    .find((m) => allowedModes.includes(m.value))?.value;

  return (
    <div className="flex flex-col gap-2">
      <div className="flex items-center justify-between gap-2">
        <span id={labelId} className="text-sm font-medium text-fg">
          {t("collab.editorExperience")}
        </span>
        {/* When the parent disables the whole group it's because a
            setCollabMode PATCH is in flight. The cards convey this
            visually (dimmed + non-interactive), but RadioCard can't carry
            a native tooltip (pointer-events-none when disabled), so we
            restore the textual "Saving…" hint here. */}
        {disabled && (
          <span className="text-xs font-medium text-muted" role="status">
            {t("common.saving")}
          </span>
        )}
      </div>
      <div
        id={id}
        role="radiogroup"
        aria-labelledby={labelId}
        aria-busy={disabled || undefined}
        title={disabled ? t("common.saving") : undefined}
        className="flex flex-col gap-2"
      >
        {MODES.map((m) => {
          const allowedByPolicy = allowedModes.includes(m.value);
          const radioDisabled = !allowedByPolicy || disabled;
          const description = allowedByPolicy
            ? t(m.descriptionKey)
            : disabledExplanation(t, m.value, encryptionMode);
          // The badge slot renders in brand colour, so it's reserved for
          // the positive "Recommended" hint; unavailability is conveyed by
          // the greyed card + the inline explanation instead.
          const badge =
            allowedByPolicy && m.value === recommended
              ? t("collab.recommended")
              : undefined;
          return (
            <RadioCard
              key={m.value}
              selected={value === m.value}
              onSelect={() => onChange(m.value)}
              disabled={radioDisabled}
              icon={m.icon}
              title={t(m.titleKey)}
              description={description}
              badge={badge}
            />
          );
        })}
      </div>
    </div>
  );
}

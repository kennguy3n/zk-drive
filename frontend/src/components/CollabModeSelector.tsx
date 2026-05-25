// CollabModeSelector renders a radio-list of collab modes
// (markdown / rich / rich_presence) gated by the parent folder's
// encryption_mode. The set of allowed modes is computed server-side
// (api/drive/document.go newDocumentResponse → AllowedModes) and
// passed in here verbatim; modes outside that set are greyed-out
// and carry a tooltip explaining WHY they're unavailable.
//
// The component is used by:
//   1. New-document dialog on DocumentListPage (initial choice)
//   2. Document settings dropdown on DocumentEditorPage
//      (change-collab-mode action)

import type { CollabMode, EncryptionMode } from "../api/client";

// MODES is the public ordering: least → most feature-rich. We
// intentionally do NOT include "disabled" in the user-facing list —
// the tombstone state is server-set on privacy regression (which is
// currently impossible since folder mode is immutable) and is not
// a thing the user picks.
const MODES: { value: CollabMode; title: string; description: string }[] = [
  {
    value: "markdown",
    title: "Markdown",
    description: "Headings, lists, basic text. Allowed in every privacy mode.",
  },
  {
    value: "rich",
    title: "Rich",
    description: "Tables, images, embeds. Requires confidential folders.",
  },
  {
    value: "rich_presence",
    title: "Rich + presence",
    description: "Live cursors and online users. Requires confidential folders.",
  },
];

export interface CollabModeSelectorProps {
  value: CollabMode;
  onChange: (m: CollabMode) => void;
  // allowedModes mirrors the server's allowed_collab_modes field
  // — drives both the disabled state of each radio AND the tooltip
  // shown when the user hovers a disabled option.
  allowedModes: CollabMode[];
  // encryptionMode is shown in the disabled-mode tooltip so the
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

// disabledExplanation returns the tooltip text shown when a mode
// is greyed out. The encryptionMode determines the specific
// rationale (strict_zk has no server-side merge → no rich).
function disabledExplanation(
  mode: CollabMode,
  encryptionMode?: EncryptionMode,
): string {
  if (encryptionMode === "strict_zk") {
    return `${mode === "rich" ? "Rich" : "Rich + presence"} requires the server to merge document updates, which is not possible in zero-knowledge folders where the server cannot read the document. Choose Markdown for this folder.`;
  }
  return `${mode === "rich" ? "Rich" : "Rich + presence"} is not allowed for this document's folder.`;
}

export default function CollabModeSelector({
  value,
  onChange,
  allowedModes,
  encryptionMode,
  id,
  disabled = false,
}: CollabModeSelectorProps) {
  return (
    <fieldset
      id={id}
      style={{
        border: "1px solid #e5e7eb",
        borderRadius: 4,
        padding: 12,
        margin: 0,
      }}
    >
      <legend style={{ fontSize: 13, color: "#374151", padding: "0 6px" }}>
        Editor experience
      </legend>
      {MODES.map((m) => {
        const allowedByPolicy = allowedModes.includes(m.value);
        const radioDisabled = !allowedByPolicy || disabled;
        const checked = value === m.value;
        const tooltip = !allowedByPolicy
          ? disabledExplanation(m.value, encryptionMode)
          : disabled
            ? "Saving\u2026"
            : m.description;
        return (
          <label
            key={m.value}
            title={tooltip}
            style={{
              display: "flex",
              alignItems: "flex-start",
              gap: 8,
              padding: "6px 0",
              cursor: radioDisabled ? "not-allowed" : "pointer",
              opacity: radioDisabled ? 0.5 : 1,
            }}
          >
            <input
              type="radio"
              name={`collab-mode-${id ?? "default"}`}
              value={m.value}
              checked={checked}
              disabled={radioDisabled}
              onChange={() => !radioDisabled && onChange(m.value)}
              style={{ marginTop: 3 }}
            />
            <span>
              <span style={{ fontWeight: 500 }}>{m.title}</span>
              <span
                style={{
                  display: "block",
                  fontSize: 12,
                  color: "#6b7280",
                }}
              >
                {m.description}
              </span>
            </span>
          </label>
        );
      })}
    </fieldset>
  );
}

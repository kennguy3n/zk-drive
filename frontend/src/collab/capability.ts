// Client-side mirror of internal/document/capability.go for use in
// the "new document" dialog where we don't yet have a server-issued
// allowed_collab_modes list (the doc doesn't exist yet). The Go
// resolver remains the source of truth at create-time — every
// /api/v1/documents POST is validated server-side against
// IsCollabModeAllowed before insertion — so this mirror is purely
// a UX optimisation. If the two ever drift, the server rejects the
// create with 422 and the dialog surfaces the error inline.

import type { CollabMode, EncryptionMode } from "../api/client";

export interface ResolvedCapability {
  serverSnapshotAllowed: boolean;
  richExtensionsAllowed: boolean;
  presenceAllowed: boolean;
}

// resolveCapability mirrors document.ResolveCapability(encryptionMode).
// Unknown / missing modes fall through to the most conservative
// capability (zero-knowledge) so a future server-side mode that
// ships before the frontend re-deploys fails closed.
export function resolveCapability(
  encryptionMode: EncryptionMode | string | undefined,
): ResolvedCapability {
  if (encryptionMode === "managed_encrypted") {
    return {
      serverSnapshotAllowed: true,
      richExtensionsAllowed: true,
      presenceAllowed: true,
    };
  }
  // strict_zk and any unknown / legacy value fall here.
  return {
    serverSnapshotAllowed: false,
    richExtensionsAllowed: false,
    presenceAllowed: false,
  };
}

// resolveAllowedCollabModes mirrors document.AllowedCollabModesFor.
// Returns the list of CollabMode values the user is allowed to pick
// at create time, ordered least → most feature-rich so a radio
// list reads top-to-bottom by capability.
export function resolveAllowedCollabModes(
  encryptionMode: EncryptionMode | string | undefined,
): CollabMode[] {
  const cap = resolveCapability(encryptionMode);
  const out: CollabMode[] = ["markdown"];
  if (cap.serverSnapshotAllowed && cap.richExtensionsAllowed) {
    out.push("rich");
  }
  if (
    cap.serverSnapshotAllowed &&
    cap.richExtensionsAllowed &&
    cap.presenceAllowed
  ) {
    out.push("rich_presence");
  }
  return out;
}

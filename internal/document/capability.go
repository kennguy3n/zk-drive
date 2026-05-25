package document

import "github.com/kennguy3n/zk-drive/internal/folder"

// Capability is the set of collab features a (folder.EncryptionMode,
// document.CollabMode) pair is allowed to exercise. The resolver
// below is the single source of truth — handlers, the WebSocket
// provider, and the frontend (via a /capabilities endpoint) all
// derive their gating from this struct rather than re-implementing
// the privacy policy.
//
// Three knobs cover every collab feature in P2:
//
//   - ServerSnapshotAllowed: can the server decrypt + merge Yjs
//     updates to maintain a canonical y_state? True for managed_
//     encrypted folders; false for strict_zk (deltas are stored
//     opaquely and forwarded to clients without merging).
//
//   - RichExtensionsAllowed: are tables, embeds, OCR'd images, and
//     other rich extensions enabled? True for managed_encrypted
//     (because the server can preview-render attached assets);
//     false for strict_zk where the server can't see the content
//     and thus can't preview-render embedded files.
//
//   - PresenceAllowed: is the Yjs awareness protocol (cursors,
//     selections, live user list) routed through the server? True
//     for managed_encrypted; false for strict_zk because awareness
//     payloads include cursor positions which would leak document
//     structure (paragraph counts, section lengths) to a server that
//     can't read the document content.
type Capability struct {
	ServerSnapshotAllowed bool `json:"server_snapshot_allowed"`
	RichExtensionsAllowed bool `json:"rich_extensions_allowed"`
	PresenceAllowed       bool `json:"presence_allowed"`
}

// ResolveCapability returns the maximum capability set for a folder
// in `encryptionMode`. The user then picks a CollabMode within
// these bounds; AllowedCollabModesFor returns the explicit list.
//
// Unrecognised modes default to the most conservative ZK capability
// (no server snapshot, no rich, no presence) so a future folder
// mode that lands before this resolver is updated fails closed.
func ResolveCapability(encryptionMode string) Capability {
	switch encryptionMode {
	case folder.EncryptionManagedEncrypted:
		return Capability{
			ServerSnapshotAllowed: true,
			RichExtensionsAllowed: true,
			PresenceAllowed:       true,
		}
	case folder.EncryptionStrictZK:
		return Capability{
			ServerSnapshotAllowed: false,
			RichExtensionsAllowed: false,
			PresenceAllowed:       false,
		}
	default:
		return Capability{}
	}
}

// AllowedCollabModesFor returns the collab_mode values the user may
// set on a document in a folder of the given encryption mode. The
// list is ordered from least to most feature-rich so a UI can use
// it as a radio-button order. The 'disabled' tombstone is NOT
// returned; it's set by the service on a privacy-regression path
// (currently impossible since folder mode is immutable) and is not
// a user-selectable mode.
func AllowedCollabModesFor(encryptionMode string) []string {
	cap := ResolveCapability(encryptionMode)
	out := []string{CollabModeMarkdown}
	if cap.ServerSnapshotAllowed && cap.RichExtensionsAllowed {
		out = append(out, CollabModeRich)
	}
	if cap.ServerSnapshotAllowed && cap.RichExtensionsAllowed && cap.PresenceAllowed {
		out = append(out, CollabModeRichPresence)
	}
	return out
}

// IsCollabModeAllowed reports whether `collabMode` is one of the
// allowed modes for a folder in `encryptionMode`. Wrong mode is the
// only way to fail the user-supplied collab_mode validation; the
// shape of the string is validated separately by IsValidCollabMode.
func IsCollabModeAllowed(encryptionMode, collabMode string) bool {
	if collabMode == CollabModeDisabled {
		// Tombstone state — always settable by the service (never
		// by a user). Reject when validating user input.
		return false
	}
	for _, allowed := range AllowedCollabModesFor(encryptionMode) {
		if allowed == collabMode {
			return true
		}
	}
	return false
}

// DefaultCollabModeFor returns the default collab_mode to assign
// when a user creates a document and doesn't specify one. We pick
// the richest mode the folder allows so a managed_encrypted folder
// gets rich+presence by default (matching Google Docs expectations)
// while strict_zk falls back to markdown.
func DefaultCollabModeFor(encryptionMode string) string {
	allowed := AllowedCollabModesFor(encryptionMode)
	if len(allowed) == 0 {
		return CollabModeMarkdown
	}
	return allowed[len(allowed)-1]
}

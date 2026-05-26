package workspace

import (
	"sort"
	"time"

	"github.com/google/uuid"
)

// Tier describes the subscription tier of a workspace.
const (
	TierFree = "free"
	TierPro  = "pro"
)

// DefaultQuotaBytes is the free-tier storage quota (5 GB).
const DefaultQuotaBytes int64 = 5 * 1024 * 1024 * 1024

// Workspace is the tenant unit — every other resource belongs to a single
// workspace.
type Workspace struct {
	ID                uuid.UUID  `json:"id"`
	Name              string     `json:"name"`
	OwnerUserID       *uuid.UUID `json:"owner_user_id,omitempty"`
	StorageQuotaBytes int64      `json:"storage_quota_bytes"`
	StorageUsedBytes  int64      `json:"storage_used_bytes"`
	Tier              string     `json:"tier"`
	// MFARequired flips the login flow to require every user in
	// the workspace to have an enrolled 2FA factor. Admins toggle
	// it via the dedicated PATCH endpoint (audited via
	// audit.ActionMFAPolicyChange). Default false.
	MFARequired bool `json:"mfa_required"`
	// SearchLanguage picks the Postgres text-search dictionary the
	// FTS path uses for stemming. Defaults to 'simple' (no
	// stemming; relies on the trigram fallback for non-Latin
	// scripts). Admins switch via PUT
	// /api/admin/workspace/search-language, validated against the
	// allow-list in workspace.IsSupportedSearchLanguage.
	SearchLanguage string    `json:"search_language"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// DefaultSearchLanguage is the value migration 032 backfills onto
// every existing workspace and the default for newly-created rows
// when the caller does not pick a language. 'simple' is a
// dictionary-less Postgres path that tokenises on whitespace
// without any language-specific stemming — fine for the trigram
// fallback (which handles CJK + accented text) and never wrong for
// any language. Workspaces that want stemming opt in via the admin
// endpoint.
const DefaultSearchLanguage = "simple"

// supportedSearchLanguages enumerates the Postgres dictionaries
// the admin endpoint will accept. The 'simple' entry is the
// dictionary-less default; the rest are the Snowball stemmers that
// ship with a stock Postgres install (regconfig pg_catalog.*). Any
// addition here must also be safe to drop into to_tsvector(lang,
// ...) at search time without a CREATE TEXT SEARCH CONFIGURATION
// step — every value in this list is.
var supportedSearchLanguages = map[string]struct{}{
	"simple":     {},
	"english":    {},
	"french":     {},
	"german":     {},
	"spanish":    {},
	"italian":    {},
	"portuguese": {},
	"dutch":      {},
	"russian":    {},
	"swedish":    {},
	"norwegian":  {},
	"danish":     {},
	"finnish":    {},
	"hungarian":  {},
	"turkish":    {},
	"romanian":   {},
}

// IsSupportedSearchLanguage reports whether lang is a Postgres
// text-search dictionary the admin endpoint will accept. Exposed so
// the api/admin handler and the workspace.Service can share the
// allow-list without duplicating it.
func IsSupportedSearchLanguage(lang string) bool {
	_, ok := supportedSearchLanguages[lang]
	return ok
}

// SupportedSearchLanguages returns a sorted copy of the allow-list.
// Used by handlers to surface the supported set in error responses
// so frontend pickers don't have to ship a duplicate list. The
// slice is freshly allocated on every call and sorted
// alphabetically so the JSON response is byte-stable across calls
// — clients that diff or hash the response (caching, ETags) see
// only real changes, not Go's randomised map iteration order.
func SupportedSearchLanguages() []string {
	out := make([]string, 0, len(supportedSearchLanguages))
	for k := range supportedSearchLanguages {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

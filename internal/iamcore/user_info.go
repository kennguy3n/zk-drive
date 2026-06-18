package iamcore

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/kennguy3n/zk-drive/internal/user"
)

// Identity is the iam-core OIDC view of an authenticated principal,
// distilled from the access token's claims into the fields zk-drive
// needs. It is the iam-core analogue of the userinfo tuple the
// Google/Microsoft SSO path extracts (see api/auth/oauth.go).
type Identity struct {
	// Subject is the iam-core `sub` claim — the stable, immutable
	// user identifier. zk-drive stores it as auth_provider_id so a
	// user resolves to the same local row even if their email
	// changes upstream.
	Subject string
	// Email and Name populate the local user row on first login.
	Email string
	Name  string
	// OrgID and TenantID identify the iam-core tenant the user
	// belongs to. zk-drive maps the (TenantID, OrgID) pair to a local
	// workspace (see tenant_mapper.go).
	OrgID    string
	TenantID string
	// Roles are the raw iam-core role strings. MappedRole collapses
	// them to a zk-drive role (admin/member).
	Roles []string
	// IssuedAt and ExpiresAt are the token's `iat` / `exp` claims.
	// They are surfaced so consumers that enforce the token lifetime
	// beyond the initial request — notably the collab WebSocket reauth
	// pump — see the same expiry the access token actually carries and
	// tear a federated realtime connection down when it lapses, exactly
	// as a built-in session token's socket is. ExpiresAt is always set
	// (the verifier requires `exp`); IssuedAt is zero when `iat` is
	// absent.
	IssuedAt  time.Time
	ExpiresAt time.Time
}

// MappedRole returns the zk-drive role (user.RoleAdmin or
// user.RoleMember) for the identity. iam-core is the authority on
// authorization, so the presence of an admin-equivalent role in the
// token grants admin in zk-drive; everything else is a member. The
// match is case-insensitive and accepts the common spellings an
// identity provider emits ("admin", "owner", and namespaced variants
// such as "zk-drive:admin").
func (id Identity) MappedRole() string {
	for _, r := range id.Roles {
		switch normalizeRole(r) {
		case "admin", "owner", "administrator":
			return user.RoleAdmin
		}
	}
	return user.RoleMember
}

func normalizeRole(r string) string {
	r = strings.ToLower(strings.TrimSpace(r))
	// Roles are frequently namespaced by the relying party, e.g.
	// "zk-drive:admin" or "drive/admin". Compare on the trailing
	// segment so the mapping is robust to the provider's prefixing
	// convention.
	if i := strings.LastIndexAny(r, ":/"); i >= 0 {
		r = r[i+1:]
	}
	return r
}

// registeredClaims is the JSON shape zk-drive parses out of an
// iam-core access token. The standard registered claims (iss, aud,
// exp, nbf, sub, iat) are validated by the verifier; the custom
// claims below carry the tenant and authorization context.
//
// Roles is decoded through a custom type because identity providers
// are inconsistent about its representation: some emit a JSON array
// (["admin","member"]), others a single space-delimited string
// ("admin member"). claimStrings normalizes both to []string.
type registeredClaims struct {
	Email    string       `json:"email"`
	Name     string       `json:"name"`
	OrgID    string       `json:"org_id"`
	TenantID string       `json:"tenant_id"`
	Roles    claimStrings `json:"roles"`
	jwt.RegisteredClaims
}

// toIdentity projects the parsed claims into the Identity the rest of
// the package consumes. Email is lower-cased and trimmed to match the
// normalization the password/SSO paths apply (api/auth/oauth.go), so
// a downstream email lookup is case-insensitive.
func (c registeredClaims) toIdentity() Identity {
	subject := ""
	if c.Subject != "" {
		subject = c.Subject
	}
	id := Identity{
		Subject:  subject,
		Email:    strings.TrimSpace(strings.ToLower(c.Email)),
		Name:     strings.TrimSpace(c.Name),
		OrgID:    strings.TrimSpace(c.OrgID),
		TenantID: strings.TrimSpace(c.TenantID),
		Roles:    c.Roles,
	}
	if c.IssuedAt != nil {
		id.IssuedAt = c.IssuedAt.Time
	}
	if c.ExpiresAt != nil {
		id.ExpiresAt = c.ExpiresAt.Time
	}
	return id
}

// claimStrings decodes an OIDC claim that may be encoded either as a
// JSON array of strings or as a single space-delimited string. This
// mirrors how jwt.ClaimStrings tolerates the `aud` claim's dual
// representation, applied here to the non-standard `roles` claim.
type claimStrings []string

func (s *claimStrings) UnmarshalJSON(data []byte) error {
	if len(data) == 0 || string(data) == "null" {
		*s = nil
		return nil
	}
	// Array form: ["admin","member"].
	if data[0] == '[' {
		var arr []string
		if err := json.Unmarshal(data, &arr); err != nil {
			return fmt.Errorf("iamcore: decode roles array: %w", err)
		}
		*s = arr
		return nil
	}
	// Scalar string form: "admin member" (space-delimited per the
	// OAuth2 scope/role convention).
	var single string
	if err := json.Unmarshal(data, &single); err != nil {
		return fmt.Errorf("iamcore: decode roles string: %w", err)
	}
	*s = claimStrings(strings.Fields(single))
	return nil
}

package fabric

import (
	"fmt"
	"strings"
)

// Policy is the local mirror of zk-object-fabric's
// metadata/placement_policy/policy.go Policy type. We carry our own
// copy to avoid a cross-repo Go-module dependency; the JSON tags are
// kept identical so the same payload round-trips through both
// services.
type Policy struct {
	Tenant string     `json:"tenant"`
	Bucket string     `json:"bucket,omitempty"`
	Spec   PolicySpec `json:"policy"`
}

// PolicySpec mirrors placement_policy.PolicySpec.
type PolicySpec struct {
	Encryption EncryptionSpec `json:"encryption"`
	Placement  PlacementSpec  `json:"placement"`
}

// EncryptionSpec mirrors placement_policy.EncryptionSpec.
type EncryptionSpec struct {
	Mode string `json:"mode"`
	KMS  string `json:"kms,omitempty"`
}

// PlacementSpec mirrors placement_policy.PlacementSpec.
type PlacementSpec struct {
	Provider      []string `json:"provider"`
	Region        []string `json:"region,omitempty"`
	Country       []string `json:"country,omitempty"`
	StorageClass  []string `json:"storage_class,omitempty"`
	CacheLocation string   `json:"cache_location,omitempty"`
}

// Validate performs structural checks. Mirrors
// placement_policy.Policy.Validate() so callers fail fast before
// pushing a bogus policy to the fabric console.
func (p *Policy) Validate() error {
	if strings.TrimSpace(p.Tenant) == "" {
		return fmt.Errorf("placement: tenant is required")
	}
	switch p.Spec.Encryption.Mode {
	case "client_side", "managed", "public_distribution":
	case "":
		return fmt.Errorf("placement: encryption.mode is required")
	default:
		return fmt.Errorf("placement: unknown encryption.mode %q", p.Spec.Encryption.Mode)
	}
	if len(p.Spec.Placement.Provider) == 0 {
		return fmt.Errorf("placement: placement.provider must list at least one provider")
	}
	for i := range p.Spec.Placement.Country {
		p.Spec.Placement.Country[i] = strings.TrimSpace(p.Spec.Placement.Country[i])
		if len(p.Spec.Placement.Country[i]) != 2 {
			return fmt.Errorf("placement: country[%d]=%q is not an ISO-3166 alpha-2 code", i, p.Spec.Placement.Country[i])
		}
	}
	return nil
}

// FirstCountry returns the first country code in the placement spec
// or "" when none is set. Used by admin handlers to update
// workspace_storage_credentials.data_residency_country alongside the
// fabric-side write.
func (p *Policy) FirstCountry() string {
	if len(p.Spec.Placement.Country) == 0 {
		return ""
	}
	return p.Spec.Placement.Country[0]
}

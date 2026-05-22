package fabric

import (
	"strings"
	"testing"
)

// validPolicy returns a known-good Policy fixture so the negative
// tests can mutate one field at a time without redefining the whole
// structure.
func validPolicy() *Policy {
	return &Policy{
		Tenant: "ws_1234",
		Bucket: "zk-drive-ws_1234",
		Spec: PolicySpec{
			Encryption: EncryptionSpec{Mode: "managed"},
			Placement: PlacementSpec{
				Provider: []string{"wasabi"},
				Region:   []string{"us-east-1"},
				Country:  []string{"US"},
			},
		},
	}
}

// TestValidateAcceptsGoodPolicy is the happy path — the fixture used
// by every other negative case must itself be valid, otherwise the
// negative tests become tautologies.
func TestValidateAcceptsGoodPolicy(t *testing.T) {
	if err := validPolicy().Validate(); err != nil {
		t.Fatalf("validPolicy fixture rejected: %v", err)
	}
}

// TestValidateRejectsMissingTenant pins the very first guard. An
// empty tenant string would mean the policy can't be associated to a
// row in workspace_storage_credentials, so it must fail fast.
func TestValidateRejectsMissingTenant(t *testing.T) {
	for _, blank := range []string{"", " ", "\t\n"} {
		p := validPolicy()
		p.Tenant = blank
		err := p.Validate()
		if err == nil || !strings.Contains(err.Error(), "tenant") {
			t.Fatalf("expected tenant error for %q, got %v", blank, err)
		}
	}
}

// TestValidateEncryptionMode walks all three accepted modes plus the
// failure cases. The mode set is enforced by Validate (rather than
// just by the migration's CHECK constraint) so callers fail before
// hitting the fabric console.
func TestValidateEncryptionMode(t *testing.T) {
	tests := []struct {
		mode    string
		wantErr bool
		errSub  string
	}{
		{"client_side", false, ""},
		{"managed", false, ""},
		{"public_distribution", false, ""},
		{"", true, "encryption.mode is required"},
		{"raw", true, `unknown encryption.mode "raw"`},
		{"MANAGED", true, `unknown encryption.mode "MANAGED"`},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.mode, func(t *testing.T) {
			p := validPolicy()
			p.Spec.Encryption.Mode = tc.mode
			err := p.Validate()
			if (err != nil) != tc.wantErr {
				t.Fatalf("mode=%q err=%v wantErr=%v", tc.mode, err, tc.wantErr)
			}
			if tc.wantErr && !strings.Contains(err.Error(), tc.errSub) {
				t.Fatalf("error %q missing substring %q", err.Error(), tc.errSub)
			}
		})
	}
}

// TestValidateRequiresProvider locks the rule that at least one
// provider is selected — an empty provider list would let the
// fabric console choose anything, breaking residency guarantees.
func TestValidateRequiresProvider(t *testing.T) {
	p := validPolicy()
	p.Spec.Placement.Provider = nil
	err := p.Validate()
	if err == nil || !strings.Contains(err.Error(), "placement.provider") {
		t.Fatalf("expected provider error, got %v", err)
	}
}

// TestValidateCountryCodesAreISO31661Alpha2 confirms the loop
// normalises whitespace and rejects anything that isn't an ISO-3166
// alpha-2 code. This is the residency contract.
func TestValidateCountryCodesAreISO31661Alpha2(t *testing.T) {
	t.Run("trims whitespace and accepts canonical codes", func(t *testing.T) {
		p := validPolicy()
		p.Spec.Placement.Country = []string{"US", " GB ", "DE"}
		if err := p.Validate(); err != nil {
			t.Fatalf("expected canonical codes to validate, got %v", err)
		}
		// Validate must normalise in place so downstream callers
		// don't carry leading/trailing whitespace into the database.
		if p.Spec.Placement.Country[1] != "GB" {
			t.Fatalf("whitespace not trimmed: %q", p.Spec.Placement.Country[1])
		}
	})
	t.Run("rejects three-letter codes", func(t *testing.T) {
		p := validPolicy()
		p.Spec.Placement.Country = []string{"USA"}
		err := p.Validate()
		if err == nil || !strings.Contains(err.Error(), "alpha-2") {
			t.Fatalf("expected alpha-2 error, got %v", err)
		}
	})
	t.Run("rejects empty entry", func(t *testing.T) {
		p := validPolicy()
		p.Spec.Placement.Country = []string{""}
		err := p.Validate()
		if err == nil {
			t.Fatalf("expected error for empty country code")
		}
	})
}

// TestFirstCountryEmpty returns "" when no country is configured so
// admin handlers can clear data_residency_country idempotently.
func TestFirstCountryEmpty(t *testing.T) {
	p := validPolicy()
	p.Spec.Placement.Country = nil
	if got := p.FirstCountry(); got != "" {
		t.Fatalf("expected empty string, got %q", got)
	}
}

// TestFirstCountryReturnsFirst returns the first code when several
// are set. Order is significant because the database column carries
// exactly one country.
func TestFirstCountryReturnsFirst(t *testing.T) {
	p := validPolicy()
	p.Spec.Placement.Country = []string{"DE", "FR", "ES"}
	if got := p.FirstCountry(); got != "DE" {
		t.Fatalf("expected DE, got %q", got)
	}
}

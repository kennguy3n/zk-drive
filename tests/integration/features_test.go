package integration

import (
	"net/http"
	"testing"

	"github.com/kennguy3n/zk-drive/internal/billing"
	"github.com/kennguy3n/zk-drive/internal/feature"
)

// featuresResponse mirrors the GET /api/features payload.
type featuresResp struct {
	Tier     string          `json:"tier"`
	Features map[string]bool `json:"features"`
}

func getFeatures(t *testing.T, env *testEnv, token string) featuresResp {
	t.Helper()
	status, body := env.httpRequest(http.MethodGet, "/api/features", token, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /api/features: status=%d body=%s", status, string(body))
	}
	var fr featuresResp
	env.decodeJSON(body, &fr)
	return fr
}

// A brand-new workspace has no billing plan row, so it resolves to the
// Free tier and must only expose the baseline feature set.
func TestFeatures_NewWorkspaceFreeBaseline(t *testing.T) {
	env := setupEnv(t)
	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "pw")

	fr := getFeatures(t, env, tok.Token)
	if fr.Tier != billing.TierFree {
		t.Fatalf("tier = %q, want free", fr.Tier)
	}
	// Response must report the full, fixed schema.
	if len(fr.Features) != len(feature.AllFeatures) {
		t.Fatalf("features map has %d keys, want %d", len(fr.Features), len(feature.AllFeatures))
	}
	for _, f := range []string{
		feature.FeatureFolders, feature.FeatureFiles,
		feature.FeatureShareLinks, feature.FeatureBasicSearch,
	} {
		if !fr.Features[f] {
			t.Errorf("free baseline feature %q should be enabled", f)
		}
	}
	for _, f := range []string{
		feature.FeatureSSO, feature.FeatureWebhooks, feature.FeatureOnlyOffice,
		feature.FeatureStrictZK, feature.FeatureCMK, feature.FeatureAISummaries,
	} {
		if fr.Features[f] {
			t.Errorf("paid feature %q must be hidden on free tier", f)
		}
	}
}

// Upgrading the workspace's billing tier unlocks the corresponding
// feature set on the very next /api/features fetch.
func TestFeatures_TierUpgradeUnlocksFeatures(t *testing.T) {
	env := setupEnv(t)
	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "pw")

	// Move to Business via the admin billing endpoint.
	status, body := env.httpRequest(http.MethodPut, "/api/admin/billing/plan", tok.Token, map[string]any{
		"tier": billing.TierBusiness,
	})
	if status != http.StatusOK {
		t.Fatalf("update plan: status=%d body=%s", status, string(body))
	}

	fr := getFeatures(t, env, tok.Token)
	if fr.Tier != billing.TierBusiness {
		t.Fatalf("tier = %q, want business", fr.Tier)
	}
	for _, f := range []string{
		feature.FeatureSSO, feature.FeatureAuditLog, feature.FeatureRetentionPolicies,
		feature.FeatureOnlyOffice, feature.FeatureClientRooms, feature.FeatureWebhooks,
		feature.FeatureKChat,
	} {
		if !fr.Features[f] {
			t.Errorf("business feature %q should be enabled after upgrade", f)
		}
	}
	// Secure-Business-only features stay hidden on Business.
	for _, f := range []string{
		feature.FeatureStrictZK, feature.FeatureCMK,
		feature.FeatureDataResidency, feature.FeatureAISummaries,
	} {
		if fr.Features[f] {
			t.Errorf("secure-business feature %q must stay hidden on business tier", f)
		}
	}

	// Secure Business unlocks the privacy/compliance controls.
	status, body = env.httpRequest(http.MethodPut, "/api/admin/billing/plan", tok.Token, map[string]any{
		"tier": billing.TierSecureBusiness,
	})
	if status != http.StatusOK {
		t.Fatalf("upgrade to secure_business: status=%d body=%s", status, string(body))
	}
	fr = getFeatures(t, env, tok.Token)
	for _, f := range []string{
		feature.FeatureStrictZK, feature.FeatureCMK,
		feature.FeatureDataResidency, feature.FeatureAISummaries,
	} {
		if !fr.Features[f] {
			t.Errorf("secure-business feature %q should be enabled after upgrade", f)
		}
	}
}

// The endpoint requires authentication.
func TestFeatures_RequiresAuth(t *testing.T) {
	env := setupEnv(t)
	status, _ := env.httpRequest(http.MethodGet, "/api/features", "", nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("unauthenticated /api/features should be 401, got %d", status)
	}
}

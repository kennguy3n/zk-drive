package platform

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"

	"github.com/kennguy3n/zk-drive/internal/billing"
)

// --- pure-logic unit tests (no database) -----------------------------

func TestThresholdCrossed(t *testing.T) {
	cases := []struct {
		op        string
		value     float64
		threshold float64
		want      bool
	}{
		{OperatorGTE, 90, 80, true},
		{OperatorGTE, 80, 80, true},
		{OperatorGTE, 79.9, 80, false},
		{OperatorLTE, 10, 20, true},
		{OperatorLTE, 20, 20, true},
		{OperatorLTE, 21, 20, false},
		{"unknown", 100, 0, false}, // fail safe: never fire on bad operator
		{"", 100, 0, false},
	}
	for _, c := range cases {
		if got := thresholdCrossed(c.op, c.value, c.threshold); got != c.want {
			t.Errorf("thresholdCrossed(%q, %v, %v) = %v, want %v", c.op, c.value, c.threshold, got, c.want)
		}
	}
}

func TestValidMetricAndOperator(t *testing.T) {
	for _, m := range []string{MetricStoragePercent, MetricUserCount, MetricBandwidthMonthlyGB} {
		if !validMetric(m) {
			t.Errorf("expected %q to be a valid metric", m)
		}
	}
	for _, m := range []string{"", "cpu", "Storage_Percent"} {
		if validMetric(m) {
			t.Errorf("expected %q to be invalid", m)
		}
	}
	if !validOperator(OperatorGTE) || !validOperator(OperatorLTE) {
		t.Errorf("gte/lte must be valid operators")
	}
	if validOperator("eq") || validOperator("") {
		t.Errorf("unexpected operator accepted")
	}
}

func TestStoragePercent(t *testing.T) {
	cases := []struct {
		used, quota int64
		want        float64
	}{
		{0, 0, 0},     // unlimited / unset quota -> 0
		{100, 0, 0},   // divide-by-zero guard
		{50, 100, 50}, // half full
		{100, 100, 100},
		{150, 100, 150}, // over quota reports >100
	}
	for _, c := range cases {
		if got := storagePercent(c.used, c.quota); got != c.want {
			t.Errorf("storagePercent(%d, %d) = %v, want %v", c.used, c.quota, got, c.want)
		}
	}
}

func TestOwnerNameFromEmail(t *testing.T) {
	cases := map[string]string{
		"alice@example.com": "alice",
		"bob.smith@corp.io": "bob.smith",
		"@nolocal.com":      "Owner", // empty local part falls back
		"noatsign":          "noatsign",
		"":                  "Owner",
	}
	for in, want := range cases {
		if got := ownerNameFromEmail(in); got != want {
			t.Errorf("ownerNameFromEmail(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNullIfEmpty(t *testing.T) {
	if nullIfEmpty("") != nil {
		t.Errorf("empty string must map to nil")
	}
	if got := nullIfEmpty("x"); got == nil || *got != "x" {
		t.Errorf("non-empty string must map to its pointer")
	}
}

// --- API key generation / verification ------------------------------

func TestGenerateAndHashAPIKey(t *testing.T) {
	key, lookup, err := generateAPIKey()
	if err != nil {
		t.Fatalf("generateAPIKey: %v", err)
	}
	if !strings.HasPrefix(key, APIKeyPrefix) {
		t.Fatalf("key %q missing prefix %q", key, APIKeyPrefix)
	}
	// The lookup id is the fixed-width selector embedded right after
	// the prefix and must be recoverable from the plaintext alone.
	gotLookup, ok := parseAPIKeyLookup(key)
	if !ok || gotLookup != lookup {
		t.Fatalf("parseAPIKeyLookup(%q) = %q, %v; want %q, true", key, gotLookup, ok, lookup)
	}
	// Two generations must differ in both the full key and the lookup.
	key2, lookup2, _ := generateAPIKey()
	if key == key2 {
		t.Fatalf("expected distinct keys, got identical")
	}
	if lookup == lookup2 {
		t.Fatalf("expected distinct lookup ids, got identical")
	}

	hash, err := hashAPIKey(key)
	if err != nil {
		t.Fatalf("hashAPIKey: %v", err)
	}
	// bcrypt verifies the original and rejects a different key.
	if err := bcrypt.CompareHashAndPassword(hash, []byte(key)); err != nil {
		t.Errorf("expected hash to verify original key: %v", err)
	}
	if err := bcrypt.CompareHashAndPassword(hash, []byte(key2)); err == nil {
		t.Errorf("expected hash to reject a different key")
	}
}

func TestAPIKeyHasPermission(t *testing.T) {
	k := &APIKey{Permissions: []string{PermTenantRead, PermTenantSuspend}}
	if !k.HasPermission(PermTenantRead) || !k.HasPermission(PermTenantSuspend) {
		t.Errorf("expected granted permissions to be reported")
	}
	if k.HasPermission(PermTenantWrite) {
		t.Errorf("permissions must be explicit, not implied")
	}
	var nilKey *APIKey
	if nilKey.HasPermission(PermTenantRead) {
		t.Errorf("nil key must never grant a permission")
	}
}

// --- reconcileOne (no DB; uses a fake inspector) ---------------------

type fakeInspector struct {
	status string
	tier   string
	err    error
}

func (f fakeInspector) SubscriptionStatus(_ context.Context, _ string) (string, string, error) {
	return f.status, f.tier, f.err
}

func strptr(s string) *string { return &s }

func TestReconcileOneWithoutInspector(t *testing.T) {
	s := NewService(nil, nil, nil, nil)
	id := uuid.New()

	// Free tier without a customer is consistent.
	if _, mismatch := s.reconcileOne(context.Background(), id, billing.TierFree, nil); mismatch {
		t.Errorf("free tier without customer should not be a mismatch")
	}
	// Paid tier without a customer is a mismatch.
	if _, mismatch := s.reconcileOne(context.Background(), id, billing.TierBusiness, nil); !mismatch {
		t.Errorf("paid tier without customer should be a mismatch")
	}
	// Any linked customer is unverifiable without an inspector.
	if entry, mismatch := s.reconcileOne(context.Background(), id, billing.TierFree, strptr("cus_1")); !mismatch || !strings.Contains(entry.Reason, "unverified") {
		t.Errorf("linked customer without inspector should be flagged unverified, got mismatch=%v reason=%q", mismatch, entry.Reason)
	}
}

func TestReconcileOneWithInspector(t *testing.T) {
	id := uuid.New()

	// Matching tier and active subscription -> no mismatch.
	s := NewService(nil, nil, nil, nil).WithSubscriptionInspector(fakeInspector{status: "active", tier: billing.TierBusiness})
	if entry, mismatch := s.reconcileOne(context.Background(), id, billing.TierBusiness, strptr("cus_ok")); mismatch {
		t.Errorf("matching tier should reconcile cleanly, got reason %q", entry.Reason)
	}

	// Tier drift -> mismatch.
	s = NewService(nil, nil, nil, nil).WithSubscriptionInspector(fakeInspector{status: "active", tier: billing.TierStarter})
	if _, mismatch := s.reconcileOne(context.Background(), id, billing.TierBusiness, strptr("cus_drift")); !mismatch {
		t.Errorf("tier drift should be a mismatch")
	}

	// No subscription found for a linked customer -> mismatch.
	s = NewService(nil, nil, nil, nil).WithSubscriptionInspector(fakeInspector{status: ""})
	if _, mismatch := s.reconcileOne(context.Background(), id, billing.TierBusiness, strptr("cus_none")); !mismatch {
		t.Errorf("missing subscription should be a mismatch")
	}

	// Inspector error -> mismatch with reason.
	s = NewService(nil, nil, nil, nil).WithSubscriptionInspector(fakeInspector{err: errors.New("boom")})
	if entry, mismatch := s.reconcileOne(context.Background(), id, billing.TierFree, strptr("cus_err")); !mismatch || !strings.Contains(entry.Reason, "boom") {
		t.Errorf("inspector error should surface as a mismatch, got mismatch=%v reason=%q", mismatch, entry.Reason)
	}
}

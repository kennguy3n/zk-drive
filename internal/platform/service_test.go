package platform

import (
	"context"
	"errors"
	"net/url"
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
	// The constant-expression length used by the compile-time bcrypt
	// ceiling guard must match the real encoder, and the key must stay
	// within bcrypt's 72-byte input window so no entropy is truncated.
	if len(key) != apiKeyPlaintextLen {
		t.Fatalf("key length %d != apiKeyPlaintextLen %d", len(key), apiKeyPlaintextLen)
	}
	if apiKeyPlaintextLen > bcryptMaxInputBytes {
		t.Fatalf("key plaintext %d exceeds bcrypt ceiling %d", apiKeyPlaintextLen, bcryptMaxInputBytes)
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

// --- dispatchFiring (no DB; uses a capturing dispatcher) ------------

// captureDispatcher records the AlertFiring snapshot handed to each
// channel so a test can assert what the dispatchers actually observed.
type captureDispatcher struct {
	webhook  AlertFiring
	email    AlertFiring
	whErr    error
	emErr    error
	gotWH    bool
	gotEmail bool
}

func (d *captureDispatcher) DispatchWebhook(_ context.Context, _ string, f AlertFiring) error {
	d.webhook, d.gotWH = f, true
	return d.whErr
}

func (d *captureDispatcher) DispatchEmail(_ context.Context, _ string, f AlertFiring) error {
	d.email, d.gotEmail = f, true
	return d.emErr
}

func TestDispatchFiringConsistentSnapshot(t *testing.T) {
	d := &captureDispatcher{}
	s := NewService(nil, nil, nil, nil).WithAlertDispatcher(d)
	rule := AlertRule{
		ID:         uuid.New(),
		WebhookURL: "https://example.com/hook",
		Email:      "ops@example.com",
	}
	firing := AlertFiring{RuleID: rule.ID, Metric: MetricStoragePercent, Value: 95}

	s.dispatchFiring(context.Background(), rule, &firing)

	if !d.gotWH || !d.gotEmail {
		t.Fatalf("expected both channels dispatched, got webhook=%v email=%v", d.gotWH, d.gotEmail)
	}
	// Both channels must see an identical, ordering-independent snapshot
	// with the cross-channel meta-flags zeroed (the email payload must
	// not observe WebhookFired=true just because the webhook ran first).
	if d.webhook.WebhookFired || d.webhook.EmailFired {
		t.Errorf("webhook payload leaked cross-channel flags: %+v", d.webhook)
	}
	if d.email.WebhookFired || d.email.EmailFired {
		t.Errorf("email payload leaked cross-channel flags: %+v", d.email)
	}
	// Alert content must still be carried on both snapshots.
	if d.webhook.Value != 95 || d.email.Value != 95 || d.webhook.RuleID != rule.ID {
		t.Errorf("dispatch payloads lost alert content: webhook=%+v email=%+v", d.webhook, d.email)
	}
	// The returned firing records the true per-channel results.
	if !firing.WebhookFired || !firing.EmailFired {
		t.Errorf("firing should record both channels fired, got %+v", firing)
	}
}

// fakeURLValidator stubs the SSRF validator: it rejects every URL when
// blockErr is set, otherwise parses it like the real validator.
type fakeURLValidator struct{ blockErr error }

func (f fakeURLValidator) Validate(_ context.Context, raw string) (*url.URL, error) {
	if f.blockErr != nil {
		return nil, f.blockErr
	}
	return url.Parse(raw)
}

func TestCreateAlertRuleRejectsBlockedWebhookURL(t *testing.T) {
	// A wired validator that blocks the URL must surface as a 400-class
	// ErrInvalidArgument before any DB work, so the nil pool is never
	// touched. This is the SSRF guard at rule-creation time.
	s := NewService(nil, nil, nil, nil).
		WithURLValidator(fakeURLValidator{blockErr: errors.New("blocked")})
	wsID := uuid.New()
	_, err := s.CreateAlertRule(context.Background(), AlertRule{
		WorkspaceID: &wsID,
		Metric:      MetricStoragePercent,
		Operator:    OperatorGTE,
		Threshold:   90,
		WebhookURL:  "http://169.254.169.254/latest/meta-data/",
	})
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("blocked webhook_url: want ErrInvalidArgument, got %v", err)
	}
}

func TestValidPermission(t *testing.T) {
	for _, p := range []string{
		PermTenantRead, PermTenantWrite, PermTenantSuspend,
		PermBillingReconcile, PermAlertsRead, PermAlertsWrite, PermKeysManage,
	} {
		if !validPermission(p) {
			t.Errorf("known permission %q rejected", p)
		}
	}
	if validPermission("tenant:readd") || validPermission("") {
		t.Errorf("unknown permission accepted")
	}
}

func TestCreateAPIKeyRejectsUnknownPermission(t *testing.T) {
	// An unknown permission string is rejected up front (before any DB
	// work) so an operator typo fails loudly instead of minting a key
	// that authenticates but is inert against every guard.
	store := NewAPIKeyStore(nil)
	_, _, err := store.Create(context.Background(), "ci-bot", []string{PermTenantRead, "tenant:readd"})
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("unknown permission: want ErrInvalidArgument, got %v", err)
	}
}

func TestDispatchFiringRecordsPerChannelFailure(t *testing.T) {
	d := &captureDispatcher{whErr: errors.New("webhook down")}
	s := NewService(nil, nil, nil, nil).WithAlertDispatcher(d)
	rule := AlertRule{ID: uuid.New(), WebhookURL: "https://x/y", Email: "ops@example.com"}
	firing := AlertFiring{RuleID: rule.ID, Value: 12}

	s.dispatchFiring(context.Background(), rule, &firing)

	// A failed webhook must not mark WebhookFired, but the email still
	// fires and gets a clean snapshot.
	if firing.WebhookFired {
		t.Errorf("failed webhook must not set WebhookFired")
	}
	if !firing.EmailFired {
		t.Errorf("email should still fire when only the webhook failed")
	}
	if d.email.WebhookFired {
		t.Errorf("email payload must not report the (failed) webhook as fired: %+v", d.email)
	}
}

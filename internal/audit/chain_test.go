package audit

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
)

func mustHasher(t *testing.T) *hasher {
	t.Helper()
	h, err := newHasher([]byte("test-audit-hmac-key-0123456789abcd"))
	if err != nil {
		t.Fatalf("newHasher: %v", err)
	}
	return h
}

// TestNewHasherRejectsEmptyKey: an empty HMAC key is a silent security
// downgrade, so construction must fail closed rather than produce a
// hasher that authenticates with no secret.
func TestNewHasherRejectsEmptyKey(t *testing.T) {
	if _, err := newHasher(nil); err == nil {
		t.Fatal("newHasher(nil) = nil error, want rejection")
	}
	if _, err := newHasher([]byte{}); err == nil {
		t.Fatal("newHasher(empty) = nil error, want rejection")
	}
}

// TestNewHasherCopiesKey: the hasher must not alias the caller's key
// slice, or a later mutation of config.AuditHMACKey would silently
// change every subsequent MAC.
func TestNewHasherCopiesKey(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	h, err := newHasher(key)
	if err != nil {
		t.Fatalf("newHasher: %v", err)
	}
	ws := uuid.New()
	before := h.genesis(ws)
	key[0] ^= 0xff
	after := h.genesis(ws)
	if !bytes.Equal(before, after) {
		t.Fatal("mutating caller key changed hasher output: key not copied")
	}
}

func sampleEntry() *Entry {
	actor := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	resID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	rt := "folder"
	ip := "203.0.113.7"
	ua := "Mozilla/5.0"
	return &Entry{
		ID:           uuid.MustParse("33333333-3333-3333-3333-333333333333"),
		WorkspaceID:  uuid.MustParse("44444444-4444-4444-4444-444444444444"),
		ActorID:      &actor,
		Action:       ActionPermissionGrant,
		ResourceType: &rt,
		ResourceID:   &resID,
		IPAddress:    &ip,
		UserAgent:    &ua,
		Metadata:     json.RawMessage(`{"role":"editor","scope":"folder"}`),
		CreatedAt:    time.Date(2025, 6, 1, 12, 0, 0, 123456000, time.UTC),
	}
}

// TestComputeDeterministic: the same entry/seq/prev must always MAC to
// the same value — the chain is meaningless if recomputation drifts.
func TestComputeDeterministic(t *testing.T) {
	h := mustHasher(t)
	e := sampleEntry()
	prev := h.genesis(e.WorkspaceID)
	a, err := h.compute(e, 1, prev)
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	b, err := h.compute(e, 1, prev)
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if !bytes.Equal(a, b) {
		t.Fatal("compute not deterministic")
	}
	if len(a) != 32 {
		t.Fatalf("hash len = %d, want 32 (HMAC-SHA256)", len(a))
	}
}

// TestComputeSensitiveToEveryField: mutating any hashed field (or the
// seq / prev_hash) must change the MAC, or that field could be tampered
// undetected.
func TestComputeSensitiveToEveryField(t *testing.T) {
	h := mustHasher(t)
	base := sampleEntry()
	prev := h.genesis(base.WorkspaceID)
	want, err := h.compute(base, 1, prev)
	if err != nil {
		t.Fatalf("compute base: %v", err)
	}

	otherActor := uuid.MustParse("99999999-9999-9999-9999-999999999999")
	otherRes := uuid.MustParse("88888888-8888-8888-8888-888888888888")
	otherRT := "file"
	otherIP := "198.51.100.9"
	otherUA := "curl/8"

	mutations := map[string]func(*Entry){
		"id":            func(e *Entry) { e.ID = uuid.MustParse("55555555-5555-5555-5555-555555555555") },
		"workspace_id":  func(e *Entry) { e.WorkspaceID = uuid.MustParse("66666666-6666-6666-6666-666666666666") },
		"actor_id":      func(e *Entry) { e.ActorID = &otherActor },
		"action":        func(e *Entry) { e.Action = ActionPermissionRevoke },
		"resource_type": func(e *Entry) { e.ResourceType = &otherRT },
		"resource_id":   func(e *Entry) { e.ResourceID = &otherRes },
		"ip_address":    func(e *Entry) { e.IPAddress = &otherIP },
		"user_agent":    func(e *Entry) { e.UserAgent = &otherUA },
		"metadata":      func(e *Entry) { e.Metadata = json.RawMessage(`{"role":"viewer"}`) },
		"created_at":    func(e *Entry) { e.CreatedAt = e.CreatedAt.Add(time.Microsecond) },
	}
	for name, mutate := range mutations {
		e := sampleEntry()
		mutate(e)
		got, err := h.compute(e, 1, prev)
		if err != nil {
			t.Fatalf("compute %s: %v", name, err)
		}
		if bytes.Equal(got, want) {
			t.Errorf("mutating %s did not change the hash", name)
		}
	}

	// seq and prev_hash are not Entry fields but are part of the MAC.
	if got, _ := h.compute(base, 2, prev); bytes.Equal(got, want) {
		t.Error("changing seq did not change the hash")
	}
	if got, _ := h.compute(base, 1, append([]byte{0}, prev...)); bytes.Equal(got, want) {
		t.Error("changing prev_hash did not change the hash")
	}
}

// TestComputeDistinguishesNilFromEmpty: a nil optional pointer must hash
// differently from a pointer to "", so an attacker cannot swap an
// absent field for a present-but-empty one (or vice versa) undetected.
func TestComputeDistinguishesNilFromEmpty(t *testing.T) {
	h := mustHasher(t)
	prev := h.genesis(uuid.Nil)

	empty := ""
	withNil := sampleEntry()
	withNil.UserAgent = nil
	withEmpty := sampleEntry()
	withEmpty.UserAgent = &empty

	a, _ := h.compute(withNil, 1, prev)
	b, _ := h.compute(withEmpty, 1, prev)
	if bytes.Equal(a, b) {
		t.Fatal("nil and empty-string user_agent hash identically")
	}
}

// TestCanonicalJSONStableAcrossKeyOrder is the crux of surviving a
// Postgres JSONB round-trip: object key order and whitespace must not
// affect the canonical bytes, so the hash computed at insert time
// matches the hash recomputed from the reordered value read back.
func TestCanonicalJSONStableAcrossKeyOrder(t *testing.T) {
	a, err := canonicalJSON(json.RawMessage(`{"b":1,"a":2,"c":{"y":1,"x":2}}`))
	if err != nil {
		t.Fatalf("canonicalJSON a: %v", err)
	}
	b, err := canonicalJSON(json.RawMessage(`{  "c":{"x":2,"y":1}, "a":2,  "b":1 }`))
	if err != nil {
		t.Fatalf("canonicalJSON b: %v", err)
	}
	if !bytes.Equal(a, b) {
		t.Fatalf("canonical forms differ:\n a=%s\n b=%s", a, b)
	}
}

// TestCanonicalJSONPreservesLargeIntegers: routing numbers through
// float64 would corrupt large/precise integers in metadata. UseNumber
// must keep them verbatim.
func TestCanonicalJSONPreservesLargeIntegers(t *testing.T) {
	const big = `{"n":10000000000000001}`
	out, err := canonicalJSON(json.RawMessage(big))
	if err != nil {
		t.Fatalf("canonicalJSON: %v", err)
	}
	if !bytes.Contains(out, []byte("10000000000000001")) {
		t.Fatalf("large integer reformatted: %s", out)
	}
}

// TestCanonicalJSONEmptyIsNil: absent metadata canonicalises to nil
// (distinct from the JSON literal null), so an entry with no metadata
// and one with `null` are encoded consistently with how the column is
// stored.
func TestCanonicalJSONEmptyIsNil(t *testing.T) {
	for _, in := range []json.RawMessage{nil, {}, json.RawMessage("   ")} {
		out, err := canonicalJSON(in)
		if err != nil {
			t.Fatalf("canonicalJSON(%q): %v", in, err)
		}
		if out != nil {
			t.Errorf("canonicalJSON(%q) = %q, want nil", in, out)
		}
	}
}

// TestGenesisBoundToWorkspace: two workspaces must not share a genesis
// hash, or a row could be replayed from one tenant's log into another.
func TestGenesisBoundToWorkspace(t *testing.T) {
	h := mustHasher(t)
	a := h.genesis(uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"))
	b := h.genesis(uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"))
	if bytes.Equal(a, b) {
		t.Fatal("genesis hash not bound to workspace id")
	}
}

// TestGenesisBoundToKey: a different key must yield a different genesis,
// so the chain cannot be forged without the operator-held key.
func TestGenesisBoundToKey(t *testing.T) {
	h1, _ := newHasher([]byte("key-one-0123456789abcdef01234567"))
	h2, _ := newHasher([]byte("key-two-0123456789abcdef01234567"))
	ws := uuid.New()
	if bytes.Equal(h1.genesis(ws), h2.genesis(ws)) {
		t.Fatal("genesis hash not bound to key")
	}
}

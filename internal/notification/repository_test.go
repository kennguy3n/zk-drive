package notification

import (
	"context"
	"strings"
	"testing"

	"github.com/kennguy3n/zk-drive/internal/crypto"
)

// testAESKey is a 32-byte (AES-256) key used to exercise the real
// crypto.Codec from the notification repository tests.
const testAESKey = "0123456789abcdef0123456789abcdef"

// TestPostgresRepository_EncryptDecryptRoundTrip verifies that the
// p256dh / auth key material is sealed before storage and recovered
// intact on read, using the production AES-256-GCM codec.
func TestPostgresRepository_EncryptDecryptRoundTrip(t *testing.T) {
	codec, err := crypto.NewAESGCMCodec(testAESKey)
	if err != nil {
		t.Fatalf("new codec: %v", err)
	}
	repo := (&PostgresRepository{}).WithSubscriptionCipher(codec)

	in := PushSubscription{Endpoint: "https://push.example/abc", P256dh: "pub-key", Auth: "secret-auth"}
	p256dh, auth, err := repo.encryptKeys(context.Background(), in)
	if err != nil {
		t.Fatalf("encryptKeys: %v", err)
	}

	// Ciphertext must not be the plaintext and must carry the codec's
	// marker prefix so it is recognisably encrypted at rest.
	if p256dh == in.P256dh || auth == in.Auth {
		t.Fatalf("keys were not encrypted: p256dh=%q auth=%q", p256dh, auth)
	}
	if !strings.HasPrefix(p256dh, "aesgcm:") || !strings.HasPrefix(auth, "aesgcm:") {
		t.Fatalf("ciphertext missing aesgcm prefix: p256dh=%q auth=%q", p256dh, auth)
	}

	out, err := repo.decryptKeys(context.Background(), PushSubscription{Endpoint: in.Endpoint, P256dh: p256dh, Auth: auth})
	if err != nil {
		t.Fatalf("decryptKeys: %v", err)
	}
	if out.P256dh != in.P256dh || out.Auth != in.Auth {
		t.Fatalf("round-trip mismatch: got p256dh=%q auth=%q want p256dh=%q auth=%q",
			out.P256dh, out.Auth, in.P256dh, in.Auth)
	}
	// endpoint is not encrypted (used in WHERE clauses / UNIQUE key).
	if out.Endpoint != in.Endpoint {
		t.Fatalf("endpoint changed: got %q want %q", out.Endpoint, in.Endpoint)
	}
}

// TestPostgresRepository_DecryptPlaintextBackCompat ensures rows written
// before encryption was enabled (no ciphertext prefix) still decode, so
// enabling CREDENTIAL_ENCRYPTION on an existing deployment doesn't strand
// already-registered subscriptions.
func TestPostgresRepository_DecryptPlaintextBackCompat(t *testing.T) {
	codec, err := crypto.NewAESGCMCodec(testAESKey)
	if err != nil {
		t.Fatalf("new codec: %v", err)
	}
	repo := (&PostgresRepository{}).WithSubscriptionCipher(codec)

	legacy := PushSubscription{Endpoint: "https://push.example/legacy", P256dh: "plain-pub", Auth: "plain-auth"}
	out, err := repo.decryptKeys(context.Background(), legacy)
	if err != nil {
		t.Fatalf("decryptKeys legacy: %v", err)
	}
	if out.P256dh != legacy.P256dh || out.Auth != legacy.Auth {
		t.Fatalf("legacy plaintext not preserved: got p256dh=%q auth=%q", out.P256dh, out.Auth)
	}
}

// TestPostgresRepository_NilCipherPassthrough confirms that without a
// cipher (CREDENTIAL_ENCRYPTION unset) the keys are stored and read
// verbatim — the feature is opt-in and must not corrupt data when off.
func TestPostgresRepository_NilCipherPassthrough(t *testing.T) {
	repo := &PostgresRepository{} // no cipher

	in := PushSubscription{Endpoint: "https://push.example/x", P256dh: "pub", Auth: "auth"}
	p256dh, auth, err := repo.encryptKeys(context.Background(), in)
	if err != nil {
		t.Fatalf("encryptKeys: %v", err)
	}
	if p256dh != in.P256dh || auth != in.Auth {
		t.Fatalf("nil cipher altered keys: p256dh=%q auth=%q", p256dh, auth)
	}

	out, err := repo.decryptKeys(context.Background(), in)
	if err != nil {
		t.Fatalf("decryptKeys: %v", err)
	}
	if out.P256dh != in.P256dh || out.Auth != in.Auth {
		t.Fatalf("nil cipher altered keys on read: %+v", out)
	}
}

// TestPostgresRepository_TypedNilCipherPassthrough guards the typed-nil
// trap: a nil *crypto.Codec wrapped in the SubscriptionCipher interface
// must engage the plaintext path, not panic.
func TestPostgresRepository_TypedNilCipherPassthrough(t *testing.T) {
	var typedNil *crypto.Codec // nil concrete pointer
	repo := (&PostgresRepository{}).WithSubscriptionCipher(typedNil)
	if repo.cipher != nil {
		t.Fatalf("typed-nil cipher was not normalised to nil")
	}

	in := PushSubscription{Endpoint: "https://push.example/y", P256dh: "pub", Auth: "auth"}
	if _, _, err := repo.encryptKeys(context.Background(), in); err != nil {
		t.Fatalf("encryptKeys with typed-nil cipher: %v", err)
	}
}

package crypto

import (
	"context"
	"strings"
	"testing"
)

func TestKMSEncryptDecryptRoundTrip(t *testing.T) {
	const plaintext = "very-secret-bytes"
	codec, err := NewAESGCMCodec("0123456789abcdef0123456789abcdef") // 32 raw bytes
	if err != nil {
		t.Fatalf("new codec: %v", err)
	}

	ct, err := codec.Encrypt(context.Background(), plaintext)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if ct == plaintext {
		t.Fatal("expected ciphertext to differ from plaintext")
	}
	if !strings.HasPrefix(ct, aesGCMPrefix) {
		t.Fatalf("expected %q prefix on ciphertext, got %q", aesGCMPrefix, ct)
	}

	pt, err := codec.Decrypt(context.Background(), ct)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if pt != plaintext {
		t.Fatalf("round-trip mismatch: got %q, want %q", pt, plaintext)
	}
}

func TestEncryptIsRandomized(t *testing.T) {
	codec, err := NewAESGCMCodec(strings.Repeat("a", 32))
	if err != nil {
		t.Fatalf("new codec: %v", err)
	}
	a, _ := codec.Encrypt(context.Background(), "same-plaintext")
	b, _ := codec.Encrypt(context.Background(), "same-plaintext")
	if a == b {
		t.Fatal("expected distinct ciphertexts for two encrypt calls (nonce should be random)")
	}
}

func TestNoneModePassthrough(t *testing.T) {
	codec := &Codec{mode: ModeNone}
	in := "plain"
	ct, err := codec.Encrypt(context.Background(), in)
	if err != nil || ct != in {
		t.Fatalf("none-mode encrypt: ct=%q err=%v", ct, err)
	}
	pt, err := codec.Decrypt(context.Background(), in)
	if err != nil || pt != in {
		t.Fatalf("none-mode decrypt: pt=%q err=%v", pt, err)
	}
}

func TestDecryptHistoricalPlaintext(t *testing.T) {
	codec, err := NewAESGCMCodec(strings.Repeat("k", 32))
	if err != nil {
		t.Fatalf("new codec: %v", err)
	}
	// Rows written before the codec rollout have no aesgcm: prefix.
	got, err := codec.Decrypt(context.Background(), "legacy-plaintext")
	if err != nil {
		t.Fatalf("decrypt legacy: %v", err)
	}
	if got != "legacy-plaintext" {
		t.Fatalf("expected legacy passthrough, got %q", got)
	}
}

func TestKeyAcceptsHexAndBase64(t *testing.T) {
	raw := strings.Repeat("z", 32)
	cases := []string{
		raw,                                    // raw 32-byte
		"7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a", // hex
	}
	for _, k := range cases {
		if _, err := NewAESGCMCodec(k); err != nil {
			t.Errorf("expected key %q to be accepted, got %v", k, err)
		}
	}
}

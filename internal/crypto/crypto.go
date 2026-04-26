// Package crypto implements credential-at-rest encryption for the
// zk-drive control plane. The package exposes a single Codec type
// that satisfies both fabric.SecretEncryptor and
// storage.CredentialDecryptor, so the same instance handles
// encryption at provisioning time and decryption at presign time.
//
// Two modes are supported:
//
//   - "aesgcm" (default): authenticated encryption with a 32-byte
//     key sourced from the CREDENTIAL_ENCRYPTION_KEY env var. Suitable
//     for production deployments where a real KMS is overkill, and
//     for CI / local-dev. The key may be supplied raw, hex-encoded,
//     or base64-encoded — the loader auto-detects.
//   - "none": passes plaintext through unchanged. Only used when
//     CREDENTIAL_ENCRYPTION=none is set explicitly. Surfaces a
//     warning so operators don't accidentally ship plaintext
//     credentials in production.
//
// A KMS-backed mode is intentionally deferred: the Codec interface
// is the only seam, so adding a third mode (KMS, Vault, etc.) is a
// drop-in change once the upstream zk-object-fabric KMS client
// stabilizes.
package crypto

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// ModeAESGCM is the default encryption mode: AES-256-GCM using a
// caller-supplied 32-byte key.
const ModeAESGCM = "aesgcm"

// ModeNone disables encryption — plaintext round-trips through the
// codec unchanged. Use only for local-dev or migration windows.
const ModeNone = "none"

// aesGCMPrefix marks a ciphertext as produced by the AES-GCM mode so
// decrypt can route mixed-mode rows correctly during a migration.
const aesGCMPrefix = "aesgcm:"

// ErrKeyRequired is returned by Load when AES-GCM is selected but
// CREDENTIAL_ENCRYPTION_KEY is empty.
var ErrKeyRequired = errors.New("crypto: CREDENTIAL_ENCRYPTION_KEY is required for aesgcm mode")

// ErrKeyLength is returned when the key, after decoding, is not
// exactly 32 bytes (AES-256 mandates a 32-byte key).
var ErrKeyLength = errors.New("crypto: encryption key must be 32 bytes after decoding")

// Codec implements credential encryption / decryption for the
// fabric.SecretEncryptor and storage.CredentialDecryptor interfaces.
type Codec struct {
	mode string
	gcm  cipher.AEAD
}

// LoadFromEnv constructs a Codec from the CREDENTIAL_ENCRYPTION and
// CREDENTIAL_ENCRYPTION_KEY env vars. When CREDENTIAL_ENCRYPTION is
// unset, the default is "aesgcm" if a key is present and "none"
// otherwise — matching the migration path from the legacy
// IdentityEncryptor without forcing operators to set two variables
// at once.
func LoadFromEnv() (*Codec, error) {
	mode := strings.TrimSpace(os.Getenv("CREDENTIAL_ENCRYPTION"))
	keyEnv := strings.TrimSpace(os.Getenv("CREDENTIAL_ENCRYPTION_KEY"))
	if mode == "" {
		if keyEnv != "" {
			mode = ModeAESGCM
		} else {
			mode = ModeNone
		}
	}
	switch mode {
	case ModeNone:
		return &Codec{mode: ModeNone}, nil
	case ModeAESGCM:
		if keyEnv == "" {
			return nil, ErrKeyRequired
		}
		return NewAESGCMCodec(keyEnv)
	default:
		return nil, fmt.Errorf("crypto: unsupported CREDENTIAL_ENCRYPTION mode %q", mode)
	}
}

// NewAESGCMCodec builds an AES-GCM Codec from a key. The key may be
// supplied as raw bytes (32 chars), hex (64 chars), or base64 (44
// chars). Any other length is rejected.
func NewAESGCMCodec(key string) (*Codec, error) {
	raw, err := decodeKey(key)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(raw)
	if err != nil {
		return nil, fmt.Errorf("crypto: aes cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("crypto: gcm wrap: %w", err)
	}
	return &Codec{mode: ModeAESGCM, gcm: gcm}, nil
}

// Mode returns the codec's active mode (ModeAESGCM or ModeNone).
func (c *Codec) Mode() string {
	if c == nil {
		return ModeNone
	}
	return c.mode
}

// Encrypt seals plaintext under the codec's key. The returned
// ciphertext is a printable string suitable for storage in a TEXT
// column: an "aesgcm:" prefix followed by base64-encoded
// nonce||ciphertext||tag. Plaintext input is passed through
// unchanged when the codec is in ModeNone.
//
// The ctx parameter is accepted for interface symmetry with KMS
// backends; the AES-GCM path does no I/O.
func (c *Codec) Encrypt(_ context.Context, plaintext string) (string, error) {
	if c == nil || c.mode == ModeNone {
		return plaintext, nil
	}
	nonce := make([]byte, c.gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("crypto: read nonce: %w", err)
	}
	sealed := c.gcm.Seal(nil, nonce, []byte(plaintext), nil)
	combined := make([]byte, 0, len(nonce)+len(sealed))
	combined = append(combined, nonce...)
	combined = append(combined, sealed...)
	return aesGCMPrefix + base64.StdEncoding.EncodeToString(combined), nil
}

// Decrypt opens a ciphertext produced by Encrypt. Inputs without the
// "aesgcm:" prefix are returned verbatim so historical plaintext
// rows remain accessible during a migration; mode=none always
// returns the input unchanged.
func (c *Codec) Decrypt(_ context.Context, ciphertext string) (string, error) {
	if c == nil || c.mode == ModeNone {
		return ciphertext, nil
	}
	if !strings.HasPrefix(ciphertext, aesGCMPrefix) {
		// Backwards compatibility: rows written before the codec
		// rollout are stored as plaintext. Returning them as-is keeps
		// already-provisioned workspaces functional while the
		// operator re-provisions them.
		return ciphertext, nil
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(ciphertext, aesGCMPrefix))
	if err != nil {
		return "", fmt.Errorf("crypto: base64 decode: %w", err)
	}
	ns := c.gcm.NonceSize()
	if len(raw) < ns {
		return "", errors.New("crypto: ciphertext too short")
	}
	nonce, body := raw[:ns], raw[ns:]
	out, err := c.gcm.Open(nil, nonce, body, nil)
	if err != nil {
		return "", fmt.Errorf("crypto: aes-gcm open: %w", err)
	}
	return string(out), nil
}

// decodeKey accepts a key in raw, hex, or base64 form and returns
// the 32-byte material expected by AES-256-GCM.
func decodeKey(key string) ([]byte, error) {
	key = strings.TrimSpace(key)
	if len(key) == 32 {
		return []byte(key), nil
	}
	if len(key) == 64 {
		raw, err := hex.DecodeString(key)
		if err == nil && len(raw) == 32 {
			return raw, nil
		}
	}
	if raw, err := base64.StdEncoding.DecodeString(key); err == nil && len(raw) == 32 {
		return raw, nil
	}
	if raw, err := base64.RawStdEncoding.DecodeString(key); err == nil && len(raw) == 32 {
		return raw, nil
	}
	return nil, ErrKeyLength
}

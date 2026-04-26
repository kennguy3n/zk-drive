package crypto

import (
	"errors"
	"strings"

	"github.com/google/uuid"
)

// ModeKMS marks a workspace as using a customer-managed key for the
// data plane (gateway-side) encryption envelope. The Codec type
// itself only handles credential-at-rest encryption — CMK is a
// fabric-side concern, so the zk-drive control plane just persists
// the URI and forwards it to zk-object-fabric on the next tenant
// provisioning / placement update. ModeKMS is exposed here purely so
// callers (admin handler, fabric provisioner) refer to it through
// the same package that owns the other mode constants.
const ModeKMS = "kms"

// CMKConfig is the per-workspace customer-managed key reference. It
// mirrors the URI shape accepted by zk-object-fabric's
// EncryptionConfig (cmd/gateway/main.go selectGatewayWrapper) so the
// upstream gateway can dispatch to AWS KMS, generic kms://, Vault
// transit, etc. without further translation.
type CMKConfig struct {
	WorkspaceID uuid.UUID
	URI         string
}

// CMKURI scheme prefixes accepted by ValidateCMKURI. Matching the set
// supported by zk-object-fabric's gateway is intentional: the URI is
// passed through unchanged to the fabric tenant config, so any
// scheme rejected here would also be rejected upstream.
const (
	cmkSchemeAWSKMS  = "arn:aws:kms:"
	cmkSchemeKMS     = "kms://"
	cmkSchemeVault   = "vault://"
	cmkSchemeTransit = "transit://"
)

// ErrInvalidCMKURI is returned by ValidateCMKURI when the URI does
// not match a supported scheme. Empty input is treated as valid (it
// resets the workspace back to the gateway-default key).
var ErrInvalidCMKURI = errors.New("crypto: cmk_uri must be empty or use arn:aws:kms:, kms://, vault://, or transit://")

// ValidateCMKURI rejects URIs that the gateway would refuse. Empty
// input is allowed: an empty cmk_uri resets the workspace back to
// the gateway-default key, which is the intended way to roll back a
// CMK rotation.
func ValidateCMKURI(uri string) error {
	uri = strings.TrimSpace(uri)
	if uri == "" {
		return nil
	}
	switch {
	case strings.HasPrefix(uri, cmkSchemeAWSKMS):
		// arn:aws:kms:<region>:<account>:key/<id> — the prefix is
		// a sufficient gate at the control-plane layer; the
		// gateway re-validates the full ARN before use.
		return nil
	case strings.HasPrefix(uri, cmkSchemeKMS),
		strings.HasPrefix(uri, cmkSchemeVault),
		strings.HasPrefix(uri, cmkSchemeTransit):
		return nil
	}
	return ErrInvalidCMKURI
}

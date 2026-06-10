package audit

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// hasher computes the tamper-evident HMAC hash chain for audit_log
// (6.6). It is created from the operator-held key (config.AuditHMACKey,
// derived from an env secret and never persisted) so a DB admin who can
// write arbitrary rows still cannot forge a chain that verifies.
//
// The chain is per-workspace: row N's EntryHash is an HMAC over its
// sequence number, the previous row's EntryHash, and its immutable
// fields. The genesis (Seq==1) row's PrevHash is a key- and
// workspace-bound constant so two workspaces can never share a prefix
// and a row cannot be replayed from one tenant's log into another's.
type hasher struct {
	key []byte
}

// newHasher returns a hasher over a defensive copy of key. A zero-length
// key is rejected: an empty HMAC key is a silent security downgrade, and
// config always supplies a 32-byte derived key, so an empty key here is
// a programming error worth surfacing.
func newHasher(key []byte) (*hasher, error) {
	if len(key) == 0 {
		return nil, errors.New("audit: HMAC chain key must not be empty")
	}
	k := make([]byte, len(key))
	copy(k, key)
	return &hasher{key: k}, nil
}

// genesis returns the synthetic PrevHash for a workspace's first row.
// It is HMAC(key, "genesis" || workspaceID) so the chain start is bound
// to both the key and the tenant.
func (h *hasher) genesis(workspaceID uuid.UUID) []byte {
	mac := hmac.New(sha256.New, h.key)
	mac.Write([]byte("zk-drive/audit-chain/genesis/v1\x00"))
	mac.Write(workspaceID[:])
	return mac.Sum(nil)
}

// compute returns the EntryHash for entry at the given seq chained onto
// prevHash. The payload is an unambiguous, length-prefixed encoding of
// every immutable field, so no two distinct entries can collide on the
// same MAC input. Metadata is canonicalised (see canonicalJSON) so the
// hash is stable across Postgres' JSONB key reordering on round-trip.
func (h *hasher) compute(e *Entry, seq int64, prevHash []byte) ([]byte, error) {
	canonMeta, err := canonicalJSON(e.Metadata)
	if err != nil {
		return nil, fmt.Errorf("audit: canonicalise metadata for hashing: %w", err)
	}
	var buf bytes.Buffer
	putUint64(&buf, uint64(seq))
	putField(&buf, prevHash)
	putField(&buf, e.ID[:])
	putField(&buf, e.WorkspaceID[:])
	putOptUUID(&buf, e.ActorID)
	putField(&buf, []byte(e.Action))
	putOptString(&buf, e.ResourceType)
	putOptUUID(&buf, e.ResourceID)
	putOptString(&buf, e.IPAddress)
	putOptString(&buf, e.UserAgent)
	// created_at as UnixMicro: TIMESTAMPTZ has microsecond
	// resolution, so encoding micros makes the in-Go value and the
	// DB round-trip value hash identically (callers must truncate to
	// micros before insert — the repository does).
	putUint64(&buf, uint64(e.CreatedAt.UnixMicro()))
	putField(&buf, canonMeta)

	mac := hmac.New(sha256.New, h.key)
	mac.Write([]byte("zk-drive/audit-chain/entry/v1\x00"))
	mac.Write(buf.Bytes())
	return mac.Sum(nil), nil
}

// putUint64 appends a fixed-width big-endian uint64.
func putUint64(buf *bytes.Buffer, v uint64) {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], v)
	buf.Write(b[:])
}

// putField appends a length-prefixed byte slice so field boundaries
// are unambiguous (prevents canonical-encoding collisions where two
// different field splits produce the same concatenation).
func putField(buf *bytes.Buffer, p []byte) {
	putUint64(buf, uint64(len(p)))
	buf.Write(p)
}

// putOptString encodes an optional string with a leading presence byte
// so a nil pointer (absent) is distinct from a pointer to "" (present
// but empty).
func putOptString(buf *bytes.Buffer, s *string) {
	if s == nil {
		buf.WriteByte(0)
		return
	}
	buf.WriteByte(1)
	putField(buf, []byte(*s))
}

// putOptUUID mirrors putOptString for optional UUIDs.
func putOptUUID(buf *bytes.Buffer, u *uuid.UUID) {
	if u == nil {
		buf.WriteByte(0)
		return
	}
	buf.WriteByte(1)
	putField(buf, u[:])
}

// canonicalJSON returns a deterministic encoding of a JSON value:
// object keys sorted (encoding/json sorts map keys), insignificant
// whitespace removed, and numbers preserved verbatim via json.Number
// (so 1e10 / 10000000000 / large integers are not reformatted through
// float64). Equivalent JSON values always produce identical bytes
// regardless of the input's key order or spacing — which is exactly
// what lets the chain survive a Postgres JSONB round-trip, since the
// stored value reorders keys but represents the same value.
//
// Empty input (no metadata) canonicalises to empty, distinct from the
// JSON literal `null`.
func canonicalJSON(raw json.RawMessage) ([]byte, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return json.Marshal(v)
}

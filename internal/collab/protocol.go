// Package collab implements the per-document WebSocket relay that
// underpins P2's collaborative editor (zk-drive's TipTap + Yjs
// surface). The package is split into three concerns:
//
//   - protocol.go  — binary wire-format codec for the standard
//     y-protocols frame layout. Stateless; pure functions.
//   - hub.go       — the goroutine-safe DocumentHub that tracks
//     per-document rooms and fans out incoming frames to other
//     room members, gated by the folder's collab capability.
//   - fold.go      — FoldFunc implementations that satisfy
//     internal/document.FoldFunc. P2b ships OpaqueConcatFold;
//     a future drop-in YjsMergeFold will land when the WASM
//     bridge is wired (managed_encrypted folders).
//
// The package is HTTP-agnostic: it has no knowledge of
// chi / middleware / *http.Request. The drive package
// (api/drive/collab.go) owns the WebSocket upgrade and pumps
// bytes between the *websocket.Conn and a *DocumentClient that
// the hub manages. This split keeps the hub trivially unit-
// testable with synthetic clients (no real socket needed).
package collab

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// Top-level wire message types. The first byte of every frame
// distinguishes the family. Values mirror the y-protocols (Yjs)
// convention so the existing JS y-websocket client can talk to us
// unmodified.
const (
	// MessageSync carries Yjs update payloads (initial state
	// exchange + ongoing incremental updates). See the SyncStep*
	// sub-types below.
	MessageSync byte = 0x00

	// MessageAwareness carries presence frames (cursor positions,
	// selection ranges, the per-user awareness state). Routed to
	// every other client in the room only when the folder's
	// Capability.PresenceAllowed is true (managed_encrypted
	// folders); dropped server-side for strict_zk folders.
	MessageAwareness byte = 0x01

	// MessageAuth is reserved for an out-of-band re-auth path
	// (e.g. token refresh mid-session). Not implemented in P2b —
	// the WebSocket upgrade is authenticated by AuthMiddleware
	// and a session covers the lifetime of the connection. We
	// reserve the byte so a future client can negotiate without
	// colliding with sync/awareness traffic.
	MessageAuth byte = 0x02
)

// Sync sub-message types (the byte immediately after MessageSync).
// Matches the Yjs sync protocol (y-protocols/sync.js).
const (
	// SyncStepStateVector is the client→server handshake. The
	// payload is the client's encoded Y.encodeStateVector(doc)
	// describing what the client already has. The server responds
	// with SyncStepUpdates carrying the diff.
	SyncStepStateVector byte = 0x00

	// SyncStepUpdates is the server→client (or peer→peer) reply
	// containing the missing updates. The payload is the encoded
	// Y.encodeStateAsUpdate(doc, clientStateVector). In P2b's
	// "dumb relay" mode the server replies with the full snapshot
	// bundle (currentState followed by tail-delta payloads) rather
	// than a precise diff — we lack a server-side Yjs Y.Doc to
	// compute the diff against the client's state vector. This
	// over-sends but is correct: Yjs's update format is
	// idempotent under Y.applyUpdate, so applying a superset is
	// safe.
	SyncStepUpdates byte = 0x01

	// SyncUpdate is an incremental update frame (client↔server,
	// or relayed peer↔peer). Payload is a single encoded Yjs
	// update produced by Y.encodeUpdate on the originating
	// client. The server persists the payload via
	// documents.AppendDelta and broadcasts to other room members.
	SyncUpdate byte = 0x02
)

// Wire-level limits applied at the codec layer. The hub enforces
// these before queueing a frame for broadcast — a malicious or
// buggy client cannot push us past these bounds. Values are
// chosen to align with internal/document.MaxDeltaPayloadBytes
// (1 MiB) so a delta that fits through the HTTP path also fits
// through the WS path.
const (
	// MaxFrameBytes caps a single decoded frame. Sync payloads
	// route to AppendDelta which rejects > MaxDeltaPayloadBytes
	// (1 MiB); awareness payloads are cursor/selection state and
	// rarely exceed a few KiB. We pick 1 MiB + a small header
	// allowance so the framing overhead doesn't accidentally
	// reject a max-sized delta.
	MaxFrameBytes = 1<<20 + 1024 // 1 MiB + 1 KiB framing slack

	// minFrameBytes is the smallest meaningful frame: 1 byte for
	// MessageSync followed by a 1-byte sub-type. Awareness frames
	// are 1 byte type + payload. Anything shorter is malformed.
	minFrameBytes = 1
)

// Frame is a decoded inbound WebSocket message. The hub builds
// outbound frames by calling Encode* helpers below.
type Frame struct {
	// Type is one of MessageSync / MessageAwareness / MessageAuth.
	Type byte

	// SubType is meaningful when Type == MessageSync (one of the
	// SyncStep* constants). Zero for other types.
	SubType byte

	// Payload is the raw bytes following the type/sub-type header.
	// For SyncUpdate, this is the encoded Yjs update that will be
	// passed to documents.AppendDelta. The hub never decodes the
	// Yjs payload — that's the client's responsibility.
	Payload []byte
}

// DecodeFrame parses a raw inbound WebSocket message into a Frame.
// Returns an error for malformed framing (empty, oversized, or
// truncated sync header). The caller is expected to close the
// connection on a decode error — these errors indicate a buggy or
// adversarial client.
func DecodeFrame(raw []byte) (Frame, error) {
	if len(raw) < minFrameBytes {
		return Frame{}, errors.New("collab: frame too short")
	}
	if len(raw) > MaxFrameBytes {
		return Frame{}, fmt.Errorf("collab: frame too large: %d bytes (max %d)", len(raw), MaxFrameBytes)
	}
	msgType := raw[0]
	switch msgType {
	case MessageSync:
		if len(raw) < 2 {
			return Frame{}, errors.New("collab: sync frame missing sub-type")
		}
		return Frame{
			Type:    MessageSync,
			SubType: raw[1],
			Payload: raw[2:],
		}, nil
	case MessageAwareness, MessageAuth:
		return Frame{
			Type:    msgType,
			Payload: raw[1:],
		}, nil
	default:
		return Frame{}, fmt.Errorf("collab: unknown message type 0x%02x", msgType)
	}
}

// EncodeSyncStepUpdates packs a SyncStepUpdates reply. Used by the
// hub when a new client joins: the server emits one of these
// carrying the snapshot bundle (concatenated y_state || tail
// delta payloads with length-prefix framing) so the client can
// apply all known state in a single Y.applyUpdate sweep on the
// frontend.
func EncodeSyncStepUpdates(payload []byte) []byte {
	out := make([]byte, 2+len(payload))
	out[0] = MessageSync
	out[1] = SyncStepUpdates
	copy(out[2:], payload)
	return out
}

// EncodeSyncUpdate packs an incremental update frame for fan-out
// to room peers. The payload is the same opaque bytes that came
// in from the originating client.
func EncodeSyncUpdate(payload []byte) []byte {
	out := make([]byte, 2+len(payload))
	out[0] = MessageSync
	out[1] = SyncUpdate
	copy(out[2:], payload)
	return out
}

// EncodeAwareness packs an awareness frame for broadcast. Awareness
// payloads are opaque to the server (they're typically a y-protocols
// awareness-update encoded with clientID + state JSON), so we just
// prepend the type byte.
func EncodeAwareness(payload []byte) []byte {
	out := make([]byte, 1+len(payload))
	out[0] = MessageAwareness
	copy(out[1:], payload)
	return out
}

// LengthPrefix wraps `b` with a 4-byte big-endian length prefix.
// Used by the snapshot bundle assembler and by OpaqueConcatFold to
// produce a stream that can be split back into segments on the
// client side.
func LengthPrefix(b []byte) []byte {
	out := make([]byte, 4+len(b))
	binary.BigEndian.PutUint32(out[:4], uint32(len(b)))
	copy(out[4:], b)
	return out
}

// AssembleSnapshotBundle concatenates the document's stored y_state
// followed by each tail-delta payload, with every segment carrying a
// 4-byte length prefix. The client splits the bundle on the
// prefixes and feeds each segment through Y.applyUpdate.
//
// We send the bundle as ONE SyncStepUpdates frame rather than N
// separate SyncUpdate frames because:
//
//  1. The client's Y.applyUpdate is idempotent and order-tolerant,
//     so a single concatenated payload is correct.
//
//  2. A single frame reduces WebSocket framing overhead (one
//     header + one decoder pass) — significant for documents
//     with hundreds of tail deltas.
//
//  3. The wire shape matches what a future server-side merge fold
//     will produce: replacing the relay-mode bundle with a true
//     Y.mergeUpdates result is a transparent swap, the client
//     code doesn't change.
func AssembleSnapshotBundle(yState []byte, tailPayloads [][]byte) []byte {
	// Pre-compute the total length so we allocate exactly once.
	total := 4 + len(yState)
	for _, p := range tailPayloads {
		total += 4 + len(p)
	}
	out := make([]byte, 0, total)
	out = append(out, LengthPrefix(yState)...)
	for _, p := range tailPayloads {
		out = append(out, LengthPrefix(p)...)
	}
	return out
}

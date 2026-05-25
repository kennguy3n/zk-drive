package collab

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestDecodeFrame_TooShort(t *testing.T) {
	if _, err := DecodeFrame(nil); err == nil {
		t.Fatal("expected error for nil input")
	}
	if _, err := DecodeFrame([]byte{}); err == nil {
		t.Fatal("expected error for empty input")
	}
}

func TestDecodeFrame_TooLarge(t *testing.T) {
	big := make([]byte, MaxFrameBytes+1)
	if _, err := DecodeFrame(big); err == nil {
		t.Fatal("expected error for oversized frame")
	}
}

func TestDecodeFrame_SyncMissingSubType(t *testing.T) {
	if _, err := DecodeFrame([]byte{MessageSync}); err == nil {
		t.Fatal("expected error for sync frame missing sub-type byte")
	}
}

func TestDecodeFrame_SyncUpdate(t *testing.T) {
	payload := []byte{0xde, 0xad, 0xbe, 0xef}
	raw := append([]byte{MessageSync, SyncUpdate}, payload...)
	f, err := DecodeFrame(raw)
	if err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if f.Type != MessageSync || f.SubType != SyncUpdate {
		t.Fatalf("unexpected type/sub-type: %d/%d", f.Type, f.SubType)
	}
	if !bytes.Equal(f.Payload, payload) {
		t.Fatalf("payload mismatch: got %x want %x", f.Payload, payload)
	}
}

func TestDecodeFrame_Awareness(t *testing.T) {
	payload := []byte(`{"user":"alice"}`)
	raw := append([]byte{MessageAwareness}, payload...)
	f, err := DecodeFrame(raw)
	if err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if f.Type != MessageAwareness {
		t.Fatalf("wrong type: %d", f.Type)
	}
	if !bytes.Equal(f.Payload, payload) {
		t.Fatalf("payload mismatch")
	}
}

func TestDecodeFrame_Auth(t *testing.T) {
	// Auth is reserved but must decode cleanly so a future client
	// negotiation doesn't trip a parse error.
	raw := []byte{MessageAuth, 0x00}
	f, err := DecodeFrame(raw)
	if err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if f.Type != MessageAuth {
		t.Fatalf("wrong type")
	}
}

func TestDecodeFrame_UnknownType(t *testing.T) {
	raw := []byte{0xff, 0x00}
	if _, err := DecodeFrame(raw); err == nil {
		t.Fatal("expected error for unknown message type")
	}
}

func TestEncodeSyncStepUpdates_RoundTrip(t *testing.T) {
	payload := []byte("hello world")
	out := EncodeSyncStepUpdates(payload)
	f, err := DecodeFrame(out)
	if err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if f.Type != MessageSync || f.SubType != SyncStepUpdates {
		t.Fatalf("unexpected type/sub-type: %d/%d", f.Type, f.SubType)
	}
	if !bytes.Equal(f.Payload, payload) {
		t.Fatalf("payload mismatch")
	}
}

func TestEncodeSyncUpdate_RoundTrip(t *testing.T) {
	payload := []byte{0x01, 0x02, 0x03}
	out := EncodeSyncUpdate(payload)
	if !bytes.Equal(out, []byte{MessageSync, SyncUpdate, 0x01, 0x02, 0x03}) {
		t.Fatalf("wrong wire format: %x", out)
	}
}

func TestEncodeAwareness_RoundTrip(t *testing.T) {
	payload := []byte("presence")
	out := EncodeAwareness(payload)
	if out[0] != MessageAwareness {
		t.Fatalf("missing awareness type byte")
	}
	if !bytes.Equal(out[1:], payload) {
		t.Fatalf("payload mismatch")
	}
}

func TestLengthPrefix(t *testing.T) {
	in := []byte("test")
	out := LengthPrefix(in)
	if len(out) != 4+len(in) {
		t.Fatalf("wrong length: %d", len(out))
	}
	got := binary.BigEndian.Uint32(out[:4])
	if got != 4 {
		t.Fatalf("wrong prefix: %d", got)
	}
	if !bytes.Equal(out[4:], in) {
		t.Fatalf("payload mismatch")
	}
}

func TestAssembleSnapshotBundle_Empty(t *testing.T) {
	// A snapshot with no state and no tail should produce a
	// single zero-length prefix (the y_state empty segment).
	out := AssembleSnapshotBundle(nil, nil)
	if len(out) != 4 {
		t.Fatalf("expected 4-byte zero prefix, got %d bytes", len(out))
	}
	if binary.BigEndian.Uint32(out[:4]) != 0 {
		t.Fatalf("expected zero prefix")
	}
}

func TestAssembleSnapshotBundle_StateAndTail(t *testing.T) {
	state := []byte("STATE")
	tail := [][]byte{[]byte("D1"), []byte("DELTA-2")}
	out := AssembleSnapshotBundle(state, tail)

	// Parse back: 4-byte prefix + 5 bytes + 4-byte prefix + 2 bytes + 4-byte prefix + 7 bytes.
	expected := 4 + 5 + 4 + 2 + 4 + 7
	if len(out) != expected {
		t.Fatalf("wrong bundle size: got %d want %d", len(out), expected)
	}

	// Walk the segments.
	segments := splitLengthPrefixed(t, out)
	if len(segments) != 3 {
		t.Fatalf("expected 3 segments, got %d", len(segments))
	}
	if !bytes.Equal(segments[0], state) {
		t.Fatalf("state segment mismatch")
	}
	if !bytes.Equal(segments[1], tail[0]) || !bytes.Equal(segments[2], tail[1]) {
		t.Fatalf("tail segments mismatch")
	}
}

// splitLengthPrefixed walks a length-prefixed bundle and returns
// each segment. Test-only helper.
func splitLengthPrefixed(t *testing.T, buf []byte) [][]byte {
	t.Helper()
	out := make([][]byte, 0)
	for len(buf) > 0 {
		if len(buf) < 4 {
			t.Fatalf("truncated prefix at offset %d", len(buf))
		}
		n := binary.BigEndian.Uint32(buf[:4])
		buf = buf[4:]
		if uint32(len(buf)) < n {
			t.Fatalf("truncated segment: need %d have %d", n, len(buf))
		}
		out = append(out, buf[:n])
		buf = buf[n:]
	}
	return out
}

package scan

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// fakeConn implements net.Conn with a pre-canned read buffer and a
// recorded write buffer. The clamd INSTREAM protocol is half-duplex
// (full request → full response) so a simple buffer model is enough
// to drive the parser end-to-end.
type fakeConn struct {
	read       *bytes.Reader
	wrote      bytes.Buffer
	closed     bool
	readErr    error
	deadlineAt time.Time
}

func (c *fakeConn) Read(p []byte) (int, error) {
	if c.readErr != nil {
		return 0, c.readErr
	}
	return c.read.Read(p)
}
func (c *fakeConn) Write(p []byte) (int, error)      { return c.wrote.Write(p) }
func (c *fakeConn) Close() error                     { c.closed = true; return nil }
func (c *fakeConn) LocalAddr() net.Addr              { return &net.IPAddr{} }
func (c *fakeConn) RemoteAddr() net.Addr             { return &net.IPAddr{} }
func (c *fakeConn) SetDeadline(t time.Time) error    { c.deadlineAt = t; return nil }
func (c *fakeConn) SetReadDeadline(time.Time) error  { return nil }
func (c *fakeConn) SetWriteDeadline(time.Time) error { return nil }

// newServiceWithFakeDialer builds a Service whose dialer returns a
// fake net.Conn pre-loaded with the given clamd response. address
// is set to a non-empty sentinel so permissive mode does not fire.
func newServiceWithFakeDialer(response []byte) (*Service, *fakeConn) {
	conn := &fakeConn{read: bytes.NewReader(response)}
	s := &Service{
		address:    "test:3310",
		permissive: false,
		dialer:     func(_ context.Context, _ string) (net.Conn, error) { return conn, nil },
		now:        func() time.Time { return time.Unix(0, 0).UTC() },
	}
	return s, conn
}

// TestNewServicePermissiveWhenAddressEmpty pins the documented
// fallback: an unconfigured CLAMAV_ADDRESS must yield a Service that
// marks every version clean without calling out — this is what keeps
// local-dev / CI green without requiring clamav.
func TestNewServicePermissiveWhenAddressEmpty(t *testing.T) {
	for _, addr := range []string{"", "   ", "\t"} {
		s := NewService(nil, nil, addr)
		if !s.permissive {
			t.Fatalf("NewService(%q) expected permissive=true", addr)
		}
		if s.address != "" {
			t.Fatalf("NewService(%q) expected address normalised to empty, got %q", addr, s.address)
		}
	}
}

// TestNewServiceWiredWhenAddressSet inverts the previous test: a
// real address activates the scanner.
func TestNewServiceWiredWhenAddressSet(t *testing.T) {
	s := NewService(nil, nil, " host:3310 ")
	if s.permissive {
		t.Fatalf("expected permissive=false when address is set")
	}
	if s.address != "host:3310" {
		t.Fatalf("expected address trimmed, got %q", s.address)
	}
}

// TestScanBytesPermissiveSkipsClamd verifies the permissive branch
// short-circuits before any I/O. The service has no dialer set, so
// any attempt to call clamd would panic — passing means we never
// got there.
func TestScanBytesPermissiveSkipsClamd(t *testing.T) {
	s := &Service{permissive: true}
	v, err := s.scanBytes(context.Background(), []byte("anything"))
	if err != nil {
		t.Fatalf("permissive mode must never error, got %v", err)
	}
	if v.Status != StatusClean {
		t.Fatalf("expected StatusClean in permissive mode, got %q", v.Status)
	}
	if !strings.Contains(v.Detail, "permissive") {
		t.Fatalf("expected Detail to mention permissive, got %q", v.Detail)
	}
}

// TestInstreamFramesBodyCorrectly walks the wire-level INSTREAM
// protocol: the framing must be [length:u32][bytes...][0x00000000].
// A regression in writeChunk (e.g. forgetting the EOF sentinel)
// would let clamd hang waiting for more chunks; we pin the frame
// layout against that.
func TestInstreamFramesBodyCorrectly(t *testing.T) {
	// Pre-load "stream: OK\x00" so the parser returns cleanly.
	s, conn := newServiceWithFakeDialer([]byte("stream: OK\x00"))
	body := []byte("hello world")

	sig, err := s.instream(context.Background(), body)
	if err != nil {
		t.Fatalf("instream: %v", err)
	}
	if sig != "" {
		t.Fatalf("expected empty sig (clean), got %q", sig)
	}

	// Pull apart the wire payload sent to clamd: command + frame +
	// EOF sentinel. The frame must declare len(body) bytes and the
	// EOF must be exactly four zero bytes.
	wire := conn.wrote.Bytes()
	if !bytes.HasPrefix(wire, []byte("zINSTREAM\x00")) {
		t.Fatalf("expected zINSTREAM command prefix, got %q", wire[:min(len(wire), 16)])
	}
	rest := wire[len("zINSTREAM\x00"):]
	if len(rest) < 4 {
		t.Fatalf("frame too short: %d bytes", len(rest))
	}
	var frameLen uint32
	if err := binary.Read(bytes.NewReader(rest[:4]), binary.BigEndian, &frameLen); err != nil {
		t.Fatalf("decode frame length: %v", err)
	}
	if frameLen != uint32(len(body)) {
		t.Fatalf("frame length = %d, want %d", frameLen, len(body))
	}
	if !bytes.Equal(rest[4:4+frameLen], body) {
		t.Fatalf("frame body mismatch: got %q want %q", rest[4:4+frameLen], body)
	}
	// Last 4 bytes must be the EOF sentinel.
	tail := rest[len(rest)-4:]
	if !bytes.Equal(tail, []byte{0, 0, 0, 0}) {
		t.Fatalf("expected 4-byte EOF sentinel, got %x", tail)
	}
}

// TestInstreamParsesQuarantine confirms a "FOUND" response yields
// the signature name with the "stream: " prefix and " FOUND" suffix
// stripped. This is what scanBytes turns into StatusQuarantined.
func TestInstreamParsesQuarantine(t *testing.T) {
	s, _ := newServiceWithFakeDialer([]byte("stream: Eicar-Test-Signature FOUND\x00"))
	sig, err := s.instream(context.Background(), []byte("payload"))
	if err != nil {
		t.Fatalf("instream: %v", err)
	}
	if sig != "Eicar-Test-Signature" {
		t.Fatalf("expected Eicar-Test-Signature, got %q", sig)
	}
}

// TestInstreamRejectsUnexpectedResponse pins the failure mode for an
// unknown clamd line — must return a non-nil error so the worker
// Naks the job rather than persisting StatusClean on garbage.
func TestInstreamRejectsUnexpectedResponse(t *testing.T) {
	s, _ := newServiceWithFakeDialer([]byte("stream: WTF\x00"))
	_, err := s.instream(context.Background(), []byte("payload"))
	if err == nil {
		t.Fatalf("expected error on unknown clamd response, got nil")
	}
	if !strings.Contains(err.Error(), "unexpected response") {
		t.Fatalf("expected unexpected-response wrap, got %v", err)
	}
}

// TestInstreamPropagatesReadError makes sure transient connection
// failures (TCP RST after partial response, EOF before sentinel)
// surface as errors rather than as a phantom "clean" verdict.
func TestInstreamPropagatesReadError(t *testing.T) {
	conn := &fakeConn{
		read:    bytes.NewReader(nil),
		readErr: errors.New("simulated peer reset"),
	}
	s := &Service{
		address: "test:3310",
		dialer:  func(_ context.Context, _ string) (net.Conn, error) { return conn, nil },
	}
	_, err := s.instream(context.Background(), []byte("payload"))
	if err == nil {
		t.Fatalf("expected error when read fails, got nil")
	}
	if !errors.Is(err, conn.readErr) && !strings.Contains(err.Error(), "simulated peer reset") {
		t.Fatalf("expected underlying read error to surface, got %v", err)
	}
}

// TestScanBytesEmitsClamdErrorVerdict checks the wrapper around
// instream when clamd is unreachable: status stays Pending (so the
// worker retries) and Detail carries the error string for operator
// debugging.
func TestScanBytesEmitsClamdErrorVerdict(t *testing.T) {
	dialErr := errors.New("dial timeout")
	s := &Service{
		address: "test:3310",
		dialer:  func(_ context.Context, _ string) (net.Conn, error) { return nil, dialErr },
	}
	v, err := s.scanBytes(context.Background(), []byte("payload"))
	if err == nil {
		t.Fatalf("expected error to propagate from dial failure")
	}
	if v.Status != StatusPending {
		t.Fatalf("expected StatusPending on transient failure, got %q", v.Status)
	}
	if !strings.Contains(v.Detail, "clamd error") {
		t.Fatalf("expected Detail to mention clamd, got %q", v.Detail)
	}
}

// TestWriteChunkFramesLengthFirst is a unit-level pin on the framing
// helper. Independent of instream so a future refactor that inlines
// it (or extracts it further) still has a tight regression net.
func TestWriteChunkFramesLengthFirst(t *testing.T) {
	var buf bytes.Buffer
	if err := writeChunk(&buf, []byte("abc")); err != nil {
		t.Fatalf("writeChunk: %v", err)
	}
	got := buf.Bytes()
	if len(got) != 4+3 {
		t.Fatalf("expected 4-byte length + 3-byte payload, got %d bytes", len(got))
	}
	var size uint32
	if err := binary.Read(bytes.NewReader(got[:4]), binary.BigEndian, &size); err != nil {
		t.Fatalf("decode size: %v", err)
	}
	if size != 3 {
		t.Fatalf("size header = %d, want 3", size)
	}
	if !bytes.Equal(got[4:], []byte("abc")) {
		t.Fatalf("payload mismatch: %q", got[4:])
	}
}

// TestWriteChunkEmptyIsSentinelOnly verifies an empty body writes a
// 4-byte zero header — the INSTREAM EOF sentinel — and no payload.
func TestWriteChunkEmptyIsSentinelOnly(t *testing.T) {
	var buf bytes.Buffer
	if err := writeChunk(&buf, nil); err != nil {
		t.Fatalf("writeChunk: %v", err)
	}
	got := buf.Bytes()
	if !bytes.Equal(got, []byte{0, 0, 0, 0}) {
		t.Fatalf("expected 4-byte zero EOF, got %x", got)
	}
}

// TestStatusConstants pins the four scan_status string values
// against the migration 008 CHECK constraint. Drift here breaks
// every existing row's persisted status.
func TestStatusConstants(t *testing.T) {
	for _, tc := range []struct {
		got, want string
	}{
		{StatusPending, "pending"},
		{StatusScanning, "scanning"},
		{StatusClean, "clean"},
		{StatusQuarantined, "quarantined"},
	} {
		if tc.got != tc.want {
			t.Fatalf("status constant drift: got %q want %q", tc.got, tc.want)
		}
	}
}

// TestMaxScanBytes captures the documented 100 MiB cap. The downloader
// reads MaxScanBytes+1 to distinguish overflow from exactly-at-cap;
// drift in this constant changes that boundary and would silently
// pass through larger uploads.
func TestMaxScanBytes(t *testing.T) {
	if MaxScanBytes != 100*1024*1024 {
		t.Fatalf("MaxScanBytes drifted to %d, want %d", MaxScanBytes, 100*1024*1024)
	}
}

// TestDefaultAddress pins the documented fallback for production
// deployments that set CLAMAV_ADDRESS to a sentinel like "" without
// realising it needs to be the actual host:port.
func TestDefaultAddress(t *testing.T) {
	if DefaultAddress != "localhost:3310" {
		t.Fatalf("DefaultAddress drifted to %q, want %q", DefaultAddress, "localhost:3310")
	}
}

// Compile-time assertion that *bytes.Reader satisfies the io.Reader
// surface fakeConn delegates to — protects against a future stdlib
// refactor that splits Read out of the Reader interface.
var _ io.Reader = (*bytes.Reader)(nil)

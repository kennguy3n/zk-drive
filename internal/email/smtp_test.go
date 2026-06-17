package email

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSMTPServer implements just enough SMTP to capture a full
// envelope + message body so the tests can assert the wire format
// the SMTPClient emits. It runs on the in-memory side of a net.Pipe
// pair so no real TCP listener is needed (which keeps the test
// suite hermetic + portable to CI sandboxes that block sockets).
type fakeSMTPServer struct {
	t         *testing.T
	conn      net.Conn
	br        *bufio.Reader
	bw        *bufio.Writer
	mailFrom  string
	rcptTo    string
	dataBody  string
	supportSTARTTLS bool
	closed    bool
	// closeAfterFirstData makes the server hang up right after the
	// first completed DATA, simulating a relay that reaped what it
	// considered an idle socket while the client kept it pooled.
	closeAfterFirstData bool
}

func (s *fakeSMTPServer) writeLine(line string) {
	if _, err := s.bw.WriteString(line + "\r\n"); err != nil {
		s.t.Fatalf("server write: %v", err)
	}
	if err := s.bw.Flush(); err != nil {
		s.t.Fatalf("server flush: %v", err)
	}
}

// run is the minimal SMTP state machine used by the tests. It
// supports EHLO, MAIL FROM, RCPT TO, DATA, QUIT — enough to
// validate the client's wire format.
func (s *fakeSMTPServer) run() {
	defer func() {
		s.closed = true
		_ = s.conn.Close()
	}()
	s.writeLine("220 fake-smtp ready")
	for {
		line, err := s.br.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case strings.HasPrefix(strings.ToUpper(line), "EHLO"):
			s.writeLine("250-fake-smtp")
			if s.supportSTARTTLS {
				s.writeLine("250-STARTTLS")
			}
			s.writeLine("250 HELP")
		case strings.HasPrefix(strings.ToUpper(line), "HELO"):
			s.writeLine("250 fake-smtp")
		case strings.HasPrefix(strings.ToUpper(line), "MAIL FROM:"):
			s.mailFrom = line
			s.writeLine("250 ok")
		case strings.HasPrefix(strings.ToUpper(line), "RCPT TO:"):
			s.rcptTo = line
			s.writeLine("250 ok")
		case strings.ToUpper(line) == "DATA":
			s.writeLine("354 send data")
			var body strings.Builder
			for {
				dl, err := s.br.ReadString('\n')
				if err != nil {
					s.dataBody = body.String()
					s.writeLine("250 ok")
					return
				}
				if dl == ".\r\n" || strings.TrimRight(dl, "\r\n") == "." {
					break
				}
				body.WriteString(dl)
			}
			s.dataBody = body.String()
			s.writeLine("250 ok")
			if s.closeAfterFirstData {
				return
			}
		case strings.ToUpper(line) == "QUIT":
			s.writeLine("221 bye")
			return
		case strings.HasPrefix(strings.ToUpper(line), "RSET"):
			s.writeLine("250 ok")
		case strings.HasPrefix(strings.ToUpper(line), "NOOP"):
			s.writeLine("250 ok")
		default:
			s.writeLine("502 unrecognised")
		}
	}
}

// newPipeDialer pairs the SMTPClient's dialer with a goroutine
// running fakeSMTPServer. The returned dialer ignores the addr
// argument (since the server side is in-process) and substitutes
// the in-memory side of a net.Pipe.
func newPipeDialer(t *testing.T, server *fakeSMTPServer) func(ctx context.Context, network, addr string) (net.Conn, error) {
	t.Helper()
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		clientSide, serverSide := net.Pipe()
		server.conn = serverSide
		server.br = bufio.NewReader(serverSide)
		server.bw = bufio.NewWriter(serverSide)
		go server.run()
		return clientSide, nil
	}
}

func TestSMTPClient_SendEndToEnd(t *testing.T) {
	srv := &fakeSMTPServer{t: t}
	c, err := NewSMTPClient(SMTPConfig{
		Host:        "fake.local",
		Port:        2525,
		FromAddress: "noreply@drive.example.com",
		FromName:    "ZK Drive",
		TLSMode:     TLSModeNone,
		Timeout:     2 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewSMTPClient: %v", err)
	}
	c.dialer = newPipeDialer(t, srv)

	if !c.IsConfigured() {
		t.Fatalf("IsConfigured should be true when host+port set")
	}

	if err := c.Send(context.Background(), Message{
		To:            "bob@example.com",
		RecipientName: "Bob",
		Subject:       "Hello",
		TextBody:      "Hi Bob.",
		HTMLBody:      "<p>Hi Bob.</p>",
		Headers:       map[string]string{"Auto-Submitted": "auto-generated"},
		TemplateName:  "guest_invite",
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if !strings.Contains(srv.mailFrom, "noreply@drive.example.com") {
		t.Errorf("MAIL FROM not captured: %s", srv.mailFrom)
	}
	if !strings.Contains(srv.rcptTo, "bob@example.com") {
		t.Errorf("RCPT TO not captured: %s", srv.rcptTo)
	}
	body := srv.dataBody
	for _, needle := range []string{
		"From: \"ZK Drive\" <noreply@drive.example.com>",
		"To: \"Bob\" <bob@example.com>",
		"Subject: Hello",
		"Date: ",
		"Message-ID: <",
		"MIME-Version: 1.0",
		"Auto-Submitted: auto-generated",
		"Content-Type: multipart/alternative; boundary=\"",
		"Content-Type: text/plain; charset=UTF-8",
		"Content-Type: text/html; charset=UTF-8",
		"Hi Bob.",
		"<p>Hi Bob.</p>",
	} {
		if !strings.Contains(body, needle) {
			t.Errorf("DATA body missing %q\n----\n%s", needle, body)
		}
	}
}

// TestSMTPClient_TextOnlyEmitsSinglePart verifies the
// downgrade-to-text/plain path when HTMLBody is empty.
func TestSMTPClient_TextOnlyEmitsSinglePart(t *testing.T) {
	srv := &fakeSMTPServer{t: t}
	c, err := NewSMTPClient(SMTPConfig{
		Host:        "fake.local",
		Port:        2525,
		FromAddress: "noreply@drive.example.com",
		TLSMode:     TLSModeNone,
		Timeout:     2 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewSMTPClient: %v", err)
	}
	c.dialer = newPipeDialer(t, srv)

	if err := c.Send(context.Background(), Message{
		To:       "bob@example.com",
		Subject:  "Plain only",
		TextBody: "Hi Bob.",
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if strings.Contains(srv.dataBody, "multipart/alternative") {
		t.Fatalf("text-only send should not emit multipart/alternative:\n%s", srv.dataBody)
	}
	if !strings.Contains(srv.dataBody, "Content-Type: text/plain; charset=UTF-8") {
		t.Fatalf("text-only send should emit text/plain:\n%s", srv.dataBody)
	}
}

// TestSMTPClient_RejectsInvalidToAddress proves the client refuses
// to even open a connection when the recipient address is invalid.
// This is what classifies the metric as address_invalid upstream.
func TestSMTPClient_RejectsInvalidToAddress(t *testing.T) {
	c, err := NewSMTPClient(SMTPConfig{
		Host:        "fake.local",
		Port:        2525,
		FromAddress: "noreply@drive.example.com",
		TLSMode:     TLSModeNone,
	})
	if err != nil {
		t.Fatalf("NewSMTPClient: %v", err)
	}
	c.dialer = func(ctx context.Context, network, addr string) (net.Conn, error) {
		return nil, errors.New("should never dial")
	}
	err = c.Send(context.Background(), Message{
		To:       "not-an-address",
		Subject:  "x",
		TextBody: "y",
	})
	if err == nil {
		t.Fatalf("expected error for invalid To address")
	}
	if !errors.Is(err, ErrInvalidAddress) {
		t.Errorf("error did not wrap ErrInvalidAddress: %v", err)
	}
}

// TestSMTPClient_DialFailureSurfaced asserts the wrapping format
// (used by metrics classification) stays stable. If a future
// change replaces the wrap-with-fmt.Errorf pattern, this guards
// against accidentally losing the underlying error string.
func TestSMTPClient_DialFailureSurfaced(t *testing.T) {
	c, err := NewSMTPClient(SMTPConfig{
		Host:        "fake.local",
		Port:        2525,
		FromAddress: "noreply@drive.example.com",
		TLSMode:     TLSModeNone,
	})
	if err != nil {
		t.Fatalf("NewSMTPClient: %v", err)
	}
	c.dialer = func(ctx context.Context, network, addr string) (net.Conn, error) {
		return nil, errors.New("connection refused")
	}
	err = c.Send(context.Background(), Message{
		To:       "bob@example.com",
		Subject:  "x",
		TextBody: "y",
	})
	if err == nil {
		t.Fatalf("expected dial error")
	}
	if !strings.Contains(err.Error(), "dial") || !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("error did not wrap dial cause: %v", err)
	}
}

// TestNewSMTPClient_ValidatesTLSMode pins the constructor's
// guardrail. A typo'd SMTP_TLS_MODE must fail at boot, not at
// first send.
func TestNewSMTPClient_ValidatesTLSMode(t *testing.T) {
	_, err := NewSMTPClient(SMTPConfig{
		Host:        "fake.local",
		Port:        2525,
		FromAddress: "noreply@drive.example.com",
		TLSMode:     "bogus",
	})
	if err == nil {
		t.Fatalf("expected error for invalid TLS mode")
	}
	if !strings.Contains(err.Error(), "invalid TLS mode") {
		t.Errorf("error = %v, want 'invalid TLS mode'", err)
	}
}

// TestNewSMTPClient_RequiresFromAddressWhenConfigured asserts the
// constructor refuses a half-config (host set, from address
// empty) — the boot would otherwise come up and fail on first
// send.
func TestNewSMTPClient_RequiresFromAddressWhenConfigured(t *testing.T) {
	_, err := NewSMTPClient(SMTPConfig{
		Host:        "fake.local",
		Port:        2525,
		FromAddress: "",
		TLSMode:     TLSModeSTARTTLS,
	})
	if err == nil {
		t.Fatalf("expected error for missing FromAddress")
	}
	if !strings.Contains(err.Error(), "SMTP_FROM_ADDRESS") {
		t.Errorf("error = %v, want mention of SMTP_FROM_ADDRESS", err)
	}
}

// TestNewSMTPClient_UnconfiguredHostBootsCleanly verifies the
// graceful-disable branch: empty Host yields a client that
// reports IsConfigured() == false and ErrNotConfigured on Send.
// Disable test goes through Send so the metric path is exercised.
func TestNewSMTPClient_UnconfiguredHostBootsCleanly(t *testing.T) {
	c, err := NewSMTPClient(SMTPConfig{})
	if err != nil {
		t.Fatalf("NewSMTPClient: %v", err)
	}
	if c.IsConfigured() {
		t.Fatalf("IsConfigured should be false when Host is empty")
	}
	if err := c.Send(context.Background(), Message{To: "bob@example.com", TextBody: "x"}); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("Send returned %v, want ErrNotConfigured", err)
	}
}

// TestSMTPClient_HangingRelayDoesNotLeakGoroutine pins the
// post-dial deadline guarantee: a relay that accepts the TCP
// connection then hangs mid-conversation must NOT block Send
// forever. The fix sets conn.SetDeadline based on ctx.Deadline()
// after dial, so subsequent reads/writes against the socket
// time out at the same wall-clock instant as the outer context.
//
// Regression test pinning the dispatch-timeout contract — the
// comment on guestInviteEmailDispatchTimeout
// previously claimed "the goroutine cannot leak even if the
// relay accepts the connection and then hangs indefinitely on a
// write," which was false until SetDeadline was wired through.
func TestSMTPClient_HangingRelayDoesNotLeakGoroutine(t *testing.T) {
	// A "hanging" relay: accept the connection, send the 220
	// banner, but NEVER respond to EHLO. The client should
	// detect ctx.Deadline() passing and return an error within
	// ~the outer context timeout — NOT block forever.
	c, err := NewSMTPClient(SMTPConfig{
		Host:        "fake.local",
		Port:        2525,
		FromAddress: "noreply@drive.example.com",
		TLSMode:     TLSModeNone,
		Timeout:     30 * time.Second, // generous outer fallback — not what the test exercises
	})
	if err != nil {
		t.Fatalf("NewSMTPClient: %v", err)
	}
	c.dialer = func(ctx context.Context, network, addr string) (net.Conn, error) {
		clientSide, serverSide := net.Pipe()
		// Server side: send the 220 banner, then DELIBERATELY
		// stop responding. Any subsequent read on clientSide
		// will block indefinitely without our deadline fix.
		go func() {
			defer func() { _ = serverSide.Close() }()
			bw := bufio.NewWriter(serverSide)
			_, _ = bw.WriteString("220 hanging-relay\r\n")
			_ = bw.Flush()
			// Hang forever: read but never reply. This simulates
			// a misbehaving relay that consumed our EHLO but
			// won't send a 250 response.
			_, _ = io.Copy(io.Discard, serverSide)
		}()
		return clientSide, nil
	}

	// Drive Send with a SHORT outer-context deadline. The fix
	// must convert this into a conn.SetDeadline so the EHLO
	// read returns with an i/o-timeout error before the test
	// wall-clock budget elapses.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- c.Send(ctx, Message{To: "bob@example.com", Subject: "x", TextBody: "y"})
	}()

	// 2 seconds is 10x the conn-deadline window — generous
	// enough to absorb CI scheduling jitter, tight enough to
	// catch a regression where SetDeadline is removed (the test
	// would then block until the t.Deadline harness kills it).
	select {
	case err := <-done:
		if err == nil {
			t.Fatalf("Send against a hanging relay returned nil; expected a timeout error")
		}
		// We don't assert the exact error string (net.OpError
		// vs context.DeadlineExceeded depending on Go version),
		// only that Send completed within the bounded window.
		if !strings.Contains(strings.ToLower(err.Error()), "timeout") &&
			!strings.Contains(strings.ToLower(err.Error()), "deadline") &&
			!strings.Contains(strings.ToLower(err.Error()), "i/o") {
			t.Logf("Send error against hanging relay (acceptable variants: timeout / deadline / i/o): %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("Send blocked >2s against a hanging relay — conn.SetDeadline was not applied post-dial; goroutine would leak in production")
	}
}

// TestWriteRFC5322_RejectsCRLFInjectionInCustomHeaders pins the
// defense-in-depth guard in writeRFC5322. Today the only call-site
// passes a hardcoded {"Auto-Submitted": "auto-generated"} so the
// pre-condition holds, but a future call-site forwarding
// user-controlled metadata (e.g. workspace_name as a header) would
// silently introduce a header injection vector. Validating at the
// writer instead of documenting "callers must not pass untrusted
// values" matches the pattern Go's net/http uses to reject
// malformed inputs at the boundary.
//
// Regression test pinning CRLF-injection rejection in custom headers.
func TestWriteRFC5322_RejectsCRLFInjectionInCustomHeaders(t *testing.T) {
	cases := []struct {
		name   string
		header map[string]string
	}{
		{
			name:   "LF in value smuggles second header",
			header: map[string]string{"X-Custom": "innocent\nBcc: attacker@example.com"},
		},
		{
			name:   "CR in value smuggles second header",
			header: map[string]string{"X-Custom": "innocent\rBcc: attacker@example.com"},
		},
		{
			name:   "CRLF in value smuggles second header",
			header: map[string]string{"X-Custom": "innocent\r\nBcc: attacker@example.com"},
		},
		{
			name:   "NUL in value rejected",
			header: map[string]string{"X-Custom": "innocent\x00trailing"},
		},
		{
			name:   "LF in key rejected",
			header: map[string]string{"X-Custom\nBcc": "value"},
		},
		{
			name:   "colon in key rejected",
			header: map[string]string{"X:Custom": "value"},
		},
		{
			name:   "space in key rejected",
			header: map[string]string{"X Custom": "value"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			err := writeRFC5322(&buf, SMTPConfig{
				FromAddress: "noreply@drive.example.com",
				FromName:    "Drive",
			}, Message{
				To:       "bob@example.com",
				Subject:  "hi",
				TextBody: "body",
				Headers:  tc.header,
			})
			if err == nil {
				// If we wrote successfully, look for the injection
				// pattern in the body — that's a real bug.
				out := buf.String()
				if strings.Contains(strings.ToLower(out), "bcc:") {
					t.Fatalf("CRLF injection succeeded — output contains Bcc header that the caller did NOT explicitly set: %q", out)
				}
				t.Fatalf("writeRFC5322 accepted malformed header where it should reject: %q", tc.header)
			}
			// Verify the error mentions header validation so a
			// future refactor of the error wrapping doesn't silently
			// regress.
			if !strings.Contains(err.Error(), "invalid custom header") &&
				!strings.Contains(err.Error(), "header") {
				t.Errorf("expected error to mention header validation; got %v", err)
			}
		})
	}
}

// TestWriteRFC5322_AcceptsValidCustomHeader pins that the guard
// doesn't false-positive on the legitimate production header.
func TestWriteRFC5322_AcceptsValidCustomHeader(t *testing.T) {
	var buf bytes.Buffer
	err := writeRFC5322(&buf, SMTPConfig{
		FromAddress: "noreply@drive.example.com",
		FromName:    "Drive",
	}, Message{
		To:       "bob@example.com",
		Subject:  "hi",
		TextBody: "body",
		Headers:  map[string]string{"Auto-Submitted": "auto-generated"},
	})
	if err != nil {
		t.Fatalf("writeRFC5322 rejected legitimate Auto-Submitted header: %v", err)
	}
	if !strings.Contains(buf.String(), "Auto-Submitted: auto-generated") {
		t.Errorf("expected Auto-Submitted header in output; got %q", buf.String())
	}
}

// TestGenerateMessageID_StablyShaped pins the Message-ID format
// (RFC 2822 angle-bracketed, contains an "@" + From domain) so a
// future refactor that breaks Postmark / SES dedup is caught.
func TestGenerateMessageID_StablyShaped(t *testing.T) {
	id, err := generateMessageID("noreply@drive.example.com")
	if err != nil {
		t.Fatalf("generateMessageID: %v", err)
	}
	if !strings.HasPrefix(id, "<") || !strings.HasSuffix(id, ">") {
		t.Errorf("Message-ID should be angle-bracketed: %q", id)
	}
	if !strings.Contains(id, "@drive.example.com>") {
		t.Errorf("Message-ID should end with @<from-domain>: %q", id)
	}
}

// helper: keep the suite happy if any leftover fakeSMTPServer
// goroutine is still alive at test exit.
var _ = io.EOF
var _ sync.Mutex
var _ = fmt.Sprintf

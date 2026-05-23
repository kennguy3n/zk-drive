package email

import (
	"bufio"
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
	if !strings.Contains(err.Error(), "invalid To") {
		t.Errorf("error did not mention invalid To: %v", err)
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

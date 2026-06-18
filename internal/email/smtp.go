package email

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/mail"
	"net/smtp"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

// smtpTracerName is the instrumentation-scope name. Resolved
// lazily via otel.Tracer on each Send so the tracer picks up the
// real provider installed by tracing.Init rather than the no-op
// global that existed at package init.
const smtpTracerName = "github.com/kennguy3n/zk-drive/internal/email"

// SMTPConfig is the configuration block consumed by NewSMTPClient.
// Pulled out of internal/config so the email package stays
// importable from places that don't want the larger config surface.
type SMTPConfig struct {
	// Host:Port pair of the SMTP relay. Required for the client to
	// be "configured" — when empty, callers should fall back to
	// NoopClient.
	Host string
	Port int

	// Username and Password are the SMTP-AUTH credentials. Both
	// optional: when both are empty the client skips AUTH and
	// connects anonymously (useful for in-cluster relays that are
	// already locked down to the cluster's network).
	Username string
	Password string

	// FromAddress is the envelope sender (MAIL FROM) and the
	// canonical From: header address. Required. Must be a single
	// addr-spec, no display name.
	FromAddress string

	// FromName is the optional display name on the From: header.
	// When empty the header omits the display-name part.
	FromName string

	// TLSMode selects how TLS is negotiated. One of "implicit"
	// (wrap the socket in TLS before SMTP), "starttls" (issue
	// STARTTLS after EHLO), or "none" (plain text only — use
	// only for local dev / containerised relays on a private
	// network). Default starttls.
	TLSMode string

	// TLSServerName overrides the SNI / certificate-verify
	// hostname. Defaults to Host. Operators with a relay
	// reachable by IP but presenting a cert for a hostname set
	// this to that hostname.
	TLSServerName string

	// TLSInsecureSkipVerify disables certificate verification.
	// Off by default. Operators with self-signed local relays
	// can flip this on; production should keep it off.
	TLSInsecureSkipVerify bool

	// Timeout caps the per-send wall time (connect + EHLO + AUTH
	// + DATA). Defaults to 30s. The send is also bounded by the
	// context passed to Send.
	Timeout time.Duration

	// PoolMaxIdle caps the number of warm connections kept open
	// between sends. 0 → default (2). A negative value disables
	// pooling entirely: every send dials a fresh connection and
	// closes it afterwards (the pre-pooling behaviour).
	PoolMaxIdle int

	// PoolMaxConnIdleTime evicts a pooled connection that has been
	// idle longer than this before it is reused — kept below the
	// 30-60s most relays use to reap idle sockets so a reused
	// connection is rarely already half-closed. 0 → default (15s).
	PoolMaxConnIdleTime time.Duration

	// PoolMaxConnLifetime caps the total age of a pooled connection
	// regardless of idleness, so a long-lived connection is
	// periodically refreshed (relay restart, credential rotation,
	// NAT rebinding). 0 → default (5m).
	PoolMaxConnLifetime time.Duration
}

// TLS mode constants — exported so tests and config validation can
// reference the legal values without typo'ing the strings.
const (
	TLSModeImplicit = "implicit"
	TLSModeSTARTTLS = "starttls"
	TLSModeNone     = "none"
)

// DefaultTimeout is applied when SMTPConfig.Timeout is <= 0.
const DefaultTimeout = 30 * time.Second

// SMTPClient is the production Sender. It keeps a small pool of warm
// connections (already past EHLO / STARTTLS / AUTH) so a burst of
// sends — e.g. a workspace owner inviting several guests at once —
// reuses an established TLS+AUTH session instead of paying a fresh
// handshake per message. The pool bounds only the number of IDLE
// connections (PoolMaxIdle); a send that finds no warm connection
// dials a fresh one, and connections idle past PoolMaxConnIdleTime
// or older than PoolMaxConnLifetime are evicted so a relay that
// reaped an idle socket never serves a half-open connection. Pooling
// can be disabled with a negative PoolMaxIdle (every send then dials
// fresh and closes afterwards).
type SMTPClient struct {
	cfg        SMTPConfig
	configured bool

	// pool holds warm, authenticated connections for reuse. Always
	// non-nil for a configured client (a disabled negative-MaxIdle
	// pool simply never retains a connection).
	pool *connPool

	// dialer is overridable by tests so they can capture the SMTP
	// conversation without a real network listener. Production
	// always sets this to net.Dial via newDefaultDialer().
	dialer func(ctx context.Context, network, addr string) (net.Conn, error)

	// hostnameOnce caches the local hostname (used for EHLO).
	// SMTP HELO/EHLO requires a domain identifier; an empty or
	// "localhost" string is rejected by some relays. We resolve
	// os.Hostname lazily.
	hostnameOnce sync.Once
	hostname     string
}

// NewSMTPClient validates the config and returns a usable client.
// Empty Host yields a client that reports IsConfigured()==false —
// callers should prefer NoopClient in that case (this constructor
// still returns successfully so the boot sequence can defer the
// configured/not-configured decision to a single code path).
func NewSMTPClient(cfg SMTPConfig) (*SMTPClient, error) {
	cfg.TLSMode = strings.ToLower(strings.TrimSpace(cfg.TLSMode))
	if cfg.TLSMode == "" {
		cfg.TLSMode = TLSModeSTARTTLS
	}
	switch cfg.TLSMode {
	case TLSModeImplicit, TLSModeSTARTTLS, TLSModeNone:
	default:
		return nil, fmt.Errorf("email: invalid TLS mode %q (want implicit|starttls|none)", cfg.TLSMode)
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultTimeout
	}
	// Resolve pooling sizing. PoolMaxIdle is left untouched when
	// negative (operator opt-out — pooling disabled); a zero takes
	// the default. The two duration knobs always take their default
	// when non-positive.
	if cfg.PoolMaxIdle == 0 {
		cfg.PoolMaxIdle = defaultPoolMaxIdle
	}
	if cfg.PoolMaxConnIdleTime <= 0 {
		cfg.PoolMaxConnIdleTime = defaultPoolMaxConnIdleTime
	}
	if cfg.PoolMaxConnLifetime <= 0 {
		cfg.PoolMaxConnLifetime = defaultPoolMaxConnLifetime
	}
	configured := strings.TrimSpace(cfg.Host) != "" && cfg.Port != 0
	if configured {
		if strings.TrimSpace(cfg.FromAddress) == "" {
			return nil, errors.New("email: SMTP_FROM_ADDRESS is required when SMTP_HOST is set")
		}
		if _, err := mail.ParseAddress(cfg.FromAddress); err != nil {
			return nil, fmt.Errorf("email: invalid FromAddress: %w", err)
		}
	}
	c := &SMTPClient{cfg: cfg, configured: configured}
	c.pool = newConnPool(cfg.PoolMaxIdle, cfg.PoolMaxConnIdleTime, cfg.PoolMaxConnLifetime)
	c.dialer = newDefaultDialer(cfg.Timeout)
	return c, nil
}

func newDefaultDialer(timeout time.Duration) func(ctx context.Context, network, addr string) (net.Conn, error) {
	d := &net.Dialer{Timeout: timeout}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		return d.DialContext(ctx, network, addr)
	}
}

// IsConfigured implements Sender.
func (c *SMTPClient) IsConfigured() bool { return c.configured }

// Send implements Sender. Connects, optionally STARTTLS, optionally
// AUTHs, then MAIL FROM / RCPT TO / DATA. Returns context errors
// transparently so the caller can distinguish a transport failure
// from a request cancellation.
//
// A single span wraps the entire SMTP conversation (dial → TLS →
// AUTH → MAIL FROM → RCPT TO → DATA → QUIT). Per-phase events
// (added via span.AddEvent below) let the trace backend show
// inter-phase wait times without inflating span count. Phase
// attributes mirror the [OTel SMTP semantic conventions] —
// network.peer.address, network.peer.port, tls.mode — so any
// backend that knows OTel can group / filter sends without custom
// mapping.
//
// [OTel SMTP semantic conventions]: https://opentelemetry.io/docs/specs/semconv/messaging/
func (c *SMTPClient) Send(ctx context.Context, msg Message) (sendErr error) {
	ctx, span := otel.Tracer(smtpTracerName).Start(ctx,
		"smtp.send",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			semconv.NetworkPeerAddress(c.cfg.Host),
			semconv.NetworkPeerPort(c.cfg.Port),
			attribute.String("tls.mode", c.cfg.TLSMode),
			attribute.Int("email.message.size", len(msg.TextBody)+len(msg.HTMLBody)+len(msg.Subject)),
			attribute.String("email.template", msg.TemplateName),
		),
	)
	defer func() {
		if sendErr != nil {
			span.RecordError(sendErr)
			span.SetStatus(codes.Error, "smtp send failed")
		}
		span.End()
	}()

	if !c.configured {
		return ErrNotConfigured
	}
	if strings.TrimSpace(msg.To) == "" {
		return fmt.Errorf("%w: Message.To is required", ErrInvalidAddress)
	}
	if _, err := mail.ParseAddress(msg.To); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidAddress, err)
	}

	// One wall-clock deadline bounds the whole conversation — the
	// RSET liveness probe on a reused connection, the handshake on a
	// fresh one, and the MAIL FROM / RCPT TO / DATA exchange either
	// way. Derived from the caller's ctx when it carries a deadline
	// (matches caller intent, e.g. dispatchGuestInviteEmail's
	// detached 60s budget), else now+Timeout for a deadline-free
	// context (tests, scripts). See acquire / establishConn for how
	// it is applied to the socket.
	convDeadline := time.Now().Add(c.cfg.Timeout)
	if d, ok := ctx.Deadline(); ok {
		convDeadline = d
	}

	pc, reused, err := c.acquire(ctx, convDeadline)
	if err != nil {
		return err
	}
	span.SetAttributes(attribute.Bool("smtp.conn.reused", reused))

	// A connection that errors mid-transaction is in an unknown
	// protocol state, so it is closed rather than returned to the
	// pool; only a cleanly-completed send is handed back for reuse.
	if err := c.deliver(pc.cli, msg); err != nil {
		pc.close()
		return err
	}
	c.pool.put(pc)
	return nil
}

// acquire returns a connection that is one MAIL FROM away from
// sending: a warm pooled connection when one is available and still
// live, otherwise a freshly dialled+handshaked+authenticated one.
// reused reports which (surfaced on the span). The caller owns the
// returned connection and MUST either hand it back via pool.put (on
// a clean send) or tear it down via pc.close (on any error).
func (c *SMTPClient) acquire(ctx context.Context, convDeadline time.Time) (*pooledConn, bool, error) {
	for {
		warm := c.pool.get()
		if warm == nil {
			break
		}
		// The pooled socket still carries the (now-expired) deadline
		// from its previous send, so re-stamp it before any I/O. Then
		// RSET: it clears any half-finished transaction AND proves the
		// relay has not silently dropped the idle socket. A failure
		// means the connection is dead — discard it and try the next
		// warm one, falling through to a fresh dial when none remain.
		if err := warm.conn.SetDeadline(convDeadline); err != nil {
			warm.close()
			continue
		}
		if err := warm.cli.Reset(); err != nil {
			warm.close()
			continue
		}
		return warm, true, nil
	}
	fresh, err := c.establishConn(ctx, convDeadline)
	if err != nil {
		return nil, false, err
	}
	return fresh, false, nil
}

// establishConn dials and brings a connection up to the warm state —
// past EHLO, optional STARTTLS, optional AUTH — so it is ready for
// MAIL FROM. convDeadline is stamped on the socket immediately after
// dial so every subsequent read/write (handshake, AUTH, and later
// the transaction) is bounded: net/smtp's high-level API takes no
// context, so without this a relay that accepts the TCP connection
// then hangs mid-conversation would leak the calling goroutine
// forever. The deadline covers plaintext, STARTTLS-upgraded, and
// implicit-TLS sockets alike (the tls.Conn wraps the deadlined
// net.Conn). Any failure before the connection is wrapped closes the
// raw socket so a half-open connection is never leaked.
func (c *SMTPClient) establishConn(ctx context.Context, convDeadline time.Time) (pc *pooledConn, err error) {
	addr := net.JoinHostPort(c.cfg.Host, fmt.Sprintf("%d", c.cfg.Port))
	dialCtx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()

	conn, err := c.dialer(dialCtx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("email: dial %s: %w", addr, err)
	}
	createdAt := time.Now()
	// Close the raw socket on every failure path below; cleared once
	// the connection is successfully wrapped in the returned pooledConn.
	ok := false
	defer func() {
		if !ok {
			_ = conn.Close()
		}
	}()

	if err := conn.SetDeadline(convDeadline); err != nil {
		return nil, fmt.Errorf("email: set conn deadline: %w", err)
	}

	if c.cfg.TLSMode == TLSModeImplicit {
		tlsConn := tls.Client(conn, c.tlsConfig())
		if err := tlsConn.HandshakeContext(dialCtx); err != nil {
			return nil, fmt.Errorf("email: implicit TLS handshake: %w", err)
		}
		conn = tlsConn
	}

	cli, err := smtp.NewClient(conn, c.cfg.Host)
	if err != nil {
		return nil, fmt.Errorf("email: smtp.NewClient: %w", err)
	}

	if err := cli.Hello(c.localHostname()); err != nil {
		return nil, fmt.Errorf("email: EHLO: %w", err)
	}

	if c.cfg.TLSMode == TLSModeSTARTTLS {
		if advertised, _ := cli.Extension("STARTTLS"); !advertised {
			return nil, errors.New("email: server does not advertise STARTTLS")
		}
		if err := cli.StartTLS(c.tlsConfig()); err != nil {
			return nil, fmt.Errorf("email: STARTTLS: %w", err)
		}
	}

	if c.cfg.Username != "" || c.cfg.Password != "" {
		auth := smtp.PlainAuth("", c.cfg.Username, c.cfg.Password, c.cfg.Host)
		if err := cli.Auth(auth); err != nil {
			return nil, fmt.Errorf("email: AUTH: %w", err)
		}
	}

	ok = true
	return &pooledConn{cli: cli, conn: conn, createdAt: createdAt, lastUsed: createdAt}, nil
}

// deliver runs a single mail transaction (MAIL FROM / RCPT TO / DATA
// + body) on an already-warm connection. The connection's deadline
// has been set by acquire / establishConn, so deliver does no socket
// timeout bookkeeping of its own.
func (c *SMTPClient) deliver(cli *smtp.Client, msg Message) error {
	if err := cli.Mail(c.cfg.FromAddress); err != nil {
		return fmt.Errorf("email: MAIL FROM: %w", err)
	}
	if err := cli.Rcpt(msg.To); err != nil {
		return fmt.Errorf("email: RCPT TO: %w", err)
	}
	wc, err := cli.Data()
	if err != nil {
		return fmt.Errorf("email: DATA: %w", err)
	}
	if err := writeRFC5322(wc, c.cfg, msg); err != nil {
		_ = wc.Close()
		return fmt.Errorf("email: write body: %w", err)
	}
	if err := wc.Close(); err != nil {
		return fmt.Errorf("email: close DATA: %w", err)
	}
	return nil
}

// Close releases the connection pool, tearing down every warm
// connection. It implements io.Closer so the email Service can
// release the client during graceful shutdown. In-flight sends are
// unaffected (their connection is no longer in the idle set) and
// complete normally, closing on put. Safe to call more than once.
func (c *SMTPClient) Close() error {
	if c.pool != nil {
		c.pool.close()
	}
	return nil
}

func (c *SMTPClient) tlsConfig() *tls.Config {
	serverName := c.cfg.TLSServerName
	if serverName == "" {
		serverName = c.cfg.Host
	}
	return &tls.Config{
		ServerName:         serverName,
		InsecureSkipVerify: c.cfg.TLSInsecureSkipVerify, //nolint:gosec // operator opt-in for self-signed dev relays
		MinVersion:         tls.VersionTLS12,
	}
}

func (c *SMTPClient) localHostname() string {
	c.hostnameOnce.Do(func() {
		c.hostname = hostnameOrDefault()
	})
	return c.hostname
}

// hostnameOrDefault returns a non-empty hostname suitable for
// EHLO. Falls back to a literal that some relays accept ([127.0.0.1])
// rather than the empty string (which fails strict relays).
func hostnameOrDefault() string {
	if h := osHostname(); h != "" && h != "localhost" {
		return h
	}
	return "[127.0.0.1]"
}

// writeRFC5322 emits the RFC 5322 + MIME body for a single
// Message. Headers we always set: From, To, Subject (RFC 2047
// encoded if non-ASCII), Date, Message-ID, MIME-Version. Bodies:
// when HTMLBody is empty we emit a plain text/plain part;
// otherwise we emit a multipart/alternative with the text part
// first (per RFC 2046 §5.1.4, less-preferred parts come first).
func writeRFC5322(w io.Writer, cfg SMTPConfig, msg Message) error {
	var buf bytes.Buffer

	// From header — wrap with display name if present.
	from := cfg.FromAddress
	if cfg.FromName != "" {
		from = (&mail.Address{Name: cfg.FromName, Address: cfg.FromAddress}).String()
	}
	to := msg.To
	if msg.RecipientName != "" {
		to = (&mail.Address{Name: msg.RecipientName, Address: msg.To}).String()
	}

	fmt.Fprintf(&buf, "From: %s\r\n", from)
	fmt.Fprintf(&buf, "To: %s\r\n", to)
	fmt.Fprintf(&buf, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", msg.Subject))
	fmt.Fprintf(&buf, "Date: %s\r\n", time.Now().UTC().Format(time.RFC1123Z))
	mid, err := generateMessageID(cfg.FromAddress)
	if err != nil {
		return err
	}
	fmt.Fprintf(&buf, "Message-ID: %s\r\n", mid)
	fmt.Fprint(&buf, "MIME-Version: 1.0\r\n")
	// Caller-supplied custom headers (e.g. Auto-Submitted).
	//
	// Defense-in-depth: reject any header key or value that contains
	// CR, LF, or NUL. RFC 5322 §2.2 limits field-names to printable
	// ASCII excluding ":", and field-values to printable ASCII with
	// CRLF reserved for folding only — so a key or value containing
	// raw CR/LF could be used to inject arbitrary additional headers
	// (e.g. a malicious workspace_name value smuggling a "Bcc:
	// attacker@example.com" line). Today the only call-site passes
	// the hardcoded {"Auto-Submitted": "auto-generated"} map, so the
	// pre-condition holds — but a future call-site that forwards
	// user-controlled metadata would silently introduce a header
	// injection vulnerability without this guard. Validating at the
	// writer (instead of documenting "callers must not pass untrusted
	// values") follows the same principle as Go's net/http header
	// validation: parsers/writers reject malformed input rather than
	// relying on callers to remember.
	for k, v := range msg.Headers {
		// Skip headers we manage to keep RFC 5322 sanity.
		switch strings.ToLower(k) {
		case "from", "to", "subject", "date", "message-id", "mime-version",
			"content-type", "content-transfer-encoding":
			continue
		}
		if err := validateHeaderKV(k, v); err != nil {
			return fmt.Errorf("email: invalid custom header %q: %w", k, err)
		}
		fmt.Fprintf(&buf, "%s: %s\r\n", k, v)
	}

	if msg.HTMLBody == "" {
		fmt.Fprint(&buf, "Content-Type: text/plain; charset=UTF-8\r\n")
		fmt.Fprint(&buf, "Content-Transfer-Encoding: 8bit\r\n")
		fmt.Fprint(&buf, "\r\n")
		fmt.Fprint(&buf, msg.TextBody)
	} else {
		boundary, err := randomBoundary()
		if err != nil {
			return err
		}
		fmt.Fprintf(&buf, "Content-Type: multipart/alternative; boundary=\"%s\"\r\n", boundary)
		fmt.Fprint(&buf, "\r\n")
		fmt.Fprintf(&buf, "--%s\r\n", boundary)
		fmt.Fprint(&buf, "Content-Type: text/plain; charset=UTF-8\r\n")
		fmt.Fprint(&buf, "Content-Transfer-Encoding: 8bit\r\n\r\n")
		fmt.Fprint(&buf, msg.TextBody)
		fmt.Fprint(&buf, "\r\n")
		fmt.Fprintf(&buf, "--%s\r\n", boundary)
		fmt.Fprint(&buf, "Content-Type: text/html; charset=UTF-8\r\n")
		fmt.Fprint(&buf, "Content-Transfer-Encoding: 8bit\r\n\r\n")
		fmt.Fprint(&buf, msg.HTMLBody)
		fmt.Fprint(&buf, "\r\n")
		fmt.Fprintf(&buf, "--%s--\r\n", boundary)
	}

	_, err = io.Copy(w, &buf)
	return err
}

// generateMessageID returns an RFC 2822-compliant Message-ID built
// from 128 bits of crypto-random + the From domain. Stable enough
// across relays that Postmark / SES dedupe correctly.
func generateMessageID(fromAddr string) (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("email: random message id: %w", err)
	}
	addr, err := mail.ParseAddress(fromAddr)
	if err != nil {
		return "", fmt.Errorf("email: parse from for message id: %w", err)
	}
	at := strings.LastIndex(addr.Address, "@")
	if at < 0 || at == len(addr.Address)-1 {
		return "", fmt.Errorf("email: from address missing domain: %q", fromAddr)
	}
	domain := addr.Address[at+1:]
	return fmt.Sprintf("<%s@%s>", base64.RawURLEncoding.EncodeToString(buf[:]), domain), nil
}

func randomBoundary() (string, error) {
	var buf [18]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("email: random boundary: %w", err)
	}
	return "_=_" + base64.RawURLEncoding.EncodeToString(buf[:]), nil
}

// validateHeaderKV rejects a header key/value pair that would
// violate RFC 5322 §2.2 or enable header injection. Rules:
//
//   - Key MUST be a non-empty sequence of printable ASCII chars
//     excluding ":" (field-name = 1*ftext, where ftext is %d33-57
//     / %d59-126 — i.e., any printable except colon). We reject
//     anything outside that range to keep parity with net/textproto.
//   - Key MUST NOT contain CR (0x0D), LF (0x0A), or NUL (0x00).
//   - Value MUST NOT contain CR, LF, or NUL. RFC 5322 reserves CRLF
//     exclusively for header folding; an embedded CRLF in a value
//     terminates the current header and starts a new one, which is
//     the classic header-injection vector.
//
// The rules deliberately do NOT attempt RFC 5322 long-line folding
// or any other normalisation — they're a strict guard, not a
// rewrite. If a future call-site needs to emit values longer than
// 998 octets it can do its own folding before passing the value in.
func validateHeaderKV(key, value string) error {
	if key == "" {
		return errors.New("empty header key")
	}
	for i := 0; i < len(key); i++ {
		c := key[i]
		// Allow only printable ASCII (0x21-0x7E) excluding ":".
		if c <= 0x20 || c >= 0x7f || c == ':' {
			return fmt.Errorf("header key contains invalid byte 0x%02x at offset %d", c, i)
		}
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		if c == '\r' || c == '\n' || c == 0 {
			return fmt.Errorf("header value contains forbidden byte 0x%02x at offset %d (CR/LF/NUL would enable header injection)", c, i)
		}
	}
	return nil
}

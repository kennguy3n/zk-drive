package webhooks

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// ErrURLInvalid covers every reason ValidateURL might reject a URL:
// bad scheme, no host, malformed parse. ErrURLBlocked covers the IP
// address falling into a forbidden range (SSRF defense). They're
// separate sentinels so the API layer can distinguish "user typed
// gibberish" from "user pointed us at 169.254.169.254" — the latter
// gets logged as a probable SSRF probe attempt.
var (
	ErrURLInvalid = errors.New("webhooks: url invalid")
	ErrURLBlocked = errors.New("webhooks: url resolves to a blocked address range")
)

// Resolver is the slice of net.Resolver the validator depends on.
// Extracted as an interface so unit tests can supply a fake without
// touching the live DNS resolver. Production passes net.DefaultResolver.
type Resolver interface {
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
}

// URLValidator centralises SSRF policy. AllowHTTP controls whether
// http:// URLs are accepted (false in production, true for the
// integration test harness). AllowLoopback similarly opens up
// 127.0.0.0/8 + ::1 for dev / test where the subscriber lives on
// localhost. Both default to false on the zero value.
type URLValidator struct {
	Resolver     Resolver
	AllowHTTP    bool
	AllowLoopback bool
}

// NewURLValidator returns a validator using net.DefaultResolver and
// the production-safe defaults (https only, no loopback). Callers
// that need to relax these for dev / test mutate the fields directly.
func NewURLValidator() *URLValidator {
	return &URLValidator{Resolver: net.DefaultResolver}
}

// Validate parses the URL and confirms it points at a public,
// internet-routable endpoint. On success it returns the parsed
// *url.URL — callers persist the URL string but use the parsed value
// for the immediate delivery attempt to save a re-parse round trip.
//
// Validation is two-stage:
//  1. Syntactic — must be https (or http when AllowHTTP), must have
//     a host, must not be a userinfo URL ("https://user:pass@host"
//     is rejected to prevent credentials from leaking into logs).
//  2. Network — the hostname is resolved and every returned IP is
//     checked against the blocked ranges (loopback / link-local /
//     RFC1918 / multicast / cloud metadata IPs). ALL resolved IPs
//     must pass — a multi-A-record host where any record is private
//     is rejected, on the theory that a partial resolution to a
//     private address is almost certainly a DNS rebinding attempt.
func (v *URLValidator) Validate(ctx context.Context, raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("%w: parse: %v", ErrURLInvalid, err)
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
	case "http":
		if !v.AllowHTTP {
			return nil, fmt.Errorf("%w: http scheme not allowed", ErrURLInvalid)
		}
	default:
		return nil, fmt.Errorf("%w: scheme %q not allowed", ErrURLInvalid, u.Scheme)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("%w: missing host", ErrURLInvalid)
	}
	if u.User != nil {
		return nil, fmt.Errorf("%w: userinfo not allowed in url", ErrURLInvalid)
	}
	host := u.Hostname()
	if host == "" {
		return nil, fmt.Errorf("%w: empty hostname", ErrURLInvalid)
	}

	addrs, err := v.lookup(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("%w: resolve %s: %v", ErrURLInvalid, host, err)
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("%w: %s has no addresses", ErrURLInvalid, host)
	}
	for _, a := range addrs {
		if reason, blocked := v.blockedReason(a.IP); blocked {
			return nil, fmt.Errorf("%w: %s -> %s (%s)", ErrURLBlocked, host, a.IP, reason)
		}
	}
	return u, nil
}

// ValidateResolved is the cheaper re-validation path the delivery
// worker calls before each attempt. It accepts an already-parsed URL
// and re-resolves the host, comparing against the blocked ranges. This
// is the DNS-rebinding defense: an attacker who registered a hostname
// that resolved to 1.2.3.4 at subscription-create time can swap the
// DNS record to 169.254.169.254 between deliveries; the worker re-
// resolves on every attempt so the swap is caught.
func (v *URLValidator) ValidateResolved(ctx context.Context, u *url.URL) error {
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("%w: empty hostname", ErrURLInvalid)
	}
	addrs, err := v.lookup(ctx, host)
	if err != nil {
		return fmt.Errorf("%w: resolve %s: %v", ErrURLInvalid, host, err)
	}
	if len(addrs) == 0 {
		return fmt.Errorf("%w: %s has no addresses", ErrURLInvalid, host)
	}
	for _, a := range addrs {
		if reason, blocked := v.blockedReason(a.IP); blocked {
			return fmt.Errorf("%w: %s -> %s (%s)", ErrURLBlocked, host, a.IP, reason)
		}
	}
	return nil
}

func (v *URLValidator) lookup(ctx context.Context, host string) ([]net.IPAddr, error) {
	// If the host is already an IP literal, short-circuit DNS and
	// run the policy check directly. This is the common case for
	// the test suite (URL = http://127.0.0.1:port) and saves a
	// resolver call.
	if ip := net.ParseIP(host); ip != nil {
		return []net.IPAddr{{IP: ip}}, nil
	}
	r := v.Resolver
	if r == nil {
		r = net.DefaultResolver
	}
	return r.LookupIPAddr(ctx, host)
}

// blockedReason returns ("", false) when the IP is acceptable, or
// (reason, true) explaining why it is forbidden. Centralising the
// policy in one switch keeps the catalog easy to audit.
//
// Categories:
//   - Loopback (127.0.0.0/8, ::1): blocked unless AllowLoopback.
//   - Unspecified (0.0.0.0, ::): always blocked.
//   - Link-local (169.254.0.0/16, fe80::/10): always blocked. This
//     range INCLUDES the AWS/GCP/Azure metadata service IPs that an
//     SSRF probe most often targets.
//   - Multicast (224.0.0.0/4, ff00::/8): always blocked.
//   - RFC1918 private (10/8, 172.16/12, 192.168/16): always blocked.
//   - RFC6598 carrier NAT (100.64.0.0/10): always blocked.
//   - IPv6 ULA (fc00::/7): always blocked.
//   - Documentation ranges (192.0.2.0/24, 198.51.100.0/24,
//     203.0.113.0/24, 2001:db8::/32): always blocked.
//   - Default-gateway-style addresses (192.0.0.0/24): blocked.
//
// IPv4-mapped-IPv6 addresses (::ffff:10.0.0.1) are unmapped first so
// a single check covers both stacks.
func (v *URLValidator) blockedReason(ip net.IP) (string, bool) {
	if ip == nil {
		return "nil IP", true
	}
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	if ip.IsLoopback() {
		if v.AllowLoopback {
			return "", false
		}
		return "loopback", true
	}
	if ip.IsUnspecified() {
		return "unspecified", true
	}
	if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return "link-local (includes cloud metadata IPs)", true
	}
	if ip.IsMulticast() {
		return "multicast", true
	}
	if ip.IsPrivate() {
		return "rfc1918 private", true
	}
	for _, r := range extraBlockedRanges {
		if r.cidr.Contains(ip) {
			return r.reason, true
		}
	}
	return "", false
}

type cidrBlock struct {
	cidr   *net.IPNet
	reason string
}

// extraBlockedRanges enumerates ranges that net.IP's built-in helpers
// don't cover. Each entry carries an admin-readable reason that
// surfaces in the audit log so an operator can quickly understand why
// a subscription was rejected. Computed once at package init so the
// hot path is just a slice walk.
var extraBlockedRanges = func() []cidrBlock {
	specs := []struct {
		cidr   string
		reason string
	}{
		{"100.64.0.0/10", "rfc6598 carrier nat"},
		{"192.0.0.0/24", "rfc6890 reserved"},
		{"192.0.2.0/24", "rfc5737 documentation"},
		{"198.18.0.0/15", "rfc2544 benchmark"},
		{"198.51.100.0/24", "rfc5737 documentation"},
		{"203.0.113.0/24", "rfc5737 documentation"},
		{"240.0.0.0/4", "rfc1112 reserved (class e)"},
		{"fc00::/7", "ipv6 ula"},
		{"2001:db8::/32", "ipv6 documentation"},
	}
	out := make([]cidrBlock, 0, len(specs))
	for _, s := range specs {
		_, n, err := net.ParseCIDR(s.cidr)
		if err != nil {
			// Programmer error in the literal above — fail
			// loudly at package init rather than letting a
			// typo silently weaken SSRF defense.
			panic(fmt.Sprintf("webhooks: invalid CIDR in extraBlockedRanges: %s: %v", s.cidr, err))
		}
		out = append(out, cidrBlock{cidr: n, reason: s.reason})
	}
	return out
}()

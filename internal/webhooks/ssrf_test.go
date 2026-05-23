package webhooks

import (
	"context"
	"errors"
	"net"
	"net/url"
	"testing"
)

// fakeResolver is a deterministic Resolver for SSRF unit tests. It
// maps a hostname to a canned list of IPs so a test can simulate
// "this host resolves to 127.0.0.1" without needing the real DNS.
type fakeResolver struct {
	hosts map[string][]net.IPAddr
	err   error
}

func (f *fakeResolver) LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error) {
	if f.err != nil {
		return nil, f.err
	}
	if ips, ok := f.hosts[host]; ok {
		return ips, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
}

func ipAddrs(ips ...string) []net.IPAddr {
	out := make([]net.IPAddr, 0, len(ips))
	for _, s := range ips {
		out = append(out, net.IPAddr{IP: net.ParseIP(s)})
	}
	return out
}

// publicResolver maps every test hostname to a routable public IP
// (the documentation ranges 192.0.2/24, 198.51.100/24, 203.0.113/24
// are explicitly blocked, so we deliberately use Cloudflare /
// Google addresses here). Used for "valid URL" cases where we want
// the IP-range check to pass.
func publicResolver() *fakeResolver {
	return &fakeResolver{hosts: map[string][]net.IPAddr{
		"example.com":     ipAddrs("1.1.1.1"),
		"hooks.slack.com": ipAddrs("104.16.0.1"),
		"api.example.com": ipAddrs("8.8.8.8"),
	}}
}

func TestURLValidator_RejectsSchemes(t *testing.T) {
	t.Parallel()
	v := NewURLValidator()
	v.Resolver = publicResolver()
	cases := []string{
		"ftp://example.com/x",
		"gopher://example.com/x",
		"file:///etc/passwd",
		"javascript:alert(1)",
		"data:text/plain;base64,YWJj",
	}
	for _, raw := range cases {
		raw := raw
		t.Run(raw, func(t *testing.T) {
			t.Parallel()
			if _, err := v.Validate(context.Background(), raw); !errors.Is(err, ErrURLInvalid) {
				t.Fatalf("Validate(%q): err=%v, want ErrURLInvalid", raw, err)
			}
		})
	}
}

func TestURLValidator_HTTPGatedByAllowHTTP(t *testing.T) {
	t.Parallel()
	v := NewURLValidator()
	v.Resolver = publicResolver()
	if _, err := v.Validate(context.Background(), "http://example.com/x"); !errors.Is(err, ErrURLInvalid) {
		t.Fatalf("http rejected by default: err=%v want ErrURLInvalid", err)
	}
	v.AllowHTTP = true
	if _, err := v.Validate(context.Background(), "http://example.com/x"); err != nil {
		t.Fatalf("http allowed when AllowHTTP=true: %v", err)
	}
}

func TestURLValidator_RejectsUserinfo(t *testing.T) {
	t.Parallel()
	v := NewURLValidator()
	v.Resolver = publicResolver()
	if _, err := v.Validate(context.Background(), "https://user:pass@example.com/x"); !errors.Is(err, ErrURLInvalid) {
		t.Fatalf("userinfo URL: err=%v want ErrURLInvalid", err)
	}
}

func TestURLValidator_RejectsBlockedRanges(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		ip   string
	}{
		{"loopback_ipv4", "127.0.0.1"},
		{"loopback_ipv6", "::1"},
		{"rfc1918_10", "10.0.0.5"},
		{"rfc1918_172_16", "172.16.0.5"},
		{"rfc1918_192_168", "192.168.1.10"},
		{"link_local_ipv4", "169.254.0.1"},
		{"link_local_ipv6", "fe80::1"},
		{"multicast_ipv4", "224.0.0.1"},
		{"multicast_ipv6", "ff02::1"},
		{"cloud_metadata_aws", "169.254.169.254"},
		{"rfc6598_carrier_nat", "100.64.0.5"},
		{"ipv6_ula", "fc00::1"},
		{"documentation_192_0_2", "192.0.2.1"},
		{"unspecified", "0.0.0.0"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			v := NewURLValidator()
			v.Resolver = &fakeResolver{hosts: map[string][]net.IPAddr{
				"target.example.com": ipAddrs(c.ip),
			}}
			if _, err := v.Validate(context.Background(), "https://target.example.com/x"); !errors.Is(err, ErrURLBlocked) {
				t.Fatalf("Validate ip=%s: err=%v want ErrURLBlocked", c.ip, err)
			}
		})
	}
}

func TestURLValidator_RejectsMixedPublicAndPrivate(t *testing.T) {
	t.Parallel()
	// A multi-A-record host where ANY record is private is rejected:
	// it's almost certainly an attempt to bypass the check via DNS
	// rebinding by returning a public IP first and a private IP
	// shortly after.
	v := NewURLValidator()
	v.Resolver = &fakeResolver{hosts: map[string][]net.IPAddr{
		"mixed.example.com": ipAddrs("1.1.1.1", "127.0.0.1"),
	}}
	if _, err := v.Validate(context.Background(), "https://mixed.example.com/x"); !errors.Is(err, ErrURLBlocked) {
		t.Fatalf("mixed public+private: err=%v want ErrURLBlocked", err)
	}
}

func TestURLValidator_LoopbackOptIn(t *testing.T) {
	t.Parallel()
	// AllowLoopback bypasses the loopback block — used by the dev /
	// integration-test harness where the subscriber endpoint runs
	// on localhost.
	v := NewURLValidator()
	v.Resolver = &fakeResolver{hosts: map[string][]net.IPAddr{
		"localhost": ipAddrs("127.0.0.1"),
	}}
	v.AllowHTTP = true
	v.AllowLoopback = true
	if _, err := v.Validate(context.Background(), "http://localhost:8080/x"); err != nil {
		t.Fatalf("loopback opt-in: %v", err)
	}
}

func TestURLValidator_AcceptsPublic(t *testing.T) {
	t.Parallel()
	v := NewURLValidator()
	v.Resolver = publicResolver()
	for _, raw := range []string{
		"https://example.com/webhook",
		"https://hooks.slack.com/services/abc",
		"https://api.example.com/path?q=1",
	} {
		raw := raw
		t.Run(raw, func(t *testing.T) {
			t.Parallel()
			u, err := v.Validate(context.Background(), raw)
			if err != nil {
				t.Fatalf("Validate(%q): %v", raw, err)
			}
			if u == nil {
				t.Fatal("returned nil URL on success")
			}
		})
	}
}

func TestURLValidator_RejectsMalformedURL(t *testing.T) {
	t.Parallel()
	v := NewURLValidator()
	v.Resolver = publicResolver()
	for _, raw := range []string{
		"",
		"://no-scheme",
		"https://",
	} {
		raw := raw
		t.Run(raw, func(t *testing.T) {
			t.Parallel()
			if _, err := v.Validate(context.Background(), raw); !errors.Is(err, ErrURLInvalid) {
				t.Fatalf("Validate(%q): err=%v want ErrURLInvalid", raw, err)
			}
		})
	}
}

func TestURLValidator_ValidateResolved_RebindingDefense(t *testing.T) {
	t.Parallel()
	// ValidateResolved is called at every delivery attempt. After
	// the create-time check passes, a malicious DNS server may flip
	// the host's A record to a private IP — ValidateResolved must
	// re-check at delivery time and reject.
	v := NewURLValidator()
	v.Resolver = publicResolver()
	u, err := v.Validate(context.Background(), "https://example.com/x")
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	// Now flip the resolver to return a private IP for the same
	// hostname, and confirm ValidateResolved blocks.
	v.Resolver = &fakeResolver{hosts: map[string][]net.IPAddr{
		u.Hostname(): ipAddrs("10.0.0.1"),
	}}
	if err := v.ValidateResolved(context.Background(), u); !errors.Is(err, ErrURLBlocked) {
		t.Fatalf("ValidateResolved post-rebinding: err=%v want ErrURLBlocked", err)
	}
}

func TestURLValidator_ResolverError(t *testing.T) {
	t.Parallel()
	// When the DNS resolver returns an error (e.g. NXDOMAIN), the
	// URL is rejected — we never deliver to an unresolvable host.
	v := NewURLValidator()
	v.Resolver = &fakeResolver{}
	_, err := v.Validate(context.Background(), "https://does-not-exist.example.com/x")
	if err == nil {
		t.Fatal("expected error on NXDOMAIN, got nil")
	}
	// The exact wrapping varies; just confirm it surfaces as a
	// URL-invalid rather than panicking or returning success.
	_ = url.URL{} // touch url to keep import live across formatting
}

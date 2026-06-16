package setup

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

// TestBlockedDialAddr pins which destinations the setup probe refuses.
// Link-local (the cloud instance-metadata vector at 169.254.169.254),
// multicast, and the unspecified address are blocked; public and
// private-LAN / loopback S3 endpoints (legitimate on-prem MinIO and
// single-box installs) are allowed.
func TestBlockedDialAddr(t *testing.T) {
	cases := []struct {
		ip      string
		blocked bool
	}{
		{"169.254.169.254", true}, // AWS/Azure/GCP instance metadata
		{"169.254.0.1", true},     // link-local generally
		{"0.0.0.0", true},         // unspecified
		{"::", true},              // unspecified v6
		{"fe80::1", true},         // link-local v6
		{"224.0.0.1", true},       // multicast
		{"ff02::1", true},         // link-local multicast v6

		{"54.231.0.1", false},   // public (AWS S3)
		{"10.0.0.5", false},     // private LAN (on-prem MinIO)
		{"172.16.4.4", false},   // private LAN
		{"192.168.1.50", false}, // private LAN
		{"127.0.0.1", false},    // loopback (single-box install)
		{"::1", false},          // loopback v6
	}
	for _, c := range cases {
		ip := net.ParseIP(c.ip)
		if ip == nil {
			t.Fatalf("bad test IP %q", c.ip)
		}
		got := blockedDialAddr(ip) != ""
		if got != c.blocked {
			t.Errorf("blockedDialAddr(%s) blocked=%v, want %v", c.ip, got, c.blocked)
		}
	}
}

// TestGuardedDialContextRejectsLinkLocal verifies the dialer refuses an
// IP-literal in the metadata range before opening any connection, and
// surfaces a clear reason. Using an IP literal keeps the test
// hermetic (no DNS).
func TestGuardedDialContextRejectsLinkLocal(t *testing.T) {
	dial := guardedDialContext(&net.Dialer{Timeout: time.Second})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	conn, err := dial(ctx, "tcp", "169.254.169.254:80")
	if err == nil {
		_ = conn.Close()
		t.Fatal("guarded dialer connected to the instance-metadata address; want refusal")
	}
	if !strings.Contains(err.Error(), "refusing to connect") {
		t.Fatalf("unexpected error %q; want an SSRF refusal", err)
	}
}

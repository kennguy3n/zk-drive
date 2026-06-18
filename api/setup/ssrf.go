package setup

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"
)

// guardedHTTPClient returns an *http.Client whose dialer refuses to
// connect to link-local / metadata / multicast / unspecified addresses
// as a hardening measure. It exists solely for the first-boot setup
// wizard's TestStorage probe, which issues an outbound HeadBucket to an
// endpoint typed by an as-yet-unauthenticated caller. Without a guard
// that pre-completion window is an SSRF lever: an attacker could point
// the endpoint at the cloud instance-metadata service (169.254.169.254
// on AWS/Azure/GCP) and use the differential HeadBucket error to probe
// it. That endpoint — and link-local space generally — is NEVER a
// legitimate S3/zk-object-fabric address.
//
// Deliberately NARROW: RFC-1918 private ranges (10/8, 172.16/12,
// 192.168/16) and loopback are NOT blocked, because a supported SME
// deployment is an on-prem MinIO/Ceph gateway on the operator's own LAN
// (or a single box hosting both the gateway and zk-drive). Blocking
// those would break the very NoOps install path the wizard serves. We
// block only addresses that have no legitimate S3 use, keeping the
// guard a pure security win with zero false positives.
//
// The dialer resolves the host itself and dials the validated IP
// literal, so a name that passes the check cannot be re-resolved to a
// blocked address between the check and the connect (DNS-rebinding
// TOCTOU). Re-validation also runs on every redirect-driven dial.
//
// Proxy is deliberately nil (NOT http.ProxyFromEnvironment): with a
// proxy the transport would open the connection to the proxy and hand
// it the attacker-typed target host, so the proxy — not our dialer —
// would resolve and connect to it, and blockedDialAddr would only ever
// see the (allowed, private-LAN) proxy IP. That silently defeats the
// whole guard whenever HTTP_PROXY/HTTPS_PROXY is set. Forcing a direct
// dial keeps guardedDialContext authoritative over the real endpoint
// IP. This probe only ever targets an S3/Fabric endpoint reachable on
// the operator's own network, so it never needs an egress proxy.
func guardedHTTPClient(timeout time.Duration) *http.Client {
	dialer := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		Proxy:                 nil,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          10,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		DialContext:           guardedDialContext(dialer),
	}
	return &http.Client{Timeout: timeout, Transport: transport}
}

// guardedDialContext wraps dialer.DialContext so the destination IP is
// validated against blockedDialAddr before a connection is opened. It
// resolves the host once and dials the chosen IP directly so the
// connected address is exactly the one that passed validation.
func guardedDialContext(dialer *net.Dialer) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		if len(ips) == 0 {
			return nil, fmt.Errorf("setup: no addresses for host %q", host)
		}
		var firstErr error
		for _, ip := range ips {
			if reason := blockedDialAddr(ip.IP); reason != "" {
				if firstErr == nil {
					firstErr = fmt.Errorf("setup: refusing to connect to %s (%s)", ip.IP, reason)
				}
				continue
			}
			conn, derr := dialer.DialContext(ctx, network, net.JoinHostPort(ip.IP.String(), port))
			if derr != nil {
				if firstErr == nil {
					firstErr = derr
				}
				continue
			}
			return conn, nil
		}
		return nil, firstErr
	}
}

// blockedDialAddr returns a non-empty reason when ip must not be dialed
// by the setup probe. It blocks only addresses with no legitimate S3
// use: link-local (the cloud metadata vector), multicast, and the
// unspecified address. Private/RFC-1918 and loopback are intentionally
// allowed (on-prem MinIO / single-box installs).
func blockedDialAddr(ip net.IP) string {
	switch {
	case ip.IsUnspecified():
		return "unspecified address"
	case ip.IsLinkLocalUnicast():
		return "link-local / instance-metadata range"
	case ip.IsMulticast():
		// Covers global, link-local and interface-local multicast.
		return "multicast address"
	default:
		return ""
	}
}

// zk-drive healthcheck binary — a dependency-free HTTP liveness probe.
//
// The slim server image (Dockerfile.server) is built FROM
// gcr.io/distroless/static-debian12, which ships no shell, no wget, and
// no curl, and the combined image's debian-slim base does not install
// them either. Container-level health checks (ECS task definitions,
// docker-compose `healthcheck:`) therefore cannot shell out to
// `wget`/`curl`. This tiny static binary fills that gap: it performs one
// HTTP GET against the server's /healthz endpoint and maps the result to
// an exit code (0 healthy, 1 unhealthy), which is exactly what those
// orchestrators consume. It keeps parity with the GCP Cloud Run and Helm
// deployments, which use native httpGet probes.
//
// Target URL resolution:
//   - HEALTHCHECK_URL, if set, is used verbatim.
//   - otherwise the port is taken from LISTEN_ADDR (the same var the
//     server binds, default ":8080") and probed on the loopback
//     interface at /healthz.
package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"
)

// probeTimeout bounds the whole check. It is comfortably under the
// shortest health-check interval/timeout configured by the deployments
// (ECS uses timeout=5s) so a hung server reports unhealthy rather than
// stalling the orchestrator's probe.
const probeTimeout = 3 * time.Second

func main() {
	if err := check(); err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck:", err)
		os.Exit(1)
	}
}

func check() error {
	url := healthURL()

	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("build request for %s: %w", url, err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("GET %s: %w", url, err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("GET %s: status %d", url, resp.StatusCode)
	}
	return nil
}

// healthURL resolves the endpoint to probe. An explicit HEALTHCHECK_URL
// wins; otherwise it is derived from LISTEN_ADDR so the probe follows a
// non-default port without extra configuration.
func healthURL() string {
	if u := os.Getenv("HEALTHCHECK_URL"); u != "" {
		return u
	}
	listenAddr := os.Getenv("LISTEN_ADDR")
	if listenAddr == "" {
		listenAddr = ":8080"
	}
	host, port, err := net.SplitHostPort(listenAddr)
	if err != nil {
		// LISTEN_ADDR was not host:port (e.g. a bare port). Fall back to
		// the default port rather than failing the probe on a parse quirk.
		host, port = "", "8080"
	}
	// A wildcard / empty bind host (":8080", "0.0.0.0:8080", "[::]:8080")
	// is reached from inside the container over loopback.
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return fmt.Sprintf("http://%s/healthz", net.JoinHostPort(host, port))
}

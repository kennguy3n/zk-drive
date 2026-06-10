package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHealthURL(t *testing.T) {
	tests := []struct {
		name           string
		healthcheckURL string
		listenAddr     string
		want           string
	}{
		{name: "default when nothing set", want: "http://127.0.0.1:8080/healthz"},
		{name: "explicit URL wins", healthcheckURL: "http://svc:9000/ready", want: "http://svc:9000/ready"},
		{name: "wildcard bind to loopback", listenAddr: ":9090", want: "http://127.0.0.1:9090/healthz"},
		{name: "0.0.0.0 to loopback", listenAddr: "0.0.0.0:8081", want: "http://127.0.0.1:8081/healthz"},
		{name: "ipv6 wildcard to loopback", listenAddr: "[::]:8082", want: "http://127.0.0.1:8082/healthz"},
		{name: "explicit host preserved", listenAddr: "10.0.0.5:8080", want: "http://10.0.0.5:8080/healthz"},
		{name: "bare port falls back to default", listenAddr: "8080", want: "http://127.0.0.1:8080/healthz"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.healthcheckURL != "" {
				t.Setenv("HEALTHCHECK_URL", tc.healthcheckURL)
			} else {
				t.Setenv("HEALTHCHECK_URL", "")
			}
			t.Setenv("LISTEN_ADDR", tc.listenAddr)
			if got := healthURL(); got != tc.want {
				t.Fatalf("healthURL() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestCheck exercises the exit-code contract: a 2xx is healthy (nil), a
// 5xx is unhealthy (error), and an unreachable endpoint is unhealthy.
func TestCheck(t *testing.T) {
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer healthy.Close()
	t.Setenv("HEALTHCHECK_URL", healthy.URL)
	if err := check(); err != nil {
		t.Fatalf("healthy endpoint: unexpected error %v", err)
	}

	unhealthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer unhealthy.Close()
	t.Setenv("HEALTHCHECK_URL", unhealthy.URL)
	if err := check(); err == nil {
		t.Fatal("503 endpoint: expected error, got nil")
	}

	// Point at a closed port (the just-closed server's address).
	closed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	addr := closed.URL
	closed.Close()
	t.Setenv("HEALTHCHECK_URL", addr)
	if err := check(); err == nil {
		t.Fatal("unreachable endpoint: expected error, got nil")
	}
}

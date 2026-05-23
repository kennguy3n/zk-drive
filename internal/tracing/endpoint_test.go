package tracing

import "testing"

// TestSplitOTLPEndpoint covers every operator-facing input form the
// helper is documented to accept. The trailing-slash cases (the
// reason this test exists) pin the behaviour that "https://host:port/"
// must NOT shadow the SDK default "/v1/traces" with urlPath="/".
func TestSplitOTLPEndpoint(t *testing.T) {
	tests := []struct {
		name     string
		raw      string
		wantHost string
		wantPath string
		wantErr  bool
	}{
		{
			name:     "host:port only",
			raw:      "otel-collector:4318",
			wantHost: "otel-collector:4318",
			wantPath: "",
		},
		{
			name:     "https + host:port",
			raw:      "https://otel-collector:4318",
			wantHost: "otel-collector:4318",
			wantPath: "",
		},
		{
			name:     "http + host:port",
			raw:      "http://otel-collector:4318",
			wantHost: "otel-collector:4318",
			wantPath: "",
		},
		{
			name:     "host:port with explicit path",
			raw:      "https://otel-collector:4318/v1/traces",
			wantHost: "otel-collector:4318",
			wantPath: "/v1/traces",
		},
		// Trailing-slash cases — the key invariant. A naive split-
		// then-keep would set urlPath="/" and the OTel SDK would
		// then POST against the collector's root, 404-ing every
		// export.
		{
			name:     "trailing slash on bare host",
			raw:      "https://otlp.honeycomb.io:443/",
			wantHost: "otlp.honeycomb.io:443",
			wantPath: "",
		},
		{
			name:     "trailing slash on scheme-less host:port",
			raw:      "otel-collector:4318/",
			wantHost: "otel-collector:4318",
			wantPath: "",
		},
		{
			name:     "trailing slash on explicit path",
			raw:      "https://otel-collector:4318/v1/traces/",
			wantHost: "otel-collector:4318",
			wantPath: "/v1/traces",
		},
		{
			name:    "empty string",
			raw:     "",
			wantErr: true,
		},
		{
			name:    "scheme without host",
			raw:     "https://",
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			host, urlPath, err := splitOTLPEndpoint(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got host=%q urlPath=%q", host, urlPath)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if host != tc.wantHost {
				t.Errorf("host: got %q, want %q", host, tc.wantHost)
			}
			if urlPath != tc.wantPath {
				t.Errorf("urlPath: got %q, want %q", urlPath, tc.wantPath)
			}
		})
	}
}

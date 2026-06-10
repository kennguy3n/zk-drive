package fabric

import (
	"net/http"
	"testing"
)

// TestNewClientDefaultTransportTunedForHTTP2 verifies that a Client
// built without an explicit *http.Client gets the HTTP/2-capable,
// connection-pooled transport rather than the stdlib defaults (which
// cap idle conns per host at 2 and would re-dial TLS on every burst
// request to zk-object-fabric).
func TestNewClientDefaultTransportTunedForHTTP2(t *testing.T) {
	c := NewClient(ClientConfig{BaseURL: "https://fabric.example.com/"})
	if c.httpc == nil {
		t.Fatal("expected a default *http.Client")
	}
	tr, ok := c.httpc.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("expected *http.Transport, got %T", c.httpc.Transport)
	}
	if !tr.ForceAttemptHTTP2 {
		t.Error("ForceAttemptHTTP2 should be true so calls negotiate h2 over TLS")
	}
	if tr.MaxIdleConnsPerHost != DefaultMaxIdleConnsPerHost {
		t.Errorf("MaxIdleConnsPerHost = %d, want %d", tr.MaxIdleConnsPerHost, DefaultMaxIdleConnsPerHost)
	}
	if tr.MaxIdleConnsPerHost <= http.DefaultMaxIdleConnsPerHost {
		t.Errorf("MaxIdleConnsPerHost (%d) must exceed the stdlib default (%d)", tr.MaxIdleConnsPerHost, http.DefaultMaxIdleConnsPerHost)
	}
}

// TestNewClientHonoursSuppliedHTTPClient ensures a caller-provided
// client is used verbatim (so tests / callers can inject a custom
// transport or timeout without the default being forced on them).
func TestNewClientHonoursSuppliedHTTPClient(t *testing.T) {
	custom := &http.Client{}
	c := NewClient(ClientConfig{BaseURL: "https://fabric.example.com", HTTPClient: custom})
	if c.httpc != custom {
		t.Fatal("supplied *http.Client should be used as-is")
	}
}

// TestNewClientTrimsTrailingSlash pins the base-URL normalisation so
// url.JoinPath in the request builders never produces a double slash.
func TestNewClientTrimsTrailingSlash(t *testing.T) {
	c := NewClient(ClientConfig{BaseURL: "https://fabric.example.com/"})
	if c.baseURL != "https://fabric.example.com" {
		t.Fatalf("baseURL = %q, want trailing slash trimmed", c.baseURL)
	}
}

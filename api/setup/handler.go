// Package setup serves the first-boot guided setup wizard endpoints
// (WS8 8.2):
//
//	GET  /api/setup/status        — what is configured / still missing
//	POST /api/setup/test-storage  — validate S3 credentials before save
//	POST /api/setup/complete      — record wizard completion (admin)
//
// status and test-storage are reachable pre-authentication because the
// wizard runs before the first admin account exists; complete is
// admin-gated (the route group applies the auth + AdminOnly stack in
// cmd/server/main.go). The two public endpoints are self-disabling:
// once setup is complete they refuse to expose detail / run probes, so
// a provisioned (and possibly internet-exposed) install cannot be used
// as an anonymous config-disclosure or SSRF surface.
package setup

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/kennguy3n/zk-drive/api/middleware"
	"github.com/kennguy3n/zk-drive/internal/logging"
	"github.com/kennguy3n/zk-drive/internal/setup"
	"github.com/kennguy3n/zk-drive/internal/storage"
)

// setupService is the subset of *setup.Service the handler uses,
// extracted as an interface so the HTTP layer is unit-testable with a
// fake (no Postgres). The production implementation is *setup.Service.
type setupService interface {
	Status(ctx context.Context) (setup.Status, error)
	IsCompleted(ctx context.Context) (bool, error)
	MarkCompleted(ctx context.Context) error
}

// storageTester abstracts the one storage operation the wizard needs —
// a credential + reachability check — so the handler is unit-testable
// without a live gateway. The production implementation builds a
// storage.Client and calls HealthCheck (a HeadBucket).
type storageTester interface {
	Test(ctx context.Context, cfg storage.Config) error
}

// liveStorageTester is the production storageTester: it constructs a
// real client against the supplied credentials and issues a HeadBucket.
type liveStorageTester struct{}

func (liveStorageTester) Test(ctx context.Context, cfg storage.Config) error {
	client, err := storage.NewClient(cfg)
	if err != nil {
		return err
	}
	return client.HealthCheck(ctx)
}

// Handler serves the setup endpoints.
type Handler struct {
	svc     setupService
	tester  storageTester
	timeout time.Duration
}

// NewHandler constructs a setup Handler. svc must be non-nil; the
// storage tester defaults to the live implementation.
func NewHandler(svc setupService) *Handler {
	return &Handler{
		svc:    svc,
		tester: liveStorageTester{},
		// A connection test must not hang the wizard: a misconfigured
		// or unreachable endpoint should fail fast with a clear error
		// the operator can act on.
		timeout: 8 * time.Second,
	}
}

// withTester swaps the storage tester. Used by tests.
func (h *Handler) withTester(t storageTester) *Handler {
	h.tester = t
	return h
}

// Status serves GET /api/setup/status.
func (h *Handler) Status(w http.ResponseWriter, r *http.Request) {
	st, err := h.svc.Status(r.Context())
	if err != nil {
		logging.FromContext(r.Context()).Error("setup status failed", "err", err)
		middleware.WriteJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "failed to read setup status",
		})
		return
	}
	middleware.WriteJSON(w, http.StatusOK, st)
}

// testStorageRequest is the POST /api/setup/test-storage body. Region
// is optional (defaults to storage.DefaultRegion).
type testStorageRequest struct {
	Endpoint  string `json:"endpoint"`
	Bucket    string `json:"bucket"`
	AccessKey string `json:"access_key"`
	SecretKey string `json:"secret_key"`
	Region    string `json:"region"`
}

// testStorageResponse reports the outcome. On failure, Error carries a
// short, non-sensitive reason for the operator (never the credentials).
type testStorageResponse struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// TestStorage serves POST /api/setup/test-storage: it validates the
// S3/Fabric credentials the operator typed into step 2 by issuing a
// real HeadBucket, so they get a green check before the values are
// persisted anywhere.
//
// Self-disabling: once setup is complete this returns 403. Allowing an
// anonymous caller to drive arbitrary outbound HeadBucket requests on a
// live install is an SSRF vector; on a fresh, not-yet-configured box
// (the only time the wizard runs) there is no such exposure.
func (h *Handler) TestStorage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	completed, err := h.svc.IsCompleted(ctx)
	if err != nil {
		logging.FromContext(ctx).Error("setup test-storage completion check failed", "err", err)
		middleware.WriteJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "failed to read setup status",
		})
		return
	}
	if completed {
		middleware.WriteJSON(w, http.StatusForbidden, map[string]string{
			"error": "setup already completed",
		})
		return
	}

	var req testStorageRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		middleware.WriteJSON(w, http.StatusBadRequest, map[string]string{
			"error": "invalid request body",
		})
		return
	}

	cfg := storage.Config{
		Endpoint:  strings.TrimSpace(req.Endpoint),
		Bucket:    strings.TrimSpace(req.Bucket),
		AccessKey: strings.TrimSpace(req.AccessKey),
		SecretKey: req.SecretKey,
		Region:    strings.TrimSpace(req.Region),
	}
	if cfg.Endpoint == "" || cfg.Bucket == "" || cfg.AccessKey == "" || cfg.SecretKey == "" {
		middleware.WriteJSON(w, http.StatusOK, testStorageResponse{
			OK:    false,
			Error: "endpoint, bucket, access_key and secret_key are all required",
		})
		return
	}

	tctx, cancel := context.WithTimeout(ctx, h.timeout)
	defer cancel()
	if err := h.tester.Test(tctx, cfg); err != nil {
		// A failed connection test is an expected, user-correctable
		// outcome — return 200 with ok=false so the frontend renders
		// an inline error rather than treating it as a server fault.
		// The error string is the SDK's reason (bad credentials, no
		// such bucket, connection refused); it never contains the
		// secret key.
		middleware.WriteJSON(w, http.StatusOK, testStorageResponse{
			OK:    false,
			Error: storageTestError(err),
		})
		return
	}
	middleware.WriteJSON(w, http.StatusOK, testStorageResponse{OK: true})
}

// Complete serves POST /api/setup/complete (admin-only). Marking is
// idempotent.
func (h *Handler) Complete(w http.ResponseWriter, r *http.Request) {
	if err := h.svc.MarkCompleted(r.Context()); err != nil {
		logging.FromContext(r.Context()).Error("setup complete failed", "err", err)
		middleware.WriteJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "failed to record setup completion",
		})
		return
	}
	middleware.WriteJSON(w, http.StatusOK, map[string]bool{"setup_completed": true})
}

// storageTestError normalises an error from the storage probe into a
// short operator-facing message. It deliberately returns the raw SDK
// message (which is safe — it never echoes the secret key) but trims
// it so a multi-line AWS error does not bloat the JSON.
func storageTestError(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	if i := strings.IndexByte(msg, '\n'); i >= 0 {
		msg = msg[:i]
	}
	const max = 300
	if len(msg) > max {
		msg = msg[:max]
	}
	return msg
}

// ensure the live tester satisfies the interface at compile time.
var _ storageTester = liveStorageTester{}

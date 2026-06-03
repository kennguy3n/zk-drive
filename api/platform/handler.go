// Package platform serves the control-plane "super-API" mounted at
// /api/platform. Unlike the tenant-facing API it is authenticated by a
// platform API key (see internal/platform.APIKeyStore) rather than a
// workspace JWT, and every endpoint is gated on a coarse capability via
// middleware.RequirePlatformPermission. It exposes fleet-wide tenant
// management: provisioning, suspension, usage reporting, billing
// reconciliation, usage-alert rules, and API-key administration.
package platform

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/kennguy3n/zk-drive/api/middleware"
	platformsvc "github.com/kennguy3n/zk-drive/internal/platform"
)

// Handler serves the /api/platform routes. Both dependencies are
// required: svc performs the control-plane operations and keys backs
// both the api-keys endpoints and (via AuthenticateKey) the
// PlatformAuth middleware.
type Handler struct {
	svc  *platformsvc.PlatformService
	keys *platformsvc.APIKeyStore
}

// NewHandler constructs a platform Handler.
func NewHandler(svc *platformsvc.PlatformService, keys *platformsvc.APIKeyStore) *Handler {
	return &Handler{svc: svc, keys: keys}
}

// AuthenticateKey implements middleware.PlatformAuthenticator by
// delegating to the API-key store. The returned *platform.APIKey
// satisfies middleware.PlatformPrincipal via its HasPermission method.
// A nil match is returned as (nil, err) so the middleware emits 401.
func (h *Handler) AuthenticateKey(ctx context.Context, presented string) (middleware.PlatformPrincipal, error) {
	key, err := h.keys.Authenticate(ctx, presented)
	if err != nil {
		return nil, err
	}
	return key, nil
}

// RegisterRoutes mounts every platform endpoint onto r. The caller is
// expected to mount this under /api/platform behind PlatformAuth; each
// route additionally requires a specific capability.
func (h *Handler) RegisterRoutes(r chi.Router) {
	requireRead := middleware.RequirePlatformPermission(platformsvc.PermTenantRead)
	requireWrite := middleware.RequirePlatformPermission(platformsvc.PermTenantWrite)
	requireSuspend := middleware.RequirePlatformPermission(platformsvc.PermTenantSuspend)
	requireReconcile := middleware.RequirePlatformPermission(platformsvc.PermBillingReconcile)
	requireAlertsRead := middleware.RequirePlatformPermission(platformsvc.PermAlertsRead)
	requireAlertsWrite := middleware.RequirePlatformPermission(platformsvc.PermAlertsWrite)
	requireKeys := middleware.RequirePlatformPermission(platformsvc.PermKeysManage)

	r.With(requireWrite).Post("/workspaces", h.ProvisionWorkspace)
	r.With(requireRead).Get("/workspaces", h.ListWorkspaces)
	r.With(requireRead).Get("/workspaces/{id}", h.GetWorkspace)
	r.With(requireSuspend).Post("/workspaces/{id}/suspend", h.SuspendWorkspace)
	r.With(requireSuspend).Post("/workspaces/{id}/resume", h.ResumeWorkspace)
	r.With(requireRead).Get("/workspaces/{id}/usage", h.GetWorkspaceUsage)

	r.With(requireReconcile).Post("/billing/reconcile", h.ReconcileBilling)

	r.With(requireAlertsWrite).Post("/alerts/evaluate", h.EvaluateAlerts)
	r.With(requireAlertsRead).Get("/alerts/rules", h.ListAlertRules)
	r.With(requireAlertsWrite).Post("/alerts/rules", h.CreateAlertRule)
	r.With(requireAlertsWrite).Delete("/alerts/rules/{id}", h.DeleteAlertRule)

	r.With(requireKeys).Post("/api-keys", h.CreateAPIKey)
	r.With(requireKeys).Get("/api-keys", h.ListAPIKeys)
	r.With(requireKeys).Delete("/api-keys/{id}", h.RevokeAPIKey)
}

// ---- workspaces -----------------------------------------------------

type provisionRequest struct {
	Name         string `json:"name"`
	OwnerEmail   string `json:"owner_email"`
	Tier         string `json:"tier"`
	PlacementRef string `json:"placement_ref"`
}

// ProvisionWorkspace handles POST /api/platform/workspaces.
func (h *Handler) ProvisionWorkspace(w http.ResponseWriter, r *http.Request) {
	var req provisionRequest
	if !decode(w, r, &req) {
		return
	}
	ws, err := h.svc.ProvisionWorkspace(r.Context(), req.Name, req.OwnerEmail, req.Tier, req.PlacementRef)
	if err != nil {
		h.respondErr(w, r, "provision workspace", err)
		return
	}
	middleware.WriteJSON(w, http.StatusCreated, ws)
}

type listWorkspacesResponse struct {
	Workspaces []platformsvc.WorkspaceSummary `json:"workspaces"`
	Total      int                            `json:"total"`
	Limit      int                            `json:"limit"`
	Offset     int                            `json:"offset"`
}

// ListWorkspaces handles GET /api/platform/workspaces with filters.
func (h *Handler) ListWorkspaces(w http.ResponseWriter, r *http.Request) {
	filters, err := parseListFilters(r)
	if err != nil {
		middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeValidation, err.Error())
		return
	}
	summaries, total, err := h.svc.ListWorkspaces(r.Context(), filters)
	if err != nil {
		h.respondErr(w, r, "list workspaces", err)
		return
	}
	middleware.WriteJSON(w, http.StatusOK, listWorkspacesResponse{
		Workspaces: summaries,
		Total:      total,
		Limit:      filters.Limit,
		Offset:     filters.Offset,
	})
}

// GetWorkspace handles GET /api/platform/workspaces/{id}.
func (h *Handler) GetWorkspace(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	summary, err := h.svc.GetWorkspace(r.Context(), id)
	if err != nil {
		h.respondErr(w, r, "get workspace", err)
		return
	}
	middleware.WriteJSON(w, http.StatusOK, summary)
}

type suspendRequest struct {
	Reason string `json:"reason"`
}

// SuspendWorkspace handles POST /api/platform/workspaces/{id}/suspend.
func (h *Handler) SuspendWorkspace(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	var req suspendRequest
	// Body is optional: a reason-less suspension is valid. Use > 0 (not
	// != 0) because net/http reports ContentLength as -1 for requests
	// without a Content-Length header (e.g. HTTP/1.1 chunked), which
	// would otherwise force a decode of an empty body and 400.
	if r.ContentLength > 0 {
		if !decode(w, r, &req) {
			return
		}
	}
	if err := h.svc.SuspendWorkspace(r.Context(), id, req.Reason); err != nil {
		h.respondErr(w, r, "suspend workspace", err)
		return
	}
	middleware.WriteJSON(w, http.StatusOK, map[string]any{"id": id, "suspended": true})
}

// ResumeWorkspace handles POST /api/platform/workspaces/{id}/resume.
func (h *Handler) ResumeWorkspace(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	if err := h.svc.ResumeWorkspace(r.Context(), id); err != nil {
		h.respondErr(w, r, "resume workspace", err)
		return
	}
	middleware.WriteJSON(w, http.StatusOK, map[string]any{"id": id, "suspended": false})
}

// GetWorkspaceUsage handles GET /api/platform/workspaces/{id}/usage.
func (h *Handler) GetWorkspaceUsage(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	report, err := h.svc.GetWorkspaceUsage(r.Context(), id)
	if err != nil {
		h.respondErr(w, r, "get workspace usage", err)
		return
	}
	middleware.WriteJSON(w, http.StatusOK, report)
}

// ---- billing --------------------------------------------------------

// ReconcileBilling handles POST /api/platform/billing/reconcile.
func (h *Handler) ReconcileBilling(w http.ResponseWriter, r *http.Request) {
	report, err := h.svc.BulkReconcileBilling(r.Context())
	if err != nil {
		h.respondErr(w, r, "reconcile billing", err)
		return
	}
	middleware.WriteJSON(w, http.StatusOK, report)
}

// ---- alerts ---------------------------------------------------------

// EvaluateAlerts handles POST /api/platform/alerts/evaluate.
func (h *Handler) EvaluateAlerts(w http.ResponseWriter, r *http.Request) {
	firings, err := h.svc.EvaluateUsageAlerts(r.Context())
	if err != nil {
		h.respondErr(w, r, "evaluate alerts", err)
		return
	}
	middleware.WriteJSON(w, http.StatusOK, map[string]any{"firings": firings, "count": len(firings)})
}

// ListAlertRules handles GET /api/platform/alerts/rules.
func (h *Handler) ListAlertRules(w http.ResponseWriter, r *http.Request) {
	rules, err := h.svc.ListAlertRules(r.Context())
	if err != nil {
		h.respondErr(w, r, "list alert rules", err)
		return
	}
	middleware.WriteJSON(w, http.StatusOK, map[string]any{"rules": rules})
}

type createAlertRuleRequest struct {
	WorkspaceID *uuid.UUID `json:"workspace_id"`
	Metric      string     `json:"metric"`
	Threshold   float64    `json:"threshold"`
	Operator    string     `json:"operator"`
	WebhookURL  string     `json:"webhook_url"`
	Email       string     `json:"email"`
}

// CreateAlertRule handles POST /api/platform/alerts/rules.
func (h *Handler) CreateAlertRule(w http.ResponseWriter, r *http.Request) {
	var req createAlertRuleRequest
	if !decode(w, r, &req) {
		return
	}
	rule, err := h.svc.CreateAlertRule(r.Context(), platformsvc.AlertRule{
		WorkspaceID: req.WorkspaceID,
		Metric:      req.Metric,
		Threshold:   req.Threshold,
		Operator:    req.Operator,
		WebhookURL:  req.WebhookURL,
		Email:       req.Email,
	})
	if err != nil {
		h.respondErr(w, r, "create alert rule", err)
		return
	}
	middleware.WriteJSON(w, http.StatusCreated, rule)
}

// DeleteAlertRule handles DELETE /api/platform/alerts/rules/{id}.
func (h *Handler) DeleteAlertRule(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	if err := h.svc.DeleteAlertRule(r.Context(), id); err != nil {
		h.respondErr(w, r, "delete alert rule", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- api keys -------------------------------------------------------

type createAPIKeyRequest struct {
	Label       string   `json:"label"`
	Permissions []string `json:"permissions"`
}

type createAPIKeyResponse struct {
	Key    string              `json:"key"`
	APIKey *platformsvc.APIKey `json:"api_key"`
}

// CreateAPIKey handles POST /api/platform/api-keys. The plaintext key
// is returned exactly once in this response and is never retrievable
// again.
func (h *Handler) CreateAPIKey(w http.ResponseWriter, r *http.Request) {
	var req createAPIKeyRequest
	if !decode(w, r, &req) {
		return
	}
	key, plaintext, err := h.keys.Create(r.Context(), req.Label, req.Permissions)
	if err != nil {
		h.respondErr(w, r, "create api key", err)
		return
	}
	middleware.WriteJSON(w, http.StatusCreated, createAPIKeyResponse{Key: plaintext, APIKey: key})
}

// ListAPIKeys handles GET /api/platform/api-keys. Keys are returned
// redacted (metadata only — never the hash or plaintext).
func (h *Handler) ListAPIKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := h.keys.List(r.Context())
	if err != nil {
		h.respondErr(w, r, "list api keys", err)
		return
	}
	middleware.WriteJSON(w, http.StatusOK, map[string]any{"api_keys": keys})
}

// RevokeAPIKey handles DELETE /api/platform/api-keys/{id}.
func (h *Handler) RevokeAPIKey(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	if err := h.keys.Revoke(r.Context(), id); err != nil {
		h.respondErr(w, r, "revoke api key", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- helpers --------------------------------------------------------

// decode reads a JSON body into dst, writing a 400 and returning false
// on malformed input.
func decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeMalformedJSON, "malformed JSON body")
		return false
	}
	return true
}

// parseID parses the {id} path param as a UUID, writing a 400 and
// returning false when invalid.
func parseID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeValidation, "invalid id")
		return uuid.Nil, false
	}
	return id, true
}

// parseListFilters builds ListFilters from the request query string.
func parseListFilters(r *http.Request) (platformsvc.ListFilters, error) {
	q := r.URL.Query()
	f := platformsvc.ListFilters{Tier: strings.TrimSpace(q.Get("tier"))}

	if v := strings.TrimSpace(q.Get("suspended")); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return f, errors.New("invalid suspended: want true or false")
		}
		f.Suspended = &b
	}
	if v := strings.TrimSpace(q.Get("min_storage_percent")); v != "" {
		n, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return f, errors.New("invalid min_storage_percent")
		}
		f.MinStoragePercent = n
	}
	if v := strings.TrimSpace(q.Get("max_storage_percent")); v != "" {
		n, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return f, errors.New("invalid max_storage_percent")
		}
		f.MaxStoragePercent = n
	}
	if v := strings.TrimSpace(q.Get("created_after")); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return f, errors.New("invalid created_after: want RFC3339")
		}
		f.CreatedAfter = &t
	}
	if v := strings.TrimSpace(q.Get("created_before")); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return f, errors.New("invalid created_before: want RFC3339")
		}
		f.CreatedBefore = &t
	}
	if v := strings.TrimSpace(q.Get("limit")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return f, errors.New("invalid limit")
		}
		f.Limit = n
	}
	if v := strings.TrimSpace(q.Get("offset")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return f, errors.New("invalid offset")
		}
		f.Offset = n
	}

	// Normalise so the echoed limit/offset in the response match what
	// the service actually applied.
	if f.Limit <= 0 {
		f.Limit = platformsvc.DefaultListLimit
	}
	if f.Limit > platformsvc.MaxListLimit {
		f.Limit = platformsvc.MaxListLimit
	}
	if f.Offset < 0 {
		f.Offset = 0
	}
	return f, nil
}

// respondErr maps a service error to the appropriate HTTP status and
// stable error code. op is a short operation label (e.g. "provision
// workspace") used by RespondInternalError to log unexpected 500s with
// request context (request_id, path, method) while keeping err out of
// the client-facing body.
func (h *Handler) respondErr(w http.ResponseWriter, r *http.Request, op string, err error) {
	switch {
	case errors.Is(err, platformsvc.ErrNotFound):
		middleware.RespondError(w, http.StatusNotFound, middleware.ErrCodeNotFound, "not found")
	case errors.Is(err, platformsvc.ErrInvalidArgument):
		middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeValidation, err.Error())
	default:
		middleware.RespondInternalError(w, r, op, err)
	}
}

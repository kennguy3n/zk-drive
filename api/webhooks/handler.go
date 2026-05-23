// Package webhooks serves the workspace-admin REST surface for
// outbound webhook subscriptions (WS-24). Routes:
//
//	POST   /api/workspaces/{workspaceID}/webhooks
//	GET    /api/workspaces/{workspaceID}/webhooks
//	GET    /api/workspaces/{workspaceID}/webhooks/{id}
//	DELETE /api/workspaces/{workspaceID}/webhooks/{id}
//	GET    /api/workspaces/{workspaceID}/webhooks/{id}/deliveries
//	POST   /api/workspaces/{workspaceID}/webhooks/{id}/test
//	POST   /api/workspaces/{workspaceID}/webhooks/{id}/resume
//
// All routes are admin-only. The handler delegates persistence to
// internal/webhooks.Repository (pgx-backed in production) and
// publishing to internal/webhooks.Publisher.
package webhooks

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/kennguy3n/zk-drive/api/middleware"
	"github.com/kennguy3n/zk-drive/internal/audit"
	"github.com/kennguy3n/zk-drive/internal/logging"
	"github.com/kennguy3n/zk-drive/internal/user"
	"github.com/kennguy3n/zk-drive/internal/webhooks"
)

// Handler is the chi-compatible HTTP handler. Constructed with the
// repository (required) plus optional publisher + audit / URL
// validator. A nil publisher disables the "test" endpoint (returns
// 503 Service Unavailable); a nil audit service silently drops audit
// trail entries; a nil validator falls back to the production
// default.
type Handler struct {
	repo      webhooks.Repository
	publisher *webhooks.Publisher
	validator *webhooks.URLValidator
	audit     *audit.Service
}

// NewHandler constructs a Handler. The repository is required;
// publisher / audit / validator are optional and can be wired via
// the With* methods.
func NewHandler(repo webhooks.Repository) *Handler {
	return &Handler{repo: repo, validator: webhooks.NewURLValidator()}
}

// WithPublisher wires the JetStream publisher so the POST /test
// endpoint can enqueue synthetic events. A nil publisher disables
// the test endpoint.
func (h *Handler) WithPublisher(p *webhooks.Publisher) *Handler {
	h.publisher = p
	return h
}

// WithValidator overrides the default URLValidator. Used by tests to
// inject a deterministic resolver; production callers can leave the
// default in place.
func (h *Handler) WithValidator(v *webhooks.URLValidator) *Handler {
	if v != nil {
		h.validator = v
	}
	return h
}

// WithAudit wires an audit-log service so create / delete / pause
// operations on subscriptions land in audit_log. Nil-safe — when no
// audit service is wired, the subscription-management routes work
// unchanged but no audit row is written.
func (h *Handler) WithAudit(s *audit.Service) *Handler {
	h.audit = s
	return h
}

// RegisterRoutes mounts the routes on r. The caller is responsible
// for ensuring r has already had the auth + workspace-context
// middlewares applied; this handler enforces the admin role itself
// because membership alone is not sufficient.
func (h *Handler) RegisterRoutes(r chi.Router) {
	r.Post("/", h.Create)
	r.Get("/", h.List)
	r.Get("/{id}", h.Get)
	r.Delete("/{id}", h.Delete)
	r.Get("/{id}/deliveries", h.ListDeliveries)
	r.Post("/{id}/test", h.Test)
	r.Post("/{id}/resume", h.Resume)
}

// createRequest is the wire payload for POST /webhooks.
type createRequest struct {
	URL         string `json:"url"`
	EventType   string `json:"event_type"`
	Description string `json:"description,omitempty"`
}

// subscriptionView is the wire shape returned to admins. Mirrors
// the Subscription struct but with Secret zeroed UNLESS this is the
// response to a create (which is the only point at which the admin
// can capture the secret).
type subscriptionView struct {
	ID                  uuid.UUID `json:"id"`
	WorkspaceID         uuid.UUID `json:"workspace_id"`
	URL                 string    `json:"url"`
	EventType           string    `json:"event_type"`
	Description         string    `json:"description,omitempty"`
	Secret              string    `json:"secret,omitempty"`
	Active              bool      `json:"active"`
	ConsecutiveFailures int       `json:"consecutive_failures"`
	LastSucceededAt     *string   `json:"last_succeeded_at,omitempty"`
	LastAttemptedAt     *string   `json:"last_attempted_at,omitempty"`
	AutoPausedAt        *string   `json:"auto_paused_at,omitempty"`
	CreatedAt           string    `json:"created_at"`
	UpdatedAt           string    `json:"updated_at"`
}

// deliveryView is the wire shape for the per-delivery rows.
type deliveryView struct {
	ID            uuid.UUID `json:"id"`
	EventID       uuid.UUID `json:"event_id"`
	EventType     string    `json:"event_type"`
	AttemptNumber int       `json:"attempt_number"`
	Outcome       string    `json:"outcome"`
	StatusCode    int       `json:"status_code"`
	ResponseBody  string    `json:"response_body,omitempty"`
	ErrorMessage  string    `json:"error_message,omitempty"`
	DurationMs    int       `json:"duration_ms"`
	AttemptedAt   string    `json:"attempted_at"`
	NextRetryAt   *string   `json:"next_retry_at,omitempty"`
}

// toView projects a Subscription into the wire shape. includeSecret
// is true ONLY for the create response so a re-read via GET / LIST
// never returns the secret.
func toView(s *webhooks.Subscription, includeSecret bool) subscriptionView {
	v := subscriptionView{
		ID:                  s.ID,
		WorkspaceID:         s.WorkspaceID,
		URL:                 s.URL,
		EventType:           string(s.EventType),
		Description:         s.Description,
		Active:              s.Active,
		ConsecutiveFailures: s.ConsecutiveFailures,
		CreatedAt:           s.CreatedAt.UTC().Format("2006-01-02T15:04:05.000000Z"),
		UpdatedAt:           s.UpdatedAt.UTC().Format("2006-01-02T15:04:05.000000Z"),
	}
	if includeSecret {
		v.Secret = s.Secret
	}
	if s.LastSucceededAt != nil {
		t := s.LastSucceededAt.UTC().Format("2006-01-02T15:04:05.000000Z")
		v.LastSucceededAt = &t
	}
	if s.LastAttemptedAt != nil {
		t := s.LastAttemptedAt.UTC().Format("2006-01-02T15:04:05.000000Z")
		v.LastAttemptedAt = &t
	}
	if s.AutoPausedAt != nil {
		t := s.AutoPausedAt.UTC().Format("2006-01-02T15:04:05.000000Z")
		v.AutoPausedAt = &t
	}
	return v
}

func toDeliveryView(d *webhooks.Delivery) deliveryView {
	v := deliveryView{
		ID:            d.ID,
		EventID:       d.EventID,
		EventType:     string(d.EventType),
		AttemptNumber: d.AttemptNumber,
		Outcome:       string(d.Outcome),
		StatusCode:    d.StatusCode,
		ResponseBody:  d.ResponseBody,
		ErrorMessage:  d.ErrorMessage,
		DurationMs:    d.DurationMs,
		AttemptedAt:   d.AttemptedAt.UTC().Format("2006-01-02T15:04:05.000000Z"),
	}
	if d.NextRetryAt != nil {
		t := d.NextRetryAt.UTC().Format("2006-01-02T15:04:05.000000Z")
		v.NextRetryAt = &t
	}
	return v
}

// requireAdmin returns true when the caller has the admin role; on
// false it has already responded with 403 / 401 so the caller just
// has to return.
func (h *Handler) requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	role, ok := middleware.RoleFromContext(r.Context())
	if !ok {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return false
	}
	if role != user.RoleAdmin {
		http.Error(w, "admin role required", http.StatusForbidden)
		return false
	}
	return true
}

// Create POST /api/workspaces/{ws}/webhooks
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	workspaceID, ok := middleware.WorkspaceIDFromContext(r.Context())
	if !ok {
		http.Error(w, "workspace context missing", http.StatusBadRequest)
		return
	}
	var req createRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json body", http.StatusBadRequest)
		return
	}
	req.URL = strings.TrimSpace(req.URL)
	req.EventType = webhooks.NormaliseEventType(req.EventType)
	req.Description = strings.TrimSpace(req.Description)
	if req.URL == "" {
		http.Error(w, "url is required", http.StatusBadRequest)
		return
	}
	if !webhooks.IsValidEventType(req.EventType) {
		http.Error(w, "unknown event_type; see /api/workspaces/{ws}/webhooks/event-types", http.StatusBadRequest)
		return
	}
	// SSRF validation at create-time. The same Validator is re-run
	// at every delivery attempt as the DNS-rebinding defence.
	if _, err := h.validator.Validate(r.Context(), req.URL); err != nil {
		http.Error(w, "url invalid: "+err.Error(), http.StatusBadRequest)
		return
	}
	actorID, _ := middleware.UserIDFromContext(r.Context())
	sub := &webhooks.Subscription{
		WorkspaceID: workspaceID,
		CreatedBy:   actorID,
		URL:         req.URL,
		EventType:   webhooks.EventType(req.EventType),
		Description: req.Description,
	}
	if err := h.repo.Create(r.Context(), sub); err != nil {
		if errors.Is(err, webhooks.ErrSubscriptionCapReached) {
			http.Error(w, "subscription cap reached for this workspace", http.StatusConflict)
			return
		}
		writeServerError(r.Context(), w, "create subscription", err)
		return
	}
	if h.audit != nil {
		subID := sub.ID
		h.audit.LogAction(r.Context(), workspaceID, &actorID, audit.ActionWebhookSubscriptionCreate, "webhook_subscription", &subID, r, map[string]any{
			"event_type": string(sub.EventType),
			"url":        sub.URL,
		})
	}
	// Secret is included in the response ONLY this once. The admin
	// captures it now; subsequent GET/LIST never return it.
	writeJSON(w, http.StatusCreated, toView(sub, true))
}

// List GET /api/workspaces/{ws}/webhooks
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	workspaceID, _ := middleware.WorkspaceIDFromContext(r.Context())
	subs, err := h.repo.List(r.Context(), workspaceID)
	if err != nil {
		writeServerError(r.Context(), w, "list subscriptions", err)
		return
	}
	out := make([]subscriptionView, 0, len(subs))
	for _, s := range subs {
		out = append(out, toView(s, false))
	}
	writeJSON(w, http.StatusOK, map[string]any{"subscriptions": out})
}

// Get GET /api/workspaces/{ws}/webhooks/{id}
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	workspaceID, _ := middleware.WorkspaceIDFromContext(r.Context())
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	sub, err := h.repo.GetByID(r.Context(), workspaceID, id)
	if errors.Is(err, webhooks.ErrSubscriptionNotFound) {
		http.Error(w, "subscription not found", http.StatusNotFound)
		return
	}
	if err != nil {
		writeServerError(r.Context(), w, "get subscription", err)
		return
	}
	writeJSON(w, http.StatusOK, toView(sub, false))
}

// Delete DELETE /api/workspaces/{ws}/webhooks/{id}
func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	workspaceID, _ := middleware.WorkspaceIDFromContext(r.Context())
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	if err := h.repo.Delete(r.Context(), workspaceID, id); err != nil {
		if errors.Is(err, webhooks.ErrSubscriptionNotFound) {
			http.Error(w, "subscription not found", http.StatusNotFound)
			return
		}
		writeServerError(r.Context(), w, "delete subscription", err)
		return
	}
	if h.audit != nil {
		actorID, _ := middleware.UserIDFromContext(r.Context())
		subID := id
		h.audit.LogAction(r.Context(), workspaceID, &actorID, audit.ActionWebhookSubscriptionDelete, "webhook_subscription", &subID, r, nil)
	}
	w.WriteHeader(http.StatusNoContent)
}

// ListDeliveries GET /api/workspaces/{ws}/webhooks/{id}/deliveries
func (h *Handler) ListDeliveries(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	workspaceID, _ := middleware.WorkspaceIDFromContext(r.Context())
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	limit := 100
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		if n, perr := strconv.Atoi(raw); perr == nil && n > 0 && n <= 500 {
			limit = n
		}
	}
	deliveries, err := h.repo.ListDeliveries(r.Context(), workspaceID, id, limit)
	if err != nil {
		writeServerError(r.Context(), w, "list deliveries", err)
		return
	}
	out := make([]deliveryView, 0, len(deliveries))
	for _, d := range deliveries {
		out = append(out, toDeliveryView(d))
	}
	writeJSON(w, http.StatusOK, map[string]any{"deliveries": out})
}

// Test POST /api/workspaces/{ws}/webhooks/{id}/test
// Enqueues a synthetic event onto the JetStream subject so the
// admin can verify connectivity end-to-end (subscription -> worker
// -> subscriber endpoint -> response captured in delivery history).
func (h *Handler) Test(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	if h.publisher == nil {
		http.Error(w, "webhook publisher not configured (NATS unavailable)", http.StatusServiceUnavailable)
		return
	}
	workspaceID, _ := middleware.WorkspaceIDFromContext(r.Context())
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	sub, err := h.repo.GetByID(r.Context(), workspaceID, id)
	if errors.Is(err, webhooks.ErrSubscriptionNotFound) {
		http.Error(w, "subscription not found", http.StatusNotFound)
		return
	}
	if err != nil {
		writeServerError(r.Context(), w, "get subscription", err)
		return
	}
	// Build a synthetic event of the subscription's event_type so
	// the worker's fan-out logic matches a real production
	// delivery. The Data payload carries a "test": true marker
	// subscribers can recognise.
	actorID, _ := middleware.UserIDFromContext(r.Context())
	raw, _ := json.Marshal(map[string]any{"test": true, "subscription_id": sub.ID})
	ev := webhooks.NewEvent(sub.EventType, workspaceID, &actorID, raw)
	if err := h.publisher.Publish(r.Context(), ev); err != nil {
		writeServerError(r.Context(), w, "publish test event", err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"event_id":   ev.ID,
		"event_type": string(sub.EventType),
		"status":     "enqueued",
	})
}

// Resume POST /api/workspaces/{ws}/webhooks/{id}/resume
// Re-activates a subscription that has been auto-paused after the
// AutoPauseThreshold consecutive failures. Resets consecutive_failures
// and clears auto_paused_at.
func (h *Handler) Resume(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	workspaceID, _ := middleware.WorkspaceIDFromContext(r.Context())
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	if err := h.repo.SetActive(r.Context(), workspaceID, id, true); err != nil {
		if errors.Is(err, webhooks.ErrSubscriptionNotFound) {
			http.Error(w, "subscription not found", http.StatusNotFound)
			return
		}
		writeServerError(r.Context(), w, "resume subscription", err)
		return
	}
	if h.audit != nil {
		actorID, _ := middleware.UserIDFromContext(r.Context())
		subID := id
		h.audit.LogAction(r.Context(), workspaceID, &actorID, audit.ActionWebhookSubscriptionResume, "webhook_subscription", &subID, r, nil)
	}
	w.WriteHeader(http.StatusNoContent)
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeServerError(ctx context.Context, w http.ResponseWriter, op string, err error) {
	logging.FromContext(ctx).Error("webhooks handler "+op, "err", err)
	http.Error(w, "internal server error", http.StatusInternalServerError)
}

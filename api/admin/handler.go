// Package admin serves the admin-only HTTP endpoints: audit log,
// retention policy CRUD, user management, and workspace storage
// stats. All routes require the admin role — enforced by the
// middleware.AdminOnly wrapper in cmd/server/main.go.
package admin

import (
	"context"
	"encoding/json"
	"errors"

	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/kennguy3n/zk-drive/internal/logging"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/zk-drive/api/middleware"
	"github.com/kennguy3n/zk-drive/internal/audit"
	"github.com/kennguy3n/zk-drive/internal/billing"
	cryptopkg "github.com/kennguy3n/zk-drive/internal/crypto"
	"github.com/kennguy3n/zk-drive/internal/fabric"
	"github.com/kennguy3n/zk-drive/internal/retention"
	"github.com/kennguy3n/zk-drive/internal/storage"
	"github.com/kennguy3n/zk-drive/internal/user"
	"github.com/kennguy3n/zk-drive/internal/webhooks"
	"github.com/kennguy3n/zk-drive/internal/workspace"
)

// cryptoValidateCMKURI is a thin alias so the handler stays
// readable; the actual validation lives in the crypto package
// alongside the ModeKMS / scheme prefix constants.
func cryptoValidateCMKURI(uri string) error { return cryptopkg.ValidateCMKURI(uri) }

// FabricClient is the subset of fabric.Client the admin handler
// needs. Wrapping the upstream client in an interface keeps the
// handler unit-testable with an in-memory fake.
type FabricClient interface {
	GetPlacement(ctx context.Context, tenantID string) (*fabric.Policy, error)
	PutPlacement(ctx context.Context, tenantID string, p *fabric.Policy) error
	PutCMK(ctx context.Context, tenantID, cmkURI string) error
}

// Handler serves admin HTTP endpoints. All service dependencies are
// optional: when a service is nil the corresponding route returns 501
// so the rest of the admin surface keeps functioning.
type Handler struct {
	pool         *pgxpool.Pool
	users        *user.Service
	workspaces   *workspace.Service
	audit        *audit.Service
	retention    *retention.Service
	billing      *billing.Service
	stripe       *billing.StripeService
	fabric       FabricClient
	provisioner  *fabric.Provisioner
	storeFactory *storage.ClientFactory
	webhooks     MemberEventPublisher
}

// NewHandler constructs a Handler. Pass nil for services that are
// not wired yet; the related routes will respond 501 Not Implemented.
func NewHandler(pool *pgxpool.Pool, users *user.Service, aud *audit.Service, ret *retention.Service) *Handler {
	return &Handler{pool: pool, users: users, audit: aud, retention: ret}
}

// WithWorkspaces wires the workspace service so the MFA policy
// toggle endpoint (PATCH /workspace/mfa-policy) can flip the
// workspaces.mfa_required column. Optional: the route responds 501
// when nil.
func (h *Handler) WithWorkspaces(ws *workspace.Service) *Handler {
	h.workspaces = ws
	return h
}

// WithBilling wires the billing service so admin billing endpoints
// stop responding 501 Not Implemented. A nil service keeps them
// disabled.
func (h *Handler) WithBilling(b *billing.Service) *Handler {
	h.billing = b
	return h
}

// WithWebhooks wires an outbound-webhook publisher so InviteUser /
// DeactivateUser emit member.* events. Nil-safe: when no publisher
// is configured (NATS unavailable), the helper methods on Handler
// short-circuit and admin operations behave exactly as before.
func (h *Handler) WithWebhooks(p MemberEventPublisher) *Handler {
	// Guard against passing a typed-nil concrete *webhooks.Publisher,
	// which would compare != nil under the interface comparison and
	// then NPE inside the emit helper. The concrete publisher's own
	// PublishMemberEvent method IS nil-safe (returns nil on a nil
	// receiver), but going through the interface here keeps the
	// invariant "no publisher configured = no emission" expressed at
	// the wire-up boundary where it is obvious. Mirrors the equivalent
	// guard on api/drive.Handler.WithWebhooks.
	if p == nil {
		h.webhooks = nil
		return h
	}
	if pub, ok := p.(*webhooks.Publisher); ok && pub == nil {
		h.webhooks = nil
		return h
	}
	h.webhooks = p
	return h
}

// WithFabric wires the placement-policy admin endpoints. The
// FabricClient talks to the upstream zk-object-fabric console; the
// provisioner is used to look up the per-workspace tenant ID and
// update the local workspace_storage_credentials row after a
// successful console PUT. The storage factory's per-workspace cache
// is invalidated on PUT so subsequent presigns pick up the new
// placement immediately.
func (h *Handler) WithFabric(c FabricClient, p *fabric.Provisioner, sf *storage.ClientFactory) *Handler {
	h.fabric = c
	h.provisioner = p
	h.storeFactory = sf
	return h
}

// RegisterRoutes wires admin routes onto r. Callers are expected to
// mount this under `/api/admin` and apply middleware.AdminOnly on the
// route group.
func (h *Handler) RegisterRoutes(r chi.Router) {
	r.Get("/audit-log", h.GetAuditLog)
	r.Get("/users", h.ListUsers)
	r.Post("/users", h.InviteUser)
	r.Delete("/users/{id}", h.DeactivateUser)
	r.Put("/users/{id}/role", h.UpdateUserRole)
	r.Get("/storage-usage", h.StorageUsage)
	r.Get("/retention-policies", h.ListRetentionPolicies)
	r.Post("/retention-policies", h.UpsertRetentionPolicy)
	r.Delete("/retention-policies/{id}", h.DeleteRetentionPolicy)
	r.Get("/billing/usage", h.BillingUsage)
	r.Put("/billing/plan", h.UpdateBillingPlan)
	r.Post("/billing/checkout-session", h.CreateCheckoutSession)
	r.Post("/billing/portal-session", h.CreatePortalSession)
	r.Get("/placement", h.GetPlacement)
	r.Put("/placement", h.PutPlacement)
	r.Get("/cmk", h.GetCMK)
	r.Put("/cmk", h.PutCMK)
	r.Patch("/workspace/mfa-policy", h.UpdateMFAPolicy)
}

// updateMFAPolicyRequest carries the boolean toggle for the
// workspace MFA policy. We use a pointer so the absence of the key
// in the JSON body returns 400 rather than silently defaulting to
// false (which would be a silent policy DOWNGRADE).
type updateMFAPolicyRequest struct {
	MFARequired *bool `json:"mfa_required"`
}

type updateMFAPolicyResponse struct {
	MFARequired bool `json:"mfa_required"`
}

// UpdateMFAPolicy flips workspaces.mfa_required for the caller's
// workspace. Mounted behind middleware.AdminOnly so only the
// workspace admin can require / un-require MFA for the workspace.
//
// The transition is audited (audit.ActionMFAPolicyChange) with the
// before/after values so a compliance auditor can later reconstruct
// who flipped the policy and when. Disabling MFA does NOT delete
// any user's enrolled credential — the credential remains active
// for that user, but other users in the workspace are no longer
// forced to enroll. This is intentional: a user who has already
// enrolled has the second-factor protection regardless of the
// workspace policy.
func (h *Handler) UpdateMFAPolicy(w http.ResponseWriter, r *http.Request) {
	if h.workspaces == nil {
		middleware.RespondError(w, http.StatusNotImplemented, middleware.ErrCodeUnsupportedOp, "workspace service not wired")
		return
	}
	workspaceID, ok := middleware.WorkspaceIDFromContext(r.Context())
	if !ok {
		middleware.RespondError(w, http.StatusInternalServerError, middleware.ErrCodeInternal, "workspace not in context")
		return
	}
	actorID, _ := middleware.UserIDFromContext(r.Context())

	var req updateMFAPolicyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeMalformedJSON, "invalid json body")
		return
	}
	if req.MFARequired == nil {
		middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeMissingField, "mfa_required is required")
		return
	}

	prev, err := h.workspaces.SetMFARequired(r.Context(), workspaceID, *req.MFARequired)
	if err != nil {
		middleware.RespondError(w, http.StatusInternalServerError, middleware.ErrCodeInternal, "update mfa policy: "+err.Error())
		return
	}

	if h.audit != nil {
		actor := actorID
		h.audit.LogAction(r.Context(), workspaceID, &actor, audit.ActionMFAPolicyChange, "", nil, r, map[string]any{
			"previous": prev,
			"current":  *req.MFARequired,
		})
	}

	writeJSON(w, http.StatusOK, updateMFAPolicyResponse{MFARequired: *req.MFARequired})
}

// GetPlacement returns the workspace's current placement policy by
// proxying through to zk-object-fabric. Responds 501 when fabric is
// not configured (e.g. local-dev with no console URL).
func (h *Handler) GetPlacement(w http.ResponseWriter, r *http.Request) {
	if h.fabric == nil || h.provisioner == nil {
		middleware.RespondError(w, http.StatusNotImplemented, middleware.ErrCodeUnsupportedOp, "fabric not configured")
		return
	}
	workspaceID, ok := middleware.WorkspaceIDFromContext(r.Context())
	if !ok {
		middleware.RespondError(w, http.StatusUnauthorized, middleware.ErrCodeAuthMissingToken, "unauthenticated")
		return
	}
	tenantID, err := h.provisioner.LookupTenantID(r.Context(), workspaceID)
	if err != nil {
		if errors.Is(err, fabric.ErrNoCredentials) {
			middleware.RespondError(w, http.StatusNotFound, middleware.ErrCodeUnsupportedOp, "workspace not provisioned with fabric")
			return
		}
		middleware.RespondError(w, http.StatusInternalServerError, middleware.ErrCodeInternal, "lookup tenant: "+err.Error())
		return
	}
	policy, err := h.fabric.GetPlacement(r.Context(), tenantID)
	if err != nil {
		if errors.Is(err, fabric.ErrPlacementNotFound) {
			middleware.RespondError(w, http.StatusNotFound, middleware.ErrCodeNotFound, "placement policy not set")
			return
		}
		middleware.RespondError(w, http.StatusBadGateway, middleware.ErrCodeUpstream, "get placement: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, policy)
}

// PutPlacement validates and forwards a placement policy to
// zk-object-fabric, then updates the local
// workspace_storage_credentials row to mirror the new policy_ref /
// data residency for fast lookups.
func (h *Handler) PutPlacement(w http.ResponseWriter, r *http.Request) {
	if h.fabric == nil || h.provisioner == nil {
		middleware.RespondError(w, http.StatusNotImplemented, middleware.ErrCodeUnsupportedOp, "fabric not configured")
		return
	}
	workspaceID, ok := middleware.WorkspaceIDFromContext(r.Context())
	if !ok {
		middleware.RespondError(w, http.StatusUnauthorized, middleware.ErrCodeAuthMissingToken, "unauthenticated")
		return
	}
	var policy fabric.Policy
	if err := json.NewDecoder(r.Body).Decode(&policy); err != nil {
		middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeMalformedJSON, "invalid json body")
		return
	}
	tenantID, err := h.provisioner.LookupTenantID(r.Context(), workspaceID)
	if err != nil {
		if errors.Is(err, fabric.ErrNoCredentials) {
			middleware.RespondError(w, http.StatusNotFound, middleware.ErrCodeUnsupportedOp, "workspace not provisioned with fabric")
			return
		}
		middleware.RespondError(w, http.StatusInternalServerError, middleware.ErrCodeInternal, "lookup tenant: "+err.Error())
		return
	}
	// Force the policy's tenant field to match the workspace's
	// resolved tenant, so callers cannot retarget another tenant via
	// the request body.
	policy.Tenant = tenantID
	if err := policy.Validate(); err != nil {
		middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeBadRequest, err.Error())
		return
	}
	if err := h.fabric.PutPlacement(r.Context(), tenantID, &policy); err != nil {
		middleware.RespondError(w, http.StatusBadGateway, middleware.ErrCodeUpstream, "put placement: "+err.Error())
		return
	}
	// Mirror the policy_ref into the local credentials row so the
	// per-workspace storage factory sees the correct placement
	// reference without re-reading from fabric on every signed URL.
	policyRef := policy.Spec.Encryption.KMS
	if policyRef == "" {
		policyRef = "custom"
	}
	if err := h.provisioner.UpdatePlacement(r.Context(), workspaceID, policyRef, policy.FirstCountry()); err != nil && !errors.Is(err, fabric.ErrNoCredentials) {
		middleware.RespondError(w, http.StatusInternalServerError, middleware.ErrCodeInternal, "update local placement mirror: "+err.Error())
		return
	}
	if h.storeFactory != nil {
		h.storeFactory.Invalidate(workspaceID)
	}
	w.WriteHeader(http.StatusNoContent)
}

// cmkRequest is the {"cmk_uri": "..."} body accepted by PutCMK.
type cmkRequest struct {
	CMKURI string `json:"cmk_uri"`
}

// cmkResponse is the body returned by GetCMK and embedded in the
// PutCMK echo so callers can confirm the canonicalised value.
type cmkResponse struct {
	CMKURI string `json:"cmk_uri"`
}

// GetCMK returns the workspace's current customer-managed key URI.
// An empty string is a valid response and means "use the gateway
// default key". Responds 404 when the workspace has no
// fabric-provisioned credentials row yet.
func (h *Handler) GetCMK(w http.ResponseWriter, r *http.Request) {
	if h.provisioner == nil {
		middleware.RespondError(w, http.StatusNotImplemented, middleware.ErrCodeUnsupportedOp, "fabric not configured")
		return
	}
	workspaceID, ok := middleware.WorkspaceIDFromContext(r.Context())
	if !ok {
		middleware.RespondError(w, http.StatusUnauthorized, middleware.ErrCodeAuthMissingToken, "unauthenticated")
		return
	}
	uri, err := h.provisioner.LookupCMK(r.Context(), workspaceID)
	if err != nil {
		if errors.Is(err, fabric.ErrNoCredentials) {
			middleware.RespondError(w, http.StatusNotFound, middleware.ErrCodeUnsupportedOp, "workspace not provisioned with fabric")
			return
		}
		middleware.RespondError(w, http.StatusInternalServerError, middleware.ErrCodeInternal, "lookup cmk: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, cmkResponse{CMKURI: uri})
}

// PutCMK persists a workspace's customer-managed key URI. The URI
// scheme is validated by crypto.ValidateCMKURI before any state
// mutation. The local row is updated first; the upstream fabric
// console is then notified best-effort so a console outage doesn't
// roll back a successful local persistence — the next placement
// reconciliation will re-sync.
func (h *Handler) PutCMK(w http.ResponseWriter, r *http.Request) {
	if h.provisioner == nil {
		middleware.RespondError(w, http.StatusNotImplemented, middleware.ErrCodeUnsupportedOp, "fabric not configured")
		return
	}
	workspaceID, ok := middleware.WorkspaceIDFromContext(r.Context())
	if !ok {
		middleware.RespondError(w, http.StatusUnauthorized, middleware.ErrCodeAuthMissingToken, "unauthenticated")
		return
	}
	var req cmkRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeMalformedJSON, "invalid json body")
		return
	}
	uri := strings.TrimSpace(req.CMKURI)
	if err := cryptoValidateCMKURI(uri); err != nil {
		middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeBadRequest, err.Error())
		return
	}
	if err := h.provisioner.UpdateCMK(r.Context(), workspaceID, uri); err != nil {
		if errors.Is(err, fabric.ErrNoCredentials) {
			middleware.RespondError(w, http.StatusNotFound, middleware.ErrCodeUnsupportedOp, "workspace not provisioned with fabric")
			return
		}
		middleware.RespondError(w, http.StatusInternalServerError, middleware.ErrCodeInternal, "update cmk: "+err.Error())
		return
	}
	// Best-effort fabric console notification: log and ignore errors
	// so a transient console outage does not roll back the local
	// persisted value. Operators reconcile via a follow-up PUT once
	// the console is reachable again. Cache invalidation runs
	// regardless so the next request sees the new URI.
	if h.fabric != nil {
		tenantID, terr := h.provisioner.LookupTenantID(r.Context(), workspaceID)
		if terr != nil {
			logging.FromContext(r.Context()).Error("admin.PutCMK lookup tenant id failed", "workspace_id", workspaceID, "err", terr)
		} else if perr := h.fabric.PutCMK(r.Context(), tenantID, uri); perr != nil {
			logging.FromContext(r.Context()).Error("admin.PutCMK fabric console update failed", "workspace_id", workspaceID, "tenant_id", tenantID, "err", perr)
		}
	}
	if h.storeFactory != nil {
		h.storeFactory.Invalidate(workspaceID)
	}
	writeJSON(w, http.StatusOK, cmkResponse{CMKURI: uri})
}

// GetAuditLog returns paginated audit entries. Filters by action
// when the ?action=... query param is present.
func (h *Handler) GetAuditLog(w http.ResponseWriter, r *http.Request) {
	if h.audit == nil {
		middleware.RespondError(w, http.StatusNotImplemented, middleware.ErrCodeUnsupportedOp, "audit not configured")
		return
	}
	workspaceID, ok := middleware.WorkspaceIDFromContext(r.Context())
	if !ok {
		middleware.RespondError(w, http.StatusUnauthorized, middleware.ErrCodeAuthMissingToken, "unauthenticated")
		return
	}
	limit := parseIntQuery(r, "limit", 50)
	offset := parseIntQuery(r, "offset", 0)
	action := strings.TrimSpace(r.URL.Query().Get("action"))
	entries, err := h.audit.List(r.Context(), workspaceID, action, limit, offset)
	if err != nil {
		middleware.RespondError(w, http.StatusInternalServerError, middleware.ErrCodeInternal, "list audit: "+err.Error())
		return
	}
	if entries == nil {
		entries = []*audit.Entry{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"entries": entries,
		"limit":   limit,
		"offset":  offset,
	})
}

// User management ----------------------------------------------------

type inviteUserRequest struct {
	Email    string `json:"email"`
	Name     string `json:"name"`
	Password string `json:"password"`
	Role     string `json:"role"`
}

type roleRequest struct {
	Role string `json:"role"`
}

type userView struct {
	ID            uuid.UUID  `json:"id"`
	Email         string     `json:"email"`
	Name          string     `json:"name"`
	Role          string     `json:"role"`
	LastLoginAt   *time.Time `json:"last_login_at,omitempty"`
	DeactivatedAt *time.Time `json:"deactivated_at,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
}

func toUserView(u *user.User) userView {
	return userView{
		ID:            u.ID,
		Email:         u.Email,
		Name:          u.Name,
		Role:          u.Role,
		LastLoginAt:   u.LastLoginAt,
		DeactivatedAt: u.DeactivatedAt,
		CreatedAt:     u.CreatedAt,
	}
}

// ListUsers returns every user in the caller's workspace.
func (h *Handler) ListUsers(w http.ResponseWriter, r *http.Request) {
	if h.users == nil {
		middleware.RespondError(w, http.StatusNotImplemented, middleware.ErrCodeUnsupportedOp, "users not configured")
		return
	}
	workspaceID, _ := middleware.WorkspaceIDFromContext(r.Context())
	list, err := h.users.List(r.Context(), workspaceID)
	if err != nil {
		middleware.RespondError(w, http.StatusInternalServerError, middleware.ErrCodeInternal, "list users: "+err.Error())
		return
	}
	out := make([]userView, 0, len(list))
	for _, u := range list {
		out = append(out, toUserView(u))
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": out})
}

// InviteUser creates a new user in the caller's workspace. The admin
// provides a one-time password; the user is expected to change it on
// first login.
func (h *Handler) InviteUser(w http.ResponseWriter, r *http.Request) {
	if h.users == nil {
		middleware.RespondError(w, http.StatusNotImplemented, middleware.ErrCodeUnsupportedOp, "users not configured")
		return
	}
	workspaceID, _ := middleware.WorkspaceIDFromContext(r.Context())
	actor, _ := middleware.UserIDFromContext(r.Context())
	var req inviteUserRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeMalformedJSON, "invalid json body")
		return
	}
	req.Email = strings.TrimSpace(strings.ToLower(req.Email))
	req.Name = strings.TrimSpace(req.Name)
	if req.Role == "" {
		req.Role = user.RoleMember
	}
	if req.Role != user.RoleAdmin && req.Role != user.RoleMember {
		middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeValidation, "role must be admin or member")
		return
	}
	if req.Email == "" || req.Name == "" || req.Password == "" {
		middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeMissingField, "email, name, password are required")
		return
	}
	if err := h.billing.CheckUserQuota(r.Context(), workspaceID); err != nil {
		writeBillingError(w, err)
		return
	}
	u, err := h.users.Create(r.Context(), workspaceID, req.Email, req.Name, req.Password, req.Role)
	if err != nil {
		middleware.RespondError(w, http.StatusInternalServerError, middleware.ErrCodeInternal, "create user: "+err.Error())
		return
	}
	if h.audit != nil {
		userID := u.ID
		h.audit.LogAction(r.Context(), workspaceID, &actor, audit.ActionAdminUserInvite, "user", &userID, r, map[string]any{
			"email": u.Email,
			"role":  u.Role,
		})
	}
	h.billing.RecordUserAdded(r.Context(), workspaceID)
	h.publishMemberEvent(r.Context(), webhooks.EventMemberJoined, workspaceID, &actor, u.ID, u.Email, u.Role)
	writeJSON(w, http.StatusCreated, toUserView(u))
}

// DeactivateUser soft-deletes a user row. The row is preserved so
// audit log foreign-key history still resolves the actor.
func (h *Handler) DeactivateUser(w http.ResponseWriter, r *http.Request) {
	if h.users == nil {
		middleware.RespondError(w, http.StatusNotImplemented, middleware.ErrCodeUnsupportedOp, "users not configured")
		return
	}
	workspaceID, _ := middleware.WorkspaceIDFromContext(r.Context())
	actor, _ := middleware.UserIDFromContext(r.Context())
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeBadRequest, "invalid id")
		return
	}
	// Snapshot before deactivate so the outbound webhook carries
	// the target user's email + role. Failure to fetch (e.g. user
	// already deactivated) falls back to publishing a minimal event
	// with just the user_id below.
	snap, _ := h.users.GetByID(r.Context(), workspaceID, id)
	if err := h.users.Deactivate(r.Context(), workspaceID, id, time.Now().UTC()); err != nil {
		if errors.Is(err, user.ErrNotFound) {
			middleware.RespondError(w, http.StatusNotFound, middleware.ErrCodeNotFound, "user not found or already deactivated")
			return
		}
		middleware.RespondError(w, http.StatusInternalServerError, middleware.ErrCodeInternal, "deactivate: "+err.Error())
		return
	}
	if h.audit != nil {
		target := id
		h.audit.LogAction(r.Context(), workspaceID, &actor, audit.ActionAdminUserDeactivate, "user", &target, r, nil)
	}
	email, role := "", ""
	if snap != nil {
		email, role = snap.Email, snap.Role
	}
	h.publishMemberEvent(r.Context(), webhooks.EventMemberRemoved, workspaceID, &actor, id, email, role)
	w.WriteHeader(http.StatusNoContent)
}

// publishMemberEvent is the admin handler's centralised emit-helper
// for member.* webhook events. Nil-safe; failures are logged but
// never propagated to the caller — webhook emission is a side-effect
// and the underlying admin operation has already committed by the
// time this helper runs.
func (h *Handler) publishMemberEvent(ctx context.Context, t webhooks.EventType, workspaceID uuid.UUID, actorID *uuid.UUID, userID uuid.UUID, email, role string) {
	if h.webhooks == nil {
		return
	}
	if err := h.webhooks.PublishMemberEvent(ctx, t, workspaceID, actorID, webhooks.MemberEventData{
		UserID: userID,
		Email:  email,
		Role:   role,
	}); err != nil {
		logging.FromContext(ctx).Error("admin publish webhook member event failed",
			"event_type", string(t), "user_id", userID, "workspace_id", workspaceID, "err", err)
	}
}

// UpdateUserRole promotes or demotes a user.
func (h *Handler) UpdateUserRole(w http.ResponseWriter, r *http.Request) {
	if h.users == nil {
		middleware.RespondError(w, http.StatusNotImplemented, middleware.ErrCodeUnsupportedOp, "users not configured")
		return
	}
	workspaceID, _ := middleware.WorkspaceIDFromContext(r.Context())
	actor, _ := middleware.UserIDFromContext(r.Context())
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeBadRequest, "invalid id")
		return
	}
	var req roleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeMalformedJSON, "invalid json body")
		return
	}
	if req.Role != user.RoleAdmin && req.Role != user.RoleMember {
		middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeValidation, "role must be admin or member")
		return
	}
	if err := h.users.UpdateRole(r.Context(), workspaceID, id, req.Role); err != nil {
		if errors.Is(err, user.ErrNotFound) {
			middleware.RespondError(w, http.StatusNotFound, middleware.ErrCodeNotFound, "user not found")
			return
		}
		middleware.RespondError(w, http.StatusInternalServerError, middleware.ErrCodeInternal, "update role: "+err.Error())
		return
	}
	if h.audit != nil {
		target := id
		h.audit.LogAction(r.Context(), workspaceID, &actor, audit.ActionAdminUserRoleChange, "user", &target, r, map[string]any{
			"role": req.Role,
		})
	}
	w.WriteHeader(http.StatusNoContent)
}

// StorageUsage returns the aggregate bytes-in-use for a workspace
// along with a per-user breakdown. The aggregation reads the files
// table directly; the numbers reflect logical sizes, not the
// on-disk footprint of any particular storage tier.
type storageUsageResponse struct {
	TotalBytes int64            `json:"total_bytes"`
	PerUser    []userUsageEntry `json:"per_user"`
}

type userUsageEntry struct {
	UserID     uuid.UUID `json:"user_id"`
	Email      string    `json:"email"`
	TotalBytes int64     `json:"total_bytes"`
	FileCount  int64     `json:"file_count"`
}

// StorageUsage aggregates files.size_bytes by created_by.
func (h *Handler) StorageUsage(w http.ResponseWriter, r *http.Request) {
	workspaceID, _ := middleware.WorkspaceIDFromContext(r.Context())
	const q = `
SELECT u.id, u.email, COALESCE(SUM(f.size_bytes), 0)::bigint, COUNT(f.id)::bigint
FROM users u
LEFT JOIN files f ON f.created_by = u.id AND f.workspace_id = u.workspace_id AND f.deleted_at IS NULL
WHERE u.workspace_id = $1
GROUP BY u.id, u.email
ORDER BY u.email`
	rows, err := h.pool.Query(r.Context(), q, workspaceID)
	if err != nil {
		middleware.RespondError(w, http.StatusInternalServerError, middleware.ErrCodeInternal, "query usage: "+err.Error())
		return
	}
	defer rows.Close()
	resp := storageUsageResponse{PerUser: []userUsageEntry{}}
	for rows.Next() {
		var entry userUsageEntry
		if err := rows.Scan(&entry.UserID, &entry.Email, &entry.TotalBytes, &entry.FileCount); err != nil {
			middleware.RespondError(w, http.StatusInternalServerError, middleware.ErrCodeInternal, "scan usage: "+err.Error())
			return
		}
		resp.TotalBytes += entry.TotalBytes
		resp.PerUser = append(resp.PerUser, entry)
	}
	writeJSON(w, http.StatusOK, resp)
}

// Retention --------------------------------------------------------

type retentionPolicyRequest struct {
	FolderID         *string `json:"folder_id,omitempty"`
	MaxVersions      *int    `json:"max_versions,omitempty"`
	MaxAgeDays       *int    `json:"max_age_days,omitempty"`
	ArchiveAfterDays *int    `json:"archive_after_days,omitempty"`
}

// ListRetentionPolicies returns every policy in the caller's workspace.
func (h *Handler) ListRetentionPolicies(w http.ResponseWriter, r *http.Request) {
	if h.retention == nil {
		middleware.RespondError(w, http.StatusNotImplemented, middleware.ErrCodeUnsupportedOp, "retention not configured")
		return
	}
	workspaceID, _ := middleware.WorkspaceIDFromContext(r.Context())
	list, err := h.retention.List(r.Context(), workspaceID)
	if err != nil {
		middleware.RespondError(w, http.StatusInternalServerError, middleware.ErrCodeInternal, "list: "+err.Error())
		return
	}
	if list == nil {
		list = []*retention.Policy{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"policies": list})
}

// UpsertRetentionPolicy creates or replaces a policy keyed on
// (workspace, folder).
func (h *Handler) UpsertRetentionPolicy(w http.ResponseWriter, r *http.Request) {
	if h.retention == nil {
		middleware.RespondError(w, http.StatusNotImplemented, middleware.ErrCodeUnsupportedOp, "retention not configured")
		return
	}
	workspaceID, _ := middleware.WorkspaceIDFromContext(r.Context())
	actor, _ := middleware.UserIDFromContext(r.Context())
	var req retentionPolicyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeMalformedJSON, "invalid json body")
		return
	}
	p := &retention.Policy{
		WorkspaceID:      workspaceID,
		MaxVersions:      req.MaxVersions,
		MaxAgeDays:       req.MaxAgeDays,
		ArchiveAfterDays: req.ArchiveAfterDays,
	}
	if actor != uuid.Nil {
		a := actor
		p.CreatedBy = &a
	}
	if req.FolderID != nil && *req.FolderID != "" {
		fid, err := uuid.Parse(*req.FolderID)
		if err != nil {
			middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeBadRequest, "invalid folder_id")
			return
		}
		p.FolderID = &fid
	}
	out, err := h.retention.Upsert(r.Context(), p)
	if err != nil {
		middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeBadRequest, err.Error())
		return
	}
	if h.audit != nil {
		pid := out.ID
		h.audit.LogAction(r.Context(), workspaceID, &actor, audit.ActionRetentionPolicyUpsert, "retention_policy", &pid, r, map[string]any{
			"folder_id":          out.FolderID,
			"max_versions":       out.MaxVersions,
			"max_age_days":       out.MaxAgeDays,
			"archive_after_days": out.ArchiveAfterDays,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// DeleteRetentionPolicy removes a policy by id.
func (h *Handler) DeleteRetentionPolicy(w http.ResponseWriter, r *http.Request) {
	if h.retention == nil {
		middleware.RespondError(w, http.StatusNotImplemented, middleware.ErrCodeUnsupportedOp, "retention not configured")
		return
	}
	workspaceID, _ := middleware.WorkspaceIDFromContext(r.Context())
	actor, _ := middleware.UserIDFromContext(r.Context())
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeBadRequest, "invalid id")
		return
	}
	if err := h.retention.Delete(r.Context(), workspaceID, id); err != nil {
		if errors.Is(err, retention.ErrNotFound) {
			middleware.RespondError(w, http.StatusNotFound, middleware.ErrCodeNotFound, "policy not found")
			return
		}
		middleware.RespondError(w, http.StatusInternalServerError, middleware.ErrCodeInternal, "delete: "+err.Error())
		return
	}
	if h.audit != nil {
		target := id
		h.audit.LogAction(r.Context(), workspaceID, &actor, audit.ActionRetentionPolicyDelete, "retention_policy", &target, r, nil)
	}
	w.WriteHeader(http.StatusNoContent)
}

// Helpers ----------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func parseIntQuery(r *http.Request, key string, def int) int {
	s := strings.TrimSpace(r.URL.Query().Get(key))
	if s == "" {
		return def
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return v
}

// Billing -----------------------------------------------------------

type updatePlanRequest struct {
	Tier                     string `json:"tier"`
	MaxStorageBytes          *int64 `json:"max_storage_bytes,omitempty"`
	MaxUsers                 *int   `json:"max_users,omitempty"`
	MaxBandwidthBytesMonthly *int64 `json:"max_bandwidth_bytes_monthly,omitempty"`
}

// BillingUsage returns the workspace's current usage versus its plan
// limits. Admin-only because non-admins shouldn't see other users'
// counts.
func (h *Handler) BillingUsage(w http.ResponseWriter, r *http.Request) {
	if h.billing == nil {
		middleware.RespondError(w, http.StatusNotImplemented, middleware.ErrCodeUnsupportedOp, "billing not configured")
		return
	}
	workspaceID, _ := middleware.WorkspaceIDFromContext(r.Context())
	summary, err := h.billing.GetUsageSummary(r.Context(), workspaceID)
	if err != nil {
		middleware.RespondError(w, http.StatusInternalServerError, middleware.ErrCodeInternal, "billing usage: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, summary)
}

// UpdateBillingPlan upserts the workspace's plan row.
func (h *Handler) UpdateBillingPlan(w http.ResponseWriter, r *http.Request) {
	if h.billing == nil {
		middleware.RespondError(w, http.StatusNotImplemented, middleware.ErrCodeUnsupportedOp, "billing not configured")
		return
	}
	workspaceID, _ := middleware.WorkspaceIDFromContext(r.Context())
	actor, _ := middleware.UserIDFromContext(r.Context())
	var req updatePlanRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeMalformedJSON, "invalid json body")
		return
	}
	if !billing.IsValidTier(req.Tier) {
		middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeBadRequest, "invalid tier")
		return
	}
	plan := &billing.Plan{
		WorkspaceID:              workspaceID,
		Tier:                     req.Tier,
		MaxStorageBytes:          req.MaxStorageBytes,
		MaxUsers:                 req.MaxUsers,
		MaxBandwidthBytesMonthly: req.MaxBandwidthBytesMonthly,
	}
	out, err := h.billing.UpsertPlan(r.Context(), plan)
	if err != nil {
		middleware.RespondError(w, http.StatusInternalServerError, middleware.ErrCodeInternal, "upsert plan: "+err.Error())
		return
	}
	if h.audit != nil {
		pid := out.ID
		h.audit.LogAction(r.Context(), workspaceID, &actor, audit.ActionAdminBillingUpdate, "billing_plan", &pid, r, map[string]any{
			"tier": out.Tier,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// writeBillingError maps billing.ErrQuotaExceeded to 402 Payment
// Required with the actionable WORKSPACE_QUOTA_EXCEEDED code so the
// frontend can prompt the user to upgrade their plan. Other billing
// errors fall through to a generic 500.
func writeBillingError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, billing.ErrQuotaExceeded):
		middleware.RespondError(w, http.StatusPaymentRequired, middleware.ErrCodeQuotaExceeded, err.Error())
	default:
		middleware.RespondError(w, http.StatusInternalServerError, middleware.ErrCodeInternal, err.Error())
	}
}

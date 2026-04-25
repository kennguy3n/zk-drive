// Package admin serves the admin-only HTTP endpoints: audit log,
// retention policy CRUD, user management, and workspace storage
// stats. All routes require the admin role — enforced by the
// middleware.AdminOnly wrapper in cmd/server/main.go.
package admin

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/zk-drive/api/middleware"
	"github.com/kennguy3n/zk-drive/internal/audit"
	"github.com/kennguy3n/zk-drive/internal/billing"
	"github.com/kennguy3n/zk-drive/internal/retention"
	"github.com/kennguy3n/zk-drive/internal/user"
)

// Handler serves admin HTTP endpoints. All service dependencies are
// optional: when a service is nil the corresponding route returns 501
// so the rest of the admin surface keeps functioning.
type Handler struct {
	pool      *pgxpool.Pool
	users     *user.Service
	audit     *audit.Service
	retention *retention.Service
	billing   *billing.Service
}

// NewHandler constructs a Handler. Pass nil for services that are
// not wired yet; the related routes will respond 501 Not Implemented.
func NewHandler(pool *pgxpool.Pool, users *user.Service, aud *audit.Service, ret *retention.Service) *Handler {
	return &Handler{pool: pool, users: users, audit: aud, retention: ret}
}

// WithBilling wires the billing service so admin billing endpoints
// stop responding 501 Not Implemented. A nil service keeps them
// disabled.
func (h *Handler) WithBilling(b *billing.Service) *Handler {
	h.billing = b
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
}

// GetAuditLog returns paginated audit entries. Filters by action
// when the ?action=... query param is present.
func (h *Handler) GetAuditLog(w http.ResponseWriter, r *http.Request) {
	if h.audit == nil {
		http.Error(w, "audit not configured", http.StatusNotImplemented)
		return
	}
	workspaceID, ok := middleware.WorkspaceIDFromContext(r.Context())
	if !ok {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	limit := parseIntQuery(r, "limit", 50)
	offset := parseIntQuery(r, "offset", 0)
	action := strings.TrimSpace(r.URL.Query().Get("action"))
	entries, err := h.audit.List(r.Context(), workspaceID, action, limit, offset)
	if err != nil {
		http.Error(w, "list audit: "+err.Error(), http.StatusInternalServerError)
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
		http.Error(w, "users not configured", http.StatusNotImplemented)
		return
	}
	workspaceID, _ := middleware.WorkspaceIDFromContext(r.Context())
	list, err := h.users.List(r.Context(), workspaceID)
	if err != nil {
		http.Error(w, "list users: "+err.Error(), http.StatusInternalServerError)
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
		http.Error(w, "users not configured", http.StatusNotImplemented)
		return
	}
	workspaceID, _ := middleware.WorkspaceIDFromContext(r.Context())
	actor, _ := middleware.UserIDFromContext(r.Context())
	var req inviteUserRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json body", http.StatusBadRequest)
		return
	}
	req.Email = strings.TrimSpace(strings.ToLower(req.Email))
	req.Name = strings.TrimSpace(req.Name)
	if req.Role == "" {
		req.Role = user.RoleMember
	}
	if req.Role != user.RoleAdmin && req.Role != user.RoleMember {
		http.Error(w, "role must be admin or member", http.StatusBadRequest)
		return
	}
	if req.Email == "" || req.Name == "" || req.Password == "" {
		http.Error(w, "email, name, password are required", http.StatusBadRequest)
		return
	}
	if err := h.billing.CheckUserQuota(r.Context(), workspaceID); err != nil {
		writeBillingError(w, err)
		return
	}
	u, err := h.users.Create(r.Context(), workspaceID, req.Email, req.Name, req.Password, req.Role)
	if err != nil {
		http.Error(w, "create user: "+err.Error(), http.StatusInternalServerError)
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
	writeJSON(w, http.StatusCreated, toUserView(u))
}

// DeactivateUser soft-deletes a user row. The row is preserved so
// audit log foreign-key history still resolves the actor.
func (h *Handler) DeactivateUser(w http.ResponseWriter, r *http.Request) {
	if h.users == nil {
		http.Error(w, "users not configured", http.StatusNotImplemented)
		return
	}
	workspaceID, _ := middleware.WorkspaceIDFromContext(r.Context())
	actor, _ := middleware.UserIDFromContext(r.Context())
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	if err := h.users.Deactivate(r.Context(), workspaceID, id, time.Now().UTC()); err != nil {
		if errors.Is(err, user.ErrNotFound) {
			http.Error(w, "user not found or already deactivated", http.StatusNotFound)
			return
		}
		http.Error(w, "deactivate: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if h.audit != nil {
		target := id
		h.audit.LogAction(r.Context(), workspaceID, &actor, audit.ActionAdminUserDeactivate, "user", &target, r, nil)
	}
	w.WriteHeader(http.StatusNoContent)
}

// UpdateUserRole promotes or demotes a user.
func (h *Handler) UpdateUserRole(w http.ResponseWriter, r *http.Request) {
	if h.users == nil {
		http.Error(w, "users not configured", http.StatusNotImplemented)
		return
	}
	workspaceID, _ := middleware.WorkspaceIDFromContext(r.Context())
	actor, _ := middleware.UserIDFromContext(r.Context())
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	var req roleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json body", http.StatusBadRequest)
		return
	}
	if req.Role != user.RoleAdmin && req.Role != user.RoleMember {
		http.Error(w, "role must be admin or member", http.StatusBadRequest)
		return
	}
	if err := h.users.UpdateRole(r.Context(), workspaceID, id, req.Role); err != nil {
		if errors.Is(err, user.ErrNotFound) {
			http.Error(w, "user not found", http.StatusNotFound)
			return
		}
		http.Error(w, "update role: "+err.Error(), http.StatusInternalServerError)
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
		http.Error(w, "query usage: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	resp := storageUsageResponse{PerUser: []userUsageEntry{}}
	for rows.Next() {
		var entry userUsageEntry
		if err := rows.Scan(&entry.UserID, &entry.Email, &entry.TotalBytes, &entry.FileCount); err != nil {
			http.Error(w, "scan usage: "+err.Error(), http.StatusInternalServerError)
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
		http.Error(w, "retention not configured", http.StatusNotImplemented)
		return
	}
	workspaceID, _ := middleware.WorkspaceIDFromContext(r.Context())
	list, err := h.retention.List(r.Context(), workspaceID)
	if err != nil {
		http.Error(w, "list: "+err.Error(), http.StatusInternalServerError)
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
		http.Error(w, "retention not configured", http.StatusNotImplemented)
		return
	}
	workspaceID, _ := middleware.WorkspaceIDFromContext(r.Context())
	actor, _ := middleware.UserIDFromContext(r.Context())
	var req retentionPolicyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json body", http.StatusBadRequest)
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
			http.Error(w, "invalid folder_id", http.StatusBadRequest)
			return
		}
		p.FolderID = &fid
	}
	out, err := h.retention.Upsert(r.Context(), p)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
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
		http.Error(w, "retention not configured", http.StatusNotImplemented)
		return
	}
	workspaceID, _ := middleware.WorkspaceIDFromContext(r.Context())
	actor, _ := middleware.UserIDFromContext(r.Context())
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	if err := h.retention.Delete(r.Context(), workspaceID, id); err != nil {
		if errors.Is(err, retention.ErrNotFound) {
			http.Error(w, "policy not found", http.StatusNotFound)
			return
		}
		http.Error(w, "delete: "+err.Error(), http.StatusInternalServerError)
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
		http.Error(w, "billing not configured", http.StatusNotImplemented)
		return
	}
	workspaceID, _ := middleware.WorkspaceIDFromContext(r.Context())
	summary, err := h.billing.GetUsageSummary(r.Context(), workspaceID)
	if err != nil {
		http.Error(w, "billing usage: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, summary)
}

// UpdateBillingPlan upserts the workspace's plan row.
func (h *Handler) UpdateBillingPlan(w http.ResponseWriter, r *http.Request) {
	if h.billing == nil {
		http.Error(w, "billing not configured", http.StatusNotImplemented)
		return
	}
	workspaceID, _ := middleware.WorkspaceIDFromContext(r.Context())
	actor, _ := middleware.UserIDFromContext(r.Context())
	var req updatePlanRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json body", http.StatusBadRequest)
		return
	}
	if !billing.IsValidTier(req.Tier) {
		http.Error(w, "invalid tier", http.StatusBadRequest)
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
		http.Error(w, "upsert plan: "+err.Error(), http.StatusInternalServerError)
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
// Required so the frontend can prompt the user to upgrade their plan.
func writeBillingError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, billing.ErrQuotaExceeded):
		http.Error(w, err.Error(), http.StatusPaymentRequired)
	default:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

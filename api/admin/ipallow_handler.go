package admin

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/kennguy3n/zk-drive/api/middleware"
	"github.com/kennguy3n/zk-drive/internal/audit"
	"github.com/kennguy3n/zk-drive/internal/workspace"
)

// WithIPAllow wires the IP-allowlist service so the
// /ip-allowlist admin endpoints stop responding 501. Optional: when
// nil the routes report the feature is not configured, mirroring the
// other optional-service builders on Handler.
func (h *Handler) WithIPAllow(svc *workspace.IPAllowService) *Handler {
	h.ipAllow = svc
	return h
}

// ipRuleResponse is the wire shape for a single allowlist rule.
// Mirrors workspace.IPRule but is declared here so the API contract
// is owned by the handler package and can evolve independently of
// the internal model.
type ipRuleResponse struct {
	ID        uuid.UUID `json:"id"`
	CIDR      string    `json:"cidr"`
	Label     string    `json:"label"`
	CreatedBy uuid.UUID `json:"created_by"`
	CreatedAt string    `json:"created_at"`
}

func toIPRuleResponse(r workspace.IPRule) ipRuleResponse {
	return ipRuleResponse{
		ID:        r.ID,
		CIDR:      r.CIDR,
		Label:     r.Label,
		CreatedBy: r.CreatedBy,
		CreatedAt: r.CreatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
	}
}

// listIPAllowResponse bundles the rules with the master switch so an
// admin UI can render the toggle and the table from a single GET.
type listIPAllowResponse struct {
	Enabled bool             `json:"enabled"`
	Rules   []ipRuleResponse `json:"rules"`
}

// ListIPAllowRules handles GET /api/admin/ip-allowlist.
func (h *Handler) ListIPAllowRules(w http.ResponseWriter, r *http.Request) {
	if h.ipAllow == nil {
		middleware.RespondError(w, http.StatusNotImplemented, middleware.ErrCodeUnsupportedOp, "ip allowlist service not wired")
		return
	}
	workspaceID, ok := middleware.WorkspaceIDFromContext(r.Context())
	if !ok {
		middleware.RespondError(w, http.StatusInternalServerError, middleware.ErrCodeInternal, "workspace not in context")
		return
	}
	enabled, err := h.ipAllow.IsEnabled(r.Context(), workspaceID)
	if err != nil {
		middleware.RespondInternalError(w, r, "ip allowlist enabled", err)
		return
	}
	rules, err := h.ipAllow.ListRules(r.Context(), workspaceID)
	if err != nil {
		middleware.RespondInternalError(w, r, "list ip allowlist", err)
		return
	}
	out := listIPAllowResponse{Enabled: enabled, Rules: make([]ipRuleResponse, 0, len(rules))}
	for _, rule := range rules {
		out.Rules = append(out.Rules, toIPRuleResponse(rule))
	}
	writeJSON(w, http.StatusOK, out)
}

// addIPAllowRuleRequest is the POST body. CIDR is required; label is
// optional free text the admin uses to remember what a range is.
type addIPAllowRuleRequest struct {
	CIDR  string `json:"cidr"`
	Label string `json:"label"`
}

// AddIPAllowRule handles POST /api/admin/ip-allowlist.
func (h *Handler) AddIPAllowRule(w http.ResponseWriter, r *http.Request) {
	if h.ipAllow == nil {
		middleware.RespondError(w, http.StatusNotImplemented, middleware.ErrCodeUnsupportedOp, "ip allowlist service not wired")
		return
	}
	workspaceID, ok := middleware.WorkspaceIDFromContext(r.Context())
	if !ok {
		middleware.RespondError(w, http.StatusInternalServerError, middleware.ErrCodeInternal, "workspace not in context")
		return
	}
	actorID, _ := middleware.UserIDFromContext(r.Context())

	var req addIPAllowRuleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeMalformedJSON, "invalid json body")
		return
	}
	if req.CIDR == "" {
		middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeMissingField, "cidr is required")
		return
	}

	rule, err := h.ipAllow.AddRule(r.Context(), workspaceID, req.CIDR, req.Label, actorID)
	if err != nil {
		switch {
		case errors.Is(err, workspace.ErrInvalidCIDR):
			middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeInvalidCIDR, "cidr is not a valid network")
		case errors.Is(err, workspace.ErrPrivateCIDR):
			middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodePrivateCIDR, "cidr must be a public range")
		case errors.Is(err, workspace.ErrTooManyRules):
			middleware.RespondError(w, http.StatusConflict, middleware.ErrCodeRuleCapExceeded, "ip allowlist rule cap reached")
		case errors.Is(err, workspace.ErrDuplicateCIDR):
			middleware.RespondError(w, http.StatusConflict, middleware.ErrCodeDuplicateCIDR, "cidr is already allowlisted")
		default:
			middleware.RespondInternalError(w, r, "add ip allowlist rule", err)
		}
		return
	}

	if h.audit != nil {
		actor := actorID
		target := rule.ID
		h.audit.LogAction(r.Context(), workspaceID, &actor, audit.ActionIPAllowRuleAdd, "ip_allowlist_rule", &target, r, map[string]any{
			"cidr":  rule.CIDR,
			"label": rule.Label,
		})
	}

	writeJSON(w, http.StatusCreated, toIPRuleResponse(*rule))
}

// RemoveIPAllowRule handles DELETE /api/admin/ip-allowlist/{id}.
func (h *Handler) RemoveIPAllowRule(w http.ResponseWriter, r *http.Request) {
	if h.ipAllow == nil {
		middleware.RespondError(w, http.StatusNotImplemented, middleware.ErrCodeUnsupportedOp, "ip allowlist service not wired")
		return
	}
	workspaceID, ok := middleware.WorkspaceIDFromContext(r.Context())
	if !ok {
		middleware.RespondError(w, http.StatusInternalServerError, middleware.ErrCodeInternal, "workspace not in context")
		return
	}
	actorID, _ := middleware.UserIDFromContext(r.Context())

	ruleID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeBadRequest, "invalid id")
		return
	}

	if err := h.ipAllow.RemoveRule(r.Context(), workspaceID, ruleID); err != nil {
		if errors.Is(err, workspace.ErrNotFound) {
			middleware.RespondError(w, http.StatusNotFound, middleware.ErrCodeNotFound, "rule not found")
			return
		}
		if errors.Is(err, workspace.ErrCannotRemoveLastRule) {
			middleware.RespondError(w, http.StatusConflict, middleware.ErrCodeAllowlistLastRule, "disable the ip allowlist before removing its last rule")
			return
		}
		middleware.RespondInternalError(w, r, "remove ip allowlist rule", err)
		return
	}

	if h.audit != nil {
		actor := actorID
		target := ruleID
		h.audit.LogAction(r.Context(), workspaceID, &actor, audit.ActionIPAllowRuleRemove, "ip_allowlist_rule", &target, r, nil)
	}

	w.WriteHeader(http.StatusNoContent)
}

// updateIPAllowPolicyRequest carries the master switch. We use a
// pointer so an absent key returns 400 rather than silently
// disabling the allowlist (a silent security DOWNGRADE), mirroring
// the MFA policy endpoint.
type updateIPAllowPolicyRequest struct {
	Enabled *bool `json:"enabled"`
}

type updateIPAllowPolicyResponse struct {
	Enabled bool `json:"enabled"`
}

// UpdateIPAllowPolicy handles PATCH /api/admin/ip-allowlist/policy.
func (h *Handler) UpdateIPAllowPolicy(w http.ResponseWriter, r *http.Request) {
	if h.ipAllow == nil {
		middleware.RespondError(w, http.StatusNotImplemented, middleware.ErrCodeUnsupportedOp, "ip allowlist service not wired")
		return
	}
	workspaceID, ok := middleware.WorkspaceIDFromContext(r.Context())
	if !ok {
		middleware.RespondError(w, http.StatusInternalServerError, middleware.ErrCodeInternal, "workspace not in context")
		return
	}
	actorID, _ := middleware.UserIDFromContext(r.Context())

	var req updateIPAllowPolicyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeMalformedJSON, "invalid json body")
		return
	}
	if req.Enabled == nil {
		middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeMissingField, "enabled is required")
		return
	}

	prev, err := h.ipAllow.SetEnabled(r.Context(), workspaceID, *req.Enabled)
	if err != nil {
		if errors.Is(err, workspace.ErrNotFound) {
			middleware.RespondError(w, http.StatusNotFound, middleware.ErrCodeNotFound, "workspace not found")
			return
		}
		if errors.Is(err, workspace.ErrNoRulesToEnable) {
			middleware.RespondError(w, http.StatusConflict, middleware.ErrCodeAllowlistNoRules, "add at least one rule before enabling the ip allowlist")
			return
		}
		middleware.RespondInternalError(w, r, "update ip allowlist policy", err)
		return
	}

	if h.audit != nil {
		actor := actorID
		h.audit.LogAction(r.Context(), workspaceID, &actor, audit.ActionIPAllowPolicyChange, "", nil, r, map[string]any{
			"previous": prev,
			"current":  *req.Enabled,
		})
	}

	writeJSON(w, http.StatusOK, updateIPAllowPolicyResponse{Enabled: *req.Enabled})
}

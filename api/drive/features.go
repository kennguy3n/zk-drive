package drive

import (
	"net/http"

	"github.com/kennguy3n/zk-drive/api/middleware"
)

// featuresResponse is the GET /api/features payload. `features` is the
// complete enabled/disabled map across every known feature key (see
// internal/feature.AllFeatures); `tier` is the workspace's resolved
// billing tier so the UI can show the current plan and gate upgrade
// prompts.
type featuresResponse struct {
	Tier     string          `json:"tier"`
	Features map[string]bool `json:"features"`
}

// GetFeatures returns the active feature set for the caller's workspace.
//
// The frontend fetches this once on login (frontend/src/hooks/useFeatures.ts)
// and gates UI — admin nav, KChat, ONLYOFFICE editor, AI summary buttons,
// retention-policy UI, webhook config, strict-ZK / CMK / data-residency
// controls — behind the returned flags so a Free/Starter workspace never
// sees Business or Secure-Business surfaces.
//
// When the feature service is not wired (nil), the handler degrades to the
// Free-tier defaults rather than erroring, so the UI still renders the
// baseline (folders/files/share/search) experience.
func (h *Handler) GetFeatures(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := middleware.WorkspaceIDFromContext(r.Context())
	if !ok {
		middleware.RespondError(w, http.StatusUnauthorized, middleware.ErrCodeAuthMissingToken, "unauthenticated")
		return
	}

	features, tier, err := h.features.ActiveFeatures(r.Context(), workspaceID)
	if err != nil {
		middleware.RespondInternalError(w, r, "load features", err)
		return
	}

	writeJSON(w, http.StatusOK, featuresResponse{Tier: tier, Features: features})
}

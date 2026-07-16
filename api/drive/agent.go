package drive

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/kennguy3n/zk-drive/api/middleware"
	"github.com/kennguy3n/zk-drive/internal/ai/editor"
	"github.com/kennguy3n/zk-drive/internal/document"
	"github.com/kennguy3n/zk-drive/internal/folder"
	"github.com/kennguy3n/zk-drive/internal/permission"
)

// maxAgentBodyBytes caps the JSON body for agent tool calls.
const maxAgentBodyBytes = 256 << 10

// AgentToolAPIRequest is the JSON body for POST /api/documents/{id}/ai/agent.
type AgentToolAPIRequest struct {
	Tool     string `json:"tool"`
	Position *int   `json:"position,omitempty"`
	Content  string `json:"content,omitempty"`
	BlockID  string `json:"block_id,omitempty"`
	Start    *int   `json:"start,omitempty"`
	End      *int   `json:"end,omitempty"`
}

// DocumentAIAgent executes a single agent tool call against the
// document. The agent applies changes through the CRDT path (collab
// hub), so edits are persisted as deltas and broadcast to connected
// editors in real time.
//
// Endpoint: POST /api/documents/{id}/ai/agent
// Auth: session-authenticated, workspace membership, editor permission.
// Privacy: strict-ZK folders return 403.
func (h *Handler) DocumentAIAgent(w http.ResponseWriter, r *http.Request) {
	if h.documents == nil {
		middleware.RespondError(w, http.StatusServiceUnavailable, middleware.ErrCodeUnsupportedOp, "documents disabled")
		return
	}
	if h.agentTools == nil {
		middleware.RespondError(w, http.StatusNotImplemented, middleware.ErrCodeUnsupportedOp, "agent tools not configured")
		return
	}

	workspaceID, _ := middleware.WorkspaceIDFromContext(r.Context())
	docID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeBadRequest, "invalid document id")
		return
	}

	doc, parent, err := h.documents.GetMetadata(r.Context(), workspaceID, docID)
	if err != nil {
		writeDocumentError(w, r, err)
		return
	}

	if parent.EncryptionMode == folder.EncryptionStrictZK {
		middleware.RespondError(w, http.StatusForbidden, middleware.ErrCodeForbidden, "AI agent is not available for zero-knowledge folders")
		return
	}

	if err := h.assertResourceAccess(r.Context(), permission.ResourceFolder, doc.FolderID, permission.RoleEditor); err != nil {
		writeServiceError(w, r, err)
		return
	}

	if doc.CollabMode == document.CollabModeDisabled {
		middleware.RespondError(w, http.StatusConflict, middleware.ErrCodeConflict, "document editing is disabled")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxAgentBodyBytes)
	var req AgentToolAPIRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeMalformedJSON, "invalid json body")
		return
	}

	result, err := h.agentTools.RunTool(r.Context(), workspaceID, docID, editor.AgentToolRequest{
		Tool:     req.Tool,
		Position: req.Position,
		Content:  req.Content,
		BlockID:  req.BlockID,
		Start:    req.Start,
		End:      req.End,
	})
	if err != nil {
		var ate *editor.AgentToolError
		if errors.As(err, &ate) {
			errCode := middleware.ErrCodeBadRequest
			switch ate.Code {
			case http.StatusForbidden:
				errCode = middleware.ErrCodeForbidden
			case http.StatusConflict:
				errCode = middleware.ErrCodeConflict
			case http.StatusNotImplemented:
				errCode = middleware.ErrCodeUnsupportedOp
			case http.StatusInternalServerError:
				errCode = middleware.ErrCodeInternal
			}
			middleware.RespondError(w, ate.Code, errCode, ate.Message)
			return
		}
		writeServiceError(w, r, err)
		return
	}

	middleware.WriteJSON(w, http.StatusOK, result)
}

// AgentToolRunner is the interface for the agent tool service.
type AgentToolRunner = editor.AgentToolRunner

// WithAgentTools wires the AI agent tool service. When non-nil the
// /api/documents/{id}/ai/agent endpoint executes tool calls; when nil
// the endpoint responds 501.
func (h *Handler) WithAgentTools(s AgentToolRunner) *Handler {
	if isTypedNil(s) {
		h.agentTools = nil
		return h
	}
	h.agentTools = s
	return h
}

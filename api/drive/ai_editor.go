package drive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/kennguy3n/zk-drive/api/middleware"
	"github.com/kennguy3n/zk-drive/internal/ai/editor"
	"github.com/kennguy3n/zk-drive/internal/document"
	"github.com/kennguy3n/zk-drive/internal/folder"
	"github.com/kennguy3n/zk-drive/internal/permission"
)

// maxEditorSkillBodyBytes caps the JSON body for the AI skill endpoint.
// The selection + context fields carry document text; 256 KiB is
// generous for any realistic selection while stopping an authenticated
// client from forcing unbounded allocation before MaxContextChars
// truncation runs in SkillService.Execute. Mirrors the MaxBytesReader
// guard pattern used by push / device-token handlers.
const maxEditorSkillBodyBytes = 256 << 10

// EditorSkillRequest is the JSON body for POST /api/documents/{id}/ai/skill.
// The frontend sends the selected text (and optionally surrounding context)
// directly from the TipTap editor — the server does not need to decode the
// Yjs binary state to extract text, which keeps the skill endpoint fast and
// avoids needing the Yjs wasm runtime on the AI request path.
type EditorSkillRequest struct {
	SkillID   string `json:"skill_id"`
	Selection string `json:"selection"`
	Context   string `json:"context,omitempty"`
	Language  string `json:"language,omitempty"`
}

// EditorSkillSSE is the SSE event payload streamed back to the frontend.
// Each SSE data line is a JSON-encoded EditorSkillSSE with a type field:
// "token" for incremental output, "done" for completion, "error" for failures.
// Token uses a pointer so empty-string tokens are still serialised as
// {"type":"token","token":""} — the frontend relies on the type field
// to distinguish events, not on token presence.
type EditorSkillSSE struct {
	Type  string  `json:"type"`
	Token *string `json:"token,omitempty"`
	Error string  `json:"error,omitempty"`
}

// DocumentAISkill streams AI-generated tokens for a document skill via SSE.
//
// Endpoint: POST /api/documents/{id}/ai/skill
// Auth: session-authenticated, workspace membership, editor permission on
//
//	the document's parent folder.
//
// Privacy: strict-ZK folders return 403 (server has no plaintext). The
//
//	LLM client enforces loopback-only endpoints.
//
// Response: text/event-stream with JSON-encoded SSE events.
//
// The frontend renders streamed tokens into a "ghost block" that the user
// can accept (inserting the text into the document) or reject (discarding).
func (h *Handler) DocumentAISkill(w http.ResponseWriter, r *http.Request) {
	if h.documents == nil {
		middleware.RespondError(w, http.StatusServiceUnavailable, middleware.ErrCodeUnsupportedOp, "documents disabled")
		return
	}
	if h.editorSkills == nil {
		middleware.RespondError(w, http.StatusNotImplemented, middleware.ErrCodeUnsupportedOp, "AI skills not configured")
		return
	}

	workspaceID, _ := middleware.WorkspaceIDFromContext(r.Context())
	docID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeBadRequest, "invalid document id")
		return
	}

	// Fetch document metadata + parent folder to check encryption mode.
	doc, parent, err := h.documents.GetMetadata(r.Context(), workspaceID, docID)
	if err != nil {
		writeDocumentError(w, r, err)
		return
	}

	// Strict-ZK folders: server has no plaintext, AI is impossible.
	if parent.EncryptionMode == folder.EncryptionStrictZK {
		middleware.RespondError(w, http.StatusForbidden, middleware.ErrCodeForbidden, "AI skills are not available for zero-knowledge folders")
		return
	}

	// Editor permission required — AI skills modify document content.
	if err := h.assertResourceAccess(r.Context(), permission.ResourceFolder, doc.FolderID, permission.RoleEditor); err != nil {
		writeServiceError(w, r, err)
		return
	}

	// Disabled documents can't be edited.
	if doc.CollabMode == document.CollabModeDisabled {
		middleware.RespondError(w, http.StatusConflict, middleware.ErrCodeConflict, "document editing is disabled")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxEditorSkillBodyBytes)
	var req EditorSkillRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeMalformedJSON, "invalid json body")
		return
	}

	skillID := editor.SkillID(strings.TrimSpace(req.SkillID))
	if skillID == "" {
		middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeBadRequest, "skill_id is required")
		return
	}

	// Pre-validate synchronously before opening the SSE stream so
	// bad-request errors (unknown skill, empty selection, LLM not
	// configured) return proper HTTP status codes instead of being
	// buried in an SSE error event after 200 OK.
	if err := h.editorSkills.Validate(skillID, editor.SkillRequest{
		Selection: req.Selection,
		Context:   req.Context,
		Language:  req.Language,
	}); err != nil {
		switch {
		case errors.Is(err, editor.ErrLLMNotConfigured):
			middleware.RespondError(w, http.StatusNotImplemented, middleware.ErrCodeUnsupportedOp, "AI skills not configured")
		case errors.Is(err, editor.ErrUnknownSkill):
			middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeBadRequest, "unknown skill")
		case errors.Is(err, editor.ErrEmptySelection):
			middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeBadRequest, "selection is required for this skill")
		default:
			middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeBadRequest, err.Error())
		}
		return
	}

	// Acquire a concurrency slot so a single workspace can't
	// exhaust the local LLM's inference capacity. Release on
	// stream completion (success, error, or client disconnect).
	select {
	case h.editorSkillSem <- struct{}{}:
		defer func() { <-h.editorSkillSem }()
	default:
		middleware.RespondError(w, http.StatusTooManyRequests, middleware.ErrCodeRateLimit, "AI skill concurrency limit reached, try again shortly")
		return
	}

	// Set SSE headers and flush immediately so the client knows the
	// stream is open.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	flusher, ok := w.(http.Flusher)
	if !ok {
		// Fallback: write a single error event and return.
		writeSSEEvent(w, EditorSkillSSE{Type: "error", Error: "streaming not supported"})
		return
	}

	// Execute the skill — returns token + error channels.
	tokens, errs := h.editorSkills.Execute(r.Context(), skillID, editor.SkillRequest{
		Selection: req.Selection,
		Context:   req.Context,
		Language:  req.Language,
	})

	// Heartbeat ticker: send an SSE comment frame every 15 seconds
	// so intermediate proxies (nginx, CDN) don't time out the
	// connection during slow LLM cold-start inference. Comment
	// frames (lines starting with ":") are ignored by the
	// frontend's SSE parser but keep the TCP connection alive.
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()

	// Stream tokens as SSE events.
	for {
		select {
		case token, ok := <-tokens:
			if !ok {
				// Token channel closed — drain the error channel
				// with a blocking receive so we never miss an error
				// that was written between close(tokens) and
				// close(errs) in SkillService.Execute.
				if err, ok := <-errs; ok && err != nil {
					writeSSEEvent(w, EditorSkillSSE{Type: "error", Error: err.Error()})
					flusher.Flush()
					return
				}
				writeSSEEvent(w, EditorSkillSSE{Type: "done"})
				flusher.Flush()
				return
			}
			writeSSEEvent(w, EditorSkillSSE{Type: "token", Token: &token})
			flusher.Flush()
		case err, ok := <-errs:
			if ok && err != nil {
				writeSSEEvent(w, EditorSkillSSE{Type: "error", Error: err.Error()})
				flusher.Flush()
			}
			return
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			// SSE comment frame — keeps the connection alive without
			// delivering a data event the frontend would parse.
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}

// writeSSEEvent writes one SSE data line to w. If JSON marshalling
// fails (extremely unlikely for this struct), writes a generic error
// event so the frontend receives a parseable payload rather than
// "data: null\n\n".
func writeSSEEvent(w http.ResponseWriter, event EditorSkillSSE) {
	data, err := json.Marshal(event)
	if err != nil {
		data, _ = json.Marshal(EditorSkillSSE{Type: "error", Error: "internal encoding error"})
	}
	fmt.Fprintf(w, "data: %s\n\n", data)
}

// EditorSkillRunner is the narrow interface the drive handler needs from
// the AI editor skill service. Declared here so the handler depends on
// the contract rather than the concrete *editor.SkillService type.
type EditorSkillRunner interface {
	Validate(skillID editor.SkillID, req editor.SkillRequest) error
	Execute(ctx context.Context, skillID editor.SkillID, req editor.SkillRequest) (<-chan string, <-chan error)
	Model() string
}

// WithEditorSkills wires the AI editor skill service. When non-nil the
// /api/documents/{id}/ai/skill endpoint streams LLM tokens via SSE;
// when nil the endpoint responds 501. Guards against a typed-nil
// concrete pointer (e.g. (*editor.SkillService)(nil)) that would
// compare != nil under the interface and NPE in Execute.
func (h *Handler) WithEditorSkills(s EditorSkillRunner) *Handler {
	if isTypedNil(s) {
		h.editorSkills = nil
		return h
	}
	h.editorSkills = s
	return h
}

// AIFeedbackRequest is the JSON body for POST /api/documents/{id}/ai/feedback.
type AIFeedbackRequest struct {
	SkillID string `json:"skill_id"`
	Rating  string `json:"rating"` // "up" or "down"
	Model   string `json:"model,omitempty"`
}

// DocumentAIFeedback records user feedback (thumbs up/down) on AI skill
// output for quality monitoring. The feedback is stored in the
// ai_skill_feedback table and used by operators to track AI quality
// across model upgrades.
//
// Endpoint: POST /api/documents/{id}/ai/feedback
// Auth: session-authenticated, workspace membership.
func (h *Handler) DocumentAIFeedback(w http.ResponseWriter, r *http.Request) {
	if h.pool == nil {
		middleware.RespondError(w, http.StatusServiceUnavailable, middleware.ErrCodeUnsupportedOp, "database unavailable")
		return
	}

	workspaceID, _ := middleware.WorkspaceIDFromContext(r.Context())
	docID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeBadRequest, "invalid document id")
		return
	}

	// Verify the document exists and belongs to this workspace.
	if _, _, err := h.documents.GetMetadata(r.Context(), workspaceID, docID); err != nil {
		writeDocumentError(w, r, err)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	var req AIFeedbackRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeMalformedJSON, "invalid json body")
		return
	}

	if req.Rating != "up" && req.Rating != "down" {
		middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeBadRequest, "rating must be 'up' or 'down'")
		return
	}
	if strings.TrimSpace(req.SkillID) == "" {
		middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeBadRequest, "skill_id is required")
		return
	}

	userID, _ := middleware.UserIDFromContext(r.Context())

	// Populate model from the skill service if the frontend didn't
	// provide one — the frontend doesn't know which model the server
	// used, so the backend fills it in for quality tracking.
	model := req.Model
	if model == "" && h.editorSkills != nil {
		model = h.editorSkills.Model()
	}

	_, err = h.pool.Exec(r.Context(),
		`INSERT INTO ai_skill_feedback (workspace_id, document_id, user_id, skill_id, rating, model)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		workspaceID, docID, userID, req.SkillID, req.Rating, model,
	)
	if err != nil {
		writeServiceError(w, r, err)
		return
	}

	middleware.WriteJSON(w, http.StatusCreated, map[string]bool{"ok": true})
}

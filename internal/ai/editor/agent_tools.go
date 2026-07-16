package editor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/kennguy3n/zk-drive/internal/collab"
	"github.com/kennguy3n/zk-drive/internal/document"
)

// AgentToolRequest is the input for an agent tool call. The tool
// name determines which fields are required.
type AgentToolRequest struct {
	Tool     string `json:"tool"`
	Position *int   `json:"position,omitempty"`
	Content  string `json:"content,omitempty"`
	BlockID  string `json:"block_id,omitempty"`
	Start    *int   `json:"start,omitempty"`
	End      *int   `json:"end,omitempty"`
}

// AgentToolResult is the output of an agent tool call.
type AgentToolResult struct {
	OK      bool   `json:"ok"`
	Message string `json:"message,omitempty"`
	Outline string `json:"outline,omitempty"`
}

// AgentToolError is returned when a tool call fails validation or
// execution. Callers map these to appropriate HTTP status codes.
type AgentToolError struct {
	Code    int
	Message string
}

func (e *AgentToolError) Error() string { return e.Message }

// ErrAgentToolUnknown is returned when the requested tool name is not
// recognized.
var ErrAgentToolUnknown = &AgentToolError{Code: 400, Message: "unknown agent tool"}

// AgentToolRunner is the interface the API handler depends on for
// executing agent tool calls. Declared here so the handler can be
// tested with a mock.
type AgentToolRunner interface {
	RunTool(ctx context.Context, workspaceID, documentID uuid.UUID, req AgentToolRequest) (*AgentToolResult, error)
}

// AgentToolService implements AgentToolRunner using the collab hub
// and document service to apply Yjs updates through the CRDT path.
//
// The service creates a short-lived AgentClient per tool call,
// applies the update, and closes the client. This is simpler than
// pooling agent clients and sufficient for the expected low
// frequency of agent operations.
type AgentToolService struct {
	hub  *collab.DocumentHub
	docs *document.Service
}

// NewAgentToolService creates an AgentToolService. Both hub and docs
// must be non-nil — the agent needs the hub to broadcast updates and
// the document service to fetch snapshots and persist deltas.
func NewAgentToolService(hub *collab.DocumentHub, docs *document.Service) *AgentToolService {
	return &AgentToolService{hub: hub, docs: docs}
}

// RunTool dispatches the requested tool. Each tool validates input,
// creates an AgentClient, applies the Yjs update, and returns the
// result.
func (s *AgentToolService) RunTool(ctx context.Context, workspaceID, documentID uuid.UUID, req AgentToolRequest) (*AgentToolResult, error) {
	switch req.Tool {
	case "insert_block":
		return s.runInsertBlock(ctx, workspaceID, documentID, req)
	case "replace_block":
		return s.runReplaceBlock(ctx, workspaceID, documentID, req)
	case "delete_block":
		return s.runDeleteBlock(ctx, workspaceID, documentID, req)
	case "get_document_outline":
		return s.runGetOutline(ctx, workspaceID, documentID)
	case "get_block_range":
		return s.runGetBlockRange(ctx, workspaceID, documentID, req)
	case "append_to_block":
		return s.runAppendToBlock(ctx, workspaceID, documentID, req)
	default:
		return nil, ErrAgentToolUnknown
	}
}

func (s *AgentToolService) runInsertBlock(ctx context.Context, workspaceID, documentID uuid.UUID, req AgentToolRequest) (*AgentToolResult, error) {
	if strings.TrimSpace(req.Content) == "" {
		return nil, &AgentToolError{Code: 400, Message: "content is required for insert_block"}
	}
	update, err := buildParagraphInsertUpdate(req.Content, req.Position)
	if err != nil {
		return nil, fmt.Errorf("agent: build insert update: %w", err)
	}
	if err := s.applyAgentUpdate(ctx, workspaceID, documentID, update); err != nil {
		return nil, err
	}
	return &AgentToolResult{OK: true, Message: "block inserted"}, nil
}

func (s *AgentToolService) runReplaceBlock(ctx context.Context, workspaceID, documentID uuid.UUID, req AgentToolRequest) (*AgentToolResult, error) {
	if strings.TrimSpace(req.Content) == "" {
		return nil, &AgentToolError{Code: 400, Message: "content is required for replace_block"}
	}
	if strings.TrimSpace(req.BlockID) == "" {
		return nil, &AgentToolError{Code: 400, Message: "block_id is required for replace_block"}
	}
	update, err := buildBlockReplaceUpdate(req.BlockID, req.Content)
	if err != nil {
		return nil, fmt.Errorf("agent: build replace update: %w", err)
	}
	if err := s.applyAgentUpdate(ctx, workspaceID, documentID, update); err != nil {
		return nil, err
	}
	return &AgentToolResult{OK: true, Message: "block replaced"}, nil
}

func (s *AgentToolService) runDeleteBlock(ctx context.Context, workspaceID, documentID uuid.UUID, req AgentToolRequest) (*AgentToolResult, error) {
	if strings.TrimSpace(req.BlockID) == "" {
		return nil, &AgentToolError{Code: 400, Message: "block_id is required for delete_block"}
	}
	update, err := buildBlockDeleteUpdate(req.BlockID)
	if err != nil {
		return nil, fmt.Errorf("agent: build delete update: %w", err)
	}
	if err := s.applyAgentUpdate(ctx, workspaceID, documentID, update); err != nil {
		return nil, err
	}
	return &AgentToolResult{OK: true, Message: "block deleted"}, nil
}

func (s *AgentToolService) runGetOutline(ctx context.Context, workspaceID, documentID uuid.UUID) (*AgentToolResult, error) {
	snap, err := s.docs.Snapshot(ctx, workspaceID, documentID)
	if err != nil {
		return nil, fmt.Errorf("agent: fetch snapshot for outline: %w", err)
	}
	outline := extractOutlineHeuristic(snap.Document.YState, snap.TailDeltas)
	return &AgentToolResult{OK: true, Outline: outline}, nil
}

func (s *AgentToolService) runGetBlockRange(ctx context.Context, workspaceID, documentID uuid.UUID, req AgentToolRequest) (*AgentToolResult, error) {
	if req.Start == nil || req.End == nil {
		return nil, &AgentToolError{Code: 400, Message: "start and end are required for get_block_range"}
	}
	snap, err := s.docs.Snapshot(ctx, workspaceID, documentID)
	if err != nil {
		return nil, fmt.Errorf("agent: fetch snapshot for block range: %w", err)
	}
	text := extractTextHeuristic(snap.Document.YState, snap.TailDeltas)
	start := *req.Start
	end := *req.End
	if start < 0 {
		start = 0
	}
	if end > len(text) {
		end = len(text)
	}
	if start > end {
		return nil, &AgentToolError{Code: 400, Message: "start must be <= end"}
	}
	return &AgentToolResult{OK: true, Message: text[start:end]}, nil
}

func (s *AgentToolService) runAppendToBlock(ctx context.Context, workspaceID, documentID uuid.UUID, req AgentToolRequest) (*AgentToolResult, error) {
	if strings.TrimSpace(req.Content) == "" {
		return nil, &AgentToolError{Code: 400, Message: "content is required for append_to_block"}
	}
	if strings.TrimSpace(req.BlockID) == "" {
		return nil, &AgentToolError{Code: 400, Message: "block_id is required for append_to_block"}
	}
	update, err := buildAppendToUpdate(req.BlockID, req.Content)
	if err != nil {
		return nil, fmt.Errorf("agent: build append update: %w", err)
	}
	if err := s.applyAgentUpdate(ctx, workspaceID, documentID, update); err != nil {
		return nil, err
	}
	return &AgentToolResult{OK: true, Message: "text appended"}, nil
}

// applyAgentUpdate creates a short-lived AgentClient, applies the
// Yjs update, and closes the client. The hub persists the delta and
// broadcasts to connected editors.
func (s *AgentToolService) applyAgentUpdate(ctx context.Context, workspaceID, documentID uuid.UUID, update []byte) error {
	if len(update) == 0 {
		return errors.New("agent: empty update payload")
	}
	if !looksLikeYjsUpdate(update) {
		return &AgentToolError{Code: http.StatusInternalServerError, Message: "agent: generated payload is not a valid Yjs update"}
	}
	ac, err := collab.NewAgentClient(ctx, s.hub, s.docs, workspaceID, documentID)
	if err != nil {
		return fmt.Errorf("agent: create client: %w", err)
	}
	defer ac.Close()

	if err := ac.ApplyUpdate(ctx, update); err != nil {
		return fmt.Errorf("agent: apply update: %w", err)
	}
	return nil
}

// --- Yjs update builders ---
//
// These builders construct minimal Yjs update payloads. The current
// implementation uses a JSON-based encoding that describes the
// intended operation. A future commit will replace these with proper
// Yjs binary encoding via the existing yjswasm runtime.

type yjsUpdate struct {
	Op       string `json:"op"`
	Content  string `json:"content,omitempty"`
	Position *int   `json:"position,omitempty"`
	BlockID  string `json:"block_id,omitempty"`
}

func buildParagraphInsertUpdate(content string, position *int) ([]byte, error) {
	return json.Marshal(yjsUpdate{
		Op:       "insert_paragraph",
		Content:  content,
		Position: position,
	})
}

func buildBlockReplaceUpdate(blockID, content string) ([]byte, error) {
	return json.Marshal(yjsUpdate{
		Op:      "replace_block",
		BlockID: blockID,
		Content: content,
	})
}

func buildBlockDeleteUpdate(blockID string) ([]byte, error) {
	return json.Marshal(yjsUpdate{
		Op:      "delete_block",
		BlockID: blockID,
	})
}

func buildAppendToUpdate(blockID, content string) ([]byte, error) {
	return json.Marshal(yjsUpdate{
		Op:      "append_to_block",
		BlockID: blockID,
		Content: content,
	})
}

// extractOutlineHeuristic scans the Yjs binary state for heading
// text. Best-effort extraction — a production implementation would
// use the yrs WASM runtime to parse the document structure.
func extractOutlineHeuristic(yState []byte, tail []*document.Delta) string {
	allBytes := make([]byte, len(yState))
	copy(allBytes, yState)
	for _, d := range tail {
		allBytes = append(allBytes, d.Payload...)
	}
	return extractHeadingsFromBytes(allBytes)
}

func extractTextHeuristic(yState []byte, tail []*document.Delta) string {
	allBytes := make([]byte, len(yState))
	copy(allBytes, yState)
	for _, d := range tail {
		allBytes = append(allBytes, d.Payload...)
	}
	return extractReadableText(allBytes)
}

func extractHeadingsFromBytes(data []byte) string {
	var b strings.Builder
	text := extractReadableText(data)
	lines := strings.Split(text, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "# ") {
			b.WriteString("1\t")
			b.WriteString(strings.TrimPrefix(line, "# "))
			b.WriteString("\n")
		} else if strings.HasPrefix(line, "## ") {
			b.WriteString("2\t")
			b.WriteString(strings.TrimPrefix(line, "## "))
			b.WriteString("\n")
		} else if strings.HasPrefix(line, "### ") {
			b.WriteString("3\t")
			b.WriteString(strings.TrimPrefix(line, "### "))
			b.WriteString("\n")
		}
	}
	return b.String()
}

func extractReadableText(data []byte) string {
	var b strings.Builder
	inRun := false
	for _, by := range data {
		if (by >= 0x20 && by < 0x7f) || by >= 0x80 {
			b.WriteByte(by)
			inRun = true
		} else {
			if inRun {
				b.WriteByte('\n')
				inRun = false
			}
		}
	}
	return b.String()
}

// looksLikeYjsUpdate performs a minimal sanity check that the payload
// is a Yjs binary update (Y.encodeUpdate output) and not a JSON
// placeholder or other non-Yjs format. Yjs updates start with a
// struct header byte in the range 0x00–0x0B (update encoder tags),
// while JSON begins with '{' (0x7B) or '[' (0x5B).
func looksLikeYjsUpdate(payload []byte) bool {
	if len(payload) == 0 {
		return false
	}
	first := payload[0]
	if first == '{' || first == '[' || first == '"' {
		return false
	}
	return first <= 0x0B
}

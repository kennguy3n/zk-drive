package editor

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/kennguy3n/zk-drive/internal/ai"
)

// AgentSkillRequest is the input for the multi-step agent skill.
// The LLM receives the document outline + the user's instruction and
// produces a sequence of tool calls that are executed in order.
type AgentSkillRequest struct {
	Instruction string
	Outline     string
	Context     string
}

// AgentSkillResult is the output of a multi-step agent skill run.
type AgentSkillResult struct {
	ToolCalls []AgentToolRequest
	Summary   string
}

// RunAgentSkill executes a multi-step agent skill: it builds a prompt
// from the document outline + user instruction, calls the LLM, parses
// the response as a sequence of tool calls, and executes each one
// via the AgentToolService.
//
// This is designed for 8B models with tool-calling capability. The
// prompt is structured to produce a simple JSON array of tool calls
// that can be parsed without complex reasoning.
//
// ctx controls the overall deadline (LLM call + tool execution).
// The skill is single-pass: one LLM call, then execute the returned
// tool calls in order. No re-planning or verification loop.
func RunAgentSkill(
	ctx context.Context,
	llm ai.LLMClient,
	tools AgentToolRunner,
	workspaceID, documentID uuid.UUID,
	req AgentSkillRequest,
) (*AgentSkillResult, error) {
	if llm == nil {
		return nil, ErrLLMNotConfigured
	}

	prompt := buildAgentSkillPrompt(req)
	response, err := llm.Generate(ctx, prompt)
	if err != nil {
		return nil, fmt.Errorf("agent skill: LLM call failed: %w", err)
	}

	toolCalls, err := parseAgentToolCalls(response)
	if err != nil {
		return nil, fmt.Errorf("agent skill: parse tool calls: %w", err)
	}

	var summary strings.Builder
	fmt.Fprintf(&summary, "Executed %d tool calls", len(toolCalls))

	for i, call := range toolCalls {
		res, err := tools.RunTool(ctx, workspaceID, documentID, call)
		if err != nil {
			return &AgentSkillResult{
				ToolCalls: toolCalls[:i],
				Summary:   fmt.Sprintf("failed at call %d/%d: %v", i+1, len(toolCalls), err),
			}, fmt.Errorf("agent skill: tool %q failed: %w", call.Tool, err)
		}
		if res != nil && res.Message != "" {
			fmt.Fprintf(&summary, "; %s", res.Message)
		}
	}

	return &AgentSkillResult{
		ToolCalls: toolCalls,
		Summary:   summary.String(),
	}, nil
}

// buildAgentSkillPrompt constructs the prompt for the multi-step
// agent skill. The prompt includes:
//   - The system instruction (you are a document editing agent).
//   - The document outline (heading structure, not full text).
//   - The user's instruction.
//   - The available tools and their expected JSON format.
//
// The prompt is designed to produce a JSON array of tool calls that
// parseAgentToolCalls can parse.
func buildAgentSkillPrompt(req AgentSkillRequest) string {
	var b strings.Builder
	b.WriteString("You are a document editing agent. You receive a document outline and an instruction. ")
	b.WriteString("You must produce a JSON array of tool calls to accomplish the instruction. ")
	b.WriteString("Each tool call is a JSON object with a \"tool\" field and optional parameters.\n\n")
	b.WriteString("Available tools:\n")
	b.WriteString("- insert_block: {\"tool\":\"insert_block\",\"content\":\"text\",\"position\":N} — insert a new paragraph\n")
	b.WriteString("- replace_block: {\"tool\":\"replace_block\",\"block_id\":\"old text\",\"content\":\"new text\"} — replace a block\n")
	b.WriteString("- delete_block: {\"tool\":\"delete_block\",\"block_id\":\"text to find\"} — delete a block\n")
	b.WriteString("- append_to_block: {\"tool\":\"append_to_block\",\"block_id\":\"text to find\",\"content\":\"text to append\"} — append text\n")
	b.WriteString("- get_document_outline: {\"tool\":\"get_document_outline\"} — get heading structure\n")
	b.WriteString("- get_block_range: {\"tool\":\"get_block_range\",\"start\":N,\"end\":N} — get text range\n\n")

	if req.Outline != "" {
		b.WriteString("Document outline (level\\theading):\n")
		b.WriteString(req.Outline)
		b.WriteString("\n\n")
	}
	if req.Context != "" {
		b.WriteString("Additional context:\n")
		b.WriteString(req.Context)
		b.WriteString("\n\n")
	}
	b.WriteString("Instruction:\n")
	b.WriteString(req.Instruction)
	b.WriteString("\n\n")
	b.WriteString("Return ONLY a JSON array of tool calls. No explanations, no markdown formatting.\n")
	b.WriteString("Example: [{\"tool\":\"insert_block\",\"content\":\"New paragraph text\"}]\n")
	return b.String()
}

// parseAgentToolCalls parses the LLM's JSON array response into a
// slice of AgentToolRequest. Tolerates leading/trailing whitespace
// and markdown code fences (```json ... ```).
func parseAgentToolCalls(response string) ([]AgentToolRequest, error) {
	response = strings.TrimSpace(response)
	// Strip markdown code fences if present.
	if strings.HasPrefix(response, "```") {
		lines := strings.Split(response, "\n")
		if len(lines) >= 2 {
			lines = lines[1:]
			if len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "```" {
				lines = lines[:len(lines)-1]
			}
			response = strings.Join(lines, "\n")
		}
	}
	response = strings.TrimSpace(response)
	if response == "" {
		return nil, fmt.Errorf("empty response")
	}
	// Find the JSON array boundaries even if the LLM added extra text.
	start := strings.Index(response, "[")
	end := strings.LastIndex(response, "]")
	if start < 0 || end < 0 || end <= start {
		return nil, fmt.Errorf("no JSON array found in response")
	}
	arrayJSON := response[start : end+1]

	var calls []AgentToolRequest
	if err := json.Unmarshal([]byte(arrayJSON), &calls); err != nil {
		return nil, fmt.Errorf("unmarshal tool calls: %w", err)
	}
	return calls, nil
}

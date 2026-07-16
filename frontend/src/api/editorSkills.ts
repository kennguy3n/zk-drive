import { currentToken } from "./client";

// EditorSkillRequest matches the Go EditorSkillRequest struct.
export interface EditorSkillRequest {
  skill_id: string;
  selection: string;
  context?: string;
  language?: string;
}

// EditorSkillSSEEvent matches the Go EditorSkillSSE struct.
export interface EditorSkillSSEEvent {
  type: "token" | "done" | "error";
  token?: string;
  error?: string;
}

// Skill IDs — kept in sync with internal/ai/editor/skills.go.
export const SKILL_IDS = [
  "improve_writing",
  "summarize",
  "expand",
  "simplify",
  "translate",
  "generate_ideas",
  "continue_writing",
  "fix_grammar",
  "change_tone",
  "generate_heading",
  "extract_action_items",
  "ask_document",
] as const;

export type SkillID = (typeof SKILL_IDS)[number];

// streamEditorSkill calls POST /api/documents/{id}/ai/skill and streams
// SSE events via callbacks. Returns an AbortController so the caller can
// cancel the stream (e.g. when the user rejects a ghost block).
//
// We use fetch + ReadableStream rather than EventSource because the
// endpoint requires a POST body (EventSource only supports GET).
export function streamEditorSkill(
  documentId: string,
  req: EditorSkillRequest,
  callbacks: {
    onToken: (token: string) => void;
    onDone: () => void;
    onError: (error: string) => void;
  },
): AbortController {
  const controller = new AbortController();
  const token = currentToken();

  const headers: Record<string, string> = {
    "Content-Type": "application/json",
  };
  if (token) {
    headers["Authorization"] = `Bearer ${token}`;
  }

  fetch(`/api/documents/${documentId}/ai/skill`, {
    method: "POST",
    headers,
    body: JSON.stringify(req),
    signal: controller.signal,
  })
    .then(async (resp) => {
      if (!resp.ok) {
        const text = await resp.text().catch(() => "");
        callbacks.onError(`HTTP ${resp.status}: ${text}`);
        return;
      }
      const reader = resp.body?.getReader();
      if (!reader) {
        callbacks.onError("No response body");
        return;
      }
      const decoder = new TextDecoder();
      let buffer = "";
      for (;;) {
        const { done, value } = await reader.read();
        if (done) break;
        buffer += decoder.decode(value, { stream: true });
        // SSE events are separated by \n\n
        const parts = buffer.split("\n\n");
        buffer = parts.pop() ?? "";
        for (const part of parts) {
          const line = part.trim();
          if (!line.startsWith("data: ")) continue;
          const jsonStr = line.slice(6);
          try {
            const event: EditorSkillSSEEvent = JSON.parse(jsonStr);
            if (event.type === "token" && event.token !== undefined) {
              callbacks.onToken(event.token);
            } else if (event.type === "done") {
              callbacks.onDone();
            } else if (event.type === "error" && event.error) {
              callbacks.onError(event.error);
            }
          } catch {
            // Ignore malformed SSE lines — the stream may include
            // partial chunks during reconnection.
          }
        }
      }
      // Process any remaining buffered data.
      if (buffer.trim().startsWith("data: ")) {
        try {
          const event: EditorSkillSSEEvent = JSON.parse(buffer.trim().slice(6));
          if (event.type === "done") callbacks.onDone();
          else if (event.type === "error") callbacks.onError(event.error ?? "unknown error");
        } catch {
          // Ignore partial data.
        }
      }
    })
    .catch((err) => {
      if (err.name === "AbortError") return;
      callbacks.onError(err.message ?? "Network error");
    });

  return controller;
}

export async function submitAIFeedback(
  documentId: string,
  req: { skill_id: string; rating: "up" | "down" },
): Promise<void> {
  const token = currentToken();
  await fetch(`/api/documents/${documentId}/ai/feedback`, {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
      ...(token ? { Authorization: `Bearer ${token}` } : {}),
    },
    body: JSON.stringify(req),
  });
}

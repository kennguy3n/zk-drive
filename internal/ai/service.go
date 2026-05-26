// Package ai hosts thread/room summary services for ZK Drive.
//
// Two paths produce a summary:
//
//  1. The rule-based scaffold (Summarize -> ruleBasedSummary). It
//     assembles a fixed-format string from folder file names and any
//     indexed content_text. Always available; never makes network
//     calls; the answer of last resort.
//
//  2. The local LLM path (Summarize -> llm.Generate, when
//     SummaryService.WithLLM has been called and the daemon is
//     reachable). On any error we silently fall back to the
//     scaffold so the user-facing endpoint never 5xx's.
//
// zk-drive's privacy posture forbids sending file content to an
// external API regardless of operator opt-in. The LLM client
// constructors enforce that the configured endpoint is loopback or
// a private address — see internal/ai/llm.go.
package ai

import (
	"context"
	"errors"
	"fmt"

	"strings"
	"time"

	"github.com/kennguy3n/zk-drive/internal/logging"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/zk-drive/internal/folder"
	"github.com/kennguy3n/zk-drive/internal/typednil"
)

// ErrStrictZKForbidden is returned when Summarize is asked to produce
// a summary for a strict-zero-knowledge folder. The server never has
// access to plaintext for those folders, so honestly there is nothing
// we could summarise; we surface the refusal explicitly so callers
// (handlers) can map it to 403 rather than returning an empty string.
var ErrStrictZKForbidden = errors.New("ai: summary not available for strict-zk folders")

// ErrFolderNotFound is returned when the folder row cannot be
// located. Handlers map this to 404 to match the regular drive API.
var ErrFolderNotFound = errors.New("ai: folder not found")

// maxFilesPerSummary caps the number of files the scaffold considers.
// The cap keeps the generated string bounded; real LLM wiring will
// revisit this alongside token-budgeting.
const maxFilesPerSummary = 50

// previewBytesPerFile caps how many characters of content_text each
// file contributes to the preview buffer.
const previewBytesPerFile = 500

// previewBytesTotal caps the number of characters from the joined
// file previews that make it into the summary string.
const previewBytesTotal = 200

// llmTimeout caps how long we wait for the local model before
// falling back to the scaffold. 15 s is comfortable for Qwen2.5-1.5B
// on a CPU-only host (typical first-token latency 2–4 s, 50–80
// tokens/s) while keeping the user-facing /summary endpoint snappy.
const llmTimeout = 15 * time.Second

// SummaryService produces a textual summary of a folder's contents.
// The pool reads files + folders. The optional llm field, when set
// via WithLLM, enables a local on-device model (Ollama-compatible)
// to produce richer summaries; on any error we silently fall back
// to the rule-based scaffold so the endpoint never 5xx's.
//
// languageResolver, when non-nil, returns the workspace's preferred
// human-language (mapped through workspace.SearchLanguage). The
// LLM path threads it through BuildSummaryPrompt so summaries
// come back in the workspace's working language; a nil resolver
// (legacy wiring) means English instructions. The resolver runs
// in the request path and is allowed to do a DB lookup — the
// workspace.Service caches it.
type SummaryService struct {
	pool             *pgxpool.Pool
	llm              LLMClient
	languageResolver WorkspaceLanguageResolver
}

// WorkspaceLanguageResolver returns the workspace's preferred
// human-language search dictionary (the value that the search
// FTS path also keys off). Implemented by *workspace.Service
// upstream; declared as an interface here so the ai package
// doesn't depend on workspace and to keep unit tests free of a
// real database.
type WorkspaceLanguageResolver interface {
	GetSearchLanguage(ctx context.Context, workspaceID uuid.UUID) (string, error)
}

// resolveWorkspaceLanguage looks up the workspace's preferred FTS
// language via resolver, returning "" so PromptLanguageFor falls
// back to English on a nil resolver, a transient lookup error, or
// an empty stored value. Errors are logged at warn (not returned)
// so AI endpoints' contract of "never 5xx because of a workspace-
// language lookup hiccup" stays intact across all three services
// (Summary, Suggestion, Expansion). logScope is the breadcrumb
// prefix in the warn message — keeps the three endpoints
// distinguishable in operator logs while sharing a single
// implementation.
//
// Devin Review ANALYSIS_0005 on commit b4b41dd flagged the
// previous per-service duplication (SummaryService.resolveLanguage,
// SuggestionService.resolveLanguage, ExpansionService.resolveLanguage
// all had identical bodies). Extracting here collapses three
// near-copies into one canonical implementation; the call sites
// pass their own resolver and log scope.
func resolveWorkspaceLanguage(ctx context.Context, resolver WorkspaceLanguageResolver, workspaceID uuid.UUID, logScope string) string {
	if resolver == nil {
		return ""
	}
	lang, err := resolver.GetSearchLanguage(ctx, workspaceID)
	if err != nil {
		logging.FromContext(ctx).Warn(logScope+": resolve workspace language failed (defaulting to English)",
			"workspace_id", workspaceID, "err", err)
		return ""
	}
	return lang
}

// NewSummaryService returns a SummaryService bound to pool. A nil pool
// is treated as a misconfiguration and panics on first use rather
// than silently returning empty summaries.
func NewSummaryService(pool *pgxpool.Pool) *SummaryService {
	return &SummaryService{pool: pool}
}

// WithLLM wires an on-device LLM client. When the client errors on
// a given request (daemon unreachable, model unavailable, decode
// failure) the service transparently falls back to the rule-based
// scaffold — callers see a successful 200 with a non-empty summary
// either way.
//
// The typed-nil guard mirrors the handler-level With* setters in
// api/drive (WithTagSuggester / WithQueryExpander / WithWebhooks /
// WithPreviews). A (*OllamaClient)(nil) wrapped in the LLMClient
// interface would slip past the s.llm != nil check inside Summarize
// and NPE at first Generate() call. Production wires from
// cmd/server/main.go after a successful NewOllamaClient that
// guarantees non-nil, but the guard keeps the contract honest for
// future wiring sites (alt deployments, test fakes). Devin Review
// ANALYSIS_0006 on commit 348b13d flagged the asymmetry; the
// architecturally correct fix was extracting isTypedNil to
// internal/typednil so both api/drive and internal/ai share one
// guard.
func (s *SummaryService) WithLLM(c LLMClient) *SummaryService {
	if typednil.IsTypedNil(c) {
		s.llm = nil
		return s
	}
	s.llm = c
	return s
}

// WithLanguageResolver wires the workspace search-language
// resolver so the LLM prompt can be localised to the workspace's
// preferred language. A nil resolver is treated as "English
// fallback" so legacy wiring keeps working unchanged. Returns the
// service to support the fluent setup pattern used elsewhere.
//
// Same typed-nil guard rationale as WithLLM above — a typed-nil
// concrete resolver wrapped in the WorkspaceLanguageResolver
// interface would pass resolveWorkspaceLanguage's `if resolver ==
// nil` check at internal/ai/service.go:115 and NPE on
// GetSearchLanguage. Devin Review ANALYSIS_0006 on commit 348b13d.
func (s *SummaryService) WithLanguageResolver(r WorkspaceLanguageResolver) *SummaryService {
	if typednil.IsTypedNil(r) {
		s.languageResolver = nil
		return s
	}
	s.languageResolver = r
	return s
}

// Summarize produces a summary of the files currently in folderID.
// Strict-ZK folders short-circuit with ErrStrictZKForbidden — the
// server has no plaintext for them by design. Managed folders try
// the configured local LLM first (when WithLLM has been called) and
// fall back to the rule-based scaffold on any error so the endpoint
// never 5xx's.
func (s *SummaryService) Summarize(ctx context.Context, workspaceID, folderID uuid.UUID) (string, error) {
	if s.pool == nil {
		return "", errors.New("ai: summary service not configured")
	}

	var mode, folderName string
	err := s.pool.QueryRow(ctx,
		`SELECT encryption_mode, name FROM folders WHERE id = $1 AND workspace_id = $2 AND deleted_at IS NULL`,
		folderID, workspaceID).Scan(&mode, &folderName)
	if err != nil {
		// Only pgx.ErrNoRows is a legitimate 404 (folder doesn't
		// exist in this workspace or is soft-deleted). Other errors
		// (connection blips, timeouts, syntax bugs) must surface as
		// 500 via the kchat handler's default arm so transient DB
		// trouble doesn't masquerade as 'folder not found' to the
		// client. Standard repository pattern — see e.g.
		// internal/workspace/repository.go:65,
		// internal/sharing/repository.go:66. Devin Review
		// ANALYSIS_0003 on PR #85 flagged this pre-existing
		// pattern (it was first introduced before WS6 landed but
		// fixing it alongside the autotag.go change keeps the two
		// QueryRow call sites consistent).
		if errors.Is(err, pgx.ErrNoRows) {
			return "", fmt.Errorf("%w: %v", ErrFolderNotFound, err)
		}
		return "", fmt.Errorf("ai: load folder for summary: %w", err)
	}
	if mode == folder.EncryptionStrictZK {
		return "", ErrStrictZKForbidden
	}

	names, preview, err := s.gatherFileContext(ctx, workspaceID, folderID)
	if err != nil {
		return "", err
	}

	// Resolve workspace language once per request. A failure here
	// is non-fatal — the fallback PromptLanguageFor("") returns
	// English instructions, so summarisation degrades gracefully
	// rather than 5xx-ing on a transient DB hiccup.
	language := resolveWorkspaceLanguage(ctx, s.languageResolver, workspaceID, "ai summary")

	if s.llm != nil {
		if out, llmErr := s.tryLLM(ctx, folderName, names, preview, language); llmErr == nil {
			return out, nil
		} else {
			// Operator-visible breadcrumb so a misconfigured
			// daemon doesn't silently degrade behaviour
			// forever. Intentionally one log line per
			// fallback — we expect this to be rare in
			// production.
			logging.FromContext(ctx).Error("ai summary local LLM failed, falling back to scaffold", "model", s.llm.Model(), "err", llmErr)
		}
	}

	return ruleBasedSummary(names, preview), nil
}

// gatherFileContext loads up to maxFilesPerSummary file rows from
// folderID and returns the file names plus a bounded preview buffer.
// Split out so both the LLM and scaffold paths see the exact same
// inputs.
func (s *SummaryService) gatherFileContext(ctx context.Context, workspaceID, folderID uuid.UUID) ([]string, string, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT name, COALESCE(content_text, '')
		 FROM files
		 WHERE folder_id = $1 AND workspace_id = $2 AND deleted_at IS NULL
		 ORDER BY created_at ASC
		 LIMIT $3`,
		folderID, workspaceID, maxFilesPerSummary)
	if err != nil {
		return nil, "", fmt.Errorf("ai: query files: %w", err)
	}
	defer rows.Close()

	names := make([]string, 0, maxFilesPerSummary)
	var previewBuf strings.Builder
	for rows.Next() {
		var name, content string
		if err := rows.Scan(&name, &content); err != nil {
			return nil, "", fmt.Errorf("ai: scan files: %w", err)
		}
		names = append(names, name)
		if content != "" {
			// truncatePreview cuts on a rune boundary so a multi-byte
			// glyph at the byte boundary can't become invalid UTF-8
			// in the LLM prompt — relevant now that WS6 ships
			// multilingual prompting and non-ASCII content_text is
			// expected on French/German/Chinese/etc. workspaces.
			previewBuf.WriteString(truncatePreview(content, previewBytesPerFile))
			previewBuf.WriteString(" ")
		}
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("ai: iterate files: %w", err)
	}

	preview := truncatePreview(strings.TrimSpace(previewBuf.String()), previewBytesTotal)
	return names, preview, nil
}

// tryLLM asks the configured local model for a summary. The bounded
// context (llmTimeout) is derived from ctx so callers cancelling the
// request still cancel inflight LLM work.
func (s *SummaryService) tryLLM(ctx context.Context, folderName string, names []string, preview string, language string) (string, error) {
	llmCtx, cancel := context.WithTimeout(ctx, llmTimeout)
	defer cancel()
	out, err := s.llm.Generate(llmCtx, BuildSummaryPrompt(folderName, names, preview, language))
	if err != nil {
		return "", err
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return "", errors.New("ai: llm returned empty summary")
	}
	return out, nil
}

// ruleBasedSummary is the deterministic fallback. Format is held
// stable — integration tests assert it shape-wise — so a flaky LLM
// daemon doesn't break gate tests like
// TestThreadSummaryRespectsEncryptionMode.
func ruleBasedSummary(names []string, preview string) string {
	return fmt.Sprintf("Room contains %d files: [%s]. Content preview: [%s]",
		len(names), strings.Join(names, ", "), preview)
}

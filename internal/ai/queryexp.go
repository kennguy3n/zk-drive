// queryexp.go — query expansion for the multilingual search endpoint.
//
// Two-tier pipeline, mirroring autotag.go:
//
//  1. Rule-based expansion (always runs, deterministic, cheap):
//       - Tokenise the query into letter+digit runs (>= 3 runes).
//       - Match each token against the workspace's existing tag
//         vocabulary using prefix equality and hyphen-aware fuzzy
//         (e.g. "marketing" matches the tag "marketing-q4-2024").
//       - Return tags whose match-strength exceeds a small threshold,
//         capped at queryExpansionMaxTerms.
//       - All output goes through canonicalTag so the suggested
//         expansion is a valid tag string that can be fed back into
//         the search endpoint without further normalisation.
//
//  2. Optional LLM refinement (only when the LLM is configured):
//       - Sends a multilingual-aware prompt asking the model to list
//         5-10 synonyms / related concepts the user might have meant.
//       - Output is normalised through the same canonicalTag pipeline
//         so the LLM's output cannot bypass validation. We DON'T
//         feed the LLM output back into search SQL — we return it as
//         the expansion list so the frontend can show the user "your
//         query also matches these terms" UI.
//
// Privacy invariant: the only network egress in this file is through
// SuggestionService.llm, which is gated by NewOllamaClient's loopback-
// or-private-IP enforcement at construction time. Workspace tags are
// read directly from Postgres via the same pool the rest of the
// service uses. No third-party API calls; no telemetry.
package ai

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/zk-drive/internal/logging"
)

// queryExpansionMaxTerms caps the number of expansion terms returned
// to the client. 8 is a balance between "useful suggestion set" and
// "user has to scan a wall of synonyms"; the frontend can render the
// full list as a horizontal chip strip.
const queryExpansionMaxTerms = 8

// queryExpansionMinRune is the minimum rune length for a query
// token to participate in tag matching. Two is the practical floor
// for English search queries — short noun-tags like "q4", "ai",
// "bi" are common and we want them to participate. Below that
// (single character) the match space explodes (e.g. "a" matches
// nearly every tag), so we set the floor at 2.
const queryExpansionMinRune = 2

// queryExpansionLLMMaxFile is the size of the workspace-tag preview
// we feed to the LLM in the prompt. 1 KiB is enough for a few
// dozen tag strings; the LLM is meant to derive synonyms from the
// query + tag vocabulary, not from file content.
const queryExpansionLLMMaxFile = 1024

// ExpansionResult is the structured response from QueryExpansion.
// Terms holds the rule-based + LLM-merged expansion list, ordered
// by descending confidence (rule-based first, LLM second). LLMUsed
// flags whether the LLM tier ran — useful for the frontend to show
// "AI-assisted" affordance only when it actually contributed.
type ExpansionResult struct {
	Terms    []string `json:"terms"`
	LLMUsed  bool     `json:"llm_used"`
	Language string   `json:"language"`
}

// ExpansionService produces query expansions for a search request.
// The pool is used for workspace tag lookups; the optional llm
// field, when set via WithLLM, adds an on-device refinement pass.
type ExpansionService struct {
	pool             *pgxpool.Pool
	llm              LLMClient
	languageResolver WorkspaceLanguageResolver
}

// NewExpansionService returns an ExpansionService bound to pool.
func NewExpansionService(pool *pgxpool.Pool) *ExpansionService {
	return &ExpansionService{pool: pool}
}

// WithLLM wires an on-device LLM client. Behavior parallels
// SummaryService.WithLLM and SuggestionService.WithLLM — any
// client error degrades to the rule-based path only.
func (s *ExpansionService) WithLLM(c LLMClient) *ExpansionService {
	s.llm = c
	return s
}

// WithLanguageResolver wires the workspace search-language resolver
// so the LLM prompt can be localised. Same pattern as the
// SummaryService / SuggestionService wirings.
func (s *ExpansionService) WithLanguageResolver(r WorkspaceLanguageResolver) *ExpansionService {
	s.languageResolver = r
	return s
}

// ExpandResult returns an ExpansionResult for query within
// workspaceID. The result always contains at least the rule-based
// expansion (which may be empty for workspaces with no tags), never
// errors on LLM failures (logged + degraded silently), and never
// blocks longer than llmTimeout on the LLM stage.
//
// Callers that prefer separate return values (e.g. the drive HTTP
// handler, which doesn't import internal/ai for transport types)
// can use Expand instead — it's a thin tuple adapter over this
// method.
func (s *ExpansionService) ExpandResult(ctx context.Context, workspaceID uuid.UUID, query string) (*ExpansionResult, error) {
	if s.pool == nil {
		return nil, errors.New("ai: expansion service not configured")
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return &ExpansionResult{Terms: nil}, nil
	}

	tagRows, err := s.pool.Query(ctx, `
SELECT DISTINCT tag FROM file_tags
WHERE workspace_id = $1
ORDER BY tag ASC
LIMIT 512`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("ai: load workspace tags for expansion: %w", err)
	}
	defer tagRows.Close()
	var workspaceTags []string
	for tagRows.Next() {
		var t string
		if err := tagRows.Scan(&t); err != nil {
			return nil, fmt.Errorf("ai: scan workspace tag: %w", err)
		}
		workspaceTags = append(workspaceTags, t)
	}
	if err := tagRows.Err(); err != nil {
		return nil, fmt.Errorf("ai: iterate workspace tags: %w", err)
	}

	language := s.resolveLanguage(ctx, workspaceID)
	res := &ExpansionResult{
		Terms:    ruleBasedExpansion(query, workspaceTags),
		Language: language,
	}

	if s.llm != nil {
		llmTerms, llmErr := s.tryLLMExpansion(ctx, query, workspaceTags, language)
		if llmErr != nil {
			logging.FromContext(ctx).Error("ai query expand: local LLM failed, returning rule-based scaffold only",
				"model", s.llm.Model(), "err", llmErr)
		} else {
			res.Terms = mergeSuggestions(res.Terms, llmTerms)
			res.LLMUsed = true
		}
	}

	if len(res.Terms) > queryExpansionMaxTerms {
		res.Terms = res.Terms[:queryExpansionMaxTerms]
	}
	return res, nil
}

func (s *ExpansionService) resolveLanguage(ctx context.Context, workspaceID uuid.UUID) string {
	if s.languageResolver == nil {
		return ""
	}
	lang, err := s.languageResolver.GetSearchLanguage(ctx, workspaceID)
	if err != nil {
		logging.FromContext(ctx).Warn("ai query expand: resolve workspace language failed (defaulting to English)",
			"workspace_id", workspaceID, "err", err)
		return ""
	}
	return lang
}

// Expand is the tuple-returning adapter over ExpandResult.
// Callers that prefer separate return values (typically transport
// layers that don't want to import internal/ai for the
// ExpansionResult type) use this. The semantics are identical to
// ExpandResult — same error handling, same LLM fallback, same caps.
func (s *ExpansionService) Expand(ctx context.Context, workspaceID uuid.UUID, query string) (terms []string, llmUsed bool, language string, err error) {
	res, e := s.ExpandResult(ctx, workspaceID, query)
	if e != nil {
		return nil, false, "", e
	}
	return res.Terms, res.LLMUsed, res.Language, nil
}

func (s *ExpansionService) tryLLMExpansion(ctx context.Context, query string, workspaceTags []string, language string) ([]string, error) {
	llmCtx, cancel := context.WithTimeout(ctx, llmTimeout)
	defer cancel()
	out, err := s.llm.Generate(llmCtx, BuildQueryExpansionPrompt(query, workspaceTags, language))
	if err != nil {
		return nil, err
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return nil, errors.New("ai: llm returned empty query expansion")
	}
	return parseTagLines(out), nil
}

// BuildQueryExpansionPrompt is the LLM prompt for query expansion.
// Exposed so tests can pin the wording and operators can inspect the
// exact text sent to the on-device model. The user content half
// passes through unchanged — only the system instruction half is
// localised via PromptLanguageFor.
//
// The instruction emphasises "synonyms and related terms" rather
// than "rewrite the query", because the response is presented to
// the user as a list of suggested expansions, not auto-applied to
// search SQL. A model that hallucinates a near-but-wrong synonym
// (e.g. "vitamin" for "supplement") is corrected by the user's
// click selection — they only confirm the ones that are actually
// related.
func BuildQueryExpansionPrompt(query string, workspaceTags []string, language string) string {
	lang := PromptLanguageFor(language)
	var b strings.Builder
	b.WriteString("You are expanding a search query for a private team workspace. ")
	b.WriteString(lang.Instruction)
	b.WriteString(" ")
	b.WriteString("Return between 3 and 8 short related terms or synonyms, one per line, ")
	b.WriteString("lowercase, hyphen-joined (e.g. quarterly-report, 2024-q4). ")
	b.WriteString("Do NOT prefix with #. Do NOT add commas. Do NOT include explanations. ")
	b.WriteString("Do NOT repeat the original query verbatim. ")
	b.WriteString("Prefer terms that exist in the workspace tag list when they fit the query.\n\n")
	b.WriteString("Search query: ")
	b.WriteString(query)
	b.WriteString("\n")
	if len(workspaceTags) > 0 {
		// truncatePreview keeps the cut on a rune boundary so a
		// multi-byte tag at the boundary (CJK, accented Latin)
		// can't get sliced into invalid UTF-8 — same rationale
		// as the autotag prompt builder.
		preview := truncatePreview(strings.Join(workspaceTags, ", "), queryExpansionLLMMaxFile)
		b.WriteString("Existing workspace tags: ")
		b.WriteString(preview)
		b.WriteString("\n")
	}
	b.WriteString("\n")
	b.WriteString(lang.AnswerInLanguage)
	b.WriteString("\nRelated terms:")
	return b.String()
}

// ruleBasedExpansion derives expansion terms from the workspace's
// existing tag vocabulary. The algorithm:
//
//  1. Tokenise query into letter+digit runs >= queryExpansionMinRune.
//  2. For each token, score every workspace tag by:
//       +3 if the tag equals the token
//       +2 if the tag contains the token as a hyphen-bounded segment
//          (e.g. "q4" matches "q4-2024" but not "iq40")
//       +1 if the tag contains the token as a substring
//  3. Sum scores across all tokens to get a per-tag total.
//  4. Return tags with total > 0, sorted by (score desc, tag asc).
//
// The bias toward hyphen-bounded matches means short-noun queries
// don't pull in tangentially related tags ("q4" doesn't pull in
// "iq40-survey") while still letting genuine substring matches
// surface ("market" pulls in "marketing-2024" via the +1 substring
// path).
func ruleBasedExpansion(query string, workspaceTags []string) []string {
	tokens := extractExpansionTokens(query)
	if len(tokens) == 0 || len(workspaceTags) == 0 {
		return nil
	}
	scores := make(map[string]int, len(workspaceTags))
	for _, tag := range workspaceTags {
		if tag == "" {
			continue
		}
		ltag := strings.ToLower(tag)
		for _, tok := range tokens {
			switch {
			case ltag == tok:
				scores[tag] += 3
			case containsHyphenSegment(ltag, tok):
				scores[tag] += 2
			case strings.Contains(ltag, tok):
				scores[tag]++
			}
		}
	}
	if len(scores) == 0 {
		return nil
	}

	type kv struct {
		tag string
		n   int
	}
	pairs := make([]kv, 0, len(scores))
	for k, v := range scores {
		if v <= 0 {
			continue
		}
		pairs = append(pairs, kv{k, v})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].n != pairs[j].n {
			return pairs[i].n > pairs[j].n
		}
		return pairs[i].tag < pairs[j].tag
	})
	if len(pairs) > queryExpansionMaxTerms {
		pairs = pairs[:queryExpansionMaxTerms]
	}
	out := make([]string, 0, len(pairs))
	for _, p := range pairs {
		t := canonicalTag(p.tag)
		if t != "" {
			out = append(out, t)
		}
	}
	return out
}

// extractExpansionTokens splits query into a normalised set of
// tokens — letter+digit runs, lowercased, >= queryExpansionMinRune
// long. Dedupes per call so a query like "report report 2024"
// counts as ("report","2024"), not ("report","report","2024"). The
// tokens are NOT filtered by part-of-speech (no stopword list);
// short noise words are dropped by the rune-count floor.
func extractExpansionTokens(query string) []string {
	if query == "" {
		return nil
	}
	out := make([]string, 0, 4)
	seen := make(map[string]struct{}, 4)
	var token strings.Builder
	flush := func() {
		if token.Len() == 0 {
			return
		}
		t := strings.ToLower(token.String())
		token.Reset()
		if len([]rune(t)) < queryExpansionMinRune {
			return
		}
		if _, dup := seen[t]; dup {
			return
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	for _, r := range query {
		switch {
		case unicode.IsLetter(r), unicode.IsDigit(r):
			token.WriteRune(r)
		default:
			flush()
		}
	}
	flush()
	return out
}

// containsHyphenSegment reports whether s contains token as a
// segment bounded by hyphens or string-ends. Used to give "q4"
// inside "q4-2024" a higher score than "q4" inside "iq40".
func containsHyphenSegment(s, token string) bool {
	if s == token {
		return true
	}
	if strings.HasPrefix(s, token+"-") {
		return true
	}
	if strings.HasSuffix(s, "-"+token) {
		return true
	}
	return strings.Contains(s, "-"+token+"-")
}

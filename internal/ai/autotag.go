// autotag.go — suggests tags for a file based on its name + extracted
// text. Two-tier pipeline:
//
//  1. Rule-based scaffold (always runs):
//       - file-extension → canonical doc-type tag (pdf, spreadsheet,
//         presentation, image, markdown, …) so every file gets at
//         least one structural tag with zero AI cost.
//       - keyword extraction on the file name + content_text using
//         document frequency (term must occur in the file but be
//         long enough to be specific) and a workspace-tag overlap
//         pass so an existing "q4-2024" tag gets re-suggested for a
//         file whose body mentions "Q4 2024".
//       - normalised through file.normalizeTag-equivalent rules so the
//         output is directly addable via file.Service.AddTag without
//         a second validation pass.
//
//  2. Optional LLM refinement (only when the SummaryService has a
//     bound LLMClient and SuggestionService.WithLLM has been wired):
//       - Sends a tightly-scoped prompt that asks for 3-8 lowercase
//         hyphenated tags, returns one-tag-per-line. We DELIBERATELY
//         post-process the LLM output through the same normaliser so
//         a hallucinated `/foo%bar` token cannot bypass the
//         validation that file.Service.AddTag would otherwise apply.
//       - LLM output is MERGED with the rule-based output, not
//         replaced — the rule-based tags are deterministic and
//         operator-trustworthy; the LLM is a quality booster, not the
//         floor.
//
// The endpoint is suggest-only: it never writes tags to the file
// table. The frontend presents the suggestions; the user confirms,
// which then calls the existing POST /files/{id}/tags handler. This
// keeps the LLM's output strictly advisory — no auto-write means an
// adversarial prompt or a confused model can't poison a workspace's
// tag taxonomy.
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

	"github.com/kennguy3n/zk-drive/internal/file"
	"github.com/kennguy3n/zk-drive/internal/folder"
	"github.com/kennguy3n/zk-drive/internal/logging"
)

// ErrTagSuggestUnavailable is returned when the file is in a folder
// the AI subsystem cannot read (strict-ZK). Handlers should map to
// 409 Conflict so the frontend can hide the "Suggest tags" button
// in that mode.
var ErrTagSuggestUnavailable = errors.New("ai: tag suggestion not available for strict-zk content")

// ErrTagSuggestFileNotFound is returned when the requested file
// doesn't exist in the workspace (or has been soft-deleted). Maps
// to 404 in the handler.
var ErrTagSuggestFileNotFound = errors.New("ai: file not found for tag suggestion")

// tagSuggestMaxFile is the LLM-prompt budget cap on the file body
// preview. 4 KiB matches typical small-model context budgets after
// allowing room for the system prompt + few-shot exemplars.
const tagSuggestMaxFile = 4 * 1024

// tagSuggestMaxOutput is the upper bound on suggestions returned to
// the client. Frontend will typically display 6-8; we cap at 12 so
// the response stays small and an aggressive LLM can't flood the
// list.
const tagSuggestMaxOutput = 12

// tagKeywordMinRuneCount filters out very short tokens during
// keyword extraction. "the", "and", "for" etc. are short enough to
// be dominated by structural words; demanding >=4 runes drops most
// stopwords without an explicit stopword list (which would need to
// be maintained per language).
const tagKeywordMinRuneCount = 4

// tagKeywordMaxRuneCount caps the longest single-word tag. A
// 32-rune cap matches the typical "compound noun" limit (e.g.
// "infrastructure-engineering") and keeps tag rows in the file_tags
// table narrow.
const tagKeywordMaxRuneCount = 32

// extensionTags maps lowercased file-extensions onto the canonical
// doc-type tag. The map values are deliberately abstract rather than
// extension-specific ("spreadsheet" not "xlsx") so the same tag
// surfaces across the variants (ods, xls, xlsm, csv all → spreadsheet).
// Listed alphabetically by extension for grep-ability.
var extensionTags = map[string]string{
	".csv":  "spreadsheet",
	".doc":  "document",
	".docx": "document",
	".gif":  "image",
	".html": "webpage",
	".htm":  "webpage",
	".jpeg": "image",
	".jpg":  "image",
	".json": "data",
	".md":   "markdown",
	".odp":  "presentation",
	".ods":  "spreadsheet",
	".odt":  "document",
	".pdf":  "pdf",
	".png":  "image",
	".ppt":  "presentation",
	".pptx": "presentation",
	".rtf":  "document",
	".svg":  "image",
	".tsv":  "spreadsheet",
	".txt":  "text",
	".webp": "image",
	".xls":  "spreadsheet",
	".xlsm": "spreadsheet",
	".xlsx": "spreadsheet",
	".xml":  "data",
	".yaml": "data",
	".yml":  "data",
}

// SuggestionService produces tag suggestions for a file based on
// its name + content_text and the workspace's existing tags. The
// pool reads files + tags; the optional llm field enables LLM
// refinement when the SummaryService has been wired with a model.
// languageResolver localises the LLM prompt to the workspace's
// preferred language (same wiring as SummaryService).
type SuggestionService struct {
	pool             *pgxpool.Pool
	llm              LLMClient
	languageResolver WorkspaceLanguageResolver
}

// NewSuggestionService returns a SuggestionService bound to pool.
// Wiring is mirror-image of SummaryService so cmd/server/main.go
// can call the same With* setters without learning a second
// pattern.
func NewSuggestionService(pool *pgxpool.Pool) *SuggestionService {
	return &SuggestionService{pool: pool}
}

// WithLLM wires an on-device LLM client. As with SummaryService,
// any client error causes the LLM stage to be skipped; the rule-
// based scaffold's output is still returned (never empty).
func (s *SuggestionService) WithLLM(c LLMClient) *SuggestionService {
	s.llm = c
	return s
}

// WithLanguageResolver wires the workspace search-language resolver
// so the LLM prompt can be localised. Mirrors SummaryService.
func (s *SuggestionService) WithLanguageResolver(r WorkspaceLanguageResolver) *SuggestionService {
	s.languageResolver = r
	return s
}

// Suggest returns up to tagSuggestMaxOutput tag suggestions for
// fileID. Strict-ZK folders short-circuit with
// ErrTagSuggestUnavailable — the server has no plaintext for them.
// Other modes try the rule-based scaffold (deterministic) and
// optionally fold in LLM refinements. The result is always
// normalised to file.normalizeTag-style canonical form so the
// frontend can pipe selections directly into POST /files/{id}/tags.
func (s *SuggestionService) Suggest(ctx context.Context, workspaceID, fileID uuid.UUID) ([]string, error) {
	if s.pool == nil {
		return nil, errors.New("ai: suggestion service not configured")
	}

	// Load file row with folder mode in a single query so we don't
	// pay two round-trips on the hot path. The JOIN against folders
	// returns 0 rows if either file or folder is missing/deleted,
	// mapped to ErrTagSuggestFileNotFound below.
	var (
		name        string
		contentText string
		mode        string
	)
	err := s.pool.QueryRow(ctx, `
SELECT f.name, COALESCE(f.content_text, ''), fo.encryption_mode
FROM files f
JOIN folders fo ON fo.id = f.folder_id AND fo.workspace_id = f.workspace_id
WHERE f.id = $1 AND f.workspace_id = $2 AND f.deleted_at IS NULL AND fo.deleted_at IS NULL`,
		fileID, workspaceID).Scan(&name, &contentText, &mode)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrTagSuggestFileNotFound, err)
	}
	if mode == folder.EncryptionStrictZK {
		return nil, ErrTagSuggestUnavailable
	}

	// Existing workspace tags drive the "echo back" pass: if the
	// workspace already uses the tag "q4-2024" and this file's body
	// mentions "q4 2024", we want to suggest it (not just suggest
	// a synthetic "2024" token). 256 is a generous upper bound for
	// "every tag a small workspace has ever used"; larger workspaces
	// fall back to LIMIT 256, biased toward recently-added tags
	// because recency correlates with topicality (a tag a user
	// added last week is probably more relevant to next week's
	// upload than a tag last touched six months ago).
	//
	// Implementation: the inner SELECT picks the most-recent
	// created_at per tag (so duplicates of the same tag across
	// many files collapse into one row with the latest timestamp);
	// the outer SELECT orders by that timestamp DESC and trims to
	// 256. The double-SELECT is necessary because a plain
	// "SELECT DISTINCT tag … ORDER BY created_at DESC" is invalid —
	// the ORDER BY column must appear in the DISTINCT projection,
	// but if it did, DISTINCT (tag, created_at) wouldn't dedupe
	// repeated tags at all (every (tag, created_at) pair is
	// unique).
	tagRows, err := s.pool.Query(ctx, `
SELECT tag FROM (
	SELECT tag, MAX(created_at) AS last_used
	FROM file_tags
	WHERE workspace_id = $1
	GROUP BY tag
) recent
ORDER BY last_used DESC
LIMIT 256`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("ai: load workspace tags for suggestion: %w", err)
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

	preview := contentText
	if len(preview) > tagSuggestMaxFile {
		preview = preview[:tagSuggestMaxFile]
	}

	// Rule-based scaffold — runs unconditionally and forms the
	// deterministic floor. Even if the LLM stage is disabled (no
	// daemon configured) or fails (network blip), the response is
	// never empty.
	suggestions := ruleBasedSuggestions(name, preview, workspaceTags)

	// LLM refinement — best-effort, never blocks. A failure logs a
	// breadcrumb and returns the rule-based suggestions unchanged.
	if s.llm != nil {
		llmSuggestions, llmErr := s.tryLLMSuggestions(ctx, name, preview, workspaceTags, s.resolveLanguage(ctx, workspaceID))
		if llmErr != nil {
			logging.FromContext(ctx).Error("ai tag suggest: local LLM failed, returning rule-based scaffold only",
				"model", s.llm.Model(), "err", llmErr)
		} else {
			suggestions = mergeSuggestions(suggestions, llmSuggestions)
		}
	}

	if len(suggestions) > tagSuggestMaxOutput {
		suggestions = suggestions[:tagSuggestMaxOutput]
	}
	return suggestions, nil
}

// resolveLanguage mirrors SummaryService.resolveLanguage — see that
// doc comment for the trade-offs. Errors are logged, not returned,
// so a transient workspace lookup failure degrades to English-prompt
// instead of 5xx-ing the suggest endpoint.
func (s *SuggestionService) resolveLanguage(ctx context.Context, workspaceID uuid.UUID) string {
	if s.languageResolver == nil {
		return ""
	}
	lang, err := s.languageResolver.GetSearchLanguage(ctx, workspaceID)
	if err != nil {
		logging.FromContext(ctx).Warn("ai tag suggest: resolve workspace language failed (defaulting to English)",
			"workspace_id", workspaceID, "err", err)
		return ""
	}
	return lang
}

// tryLLMSuggestions asks the configured local model for tag
// suggestions. The model returns a newline-separated list which we
// split, normalise (lowercase + reject /, %), and dedupe. Any line
// that fails normalisation is silently dropped — we don't want to
// surface "look at the LLM's weird output" friction to end users.
func (s *SuggestionService) tryLLMSuggestions(ctx context.Context, fileName, preview string, workspaceTags []string, language string) ([]string, error) {
	llmCtx, cancel := context.WithTimeout(ctx, llmTimeout)
	defer cancel()
	out, err := s.llm.Generate(llmCtx, BuildTagSuggestPrompt(fileName, preview, workspaceTags, language))
	if err != nil {
		return nil, err
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return nil, errors.New("ai: llm returned empty tag suggestions")
	}
	return parseTagLines(out), nil
}

// BuildTagSuggestPrompt is the LLM prompt for tag suggestion.
// Exposed so tests can pin the wording and operators can inspect
// exactly what the on-device model sees — same privacy-story
// principle as BuildSummaryPrompt.
//
// language follows the same workspace-search-language convention as
// BuildSummaryPrompt; the user content half is passed through
// unchanged.
func BuildTagSuggestPrompt(fileName, contentPreview string, workspaceTags []string, language string) string {
	lang := PromptLanguageFor(language)
	var b strings.Builder
	b.WriteString("You are suggesting concise tags for a private team workspace file. ")
	b.WriteString(lang.Instruction)
	b.WriteString(" ")
	b.WriteString("Return between 3 and 8 tags, one per line, lowercase, hyphen-joined ")
	b.WriteString("(e.g. quarterly-report, 2024-q4, customer-feedback). ")
	b.WriteString("Do NOT prefix with #. Do NOT add commas. Do NOT include explanations. ")
	b.WriteString("Use existing workspace tags verbatim when they fit the file.\n\n")
	if fileName != "" {
		b.WriteString("File name: ")
		b.WriteString(fileName)
		b.WriteString("\n")
	}
	if contentPreview != "" {
		b.WriteString("Sample content:\n")
		b.WriteString(contentPreview)
		b.WriteString("\n")
	}
	if len(workspaceTags) > 0 {
		b.WriteString("Existing workspace tags: ")
		b.WriteString(strings.Join(workspaceTags, ", "))
		b.WriteString("\n")
	}
	b.WriteString("\n")
	b.WriteString(lang.AnswerInLanguage)
	b.WriteString("\nTags:")
	return b.String()
}

// ruleBasedSuggestions assembles the deterministic baseline:
// extension-derived doc-type tag + the highest-density keywords
// from the file's name and content, plus any workspace tag whose
// canonical form is mentioned in the file body. Order matters —
// extension tag first (always present), then workspace-overlap (high
// signal), then keyword extraction (lower signal).
func ruleBasedSuggestions(fileName, contentPreview string, workspaceTags []string) []string {
	seen := make(map[string]struct{}, tagSuggestMaxOutput)
	out := make([]string, 0, tagSuggestMaxOutput)

	appendTag := func(raw string) {
		t := canonicalTag(raw)
		if t == "" {
			return
		}
		if _, dup := seen[t]; dup {
			return
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}

	// 1. extension-derived doc-type tag
	if etag := extensionTagOf(fileName); etag != "" {
		appendTag(etag)
	}

	// 2. workspace-tag overlap on the combined corpus (name +
	// preview). We lower-case the corpus once for cheap substring
	// search. The match strategy is part-wise: split the tag at
	// hyphens, then require EVERY part to appear in the corpus.
	// This handles the common shapes:
	//   - tag "marketing-q4-2024" matches body "Marketing launch
	//     plan for Q4 2024" because all three parts appear (in any
	//     order).
	//   - tag "q4-2024" matches body "the Q4 2024 plan" without
	//     needing the body to use the exact hyphen-joined form.
	//   - we also try the literal hyphen-joined form so a body that
	//     coincidentally uses the canonical tag ("see marketing-q4-2024")
	//     still matches with a single comparison.
	corpus := strings.ToLower(fileName + " " + contentPreview)
	for _, t := range workspaceTags {
		if t == "" {
			continue
		}
		if strings.Contains(corpus, t) {
			appendTag(t)
			continue
		}
		parts := strings.Split(t, "-")
		allPresent := true
		for _, p := range parts {
			if p == "" {
				continue
			}
			if !strings.Contains(corpus, p) {
				allPresent = false
				break
			}
		}
		if allPresent {
			appendTag(t)
		}
	}

	// 3. keyword extraction from the file name. The name carries
	// disproportionately high signal (users intentionally name files
	// after their content) so we run extraction over it first.
	for _, kw := range extractKeywords(fileName, 4) {
		appendTag(kw)
	}

	// 4. keyword extraction from the content preview. We pick the
	// 8 most-frequent qualifying tokens; the appendTag dedupe will
	// drop ones already covered by steps 1-3.
	for _, kw := range extractKeywords(contentPreview, 8) {
		appendTag(kw)
	}

	return out
}

// extensionTagOf returns the canonical doc-type tag for a file
// name's extension, or "" if the extension is unknown or absent.
// The extension lookup is case-insensitive.
func extensionTagOf(fileName string) string {
	idx := strings.LastIndexByte(fileName, '.')
	if idx < 0 || idx == len(fileName)-1 {
		return ""
	}
	ext := strings.ToLower(fileName[idx:])
	return extensionTags[ext]
}

// extractKeywords splits text into tokens (Unicode letters + digits),
// counts frequency, and returns the top-N tokens by frequency
// (ties broken by alphabetical order for determinism). Tokens
// shorter than tagKeywordMinRuneCount or longer than
// tagKeywordMaxRuneCount are discarded. The output is already
// lowercase so callers don't need to re-normalise.
func extractKeywords(text string, topN int) []string {
	if text == "" || topN <= 0 {
		return nil
	}
	freq := make(map[string]int, 64)
	var token strings.Builder
	flush := func() {
		if token.Len() == 0 {
			return
		}
		t := strings.ToLower(token.String())
		token.Reset()
		runes := []rune(t)
		if len(runes) < tagKeywordMinRuneCount || len(runes) > tagKeywordMaxRuneCount {
			return
		}
		// Reject all-digit tokens (e.g. "12345") — they're rarely
		// useful as tags and noise up the suggestion list. A token
		// containing at least one letter passes.
		hasLetter := false
		for _, r := range runes {
			if unicode.IsLetter(r) {
				hasLetter = true
				break
			}
		}
		if !hasLetter {
			return
		}
		freq[t]++
	}
	for _, r := range text {
		switch {
		case unicode.IsLetter(r), unicode.IsDigit(r):
			token.WriteRune(r)
		default:
			flush()
		}
	}
	flush()
	if len(freq) == 0 {
		return nil
	}

	type kv struct {
		tok string
		n   int
	}
	pairs := make([]kv, 0, len(freq))
	for k, v := range freq {
		pairs = append(pairs, kv{k, v})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].n != pairs[j].n {
			return pairs[i].n > pairs[j].n
		}
		return pairs[i].tok < pairs[j].tok
	})
	if len(pairs) > topN {
		pairs = pairs[:topN]
	}
	out := make([]string, 0, len(pairs))
	for _, p := range pairs {
		out = append(out, p.tok)
	}
	return out
}

// canonicalTag applies the same normalisation the file service
// uses for AddTag: lowercase, trim, reject empty / overlong / `/`
// / `%`. Centralised so any callee (rule-based, LLM-parsed) goes
// through one validation path — the suggest endpoint cannot ever
// return a value that AddTag would later reject.
func canonicalTag(raw string) string {
	t := strings.ToLower(strings.TrimSpace(raw))
	if t == "" {
		return ""
	}
	if strings.ContainsAny(t, "/%") {
		return ""
	}
	// Length check must use byte count to match file.Service.AddTag's
	// validation (`len(tag) > MaxTagLength` at internal/file/
	// service.go:180). Using rune count here would let a 60-rune CJK
	// or other multi-byte tag pass the suggester (60 runes ≤ 64) but
	// fail AddTag (180 bytes > 64), violating this function's
	// documented contract that the suggest endpoint cannot ever
	// return a value that AddTag would later reject. Particularly
	// relevant now that WS6 ships multilingual prompting — non-ASCII
	// suggestions are expected, not exceptional.
	if len(t) > file.MaxTagLength {
		return ""
	}
	return t
}

// parseTagLines splits an LLM completion (one tag per line) into a
// normalised, deduplicated slice. Commas, leading hashes, bullets,
// and quote marks are stripped so a model that produces
// "- #q4-launch," still yields "q4-launch". Invalid normalisations
// (empty, too long, illegal chars) are silently dropped — same
// principle as the rest of the pipeline: never surface LLM-shaped
// friction to the user.
func parseTagLines(raw string) []string {
	seen := make(map[string]struct{}, 16)
	out := make([]string, 0, 16)
	for _, line := range strings.Split(raw, "\n") {
		// Strip common decorations: bullets, hashes, quote marks,
		// trailing commas. We do this BEFORE canonicalTag so the
		// canonical pass sees clean text.
		clean := strings.TrimFunc(line, func(r rune) bool {
			return r == ' ' || r == '\t' || r == '-' || r == '*' || r == '#' || r == '"' || r == '\'' || r == ','
		})
		if clean == "" {
			continue
		}
		// Some models emit "tag1, tag2, tag3" on a single line.
		// Honour both shapes by splitting on commas after the
		// per-line trim.
		for _, part := range strings.Split(clean, ",") {
			t := canonicalTag(part)
			if t == "" {
				continue
			}
			if _, dup := seen[t]; dup {
				continue
			}
			seen[t] = struct{}{}
			out = append(out, t)
		}
	}
	return out
}

// mergeSuggestions interleaves rule-based and LLM-derived
// suggestions while preserving the rule-based ordering as the floor.
// Strategy:
//
//   - Walk the rule-based output in order, emitting each unique tag.
//   - Then walk the LLM output, emitting any tag not already seen.
//
// This keeps the deterministic extension/workspace-overlap tags at
// the head of the list (they're highest-signal and the user is
// most likely to confirm them) while letting the LLM add adjacency
// tags that the rule-based scaffold can't infer (theme summarisation,
// genre detection, etc.).
func mergeSuggestions(ruleBased, llm []string) []string {
	seen := make(map[string]struct{}, len(ruleBased)+len(llm))
	out := make([]string, 0, len(ruleBased)+len(llm))
	for _, t := range ruleBased {
		if _, dup := seen[t]; dup {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	for _, t := range llm {
		if _, dup := seen[t]; dup {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	return out
}

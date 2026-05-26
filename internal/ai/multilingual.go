// multilingual.go — maps the Postgres search-language dictionary
// names (workspace.SearchLanguage) onto the human-language
// instructions we feed to the on-device LLM.
//
// We deliberately do NOT auto-translate user content. The promptee
// stays raw (file names, content_text bytes); only the SYSTEM
// instruction half of the prompt switches language. This keeps two
// invariants:
//
//   1. The LLM receives the actual stored text (not a translation),
//      so its output reflects what is in the corpus, not a
//      round-tripped paraphrase.
//   2. We never leak a hint to an upstream translation service —
//      the privacy posture (loopback-only LLM endpoint) still
//      holds.
//
// Workspaces whose SearchLanguage value isn't in the map fall back
// to English instructions; the LLM still tends to mirror the user
// content's language in its output (most modern open-weights
// instruction-tuned models do this), but we don't depend on that
// behaviour for correctness.
package ai

import "strings"

// PromptLanguage carries the human-language wording the prompt
// helpers should use. It is intentionally narrow — three string
// fields, one per location where multilingual swap-in matters —
// rather than a giant struct, because the prompt helpers
// concatenate these into a builder and a thick struct would just
// be ceremony.
type PromptLanguage struct {
	// DisplayName is the English-language name of the language,
	// printed in operator-facing logs (e.g. "english", "french").
	// Drawn from the workspace.SearchLanguage column verbatim so
	// debug breadcrumbs link directly back to the admin-visible
	// language setting.
	DisplayName string
	// Instruction is the imperative the prompt opens with — the
	// language the LLM should answer in. We give explicit
	// instructions (in English) about which language to use so
	// even smaller models (qwen2.5:1.5b, llama3.2:1b) follow
	// it without prompt engineering tricks.
	Instruction string
	// AnswerInLanguage is the bare phrase appended near the end
	// of the prompt as a final reminder (matches the well-known
	// "answer in {LANG}" pattern that empirically nudges small
	// instruction-tuned models to honour the language even when
	// the user content is in a third language).
	AnswerInLanguage string
}

// languagePrompts maps the Postgres dictionary names from
// workspace.supportedSearchLanguages onto the prompt vocabulary.
// The map keys are kept lower-case to match the canonical form the
// workspace.IsSupportedSearchLanguage allow-list uses; the values
// are intentionally short so the prompt overhead is small.
//
// Adding a new entry requires also adding the corresponding
// dictionary to workspace.supportedSearchLanguages — keep this map
// in sync with the workspace allow-list. The
// TestPromptLanguageCoversAllSearchLanguages test pins the
// invariant.
var languagePrompts = map[string]PromptLanguage{
	"simple": {
		DisplayName:      "simple",
		Instruction:      "Answer in the same natural language the file content is written in.",
		AnswerInLanguage: "Match the user content's language.",
	},
	"english": {
		DisplayName:      "english",
		Instruction:      "Answer in English.",
		AnswerInLanguage: "Answer in English.",
	},
	"french": {
		DisplayName:      "french",
		Instruction:      "Répondez en français.",
		AnswerInLanguage: "Répondez en français.",
	},
	"german": {
		DisplayName:      "german",
		Instruction:      "Antworten Sie auf Deutsch.",
		AnswerInLanguage: "Antworten Sie auf Deutsch.",
	},
	"spanish": {
		DisplayName:      "spanish",
		Instruction:      "Responda en español.",
		AnswerInLanguage: "Responda en español.",
	},
	"italian": {
		DisplayName:      "italian",
		Instruction:      "Rispondi in italiano.",
		AnswerInLanguage: "Rispondi in italiano.",
	},
	"portuguese": {
		DisplayName:      "portuguese",
		Instruction:      "Responda em português.",
		AnswerInLanguage: "Responda em português.",
	},
	"dutch": {
		DisplayName:      "dutch",
		Instruction:      "Antwoord in het Nederlands.",
		AnswerInLanguage: "Antwoord in het Nederlands.",
	},
	"russian": {
		DisplayName:      "russian",
		Instruction:      "Отвечайте на русском языке.",
		AnswerInLanguage: "Отвечайте на русском.",
	},
	"swedish": {
		DisplayName:      "swedish",
		Instruction:      "Svara på svenska.",
		AnswerInLanguage: "Svara på svenska.",
	},
	"norwegian": {
		DisplayName:      "norwegian",
		Instruction:      "Svar på norsk.",
		AnswerInLanguage: "Svar på norsk.",
	},
	"danish": {
		DisplayName:      "danish",
		Instruction:      "Svar på dansk.",
		AnswerInLanguage: "Svar på dansk.",
	},
	"finnish": {
		DisplayName:      "finnish",
		Instruction:      "Vastaa suomeksi.",
		AnswerInLanguage: "Vastaa suomeksi.",
	},
	"hungarian": {
		DisplayName:      "hungarian",
		Instruction:      "Válaszoljon magyarul.",
		AnswerInLanguage: "Válaszoljon magyarul.",
	},
	"turkish": {
		DisplayName:      "turkish",
		Instruction:      "Türkçe yanıt verin.",
		AnswerInLanguage: "Türkçe yanıt verin.",
	},
	"romanian": {
		DisplayName:      "romanian",
		Instruction:      "Răspunde în limba română.",
		AnswerInLanguage: "Răspunde în limba română.",
	},
}

// fallbackPromptLanguage is what PromptLanguageFor returns when the
// caller passes an unknown / empty dictionary name. We default to
// English instructions plus a "match the user content language"
// fallback hint — that combination behaves well across qwen,
// llama3.2, and mistral-instruct small models.
var fallbackPromptLanguage = PromptLanguage{
	DisplayName:      "english",
	Instruction:      "Answer in English (or match the user content's language if it is not in English).",
	AnswerInLanguage: "Answer in English unless the content is clearly in another language.",
}

// PromptLanguageFor returns the human-language wording to use in a
// prompt for a given workspace.SearchLanguage value. Unknown /
// empty inputs fall back to English. The lookup is case-
// insensitive to keep boot-time wiring tolerant of operator typos
// in env-var overrides; the workspace service itself validates the
// value against the strict allow-list before persisting.
func PromptLanguageFor(searchLanguage string) PromptLanguage {
	lang, ok := languagePrompts[strings.ToLower(strings.TrimSpace(searchLanguage))]
	if !ok {
		return fallbackPromptLanguage
	}
	return lang
}

// SupportedPromptLanguages returns the set of search-language keys
// the prompt helpers recognise. Exposed for tests that pin the
// allow-list contract against workspace.SupportedSearchLanguages —
// the two must stay in lockstep so admins can never select a
// language that the AI surfaces silently downgrade.
func SupportedPromptLanguages() []string {
	out := make([]string, 0, len(languagePrompts))
	for k := range languagePrompts {
		out = append(out, k)
	}
	return out
}

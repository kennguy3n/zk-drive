package index

import (
	"bytes"
	"fmt"
	"strings"

	"golang.org/x/net/html"
)

// htmlSkipTags is the set of element names whose text content must
// NOT be indexed: scripts and style sheets aren't body text, and
// indexing them pollutes the FTS corpus with code tokens. <noscript>
// is dropped because its content is only visible when JS is off —
// not a faithful representation of the page's real text.
//
// <iframe>, <object>, <embed>, <svg> contain serialised content
// that's typically opaque (binary, base64) and not search-relevant.
var htmlSkipTags = map[string]struct{}{
	"script":   {},
	"style":    {},
	"noscript": {},
	"iframe":   {},
	"object":   {},
	"embed":    {},
	"svg":      {},
	"template": {},
	"head":     {},
}

// htmlBlockTags is the set of element names that should flush a
// newline on close so the resulting text preserves paragraph
// boundaries — important for FTS phrase queries that shouldn't
// match across <h1> / <p> boundaries that have no intervening
// whitespace in the source.
var htmlBlockTags = map[string]struct{}{
	"address":    {},
	"article":    {},
	"aside":      {},
	"blockquote": {},
	"br":         {},
	"caption":    {},
	"dd":         {},
	"div":        {},
	"dt":         {},
	"fieldset":   {},
	"figcaption": {},
	"figure":     {},
	"footer":     {},
	"form":       {},
	"h1":         {},
	"h2":         {},
	"h3":         {},
	"h4":         {},
	"h5":         {},
	"h6":         {},
	"header":     {},
	"hr":         {},
	"li":         {},
	"main":       {},
	"nav":        {},
	"ol":         {},
	"p":          {},
	"pre":        {},
	"section":    {},
	"table":      {},
	"td":         {},
	"th":         {},
	"tr":         {},
	"ul":         {},
}

// extractHTMLText returns the visible text content of an HTML
// document. Uses the html/tokenizer (NOT the parser) so we don't
// pay the cost of building a node tree — the body is large enough
// in the wild that tokenisation is materially cheaper.
//
// <script>, <style>, <noscript>, <head>, and a handful of embedded-
// object containers are skipped entirely. Block-level closing tags
// flush a newline so paragraph structure survives the FTS dictionary.
//
// Tokenizer errors (unbalanced tags, weird encodings) are forgiving
// — the html package is permissive by design. Only catastrophic
// failures (no bytes consumed) surface as errors.
func extractHTMLText(body []byte) (string, error) {
	if len(body) == 0 {
		return "", fmt.Errorf("index/html: empty body")
	}

	z := html.NewTokenizer(bytes.NewReader(body))
	var (
		sb strings.Builder
		// skipStack holds the names of currently-open skip-tag
		// ancestors. Text inside <style>…</style> is dropped
		// because skipStack is non-empty.
		skipStack []string
		// preDepth counts how many <pre> ancestors are open;
		// inside <pre> we preserve interior whitespace because
		// formatting matters for code-search.
		preDepth int
		// lastByte tracks the most recent byte written to the
		// builder. Used by ensureNewline to make the "trailing
		// newline already?" check O(1) instead of O(n)-per-call.
		// A zero value means the builder is empty (no byte has
		// been written yet), which ensureNewline treats as
		// "no leading newline needed".
		lastByte byte
		writeStr = func(s string) {
			if s == "" {
				return
			}
			sb.WriteString(s)
			lastByte = s[len(s)-1]
		}
		writeByte = func(b byte) {
			sb.WriteByte(b)
			lastByte = b
		}
	)
	for {
		tt := z.Next()
		switch tt {
		case html.ErrorToken:
			// The tokenizer signals normal end-of-input as well as
			// any other reader error via ErrorToken; in either case
			// we return whatever we managed to extract — the html
			// package is permissive by design and we don't want a
			// half-truncated body to drop everything we already
			// recovered.
			return sb.String(), nil
		case html.TextToken:
			if len(skipStack) > 0 {
				continue
			}
			text := string(z.Text())
			if preDepth == 0 {
				text = collapseHTMLWhitespace(text)
				if text == "" {
					continue
				}
			}
			writeStr(text)
		case html.StartTagToken:
			name, _ := z.TagName()
			n := string(name)
			if _, skip := htmlSkipTags[n]; skip {
				// A nested skip-tag opening (e.g. <template>
				// inside <template>, or a <head> opening inside
				// some weird wrapper) still needs to be pushed
				// onto skipStack so the matching close pops the
				// right depth — even when we're already inside
				// a skip section. The check therefore comes
				// BEFORE the in-skip-section guard below.
				skipStack = append(skipStack, n)
				continue
			}
			if len(skipStack) > 0 {
				// Inside a skip section. Two failure modes if
				// we let the next two branches run unguarded:
				//
				//   1. A <pre> nested inside <template> /
				//      <object> / <svg> would increment preDepth
				//      without a corresponding decrement, because
				//      the EndTagToken branch above bails on
				//      `if len(skipStack) > 0 { continue }`
				//      BEFORE the `if n == "pre"` decrement runs.
				//      Once we leave the skip section, preDepth
				//      stays elevated forever and we lose
				//      whitespace collapsing on every subsequent
				//      text run.
				//
				//   2. Block-tag openings inside a skip section
				//      (e.g. <div> inside <template>) would emit
				//      spurious newlines into the output even
				//      though no body text gets through — every
				//      <h1>/<p>/<div> inside <head> would leave
				//      a phantom paragraph break.
				//
				// (The skip tags the Go html tokenizer treats as
				// rawtext — <script>, <style>, <noscript>,
				// <iframe> — never produce inner start/end
				// tokens, so this only manifests for the parsed-
				// content skip tags: <template>, <head>,
				// <object>, <embed>, <svg>.)
				continue
			}
			if n == "pre" {
				preDepth++
			}
			if _, block := htmlBlockTags[n]; block {
				// Some block-level openings (e.g. <li>) start a
				// new line; emit a separator iff we're not at
				// the start of a fresh line already.
				if lastByte != 0 && lastByte != '\n' {
					writeByte('\n')
				}
			}
		case html.EndTagToken:
			name, _ := z.TagName()
			n := string(name)
			// Pop-until-match: malformed HTML can leave the stack
			// out of order (e.g. <head><script></head></script>),
			// where the closing tag matches a skip-tag deeper than
			// the top. Scan down the stack and, if we find the
			// closing tag anywhere, drop it AND every entry above
			// it. This recovers gracefully from interleaved skip
			// tags without falling through and letting downstream
			// text inherit a stale skip context. If the tag is not
			// on the stack at all (orphan close), do nothing — the
			// tokenizer's permissive parser will continue.
			if len(skipStack) > 0 {
				popped := false
				for i := len(skipStack) - 1; i >= 0; i-- {
					if skipStack[i] == n {
						skipStack = skipStack[:i]
						popped = true
						break
					}
				}
				if popped {
					continue
				}
				// Closing tag is not a skip tag — fall through
				// to the standard block-tag handling, but only
				// if we are not currently inside a skip section.
				// (Being inside a skip section means we shouldn't
				// emit any body whitespace either.)
				continue
			}
			if n == "pre" && preDepth > 0 {
				preDepth--
			}
			if _, block := htmlBlockTags[n]; block {
				writeByte('\n')
			}
		case html.SelfClosingTagToken:
			name, _ := z.TagName()
			n := string(name)
			if len(skipStack) > 0 {
				continue
			}
			if n == "br" || n == "hr" {
				writeByte('\n')
			}
		}
	}
}

// collapseHTMLWhitespace replaces runs of whitespace with a single
// space, mirroring how the browser renders inline text. Without
// this step every newline / indentation in the source HTML would
// leak into content_text and bloat the FTS index.
//
// The trim is asymmetric on purpose: leading spaces are stripped
// (the previous emission's trailing whitespace + this run's leading
// whitespace would otherwise coalesce into a duplicate) but
// trailing spaces are PRESERVED. The trailing space is what
// separates this text run from the next inline element's text run
// (`<b>hello </b><i>world</i>` must render as `hello world`, not
// `helloworld`). Block-element boundaries are handled separately
// by the caller's `ensureNewline` chain using its `lastByte`
// tracking, so collapseHTMLWhitespace only needs to manage the
// intra-run whitespace.
func collapseHTMLWhitespace(s string) string {
	var sb strings.Builder
	prevSpace := true
	for _, r := range s {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			if !prevSpace {
				sb.WriteByte(' ')
				prevSpace = true
			}
			continue
		}
		sb.WriteRune(r)
		prevSpace = false
	}
	out := sb.String()
	// Trim leading space — the previous emission's trailing
	// whitespace + this run's leading whitespace coalesce into a
	// duplicate; the caller's ensureNewline / WriteString chain
	// already handles inter-element separation.
	return strings.TrimLeft(out, " ")
}



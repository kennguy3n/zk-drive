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
		sb        strings.Builder
		// skipStack holds the names of currently-open skip-tag
		// ancestors. Text inside <style>…</style> is dropped
		// because skipStack is non-empty.
		skipStack []string
		// preDepth counts how many <pre> ancestors are open;
		// inside <pre> we preserve interior whitespace because
		// formatting matters for code-search.
		preDepth int
	)
	for {
		tt := z.Next()
		switch tt {
		case html.ErrorToken:
			err := z.Err()
			if err.Error() == "EOF" {
				return sb.String(), nil
			}
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
			sb.WriteString(text)
		case html.StartTagToken:
			name, _ := z.TagName()
			n := string(name)
			if _, skip := htmlSkipTags[n]; skip {
				skipStack = append(skipStack, n)
				continue
			}
			if n == "pre" {
				preDepth++
			}
			if _, block := htmlBlockTags[n]; block {
				// Some block-level openings (e.g. <li>) start a
				// new line; emit a separator iff we're not at
				// the start of a fresh line already.
				ensureNewline(&sb)
			}
		case html.EndTagToken:
			name, _ := z.TagName()
			n := string(name)
			if len(skipStack) > 0 && skipStack[len(skipStack)-1] == n {
				skipStack = skipStack[:len(skipStack)-1]
				continue
			}
			if n == "pre" && preDepth > 0 {
				preDepth--
			}
			if _, block := htmlBlockTags[n]; block {
				sb.WriteByte('\n')
			}
		case html.SelfClosingTagToken:
			name, _ := z.TagName()
			n := string(name)
			if n == "br" || n == "hr" {
				sb.WriteByte('\n')
			}
		}
	}
}

// collapseHTMLWhitespace replaces runs of whitespace with a single
// space, mirroring how the browser renders inline text. Without
// this step every newline / indentation in the source HTML would
// leak into content_text and bloat the FTS index.
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

// ensureNewline appends a newline only if the builder doesn't
// already end with one. Used to avoid emitting double blank lines
// when a block-level element is nested inside another (e.g. <ul>
// containing <li>s).
func ensureNewline(sb *strings.Builder) {
	s := sb.String()
	if len(s) == 0 {
		return
	}
	if s[len(s)-1] == '\n' {
		return
	}
	sb.WriteByte('\n')
}

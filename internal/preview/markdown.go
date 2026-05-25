package preview

import (
	"bytes"
	"context"
	"image"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	extast "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
)

// markdownPreviewMaxBytes caps how many bytes of source markdown we
// hand to the goldmark parser. This is intentionally smaller than the
// service-level MaxSourceBytes — markdown ASTs can balloon for big
// documents (every heading + paragraph + emphasis run is a node), and
// the visible thumbnail at 256 px tops out around 30 visible lines.
// 256 KiB gives us comfortable headroom for typical README / spec
// docs without paying parser cost on multi-megabyte input.
const markdownPreviewMaxBytes = 256 * 1024

// markdownPreviewMaxLines caps the visible lines in the rasterised
// preview. Same rationale as textPreviewMaxLines — fits a 600 px
// working canvas at the current font size.
const markdownPreviewMaxLines = 30

// goldmarkParser is the shared goldmark instance used by every
// renderMarkdown call. It's constructed once at package init time —
// goldmark documents the parser as stateless after construction and
// safe for concurrent use (each parse runs through its own
// reader / state machine), so hoisting it to a package-level
// singleton avoids re-doing the extension wiring on every call.
//
// The extension set must stay in sync with the walker switch cases
// in walkMarkdownAST — see the comment on those cases for why each
// extension is opted into here.
var goldmarkParser = goldmark.New(
	goldmark.WithExtensions(
		extension.Table,
		extension.Strikethrough,
		extension.TaskList,
	),
	goldmark.WithParserOptions(parser.WithAutoHeadingID()),
)

// renderMarkdown produces a structured preview of a markdown
// document. The pipeline is:
//
//  1. Parse the source with goldmark (CommonMark + the default
//     extension set that comes bundled). We DON'T render to HTML —
//     a raw HTML render and then stripping tags would lose the
//     structural cues (heading levels, list nesting, code-block
//     framing) that make a markdown preview visually distinct from
//     the plain-text fallback.
//  2. Walk the AST and emit a compact plain-text representation that
//     preserves structure: headings are upper-cased and prefixed with
//     `#` (so they look like banners in the rasterised output),
//     bullet / ordered list items get "- " / "N. " prefixes with
//     leading indentation that mirrors nesting depth, code blocks
//     are surrounded by horizontal-rule sentinels, and emphasis is
//     dropped (an `*italic*` run in 256 px monochrome thumbnail has
//     no useful signal).
//  3. Hand the structured text to renderTextToImage with the document
//     title (first H1, if any) as the header banner.
//
// Binary blobs masquerading as text/markdown won't crash the
// renderer — goldmark accepts any bytes and the AST walker only
// inspects element kinds and text content (control bytes are
// filtered by the downstream text rasteriser). Malformed markdown
// (unterminated fences, dangling links, etc.) parses to whatever
// recovery node goldmark chooses, which still renders as plaintext.
func renderMarkdown(_ context.Context, srcBytes []byte) (image.Image, error) {
	body := srcBytes
	if len(body) > markdownPreviewMaxBytes {
		body = clipBytesToValidUTF8(body[:markdownPreviewMaxBytes])
	}

	// We use the package-level goldmarkParser singleton — see its
	// doc comment for why the parser is hoisted out of this hot path
	// (stateless after construction, safe for concurrent use). The
	// extension set is documented on the singleton.
	//
	// We DELIBERATELY do NOT enable goldmark's default extension set
	// (extension.GFM + Linkify + Footnote + DefinitionList + Typo-
	// grapher + …) — the AST walker below switches on a fixed set
	// of node kinds and a surprise extension would silently drop
	// through to the default "text content" fallback, which would
	// just emit the marker text (e.g. an unhandled footnote
	// definition rendering as `[^1]: …`). Each extension added to
	// the singleton must have a matching walker case below.
	reader := text.NewReader(body)
	doc := goldmarkParser.Parser().Parse(reader)

	title, structured := walkMarkdownAST(doc, body)

	return renderTextToImage(structured, textPreviewOpts{
		maxLines: markdownPreviewMaxLines,
		header:   title,
	}), nil
}

// walkMarkdownAST traverses the goldmark AST and produces a compact
// plain-text representation suitable for thumbnail rasterisation.
// Returns the document title (first H1's text, empty if none) and
// the body text.
//
// The walk is a depth-first pre-order over block-level children of
// the document root. We don't visit deeply into inline subtrees —
// for inline nodes we collect their text content via the AST's
// `Text(source)` helper, which handles soft-line-break / hard-line-
// break normalisation correctly.
//
// Block-level handling:
//   - Heading: emit blank-line-blank-line wrapping so headings stand
//     out as banners. H1 also fills the title slot iff still empty.
//   - Paragraph: emit the inline text content, one logical paragraph
//     per line (the rasteriser will hard-wrap to the canvas width).
//   - List: recurse into items, prefix with depth-aware bullet
//     marker, track ordered-list index.
//   - FencedCodeBlock / CodeBlock: emit a horizontal-rule sentinel
//     line, the code lines (indented 2 spaces so they're visually
//     framed even after the rasteriser hard-wraps), and another
//     sentinel. Code is preserved verbatim — no inline emphasis
//     stripping.
//   - Blockquote: prefix every emitted line with "> ".
//   - ThematicBreak: emit a 40-dash horizontal rule.
//   - Table (extension): emit each row as tab-separated values so the
//     thumbnail picks up columnar structure.
//
// Everything else (HTML blocks, raw blocks, link reference defs)
// is dropped — they don't contribute meaningfully to a 256 px
// thumbnail.
func walkMarkdownAST(doc ast.Node, source []byte) (title, body string) {
	var (
		out      strings.Builder
		firstH1  string
		listStk  []listFrame
		blockqDp int
		// emittedAny is true once any content line has been
		// written. emitBlank is a no-op before the first content
		// line so a document that opens with `# Heading` doesn't
		// produce a leading blank line in the rasterised body
		// (the heading's surrounding emitBlank() calls would
		// otherwise prepend an empty line).
		emittedAny bool
		// trailingBlankDepth tracks the blockquote depth of the
		// most-recent emitted blank line (or -1 if the last
		// emission was a content line). emitBlank dedupes by
		// checking trailingBlankDepth against the current depth
		// — equality means we'd write a same-depth blank line
		// twice in a row, which collapses. This replaces the
		// previous approach of calling out.String() and scanning
		// the suffix, which was O(n) per call and quadratic
		// across a deeply nested document.
		trailingBlankDepth = -1
	)

	emitLine := func(line string) {
		// Apply blockquote prefix on every emission so nested
		// blockquotes accumulate. ">" doesn't compound visually
		// at the rasteriser; the depth marker is enough.
		if blockqDp > 0 {
			out.WriteString(strings.Repeat("> ", blockqDp))
		}
		out.WriteString(line)
		out.WriteByte('\n')
		emittedAny = true
		trailingBlankDepth = -1
	}

	emitBlank := func() {
		// Skip leading blanks before any content has been
		// written — otherwise a document that starts with a
		// heading would produce a wasted blank line at the top
		// of the rasterised body.
		if !emittedAny {
			return
		}
		// Dedupe: two consecutive blank lines at the same
		// blockquote depth collapse to one. State tracked in a
		// single int (-1 = trailing content, N = trailing blank
		// at depth N) avoids the O(n) buffer-copy we'd otherwise
		// pay in `out.String()` on every emitBlank.
		if trailingBlankDepth == blockqDp {
			return
		}
		// Inside a blockquote, blank lines carry the same `> `
		// prefix as content lines so a multi-paragraph quote
		// renders with `> ` separators between paragraphs rather
		// than bare empty lines breaking the visual continuity
		// of the quote.
		if blockqDp > 0 {
			out.WriteString(strings.Repeat("> ", blockqDp))
		}
		out.WriteByte('\n')
		trailingBlankDepth = blockqDp
	}

	var walk func(n ast.Node)
	walk = func(n ast.Node) {
		switch nn := n.(type) {
		case *ast.Document:
			for c := nn.FirstChild(); c != nil; c = c.NextSibling() {
				walk(c)
			}
		case *ast.Heading:
			// Banner-style: BLANK / # HEADING TEXT (caps for h1
			// & h2, normal case for h3+ so deep hierarchy still
			// reads).
			txt := nodeText(nn, source)
			if firstH1 == "" && nn.Level == 1 {
				firstH1 = txt
			}
			prefix := strings.Repeat("#", nn.Level) + " "
			banner := prefix + txt
			if nn.Level <= 2 {
				banner = prefix + strings.ToUpper(txt)
			}
			emitBlank()
			emitLine(banner)
			emitBlank()
		case *ast.Paragraph, *ast.TextBlock:
			emitLine(nodeText(nn, source))
		case *ast.List:
			depth := len(listStk)
			frame := listFrame{ordered: nn.IsOrdered(), index: nn.Start, depth: depth}
			if frame.ordered && frame.index == 0 {
				// goldmark sets Start to the user's first
				// number; 0 means default (1) per CommonMark.
				frame.index = 1
			}
			listStk = append(listStk, frame)
			for c := nn.FirstChild(); c != nil; c = c.NextSibling() {
				walk(c)
			}
			listStk = listStk[:len(listStk)-1]
			emitBlank()
		case *ast.ListItem:
			if len(listStk) == 0 {
				// AST defensive: ListItem without enclosing
				// List shouldn't happen, but emit naked text
				// rather than dropping content.
				emitLine("- " + nodeText(nn, source))
				return
			}
			frame := &listStk[len(listStk)-1]
			indent := strings.Repeat("  ", frame.depth)
			var marker string
			if frame.ordered {
				marker = formatOrdinal(frame.index) + ". "
				frame.index++
			} else {
				marker = "- "
			}
			// A list item itself contains block children; we
			// emit the first inline-content child (Paragraph
			// in loose lists, TextBlock in tight lists — both
			// expose the same inline subtree via nodeText)
			// joined inline with the marker, then recurse into
			// the rest as continuation lines.
			first := true
			for c := nn.FirstChild(); c != nil; c = c.NextSibling() {
				if first {
					if isInlineBlock(c) {
						emitLine(indent + marker + nodeText(c, source))
					} else {
						// First child isn't an inline-
						// content block — fall through
						// to a marker-only line so the
						// bullet is still visible.
						emitLine(indent + marker)
						walk(c)
					}
					first = false
					continue
				}
				walk(c)
			}
		case *ast.FencedCodeBlock, *ast.CodeBlock:
			emitBlank()
			emitLine("```")
			emitCodeLines(nn, source, emitLine)
			emitLine("```")
			emitBlank()
		case *ast.Blockquote:
			blockqDp++
			// Between sibling block-level children inside a
			// blockquote, emit a blank separator so a
			// multi-paragraph quote renders with the empty
			// `> ` separator line between paragraphs (which
			// emitBlank now prefixes via the blockquote
			// handling above). Without this, sibling
			// paragraphs in `> a\n>\n> b` would collapse
			// onto consecutive lines.
			first := true
			for c := nn.FirstChild(); c != nil; c = c.NextSibling() {
				if !first {
					emitBlank()
				}
				walk(c)
				first = false
			}
			blockqDp--
		case *ast.ThematicBreak:
			emitLine(strings.Repeat("-", 40))
		case *extast.Table:
			emitTable(nn, source, emitLine)
			emitBlank()
		default:
			// HTML blocks, raw blocks, link reference defs,
			// definition lists, footnote refs, etc. — surface
			// their text content if any, otherwise drop. This
			// keeps the preview useful for partial / mixed
			// content (e.g. a README with embedded raw HTML)
			// without us needing to special-case every leaf.
			if txt := nodeText(nn, source); txt != "" {
				emitLine(txt)
			}
		}
	}
	walk(doc)

	return firstH1, strings.TrimRight(out.String(), "\n")
}

// isInlineBlock returns true if the given AST node is an inline-
// content block — i.e. a node whose children are inline elements
// (text, emphasis, links, etc.). Goldmark uses ast.Paragraph in
// loose lists (`- a\n\n- b\n`) and ast.TextBlock in tight lists
// (`- a\n- b\n`), and we want to handle both identically when
// emitting a list-item marker inline with its first content block.
//
// Any other block (e.g. a nested list, a code block, a blockquote)
// goes through the recursive walk so its own block-level emission
// rules apply.
func isInlineBlock(n ast.Node) bool {
	switch n.(type) {
	case *ast.Paragraph, *ast.TextBlock:
		return true
	default:
		return false
	}
}

// listFrame tracks the state of an open list during the AST walk so
// nested lists indent correctly and ordered lists number
// sequentially. depth is the nesting level (0 = outermost list).
type listFrame struct {
	ordered bool
	index   int
	depth   int
}

// nodeText returns the concatenated text content of a block node,
// flattening inline children to a single line. Soft line breaks
// (a single newline in the source) collapse to a space; hard line
// breaks (trailing-two-spaces or backslash-newline in the source)
// emit a real newline.
//
// Emphasis / strong / strikethrough / code-spans / links / images
// are all flattened to their text content — the 256 px monochrome
// thumbnail can't carry the styling distinction, and emitting the
// marker characters (`*`, `_`, `~`) would just clutter the output.
func nodeText(n ast.Node, source []byte) string {
	var sb strings.Builder
	var walk func(c ast.Node)
	walk = func(c ast.Node) {
		for child := c.FirstChild(); child != nil; child = child.NextSibling() {
			switch tn := child.(type) {
			case *ast.Text:
				sb.Write(tn.Segment.Value(source))
				if tn.SoftLineBreak() {
					sb.WriteByte(' ')
				}
				if tn.HardLineBreak() {
					sb.WriteByte('\n')
				}
			case *ast.String:
				sb.Write(tn.Value)
			case *extast.TaskCheckBox:
				// Render a GFM task-list checkbox inline.
				// `[x] ` for checked, `[ ] ` for unchecked.
				// Without this, the TaskList extension's
				// AST replacement of the literal `[ ] ` /
				// `[x] ` prefix in the source would simply
				// drop the indicator from the rendered
				// preview, losing a useful "what's done"
				// signal in dev README task lists.
				if tn.IsChecked {
					sb.WriteString("[x] ")
				} else {
					sb.WriteString("[ ] ")
				}
			case *ast.AutoLink:
				// Auto-links (bare URLs) emit the URL text;
				// for a thumbnail the URL is the only
				// semantic content the user can recognise.
				sb.Write(tn.URL(source))
			case *ast.CodeSpan:
				// Code-spans preserve their text verbatim.
				// We do NOT add backtick markers — they
				// would survive as literal punctuation in
				// the rasterised output without conveying
				// useful information.
				walk(tn)
			case *ast.Link:
				// Link text (children) carries the user-
				// readable label. Drop the URL — at 256 px
				// the URL would just bloat the line.
				walk(tn)
			case *ast.Image:
				// Replace inline images with their alt
				// text. An `![diagram](url)` becomes the
				// "diagram" label — a reasonable signal
				// of what the inline image is about.
				if alt := nodeText(tn, source); alt != "" {
					sb.WriteString(alt)
				}
			default:
				// Recurse so nested emphasis / strong /
				// strikethrough flatten their children.
				walk(child)
			}
		}
	}
	walk(n)
	return strings.TrimSpace(sb.String())
}

// emitCodeLines writes a fenced or indented code block's body
// verbatim through the supplied emit. We DO NOT strip leading
// whitespace — code formatting carries information that's worth
// preserving in the rasterised output. Lines that exceed the
// canvas's column budget will be hard-cut by wrapLines at render
// time, same as plain-text source files.
func emitCodeLines(n ast.Node, source []byte, emit func(string)) {
	lines := n.Lines()
	for i := 0; i < lines.Len(); i++ {
		seg := lines.At(i)
		raw := seg.Value(source)
		// Trim only the trailing newline that the segment
		// carries (the source has a \n at end-of-line); leave
		// leading whitespace intact.
		raw = bytes.TrimRight(raw, "\n")
		emit(string(raw))
	}
}

// emitTable rasterises a GFM table extension node as tab-separated
// rows. Each row's cells are joined with a tab so the rasteriser's
// tab-expansion (handled by wrapLines, two spaces per tab) creates
// visible columns. Header row is emitted first followed by a
// separator line; data rows follow.
//
// The extension AST exposes the table as Header (the first row),
// followed by RowN children. We walk both, calling nodeText on each
// cell to flatten its inline content.
func emitTable(t *extast.Table, source []byte, emit func(string)) {
	emitRow := func(row ast.Node) {
		var cells []string
		for c := row.FirstChild(); c != nil; c = c.NextSibling() {
			cells = append(cells, nodeText(c, source))
		}
		emit(strings.Join(cells, "\t"))
	}
	for c := t.FirstChild(); c != nil; c = c.NextSibling() {
		switch row := c.(type) {
		case *extast.TableHeader:
			emitRow(row)
			emit(strings.Repeat("-", 24))
		case *extast.TableRow:
			emitRow(row)
		}
	}
}

// formatOrdinal returns a string ordinal index for a list item. It's
// trivially `Itoa` today but lives in its own helper so a future
// change (e.g. localised ordinals, or 'a / b / c' lettering for nested
// ordered lists) has a single edit point.
func formatOrdinal(n int) string {
	if n <= 0 {
		n = 1
	}
	// Avoid pulling in strconv just for this; keep allocations
	// bounded — these are list-item counts, not free-form ints.
	if n < 10 {
		return string(rune('0' + n))
	}
	var buf [16]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

func init() {
	Register(RendererFunc(renderMarkdown), "text/markdown", "text/x-markdown")
}

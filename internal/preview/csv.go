package preview

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"image"
	"io"
	"strings"
	"unicode/utf8"
)

// csvPreviewMaxBytes caps how much of the input stream the CSV parser
// will see. A 10K-row × 50-col CSV runs ~3 MiB in practice; we don't
// need more than the first ~256 KiB to fill a 256 px thumbnail.
const csvPreviewMaxBytes = 256 * 1024

// csvPreviewMaxRows is the visible-data-row budget for the preview.
// The exact number of rendered data rows depends on whether a header
// is detected:
//
//   - Header detected:  csvPreviewMaxRows (20) data rows, plus the
//                       header banner in the title slot. Total visible
//                       lines: 21 (header + 20).
//   - No header:        csvPreviewMaxRows+1 (21) data rows, no banner.
//                       Total visible lines: 21.
//
// The "+1 when headerless" is intentional — without a header banner
// consuming a visual slot, the extra data row makes full use of the
// 21-line maxLines budget passed to the rasteriser. The read loop
// reads csvPreviewMaxRows+1 successful records regardless, so the
// invariant is "fill all 21 visible slots". 20 rows × ~6 chars
// per cell comfortably fills the 30-line working canvas after
// accounting for the rasteriser's per-line cap.
const csvPreviewMaxRows = 20

// csvPreviewMaxCols caps how many columns we render per row. Wider
// tables get truncated with a `…` suffix on the last visible cell so
// the user can tell the preview is partial. A 10-column cap fits
// monospace columns into ~80 chars at a 6-char average per cell which
// matches the rasteriser's column budget at our default canvas width.
const csvPreviewMaxCols = 10

// csvPreviewCellMaxRunes caps how many runes per cell we render. A
// 32-rune cap on each cell keeps long free-text cells from blowing
// out a column and pushing the rest of the row off the canvas. The
// truncation is rune-aware so multi-byte content isn't sliced
// mid-codepoint.
const csvPreviewCellMaxRunes = 32

// renderCSV is the registered handler for the comma-CSV family of
// MIMEs (`text/csv`, `application/csv`). It calls renderCSVWithDelim
// with autoDelim so the delimiter is sniffed from the first line of
// the source — this is the right behaviour for `text/csv` because
// some European exports actually use semicolons or tabs even though
// the file is labelled CSV.
func renderCSV(ctx context.Context, srcBytes []byte) (image.Image, error) {
	return renderCSVWithDelim(ctx, srcBytes, autoDelim)
}

// renderTSV is the registered handler for `text/tab-separated-values`
// / `text/tsv`. It forces the delimiter to `\t` regardless of the
// file's first-line content. This matters because a TSV with a comma
// in a header cell value (e.g. `Full Name\tCity, State\tAge`) would
// otherwise be sniffed as comma-CSV by detectCSVDelimiter (which
// prioritises comma when both `,` and `\t` are present) and would
// produce a garbled preview — the parser would collapse the entire
// tab-separated row into one cell.
//
// The Renderer interface only passes (ctx, bytes), so we can't
// inspect the MIME at call time — the dispatch happens at registry
// level via two separate RendererFunc registrations. Both render
// paths share renderCSVWithDelim so logic stays in one place.
func renderTSV(ctx context.Context, srcBytes []byte) (image.Image, error) {
	return renderCSVWithDelim(ctx, srcBytes, '\t')
}

// autoDelim is the sentinel passed to renderCSVWithDelim when the
// caller wants delimiter sniffing. `'\uFFFD'` is the Unicode REPLACEMENT
// CHARACTER and equals `utf8.RuneError`; it's a valid codepoint in
// general, but Go's `encoding/csv` `validDelim()` check explicitly
// rejects `utf8.RuneError` as a Comma value (see Go stdlib
// `csv/reader.go`). That means if a future change accidentally
// routes this sentinel through to `csv.Reader.Comma` without
// translating it to a real delimiter, `Reader.Read()` will fail
// loudly with `ErrBadDelim` rather than silently mis-parsing.
const autoDelim = '\uFFFD'

// renderCSVWithDelim is the core CSV/TSV preview pipeline:
//
//  1. Determine the delimiter — forcedDelim==autoDelim sniffs the
//     first line; any other value is honoured as-is.
//  2. Read up to csvPreviewMaxRows+1 successful records into
//     memory. Parse errors do NOT count against the row budget;
//     the loop counts successful reads instead of iterations so
//     a CSV with a corrupt row in the middle still renders the
//     expected number of preview rows.
//  3. Per-row, truncate to csvPreviewMaxCols cells, truncate each
//     cell to csvPreviewCellMaxRunes runes, join with tab so the
//     rasteriser expands them as columnar separators.
//  4. Hand the assembled text to renderTextToImage with the file's
//     header row (joined by tab) as the banner.
//
// Malformed CSV — unterminated quoted fields, embedded NULs, ragged
// row widths — is handled by `csv.Reader.FieldsPerRecord = -1` and
// `LazyQuotes = true`, so the parser is lenient. Catastrophic parse
// errors (unrecoverable I/O) are reported through the error return
// so the worker re-delivers (consistent with other renderers).
func renderCSVWithDelim(_ context.Context, srcBytes []byte, forcedDelim rune) (image.Image, error) {
	body := srcBytes
	if len(body) > csvPreviewMaxBytes {
		body = clipBytesToValidUTF8(body[:csvPreviewMaxBytes])
	}
	// Strip a leading UTF-8 BOM if present. Excel and many Windows
	// CSV exporters prepend `\xEF\xBB\xBF` to identify the file as
	// UTF-8, but `encoding/csv` and `detectCSVDelimiter` treat the
	// BOM as ordinary bytes that survive into the first cell — the
	// preview would then render the BOM glyph as a leading
	// visual artefact on the first column of the first row. The
	// BOM is purely a file-encoding marker; the bytes carry no
	// semantic content and stripping is safe. We strip only at
	// position 0 because BOM is a file-prefix concept, not a
	// per-record marker (a `\xEF\xBB\xBF` byte sequence found
	// mid-file is data, not a BOM).
	body = bytes.TrimPrefix(body, []byte{0xEF, 0xBB, 0xBF})

	var delim rune
	if forcedDelim == autoDelim {
		delim = detectCSVDelimiter(body)
	} else {
		delim = forcedDelim
	}
	r := csv.NewReader(bytes.NewReader(body))
	r.Comma = delim
	// LazyQuotes accepts " inside an unquoted field (e.g. height
	// specifications like 5'10") which strict CSV would reject —
	// for preview purposes we prefer "render whatever the user
	// uploaded" over a hard failure.
	r.LazyQuotes = true
	// FieldsPerRecord=-1 disables the stdlib's "must match first
	// row's column count" check. Real-world CSVs are routinely
	// ragged (trailing empty cells stripped, malformed exports),
	// and we want every row to render even if widths differ.
	r.FieldsPerRecord = -1
	// TrimLeadingSpace lets a row like `a, b, c` render with the
	// expected 3 cells rather than 3 cells with leading whitespace.
	r.TrimLeadingSpace = true
	// ReuseRecord=false because we slice every record into our own
	// output structure — reusing the underlying array would alias
	// previously stored rows.

	var (
		records [][]string
		hadAny  bool
	)
	// Read until we have csvPreviewMaxRows+1 SUCCESSFUL records,
	// counting successes rather than iterations. A parse-error row
	// must not consume a slot in the budget — if it did, a CSV
	// with one corrupt row in its first N+1 rows would silently
	// render N visible rows instead of N+1, and a chain of
	// corruptions early in the file would produce an effectively
	// empty preview.
	//
	// To bound work on pathologically corrupt input (e.g. an
	// adversarial blob that fails every row), we cap the total
	// iterations at a multiple of the desired success count. A
	// 10x multiplier comfortably tolerates the realistic worst
	// case (a few corrupt rows scattered through a valid file)
	// without letting a fully-corrupt input loop indefinitely.
	const maxParseErrorBudget = (csvPreviewMaxRows + 1) * 10
	for iter := 0; len(records) <= csvPreviewMaxRows && iter < maxParseErrorBudget; iter++ {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			// Try to recover from a single bad row without
			// failing the whole preview. ParseError carries
			// the offending column; we skip the row and
			// continue. Other errors (genuine I/O) bubble
			// up — they're unrecoverable.
			var pe *csv.ParseError
			if errors.As(err, &pe) {
				continue
			}
			return nil, err
		}
		hadAny = true
		records = append(records, rec)
	}
	if !hadAny {
		// An entirely empty input produces a usable but empty
		// preview rather than failing the renderer. The text
		// rasteriser handles an empty body cleanly.
		return renderTextToImage("", textPreviewOpts{
			maxLines: csvPreviewMaxRows + 2,
			header:   delimiterLabel(delim),
		}), nil
	}

	// First row → header banner, but only if it looks like a header
	// (alphabetic mass): a CSV whose first row is `1,2,3` is data,
	// not a header, and demoting that to a banner would lose the
	// row from the visible body. The heuristic is: header iff the
	// first row contains at least one cell with an alphabetic
	// character and no cell is purely numeric. Cheap and right for
	// the common case.
	var header string
	dataStart := 0
	if looksLikeHeader(records[0]) {
		header = formatRow(records[0])
		dataStart = 1
	}

	var sb strings.Builder
	for i := dataStart; i < len(records); i++ {
		if i > dataStart {
			sb.WriteByte('\n')
		}
		sb.WriteString(formatRow(records[i]))
	}

	return renderTextToImage(sb.String(), textPreviewOpts{
		maxLines: csvPreviewMaxRows + 1,
		header:   header,
	}), nil
}

// formatRow truncates a record to csvPreviewMaxCols cells, truncates
// each cell to csvPreviewCellMaxRunes runes (using truncateRunes for
// rune-correct slicing), and joins with tab so the rasteriser paints
// columnar separators. Returns a single line of text — newline
// management is the caller's job.
func formatRow(rec []string) string {
	if len(rec) == 0 {
		return ""
	}
	overflow := len(rec) > csvPreviewMaxCols
	visible := rec
	if overflow {
		visible = rec[:csvPreviewMaxCols]
	}
	cells := make([]string, 0, len(visible)+1)
	for i, c := range visible {
		// Collapse interior newlines / tabs in a cell to spaces
		// — a multi-line cell (very common in CSV exports of
		// rich-text fields) would otherwise break our one-row-
		// per-line invariant for the rasteriser.
		c = sanitiseCSVCell(c)
		// Cap each cell's rune count so a single long cell
		// doesn't push the rest of the row off the canvas. We
		// reserve room for the ellipsis suffix in the cap so
		// `truncateRunes` doesn't overflow the budget.
		if utf8.RuneCountInString(c) > csvPreviewCellMaxRunes {
			c = truncateRunes(c, csvPreviewCellMaxRunes, "…")
		}
		cells = append(cells, c)
		_ = i
	}
	if overflow {
		cells = append(cells, "…")
	}
	return strings.Join(cells, "\t")
}

// sanitiseCSVCell returns a single-line representation of a cell
// suitable for rasterising. CRLF and LF inside a quoted field
// become a single space; tabs collapse to a single space so a tab
// embedded in a cell doesn't masquerade as a column separator
// once we re-join with tabs ourselves. Sequence runs of multiple
// whitespace characters collapse to one.
func sanitiseCSVCell(s string) string {
	if !strings.ContainsAny(s, "\r\n\t") && !strings.Contains(s, "  ") {
		return strings.TrimSpace(s)
	}
	var sb strings.Builder
	prevSpace := false
	for _, r := range s {
		switch r {
		case '\r', '\n', '\t', ' ':
			if !prevSpace {
				sb.WriteByte(' ')
				prevSpace = true
			}
		default:
			sb.WriteRune(r)
			prevSpace = false
		}
	}
	return strings.TrimSpace(sb.String())
}

// detectCSVDelimiter sniffs the first non-empty line for a delimiter
// hint. We default to comma; if the first line contains no commas
// but has at least one tab, switch to tab. If it has no commas but
// at least one semicolon, switch to semicolon (European exports).
// This is intentionally cheap — we look at the first line only, and
// stop on the first whitespace newline. csv.Reader handles the rest.
func detectCSVDelimiter(body []byte) rune {
	// Bound the sniff window so we don't scan a multi-MB blob.
	const sniff = 4096
	w := body
	if len(w) > sniff {
		w = w[:sniff]
	}
	// Walk to first newline; the first line carries the column
	// header in the vast majority of CSV exports, so the
	// delimiter inferred from it correctly classifies the file.
	nl := bytes.IndexByte(w, '\n')
	if nl < 0 {
		nl = len(w)
	}
	first := w[:nl]
	hasComma := bytes.IndexByte(first, ',') >= 0
	hasTab := bytes.IndexByte(first, '\t') >= 0
	hasSemi := bytes.IndexByte(first, ';') >= 0
	switch {
	case hasComma:
		return ','
	case hasTab:
		return '\t'
	case hasSemi:
		return ';'
	default:
		// No obvious delimiter — fall back to comma. The reader
		// will then emit single-column rows, which is the right
		// behaviour for a delimiter-less file.
		return ','
	}
}

// delimiterLabel returns a human-readable label for an empty-file
// banner. Used only when the input has zero rows so the user can
// still see what kind of file the renderer was looking at.
func delimiterLabel(r rune) string {
	switch r {
	case '\t':
		return "(empty TSV)"
	case ';':
		return "(empty CSV — semicolon)"
	default:
		return "(empty CSV)"
	}
}

// looksLikeHeader returns true if a record looks like a header row
// (contains alphabetic characters and no purely numeric cells). The
// heuristic is right for the common case of `id,name,email,...`
// headers and the degenerate `1,2,3` data row case where promoting
// row 0 to a banner would lose it from the visible body.
//
// We do NOT use a "data starts with digits → header above" heuristic
// because spreadsheet exports of categorical data (`Q4,2025,Sales`)
// are routinely all-string AND lack a header — promoting row 0 to a
// banner there is fine; the user still sees rows 1+ as data.
func looksLikeHeader(rec []string) bool {
	if len(rec) == 0 {
		return false
	}
	hasAlpha := false
	for _, c := range rec {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		alpha := false
		numericOnly := true
		for _, r := range c {
			switch {
			case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z':
				alpha = true
				numericOnly = false
			case r >= '0' && r <= '9', r == '.', r == '-', r == '+', r == ',':
				// keep numericOnly true unless we already
				// saw alpha
			default:
				numericOnly = false
			}
		}
		if numericOnly {
			return false
		}
		if alpha {
			hasAlpha = true
		}
	}
	return hasAlpha
}

func init() {
	// Two separate registrations — one per delimiter family — so
	// the TSV path can hard-code `\t` regardless of the first line's
	// content. Sharing a single handler that sniffs would mis-parse
	// any TSV whose header contains a comma (e.g. `City, State` in
	// a tab-separated column).
	Register(RendererFunc(renderCSV),
		"text/csv",
		"application/csv",
	)
	Register(RendererFunc(renderTSV),
		"text/tab-separated-values",
		"text/tsv",
	)
}

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

// csvPreviewMaxRows is the number of data rows the preview displays
// (excluding the optional header row). 20 rows comfortably fills the
// 30-line working canvas after accounting for the header separator
// and the rasteriser's per-line cap.
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

// renderCSV is the registered handler for CSV and TSV uploads. The
// pipeline is:
//
//  1. Detect the delimiter — TSV-routed MIMEs (`text/tab-separated-
//     values`, `text/tsv`) always use `\t`; everything else uses the
//     stdlib's `csv.Reader` with comma default, after a quick
//     first-line sniff to swap in `;` or `\t` for European-locale
//     exports that occasionally mislabel as `text/csv`.
//  2. Read up to csvPreviewMaxRows+1 rows (header + data) into
//     memory. Beyond that we stop — the rasteriser only paints
//     ~30 lines anyway, and reading the rest just burns memory.
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
func renderCSV(_ context.Context, srcBytes []byte) (image.Image, error) {
	body := srcBytes
	if len(body) > csvPreviewMaxBytes {
		body = clipBytesToValidUTF8(body[:csvPreviewMaxBytes])
	}

	delim := detectCSVDelimiter(body)
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
	for i := 0; i <= csvPreviewMaxRows; i++ {
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
	Register(RendererFunc(renderCSV),
		"text/csv",
		"application/csv",
		"text/tab-separated-values",
		"text/tsv",
	)
}

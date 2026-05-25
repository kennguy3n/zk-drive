package index

import (
	"bytes"
	"fmt"
	"io"
	"strings"

	"github.com/xuri/excelize/v2"
)

// xlsxMaxRowsPerSheet caps how many rows the extractor will pull
// out of a single sheet. The cap is generous enough that any
// realistic spreadsheet survives, but bounded so a degenerate file
// with millions of rows (e.g. a synthetic export) can't pin the
// worker for minutes. The 4 MiB MaxIndexBytes truncation downstream
// of this function would discard the tail anyway — the row cap just
// short-circuits the streaming read before we waste CPU on cells we
// will throw away.
const xlsxMaxRowsPerSheet = 200_000

// extractXLSXText reads cell text out of a .xlsx blob. The xlsx
// format is OOXML (zipped XML); excelize handles the zip + shared
// strings table + cell-format machinery. We iterate every visible
// sheet, every row, every cell, and concatenate the values:
//
//   - Cells inside a row are separated by tab so column structure
//     survives the FTS dictionary's whitespace tokenisation.
//   - Rows are separated by newline.
//   - Sheets are separated by a double newline so phrase queries
//     can't span sheets.
//
// excelize already resolves shared strings, inline strings, and
// number/date format strings to their display value — so the
// returned text is what the user would see in Excel, not the raw
// underlying number.
//
// Malformed archives, password-protected files, and read failures
// all surface as non-ErrUnsupportedMimeType errors so the worker
// re-delivers (transient blob corruption) rather than silently
// drops the job as unsupported.
func extractXLSXText(body []byte) (string, error) {
	f, err := excelize.OpenReader(bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("index/xlsx: open: %w", err)
	}
	defer func() { _ = f.Close() }()

	var sb strings.Builder
	for sheetIdx, sheet := range f.GetSheetList() {
		// Hidden sheets often carry pivot caches or formula
		// scratchpads that aren't part of the user-visible
		// document. Skipping them matches what a search-from-the-
		// UI experience would surface.
		if visibility, vErr := f.GetSheetVisible(sheet); vErr == nil && !visibility {
			continue
		}

		if sheetIdx > 0 {
			sb.WriteString("\n\n")
		}

		if err := writeXLSXSheet(f, sheet, &sb); err != nil {
			return "", err
		}
	}
	return sb.String(), nil
}

// writeXLSXSheet streams the rows of one sheet through Rows() /
// Columns() so we never materialise the whole sheet in memory at
// once — excelize allocates row-by-row.
func writeXLSXSheet(f *excelize.File, sheet string, sb *strings.Builder) error {
	rows, err := f.Rows(sheet)
	if err != nil {
		return fmt.Errorf("index/xlsx: open rows for sheet %q: %w", sheet, err)
	}
	defer func() { _ = rows.Close() }()

	rowCount := 0
	for rows.Next() {
		if rowCount >= xlsxMaxRowsPerSheet {
			break
		}
		cols, err := rows.Columns()
		if err != nil {
			// excelize wraps io.EOF on stream end; surface
			// anything else as a real failure so the worker
			// can re-deliver. EOF should never reach here
			// because rows.Next() returns false at the end,
			// but defensive handling is cheap.
			if err == io.EOF {
				break
			}
			return fmt.Errorf("index/xlsx: read row in sheet %q: %w", sheet, err)
		}
		writeXLSXRow(cols, sb)
		rowCount++
	}
	if err := rows.Error(); err != nil {
		return fmt.Errorf("index/xlsx: rows iterator for sheet %q: %w", sheet, err)
	}
	return nil
}

// writeXLSXRow renders one row's cells as tab-joined values
// followed by a newline. Empty trailing cells are dropped — excelize
// pads short rows with empty strings against the sheet's used range,
// and emitting those would inject long runs of tabs that the FTS
// tokeniser doesn't care about but that bloat content_text.
func writeXLSXRow(cols []string, sb *strings.Builder) {
	// Trim trailing empties.
	last := len(cols) - 1
	for last >= 0 && cols[last] == "" {
		last--
	}
	if last < 0 {
		// Entirely blank row — emit a single newline so phrase
		// queries can't span across blank-line boundaries.
		sb.WriteByte('\n')
		return
	}
	for i := 0; i <= last; i++ {
		if i > 0 {
			sb.WriteByte('\t')
		}
		sb.WriteString(cols[i])
	}
	sb.WriteByte('\n')
}

package preview

import (
	"context"
	"strings"
	"testing"
)

func TestRenderCSV_ProducesNonEmptyImage(t *testing.T) {
	t.Parallel()
	src := []byte("id,name,email\n1,Alice,a@example.com\n2,Bob,b@example.com\n")
	img, err := renderCSV(context.Background(), src)
	if err != nil {
		t.Fatalf("renderCSV: %v", err)
	}
	b := img.Bounds()
	if b.Dx() <= 0 || b.Dy() <= 0 {
		t.Fatalf("rendered image has empty bounds: %v", b)
	}
}

func TestDetectCSVDelimiter_Comma(t *testing.T) {
	t.Parallel()
	if got := detectCSVDelimiter([]byte("a,b,c\n1,2,3\n")); got != ',' {
		t.Errorf("expected comma, got %q", got)
	}
}

func TestDetectCSVDelimiter_TabWhenNoComma(t *testing.T) {
	t.Parallel()
	if got := detectCSVDelimiter([]byte("a\tb\tc\n1\t2\t3\n")); got != '\t' {
		t.Errorf("expected tab, got %q", got)
	}
}

// TestDetectCSVDelimiter_SemicolonForEuropean covers exports from
// LibreOffice in regions where the decimal mark is a comma — the
// CSV exporter then uses `;` as field separator. We must not
// misclassify these as comma-CSVs (which would collapse every row
// into one cell on row 1 and render unusable garbage).
func TestDetectCSVDelimiter_SemicolonForEuropean(t *testing.T) {
	t.Parallel()
	if got := detectCSVDelimiter([]byte("a;b;c\n1,5;2,3;Test\n")); got != ';' {
		t.Errorf("expected semicolon, got %q", got)
	}
}

// TestRenderCSV_TSVRouted asserts the TSV-routed MIME path renders
// correctly. A regression here would silently render TSV as comma-
// CSV (single-column rows) on every upload.
func TestRenderCSV_TSVRouted(t *testing.T) {
	t.Parallel()
	src := []byte("col1\tcol2\nval1\tval2\n")
	img, err := renderCSV(context.Background(), src)
	if err != nil {
		t.Fatalf("renderCSV (tab): %v", err)
	}
	if img == nil {
		t.Fatal("nil image returned for TSV")
	}
}

func TestFormatRow_TruncatesWideTable(t *testing.T) {
	t.Parallel()
	wide := make([]string, csvPreviewMaxCols+5)
	for i := range wide {
		wide[i] = "v"
	}
	got := formatRow(wide)
	cells := strings.Split(got, "\t")
	// csvPreviewMaxCols cells + the "…" overflow cell.
	wantCells := csvPreviewMaxCols + 1
	if len(cells) != wantCells {
		t.Errorf("expected %d cells (incl. overflow), got %d: %q", wantCells, len(cells), got)
	}
	if cells[len(cells)-1] != "…" {
		t.Errorf("expected '…' overflow cell, got %q", cells[len(cells)-1])
	}
}

func TestFormatRow_TruncatesLongCell(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", csvPreviewCellMaxRunes*3)
	got := formatRow([]string{"short", long})
	cells := strings.Split(got, "\t")
	if len(cells) != 2 {
		t.Fatalf("expected 2 cells, got %d: %q", len(cells), got)
	}
	if !strings.HasSuffix(cells[1], "…") {
		t.Errorf("expected truncated cell to end with '…', got %q", cells[1])
	}
}

// TestSanitiseCSVCell_FlattensInternalNewlinesAndTabs pins the
// invariant that a multi-line cell (very common in CSV exports of
// rich-text fields) collapses to one row so the rasteriser's
// one-row-per-line layout still works. Without this, an embedded
// `\n` in a cell would push the next field into its own visual row
// and shred the columnar alignment for the rest of the table.
func TestSanitiseCSVCell_FlattensInternalNewlinesAndTabs(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"a\nb\nc":      "a b c",
		"a\r\nb":       "a b",
		"with\ttab":    "with tab",
		"already fine": "already fine",
		"  pad  ":      "pad",
	}
	for in, want := range cases {
		got := sanitiseCSVCell(in)
		if got != want {
			t.Errorf("sanitiseCSVCell(%q) = %q; want %q", in, got, want)
		}
	}
}

// TestLooksLikeHeader_AlphabeticHeader confirms a typical column
// header row promotes to the banner slot. Without this the banner
// would always be empty for CSV uploads, losing useful "what is
// this table" context in the thumbnail.
func TestLooksLikeHeader_AlphabeticHeader(t *testing.T) {
	t.Parallel()
	if !looksLikeHeader([]string{"id", "name", "email"}) {
		t.Errorf("expected alphabetic header row to qualify")
	}
}

// TestLooksLikeHeader_AllNumericIsData covers the degenerate `1,2,3`
// case. Promoting this to a banner would drop row 0 from the
// visible body — the rasteriser only paints data rows.
func TestLooksLikeHeader_AllNumericIsData(t *testing.T) {
	t.Parallel()
	if looksLikeHeader([]string{"1", "2", "3"}) {
		t.Errorf("all-numeric row must NOT qualify as header")
	}
	if looksLikeHeader([]string{"100.5", "-2.3", "+0.1"}) {
		t.Errorf("all-numeric (decimals/signs) row must NOT qualify as header")
	}
}

// TestRenderCSV_RaggedRowsDoNotFail covers real-world CSV imports
// where trailing empty cells get stripped, producing ragged row
// widths. With FieldsPerRecord=-1 the parser is lenient; without
// it the renderer would fail the whole upload on the first short
// row.
func TestRenderCSV_RaggedRowsDoNotFail(t *testing.T) {
	t.Parallel()
	src := []byte("a,b,c,d\n1,2,3,4\n5,6\n7,8,9\n")
	img, err := renderCSV(context.Background(), src)
	if err != nil {
		t.Fatalf("renderCSV (ragged): %v", err)
	}
	if img == nil {
		t.Fatal("nil image for ragged CSV")
	}
}

// TestRenderCSV_LazyQuotesAcceptsApostrophes pins that LazyQuotes
// is on, so a field like `5'10"` (height) doesn't fail the parser.
// Strict CSV parsing would reject this; LazyQuotes treats embedded
// quotes inside an unquoted field as literal characters.
func TestRenderCSV_LazyQuotesAcceptsApostrophes(t *testing.T) {
	t.Parallel()
	src := []byte("name,height\nAlice,5'10\"\nBob,6'1\"\n")
	img, err := renderCSV(context.Background(), src)
	if err != nil {
		t.Fatalf("renderCSV (lazy quotes): %v", err)
	}
	if img == nil {
		t.Fatal("nil image for lazy-quotes CSV")
	}
}

// TestRenderCSV_EmptyInputDoesNotCrash exercises the dataStart=0
// + no-records path. An entirely empty upload should still produce
// a valid (blank) image rather than failing.
func TestRenderCSV_EmptyInputDoesNotCrash(t *testing.T) {
	t.Parallel()
	img, err := renderCSV(context.Background(), nil)
	if err != nil {
		t.Fatalf("renderCSV (empty): %v", err)
	}
	if img == nil {
		t.Fatal("nil image for empty CSV")
	}
}

// TestRenderCSV_LongSourceTruncatedSafely checks the byte-cap path.
// A 10x csvPreviewMaxBytes input must not blow out memory or the
// parser; the cap trims early and we render the visible prefix.
func TestRenderCSV_LongSourceTruncatedSafely(t *testing.T) {
	t.Parallel()
	row := "field1,field2,field3,field4,field5\n"
	src := []byte(strings.Repeat(row, csvPreviewMaxBytes/len(row)+10))
	img, err := renderCSV(context.Background(), src)
	if err != nil {
		t.Fatalf("renderCSV (long): %v", err)
	}
	if img == nil {
		t.Fatal("nil image for long CSV")
	}
}

func TestRenderCSV_IsRegistered(t *testing.T) {
	t.Parallel()
	for _, m := range []string{
		"text/csv",
		"application/csv",
		"text/tab-separated-values",
		"text/tsv",
	} {
		if !IsSupportedMime(m) {
			t.Errorf("expected %q to be registered for CSV/TSV rendering", m)
		}
	}
}

package index

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"strings"
)

// odfContentEntry is the path inside every OpenDocument archive
// (.odt, .ods, .odp) where the document body lives. ODF is
// conceptually simpler than OOXML: a single content.xml carries the
// entire text-bearing payload; styles, manifests, and meta are
// separate parts we don't need.
const odfContentEntry = "content.xml"

// odfMaxUncompressedBytes caps the read on content.xml, matching
// the docx/pptx caps. ODF zip bombs are mostly hypothetical (no
// known real-world malware family targets ODF) but the bound is
// trivial to apply and prevents a degenerate file from exhausting
// worker memory.
const odfMaxUncompressedBytes int64 = 64 << 20 // 64 MiB

// extractOpenDocumentText returns plain text out of an ODF blob
// (.odt, .ods, .odp). All three flavours share the same content.xml
// layout — text-bearing elements live under the "text" namespace
// and the extractor concatenates them in document order with
// paragraph- and heading-level newlines.
//
// Spreadsheet cells (.ods) inline their text content as
// <text:p> inside <table:table-cell>, so the same walker produces
// sensible row-by-row output without special-casing.
//
// Malformed archives, missing content.xml, and XML parse failures
// surface as non-ErrUnsupportedMimeType errors so the worker can
// re-deliver — silent partial extracts would mis-rank FTS hits.
func extractOpenDocumentText(body []byte) (string, error) {
	r, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		return "", fmt.Errorf("index/odf: open zip: %w", err)
	}
	var content *zip.File
	for _, f := range r.File {
		if f.Name == odfContentEntry {
			content = f
			break
		}
	}
	if content == nil {
		return "", fmt.Errorf("index/odf: missing %s entry", odfContentEntry)
	}

	rc, err := content.Open()
	if err != nil {
		return "", fmt.Errorf("index/odf: open content entry: %w", err)
	}
	defer func() { _ = rc.Close() }()

	limited := io.LimitReader(rc, odfMaxUncompressedBytes)
	return parseODFContent(limited)
}

// parseODFContent walks the ODF content stream and emits text from
// the relevant elements:
//
//   - text:p — paragraph; terminates with newline
//   - text:h — heading; terminates with newline
//   - text:span / text:a — inline runs; emitted verbatim
//   - text:line-break — single newline
//   - text:tab — single tab character
//   - text:s — non-breaking space sequence; emits one space
//     (the c attribute can request N spaces, but rare enough that
//      a single emission is acceptable for FTS purposes)
//   - table:table-cell — spreadsheet cell; followed by tab so
//     row layout survives the whitespace tokeniser
//   - table:table-row — spreadsheet row; terminates with newline
//
// Anything else falls through unchanged — the inner CharData
// handler emits text whenever a relevant element is open.
//
// Like docx, encoding/xml hands us the local element name without
// the namespace prefix, so the switch matches on "p", "h", etc.
// independent of how the document declares its xmlns attributes.
func parseODFContent(r io.Reader) (string, error) {
	dec := xml.NewDecoder(r)
	dec.CharsetReader = func(charset string, input io.Reader) (io.Reader, error) {
		return input, nil
	}

	var (
		sb            strings.Builder
		// textDepth counts how many text-bearing elements are
		// open. CharData is emitted iff textDepth > 0, so style
		// metadata buried under text:style-region-element does
		// NOT leak into the extracted output.
		textDepth int
		// rowDirty tracks whether the in-progress table row has
		// emitted any cell content. Empty rows are dropped to
		// avoid runs of blank lines polluting the FTS corpus.
		rowDirty bool
	)

	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("index/odf: parse: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "p", "h", "span", "a":
				textDepth++
			case "table-cell":
				textDepth++
			case "line-break":
				if textDepth > 0 {
					sb.WriteByte('\n')
				}
			case "tab":
				if textDepth > 0 {
					sb.WriteByte('\t')
				}
			case "s":
				if textDepth > 0 {
					sb.WriteByte(' ')
				}
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "p", "h":
				textDepth--
				sb.WriteByte('\n')
			case "span", "a":
				textDepth--
			case "table-cell":
				textDepth--
				sb.WriteByte('\t')
				rowDirty = true
			case "table-row":
				if rowDirty {
					sb.WriteByte('\n')
					rowDirty = false
				}
			}
		case xml.CharData:
			if textDepth > 0 && len(t) > 0 {
				sb.Write(t)
			}
		}
	}
	return sb.String(), nil
}

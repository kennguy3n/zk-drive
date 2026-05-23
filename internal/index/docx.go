package index

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"strings"
)

// docxDocumentEntry is the path inside a .docx zip archive where the
// primary document body lives. .docx is the WordprocessingML
// flavour of OOXML (ECMA-376); the body XML is always at this path.
const docxDocumentEntry = "word/document.xml"

// docxMaxUncompressedBytes caps how much XML the extractor will read
// out of a single docx archive. The cap is generous compared to
// MaxIndexBytes (4 MiB of text) because XML markup inflates the
// underlying string content by a small but non-zero factor — but it
// is bounded so a zip-bomb (.docx wrapping a tiny compressed entry
// that expands to gigabytes of nested <w:t>) cannot exhaust worker
// memory.
const docxMaxUncompressedBytes int64 = 64 << 20 // 64 MiB

// extractDOCXText reads plain text out of a .docx blob. The .docx
// format is a zip archive of XML parts; we open it with archive/zip
// (rejecting truncated / malformed archives), stream the body XML
// with encoding/xml, and accumulate the contents of <w:t> elements
// joined by paragraph-level newlines.
//
// Malformed archives, missing document entries, and XML parse
// failures all return a non-ErrUnsupportedMimeType error so the
// worker re-delivers the job (catches transient blob-corruption
// during upload) rather than silently dropping a real failure as
// an unsupported-type ack. The same correctness invariant pinned by
// internal/preview/pdf_test.go's "invalid PDF must NOT masquerade
// as ErrUnsupportedMime" test applies here.
func extractDOCXText(docxBytes []byte) (string, error) {
	r, err := zip.NewReader(bytes.NewReader(docxBytes), int64(len(docxBytes)))
	if err != nil {
		return "", fmt.Errorf("index/docx: open zip: %w", err)
	}

	var doc *zip.File
	for _, f := range r.File {
		if f.Name == docxDocumentEntry {
			doc = f
			break
		}
	}
	if doc == nil {
		return "", fmt.Errorf("index/docx: missing %s entry", docxDocumentEntry)
	}

	rc, err := doc.Open()
	if err != nil {
		return "", fmt.Errorf("index/docx: open entry: %w", err)
	}
	defer func() { _ = rc.Close() }()

	// Cap uncompressed read so a zip-bomb cannot exhaust memory.
	// LimitReader returns EOF at the cap; the XML decoder will then
	// either finish cleanly (file was within the cap) or surface a
	// truncation error which is a legitimate parse failure.
	limited := io.LimitReader(rc, docxMaxUncompressedBytes)
	return parseDOCXBody(limited)
}

// parseDOCXBody walks the document XML and concatenates the textual
// content of every <w:t> element. Paragraph boundaries (<w:p>) are
// flushed as newlines so the resulting text preserves enough
// structure for FTS phrase queries to make sense.
//
// Other constructs:
//   - <w:tab> renders as a single tab character — mirrors what a
//     user would see if they opened the document.
//   - <w:br> (line break) flushes a newline.
//   - <w:t xml:space="preserve"> is handled implicitly because we
//     emit the CharData verbatim; the decoder hands us the original
//     bytes including any leading/trailing whitespace.
func parseDOCXBody(r io.Reader) (string, error) {
	dec := xml.NewDecoder(r)
	// Allow non-UTF-8 input charsets to no-op pass-through. The
	// .docx spec mandates UTF-8 for the body XML, but the field is
	// permissive enough in the wild that refusing on a stray
	// charset declaration would needlessly fail real-world files.
	dec.CharsetReader = func(charset string, input io.Reader) (io.Reader, error) {
		return input, nil
	}

	var (
		sb           strings.Builder
		inText       bool
		paraDirty    bool
		decoderError error
	)

	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			decoderError = err
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "t":
				inText = true
			case "tab":
				sb.WriteByte('\t')
				paraDirty = true
			case "br":
				sb.WriteByte('\n')
				paraDirty = true
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "t":
				inText = false
			case "p":
				// Paragraph terminator: emit a newline if the
				// paragraph had any visible content. Empty
				// paragraphs (e.g. spacer <w:p/>) are dropped so
				// FTS phrase queries don't see long runs of
				// blank lines.
				if paraDirty {
					sb.WriteByte('\n')
					paraDirty = false
				}
			}
		case xml.CharData:
			if inText && len(t) > 0 {
				sb.Write(t)
				paraDirty = true
			}
		}
	}

	if decoderError != nil {
		// Surface real parse failures (malformed body XML, mid-token
		// truncation by the LimitReader at docxMaxUncompressedBytes,
		// etc.) so the worker re-delivers rather than silently
		// writing a partial extract. encoding/xml reports mid-token
		// EOF as *xml.SyntaxError, not io.ErrUnexpectedEOF — so there
		// is no special-case to swallow here. A LimitReader cap hit
		// between tokens is delivered as a clean io.EOF and handled
		// at the top of the loop (line 100 above); a cap hit
		// mid-token surfaces here and is a legitimate failure given
		// the 64 MiB cap is well above any well-formed .docx body.
		return "", fmt.Errorf("index/docx: parse body xml: %w", decoderError)
	}

	return sb.String(), nil
}

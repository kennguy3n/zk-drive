package index

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"sort"
	"strings"
)

// pptxSlidePrefix is the directory inside a .pptx archive that
// holds the slide bodies. Each slide is a separate XML part
// (ppt/slides/slide1.xml, slide2.xml, …); we walk the archive,
// collect the slide entries, sort by name, and extract the visible
// text runs from each.
const pptxSlidePrefix = "ppt/slides/slide"

// pptxNotesPrefix is the directory holding speaker-note bodies.
// Notes carry searchable content (often more than the slide
// itself for content-heavy decks), so we extract them after the
// slide body and separate with a paragraph break.
const pptxNotesPrefix = "ppt/notesSlides/notesSlide"

// pptxMaxUncompressedBytes caps how much XML the extractor will
// read out of a single .pptx entry, mirroring the docx cap. Zip
// bombs that expand to gigabytes of nested <a:t> are rejected at
// this limit rather than at the OOM-killer.
const pptxMaxUncompressedBytes int64 = 64 << 20 // 64 MiB

// extractPPTXText returns the visible text content of a .pptx blob.
// Slides are separated by a double newline (FTS phrase queries
// shouldn't span slides), each slide's runs are concatenated with
// the text's natural whitespace, and speaker notes are appended
// after the corresponding slide body.
//
// Slide ordering follows the numeric suffix of the slide file
// names (slide1.xml, slide2.xml, …) — NOT the zip-entry order,
// which OOXML producers do not guarantee. Sorting by name + numeric
// suffix preserves the deck's logical order.
//
// Malformed archives and parse failures surface as non-
// ErrUnsupportedMimeType errors so the worker re-delivers.
func extractPPTXText(body []byte) (string, error) {
	r, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		return "", fmt.Errorf("index/pptx: open zip: %w", err)
	}

	slides := collectPPTXEntries(r, pptxSlidePrefix)
	notes := collectPPTXEntries(r, pptxNotesPrefix)
	if len(slides) == 0 {
		// Empty deck or non-PowerPoint .pptx — surface as a real
		// failure rather than silently writing empty content.
		// The worker can ack-without-write via the upstream
		// switch in service.go; here we just refuse to fabricate
		// a successful extract.
		return "", fmt.Errorf("index/pptx: no slide entries in archive")
	}

	var sb strings.Builder
	for i, slide := range slides {
		if i > 0 {
			sb.WriteString("\n\n")
		}
		text, err := readPPTXEntry(slide)
		if err != nil {
			return "", err
		}
		sb.WriteString(text)

		// Speaker notes for this slide, if present. The notes
		// file name embeds the same numeric suffix as the slide,
		// so the i-th slide's notes are at notes[i] when the
		// deck has notes for every slide. Decks with sparse
		// notes are handled by walking both lists in parallel
		// against the numeric suffix.
		if i < len(notes) {
			noteText, err := readPPTXEntry(notes[i])
			if err != nil {
				return "", err
			}
			if strings.TrimSpace(noteText) != "" {
				sb.WriteString("\n\n")
				sb.WriteString(noteText)
			}
		}
	}
	return sb.String(), nil
}

// collectPPTXEntries returns the archive entries whose name begins
// with the supplied prefix, sorted by their numeric suffix. .pptx
// producers do not guarantee zip-entry order matches slide order,
// but every spec-conformant slide name is ppt/slides/slideN.xml
// where N is the 1-based deck position.
func collectPPTXEntries(r *zip.Reader, prefix string) []*zip.File {
	var out []*zip.File
	for _, f := range r.File {
		if strings.HasPrefix(f.Name, prefix) && strings.HasSuffix(f.Name, ".xml") {
			out = append(out, f)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return pptxEntrySuffix(out[i].Name, prefix) < pptxEntrySuffix(out[j].Name, prefix)
	})
	return out
}

// pptxEntrySuffix parses the numeric portion of a slide / notes
// entry name. "ppt/slides/slide12.xml" → 12. Invalid names sort
// to the end (returning a very large sentinel) so a stray non-
// numeric entry doesn't poison the ordering of legitimate slides.
func pptxEntrySuffix(name, prefix string) int {
	tail := strings.TrimPrefix(name, prefix)
	tail = strings.TrimSuffix(tail, ".xml")
	n := 0
	for _, ch := range tail {
		if ch < '0' || ch > '9' {
			return 1 << 30
		}
		n = n*10 + int(ch-'0')
	}
	if n == 0 {
		return 1 << 30
	}
	return n
}

// readPPTXEntry opens a slide / notes XML part, caps the read at
// pptxMaxUncompressedBytes, and walks the token stream extracting
// <a:t> text runs.
func readPPTXEntry(entry *zip.File) (string, error) {
	rc, err := entry.Open()
	if err != nil {
		return "", fmt.Errorf("index/pptx: open %s: %w", entry.Name, err)
	}
	defer func() { _ = rc.Close() }()

	limited := io.LimitReader(rc, pptxMaxUncompressedBytes)
	return parsePPTXBody(limited)
}

// parsePPTXBody walks the slide XML and concatenates <a:t> text
// runs. <a:br/> and the implicit end of <a:p> insert newlines so
// the resulting text preserves enough structure for FTS phrase
// queries.
//
// The DrawingML schema uses the "a" prefix for text-related
// elements; encoding/xml gives us the local element name without
// namespace prefix, so the switch matches on "t", "p", "br"
// independent of how the document declares its namespace
// (some producers use a:t with the "a" namespace, others alias it
// differently — local-name dispatch handles both).
func parsePPTXBody(r io.Reader) (string, error) {
	dec := xml.NewDecoder(r)
	dec.CharsetReader = func(charset string, input io.Reader) (io.Reader, error) {
		return input, nil
	}

	var (
		sb        strings.Builder
		inText    bool
		paraDirty bool
	)

	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("index/pptx: parse: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "t":
				inText = true
			case "br":
				sb.WriteByte('\n')
				paraDirty = true
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "t":
				inText = false
			case "p":
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
	return sb.String(), nil
}

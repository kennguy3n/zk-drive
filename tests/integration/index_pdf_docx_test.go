package integration

import (
	"archive/zip"
	"bytes"
	"context"
	_ "embed"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kennguy3n/zk-drive/internal/index"
)

// minimalPDFFixture is the same 1-page 200x200 pt PDF used by
// internal/preview/pdf_test.go. The content stream embeds the literal
// string "Hello PDF" so pdftotext extracts a known body word.
//
//go:embed testdata/minimal.pdf
var minimalPDFFixture []byte

// TestIndexWorkerExtractsTextFromPDF asserts that the full
// upload → extract → persist → FTS query loop works for PDFs:
// after a PDF file is registered and index.ExtractText writes its
// content_text, the API search endpoint surfaces the file by a word
// present only in the PDF body, not the filename.
//
// Skipped automatically when pdftotext is not on PATH so the test
// passes on hosts that intentionally strip poppler-utils — the
// graceful-skip path is exercised by the unit test in
// internal/index/pdf_test.go.
func TestIndexWorkerExtractsTextFromPDF(t *testing.T) {
	if _, err := exec.LookPath("pdftotext"); err != nil {
		t.Skip("pdftotext not available; graceful-skip path covered by internal/index/pdf_test.go")
	}

	env := setupEnv(t)
	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "pw")
	fold := createFolder(t, env, tok.Token, nil, "Docs")

	// Filename intentionally lacks any word from the PDF body so the
	// search hit must come from content_text, not the name index.
	fileID := confirmUploadHelper(
		t, env, tok.Token, fold.ID, "report-1.pdf", "application/pdf",
		int64(len(minimalPDFFixture)),
	)

	got, err := index.ExtractText("application/pdf", minimalPDFFixture)
	if err != nil {
		t.Fatalf("ExtractText(pdf): %v", err)
	}
	if !strings.Contains(got, "Hello PDF") {
		t.Fatalf("extracted pdf text missing canonical body word; got=%q", got)
	}

	svc := index.NewService(env.pool, env.storage, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := svc.PersistContent(ctx, fileID, got); err != nil {
		t.Fatalf("persist content: %v", err)
	}

	// "Hello" is a stopword in many tsvector configs but "PDF" is
	// not. Query on the latter so the assertion isn't sensitive to
	// the Postgres dictionary.
	status, raw := env.httpRequest(http.MethodGet, "/api/search?q=PDF", tok.Token, nil)
	if status != http.StatusOK {
		t.Fatalf("search: status=%d body=%s", status, string(raw))
	}
	var resp struct {
		Hits []struct {
			ID   uuid.UUID `json:"id"`
			Name string    `json:"name"`
		} `json:"hits"`
	}
	env.decodeJSON(raw, &resp)
	for _, h := range resp.Hits {
		if h.ID == fileID {
			return
		}
	}
	t.Fatalf("expected pdf file %s in search hits for body word 'PDF'; hits=%+v", fileID, resp.Hits)
}

// TestIndexWorkerExtractsTextFromDOCX exercises the equivalent loop
// for DOCX. The extractor is pure Go (archive/zip + encoding/xml) so
// no host binary is required and the test never skips.
func TestIndexWorkerExtractsTextFromDOCX(t *testing.T) {
	env := setupEnv(t)
	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "pw")
	fold := createFolder(t, env, tok.Token, nil, "Docs")

	const distinctive = "elderberrymarmalade"
	docxBytes := buildIntegrationDOCX(t, []string{
		"Project status update",
		"This release ships the " + distinctive + " behaviour.",
	})

	// Filename has no overlap with the body words we'll search for.
	fileID := confirmUploadHelper(
		t, env, tok.Token, fold.ID,
		"q3-update.docx",
		"application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		int64(len(docxBytes)),
	)

	got, err := index.ExtractText(
		"application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		docxBytes,
	)
	if err != nil {
		t.Fatalf("ExtractText(docx): %v", err)
	}
	if !strings.Contains(got, distinctive) {
		t.Fatalf("extracted docx text missing distinctive token; got=%q", got)
	}

	svc := index.NewService(env.pool, env.storage, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := svc.PersistContent(ctx, fileID, got); err != nil {
		t.Fatalf("persist content: %v", err)
	}

	status, raw := env.httpRequest(http.MethodGet, "/api/search?q="+distinctive, tok.Token, nil)
	if status != http.StatusOK {
		t.Fatalf("search: status=%d body=%s", status, string(raw))
	}
	var resp struct {
		Hits []struct {
			ID   uuid.UUID `json:"id"`
			Name string    `json:"name"`
		} `json:"hits"`
	}
	env.decodeJSON(raw, &resp)
	for _, h := range resp.Hits {
		if h.ID == fileID {
			return
		}
	}
	t.Fatalf("expected docx file %s in search hits for body word %q; hits=%+v", fileID, distinctive, resp.Hits)
}

// buildIntegrationDOCX returns the bytes of a valid .docx archive
// whose body contains the supplied paragraphs in order. Pure Go via
// archive/zip + encoding/xml so the test fixture is generated at run
// time instead of being checked in as a binary blob.
func buildIntegrationDOCX(t *testing.T, paragraphs []string) []byte {
	t.Helper()
	var sb strings.Builder
	sb.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>`)
	sb.WriteString(`<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">`)
	sb.WriteString(`<w:body>`)
	for _, p := range paragraphs {
		sb.WriteString(`<w:p><w:r><w:t xml:space="preserve">`)
		sb.WriteString(p)
		sb.WriteString(`</w:t></w:r></w:p>`)
	}
	sb.WriteString(`</w:body></w:document>`)

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("word/document.xml")
	if err != nil {
		t.Fatalf("zip create entry: %v", err)
	}
	if _, err := w.Write([]byte(sb.String())); err != nil {
		t.Fatalf("zip write entry: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

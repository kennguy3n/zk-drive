package classify

import "testing"

// TestLabelForRuleOrdering walks the pure rule table in
// labelFor. The rules are intentionally ordered most-specific →
// least so "invoice.pdf" picks LabelInvoice rather than the generic
// LabelDocument; this test pins that ordering against regressions.
func TestLabelForRuleOrdering(t *testing.T) {
	tests := []struct {
		name  string
		fname string
		mime  string
		want  string
	}{
		// Image MIME wins over name-based rules — an image named
		// "invoice.png" is still classified as an image.
		{"image jpeg by mime", "scan.jpg", "image/jpeg", LabelImage},
		{"image png by mime overrides name", "invoice.png", "image/png", LabelImage},
		{"image mime with no extension", "untitled", "image/heic", LabelImage},

		// Invoice rule fires on name substring, including case-folded
		// matches, and beats the generic PDF rule.
		{"invoice in name lowercase", "invoice-2024.pdf", "application/pdf", LabelInvoice},
		{"invoice in name uppercase", "INVOICE_FINAL.pdf", "application/pdf", LabelInvoice},
		{"invoice in name no extension", "January Invoice", "", LabelInvoice},

		// Contract rule fires before the PDF rule.
		{"contract in name lowercase", "msa-contract.pdf", "application/pdf", LabelContract},
		{"contract in name uppercase", "ContractDraft.docx", "application/msword", LabelContract},

		// PDF without invoice/contract substring picks document.
		{"plain pdf", "report.pdf", "application/pdf", LabelDocument},

		// Fallback: unknown mime + neutral name.
		{"unknown text", "notes.txt", "text/plain", LabelOther},
		{"empty mime fallback", "data", "", LabelOther},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got := labelFor(tc.fname, tc.mime)
			if got != tc.want {
				t.Fatalf("labelFor(%q, %q) = %q, want %q", tc.fname, tc.mime, got, tc.want)
			}
		})
	}
}

// TestLabelConstants pins the public label strings against accidental
// edits. The values are part of the API contract — they end up in the
// `files.classification` column and any persisted row reading
// LabelInvoice expects the literal "invoice", not whatever a refactor
// might produce.
func TestLabelConstants(t *testing.T) {
	for _, tc := range []struct {
		got, want string
	}{
		{LabelImage, "image"},
		{LabelInvoice, "invoice"},
		{LabelContract, "contract"},
		{LabelDocument, "document"},
		{LabelOther, "other"},
	} {
		if tc.got != tc.want {
			t.Fatalf("label constant drift: got %q want %q", tc.got, tc.want)
		}
	}
}

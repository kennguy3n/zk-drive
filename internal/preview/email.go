package preview

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"io"
	"net/mail"
	"strings"
)

// emailBodyPreviewBytes is the cap on how much of the email body we
// read into the preview. Headers + ~6 KiB of body is plenty for a
// 256 px thumbnail and protects against multi-megabyte HTML emails
// that could bloat the worker for no visible benefit.
const emailBodyPreviewBytes = 6 * 1024

// renderEmail parses an RFC 5322 message (.eml) and renders a
// preview that shows the most useful headers plus the first chunk
// of the body. We don't attempt to render HTML — running an HTML
// renderer over arbitrary user input is a security and rendering
// rabbit hole; the goal here is "what was this email roughly about",
// not "perfectly faithful client view".
//
// mbox files (a concatenation of RFC 5322 messages separated by
// `From ` envelope lines) are also handled: we extract the first
// message and render it. We do NOT render all messages — the preview
// is a single 256 px thumbnail and showing more than one message
// makes the output unreadable.
//
// Outlook .msg files (TNEF / CFB containers) need a separate parser;
// for now we register only RFC 822 / EML / mbox and let .msg fall
// through to the unsupported path. A future iteration can add a
// libemldb / libmsg dependency if there's demand.
func renderEmail(_ context.Context, src []byte) (image.Image, error) {
	// If the input looks like an mbox file (starts with `From `,
	// with no colon — RFC 5322 headers all have `Header: value`
	// shape), strip the envelope lines and pass only the first
	// message to mail.ReadMessage. mail.ReadMessage would otherwise
	// fail on the leading `From ` line because it's not a valid
	// header (the space-delimited form has no colon).
	if msgBytes := extractFirstMboxMessage(src); msgBytes != nil {
		src = msgBytes
	}
	msg, err := mail.ReadMessage(bytes.NewReader(src))
	if err != nil {
		return nil, fmt.Errorf("parse email: %w", err)
	}
	from := strings.TrimSpace(msg.Header.Get("From"))
	to := strings.TrimSpace(msg.Header.Get("To"))
	subject := strings.TrimSpace(msg.Header.Get("Subject"))
	date := strings.TrimSpace(msg.Header.Get("Date"))

	header := "Email"
	if subject != "" {
		header = subject
	}
	// Trim very long subjects so the header doesn't bleed off the
	// canvas. 64 chars matches our textimage column budget for the
	// 600 px canvas at the inconsolata 8 px advance. We slice by
	// rune count rather than byte length because email subjects are
	// frequently non-ASCII (CJK, Arabic, emoji) and byte-slicing in
	// the middle of a multi-byte UTF-8 sequence would render as
	// U+FFFD replacement glyphs in the preview.
	header = truncateRunes(header, 64, "…")

	var b strings.Builder
	if from != "" {
		fmt.Fprintf(&b, "From: %s\n", from)
	}
	if to != "" {
		fmt.Fprintf(&b, "To:   %s\n", to)
	}
	if date != "" {
		fmt.Fprintf(&b, "Date: %s\n", date)
	}
	if cc := strings.TrimSpace(msg.Header.Get("Cc")); cc != "" {
		fmt.Fprintf(&b, "Cc:   %s\n", cc)
	}
	b.WriteString("\n")

	bodyBytes, _ := io.ReadAll(io.LimitReader(msg.Body, emailBodyPreviewBytes+1))
	body := string(bodyBytes)
	if len(bodyBytes) > emailBodyPreviewBytes {
		body = string(bodyBytes[:emailBodyPreviewBytes]) + "\n…"
	}
	// Strip MIME multipart boundaries / Content-Type headers if this
	// looks like a multipart body — we don't decode each part, but
	// stripping the boundaries cleans up the preview enough to be
	// readable.
	body = stripMimeBoundaries(body)
	b.WriteString(body)

	return renderTextToImage(b.String(), textPreviewOpts{header: header}), nil
}

// stripMimeBoundaries drops MIME boundary lines and the
// Content-Type / Content-Transfer-Encoding pseudo-headers that
// follow them, so a multipart message body doesn't render as a wall
// of "--====_NextPart_xxx_xxxxxxxx" markers.
//
// RFC 2046 §5.1.1 defines a boundary line as "--" followed by the
// boundary token, which is at least one bcharsnospace character.
// Real world boundary patterns we have to handle: Outlook
// "------=_NextPart_000_001D...", Java mail "----=_Part_12345_xxx",
// "--===============xxx", and the simpler "--boundary-xyz" form. The
// common shape is "two-or-more dashes, then at least one non-dash
// character". Plain markdown horizontal rules ("---" with nothing
// else) are NOT boundaries and should pass through unchanged.
func stripMimeBoundaries(body string) string {
	out := make([]string, 0, 64)
	inHeader := false
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if isMimeBoundaryLine(trimmed) {
			// boundary line — start swallowing the per-part headers
			inHeader = true
			continue
		}
		if inHeader {
			if trimmed == "" {
				inHeader = false
			}
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// isMimeBoundaryLine reports whether a (trimmed) line looks like a
// MIME multipart boundary. The check is intentionally heuristic: we
// only need to be good enough to suppress boundary noise in the
// preview, not parse the message correctly. The rule is "starts with
// `--`, has at least one non-dash character after some run of
// dashes" — that matches every boundary form we have seen in the
// wild while keeping plain markdown horizontal rules out.
func isMimeBoundaryLine(trimmed string) bool {
	if !strings.HasPrefix(trimmed, "--") {
		return false
	}
	// Strip all leading dashes; if anything is left, we've got a
	// boundary token. If the line was nothing but dashes (e.g.
	// markdown "---", "----"), this returns "" and we skip it.
	rest := strings.TrimLeft(trimmed, "-")
	if rest == "" {
		return false
	}
	// Likewise, a closing boundary is "--<boundary>--". Trim trailing
	// dashes the same way so we accept those.
	rest = strings.TrimRight(rest, "-")
	return rest != ""
}

// extractFirstMboxMessage returns the bytes of the first RFC 5322
// message inside an mbox file, or nil if the input doesn't look like
// mbox. mbox format: each message starts with a line like
// `From sender@example.com Mon Jan  2 15:04:05 2006`. Subsequent
// messages start with another such line; we cut at the second one
// (or EOF) and strip the leading envelope line so net/mail can parse
// what's left.
//
// We return nil (and the caller proceeds with the original bytes) on
// anything that doesn't look like mbox, so this is a no-op on plain
// .eml files. The detection rule is strict — only the `From ` prefix
// with no colon (the actual mbox envelope shape) qualifies — to
// avoid false positives on RFC 5322 messages whose first body line
// happens to start with "From " (which is rare but legal).
func extractFirstMboxMessage(src []byte) []byte {
	// The defining test is `From ` (space) vs `From:` (colon):
	// the mbox envelope starts with the former, an RFC 5322 header
	// with the latter. The prefix check below covers both
	// false-positive cases — a header line `From: foo@example.com`
	// won't match because it has a colon at position 4 not a space,
	// and a body line `From whoever` is rare enough that we accept
	// the false positive (the preview still renders, just from one
	// line later).
	if !bytes.HasPrefix(src, []byte("From ")) {
		return nil
	}
	nl := bytes.IndexByte(src, '\n')
	if nl < 0 {
		return nil
	}
	// Skip the envelope line + its newline.
	body := src[nl+1:]
	// Find the next `\nFrom ` envelope (start of message 2) and
	// truncate before it. mbox conventionally requires a blank
	// line before the next envelope, but real-world files sometimes
	// omit it; the safer test is just "newline followed by `From `".
	if idx := bytes.Index(body, []byte("\nFrom ")); idx >= 0 {
		body = body[:idx+1] // keep trailing newline of msg 1
	}
	return body
}

func init() {
	Register(RendererFunc(renderEmail),
		"message/rfc822",
		"application/mbox",
		"application/x-mbox",
		"message/rfc2822",
	)
}

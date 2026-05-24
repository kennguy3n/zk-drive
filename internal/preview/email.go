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
// Outlook .msg files (TNEF / CFB containers) need a separate parser;
// for now we register only RFC 822 / EML and let .msg fall through
// to the unsupported path. A future iteration can add a libemldb /
// libmsg dependency if there's demand.
func renderEmail(_ context.Context, src []byte) (image.Image, error) {
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

func init() {
	Register(RendererFunc(renderEmail),
		"message/rfc822",
		"application/mbox",
		"application/x-mbox",
		"message/rfc2822",
	)
}

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
	// 600 px canvas at the inconsolata 8 px advance.
	if len(header) > 64 {
		header = header[:63] + "…"
	}

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
func stripMimeBoundaries(body string) string {
	out := make([]string, 0, 64)
	inHeader := false
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "--") && strings.Contains(trimmed, "-") && !strings.HasPrefix(trimmed, "---") {
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

func init() {
	Register(RendererFunc(renderEmail),
		"message/rfc822",
		"application/mbox",
		"application/x-mbox",
		"message/rfc2822",
	)
}

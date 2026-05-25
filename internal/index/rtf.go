package index

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// extractRTFText returns plain text out of an RTF blob. RTF is a
// hand-rolled markup language (RFC-less but extensively documented
// by Microsoft's RTF 1.9.1 spec) — we do NOT pull a heavyweight
// dependency for it; the on-disk structure is simple enough that a
// state-machine walker produces clean output.
//
// The walker handles:
//
//   - {} group nesting (for tracking destination scope)
//   - \par, \line, \cell, \row — flushed as the appropriate
//     newline / tab so paragraph and table structure survives
//   - \tab — literal tab
//   - \'XX — hex-encoded byte (treated as a code-page Latin-1
//     character; non-ASCII bytes are skipped because we don't
//     know the document's code page without parsing \ansicpg).
//     If the encoded value is < 0x80 we emit it verbatim.
//   - \uN — Unicode escape (16-bit signed integer; negative values
//     are 16-bit two's complement). Followed by one ANSI fallback
//     byte (or group) which we skip via the \ucN counter.
//   - destination control words (\fonttbl, \stylesheet, \info,
//     \colortbl, \pict, \header, \footer, \author, \operator, …)
//     mark a group whose content is metadata, not body text, and
//     are skipped entirely until the closing brace at the same
//     nesting depth.
//   - all other control words / symbols are dropped (no plaintext
//     yield).
//
// The output is the document's visible text run, separated by
// newlines for paragraph boundaries.
//
// Errors surface for truly malformed input — unbalanced braces or
// a control word with no terminator. Callers see these as
// non-ErrUnsupportedMimeType errors so the worker re-delivers.
func extractRTFText(body []byte) (string, error) {
	if len(body) == 0 {
		return "", fmt.Errorf("index/rtf: empty body")
	}
	// RTF files start with the literal sequence "{\rtf". Reject
	// anything else so we don't try to walk a non-RTF document.
	if !strings.HasPrefix(string(body), "{\\rtf") {
		return "", fmt.Errorf("index/rtf: missing RTF preamble")
	}

	var (
		sb       strings.Builder
		i        int
		depth    int
		// skipDepth, if non-zero, indicates we are inside a
		// metadata destination (\fonttbl, etc.) and must drop
		// every character until the matching close brace returns
		// us to the depth at which the destination started.
		skipDepth int
		// ucDefault is the CURRENT group's "\ucN" value — i.e.
		// how many ANSI fallback bytes the SPEC says to skip after
		// every "\u" escape in this group. RTF 1.9.1 §1.4.7: a
		// group inherits its parent's \uc value at open-brace and
		// returns to the parent's value at close-brace. The
		// default at the top level is 1, but macOS TextEdit and
		// some LibreOffice output explicitly emit "\uc0" — under
		// which "\u" must NOT consume any fallback byte. Tracking
		// the default per-group is the only way to honour both
		// dialects correctly; a hardcoded 1 silently swallows one
		// character per Unicode escape under \uc0.
		ucDefault int = 1
		// ucDefaultStack mirrors the open-brace nesting so we can
		// restore the parent group's \uc value on close-brace.
		ucDefaultStack []int
		// ucSkip is the COUNTER of how many fallback bytes still
		// need to be dropped from the current position. It is
		// repopulated to ucDefault after every \u escape.
		ucSkip int
	)

	emitRune := func(r rune) {
		if skipDepth > 0 {
			return
		}
		if ucSkip > 0 {
			ucSkip--
			return
		}
		var buf [utf8.UTFMax]byte
		n := utf8.EncodeRune(buf[:], r)
		sb.Write(buf[:n])
	}

	for i < len(body) {
		c := body[i]
		switch c {
		case '{':
			depth++
			// Inherit the parent group's \uc default; close-brace
			// will restore it. Stacking the DEFAULT (not the
			// in-flight ucSkip counter) is the correct semantics:
			// the spec's \uc value is per-group, the counter
			// itself is just the running consumption tally and
			// resets to zero on close-brace anyway.
			ucDefaultStack = append(ucDefaultStack, ucDefault)
			i++
		case '}':
			depth--
			if depth < 0 {
				return "", fmt.Errorf("index/rtf: unbalanced close brace at offset %d", i)
			}
			if skipDepth > 0 && depth < skipDepth {
				skipDepth = 0
			}
			// Restore the parent group's \uc default and clear
			// any in-flight skip — close-brace ends the unicode
			// fallback window even mid-count, mirroring how Word's
			// RTF reader treats braces as a hard boundary for
			// fallback consumption.
			if n := len(ucDefaultStack); n > 0 {
				ucDefault = ucDefaultStack[n-1]
				ucDefaultStack = ucDefaultStack[:n-1]
			}
			ucSkip = 0
			i++
		case '\\':
			// Parse a control word, control symbol, or hex escape.
			if i+1 >= len(body) {
				return "", fmt.Errorf("index/rtf: dangling backslash at end of input")
			}
			next := body[i+1]
			switch {
			case next == '\'':
				// \'XX — two hex digits encoding one byte.
				if i+3 >= len(body) {
					return "", fmt.Errorf("index/rtf: truncated hex escape at offset %d", i)
				}
				v, err := strconv.ParseUint(string(body[i+2:i+4]), 16, 8)
				if err != nil {
					return "", fmt.Errorf("index/rtf: bad hex escape at offset %d: %w", i, err)
				}
				if v < 0x80 {
					emitRune(rune(v))
				}
				// Non-ASCII hex bytes need a code-page table
				// we don't carry; drop them rather than emit
				// invalid UTF-8. Real-world RTF that's mostly
				// ASCII + Unicode escapes (the common output
				// from Word, LibreOffice, TextEdit) loses
				// nothing here.
				i += 4
			case next == '*':
				// \*\destination — an optional destination
				// the reader can skip if unknown. Always treat
				// the next control word as a metadata
				// destination so its content is dropped.
				i += 2
				name, end := parseControlWord(body, i)
				_ = name // name is consumed; behaviour is unconditional skip.
				if skipDepth == 0 {
					skipDepth = depth
				}
				i = end
			case isLetter(next):
				name, value, hasValue, end := parseControlWordWithValue(body, i+1)
				switch name {
				case "par", "line":
					if skipDepth == 0 {
						sb.WriteByte('\n')
					}
				case "tab", "cell":
					if skipDepth == 0 {
						sb.WriteByte('\t')
					}
				case "row":
					if skipDepth == 0 {
						sb.WriteByte('\n')
					}
				case "u":
					if !hasValue {
						return "", fmt.Errorf("index/rtf: \\u without value at offset %d", i)
					}
					// Negative values are 16-bit two's
					// complement; convert.
					code := value
					if code < 0 {
						code += 0x10000
					}
					if code >= 0 && code <= 0xFFFF {
						r := rune(code)
						if utf16.IsSurrogate(r) {
							// High surrogate; if the next
							// control word is also \u, pair
							// them. Otherwise we drop the
							// orphan since we can't form a
							// valid code point.
							//
							// In real-world RTF, surrogate
							// pairs are rare (most BMP code
							// points fit in a single \u),
							// and Word's RTF writer always
							// emits them as a pair we can
							// merge on the fly.
							if hi := r; utf16.IsSurrogate(hi) {
								// Look ahead for low surrogate.
								if loRune, loEnd, ok := tryReadUnicodeEscape(body, end, ucSkip); ok {
									combined := utf16.DecodeRune(hi, loRune)
									if combined != utf8.RuneError {
										emitRune(combined)
										end = loEnd
									}
								}
							}
						} else {
							emitRune(r)
						}
					}
					// \u consumes ucDefault ANSI fallback
					// bytes after the escape. Under the spec
					// default \uc1 that's one byte; under the
					// macOS / LibreOffice \uc0 dialect it's
					// ZERO bytes, in which case the byte
					// following \u is real content and must
					// NOT be eaten. Replacing the prior
					// hardcoded max(ucSkip, 1) with ucDefault
					// honours both dialects byte-for-byte.
					if ucDefault > 0 {
						ucSkip = ucDefault
					}
				case "uc":
					if hasValue {
						// New \ucN sets the group's default
						// for every subsequent \u. Negative
						// values are clamped to zero (the
						// spec calls them illegal, but real-
						// world RTF emitters occasionally
						// produce \uc-1 as a placeholder).
						if value < 0 {
							value = 0
						}
						ucDefault = value
						// Drop any in-flight skip — a fresh
						// \uc value supersedes the previous
						// default's pending counter.
						ucSkip = 0
					}
				case "fonttbl", "filetbl", "colortbl", "stylesheet",
					"latentstyles", "listtables", "rsidtbl",
					"info", "pict", "object", "shppict", "header",
					"footer", "headerl", "headerr", "footerl",
					"footerr", "headerf", "footerf",
					"comment", "ftncn", "ftnsep", "aftnsep",
					"datafield", "themedata", "colorschememapping",
					"latentstylenamemapping", "xmlnstbl":
					if skipDepth == 0 {
						skipDepth = depth
					}
				}
				i = end
			default:
				// Control symbol — single non-letter character
				// after the backslash. Most of them have no
				// plain-text effect.
				switch next {
				case '~':
					emitRune(' ') // non-breaking space
				case '-':
					// optional hyphen — emit nothing
				case '_':
					emitRune('-') // non-breaking hyphen
				case '\\', '{', '}':
					emitRune(rune(next))
				}
				i += 2
			}
		case '\r', '\n':
			// Bare CR/LF inside the document are formatting,
			// not content. RTF uses \par for real paragraph
			// breaks. Skip them.
			i++
		default:
			emitRune(rune(c))
			i++
		}
	}
	if depth != 0 {
		return "", fmt.Errorf("index/rtf: unbalanced braces (depth %d at EOF)", depth)
	}
	return sb.String(), nil
}

// parseControlWord returns the alphabetic name of an RTF control
// word starting at offset i (which must point at the first letter
// after the backslash). It returns the name and the offset of the
// first byte after the word's optional delimiter (space).
func parseControlWord(body []byte, i int) (string, int) {
	start := i
	for i < len(body) && isLetter(body[i]) {
		i++
	}
	name := string(body[start:i])
	// A single trailing space delimiter is consumed; otherwise
	// the byte that terminated the word is part of the next
	// token.
	if i < len(body) && body[i] == ' ' {
		i++
	}
	return name, i
}

// parseControlWordWithValue parses an RTF control word starting at
// offset i (the first letter, NOT the backslash). Returns the name,
// the optional signed numeric value, a flag indicating whether a
// value was present, and the offset after the word's delimiter.
//
// Per the RTF spec a control word may carry a single signed integer
// argument (e.g. \u8217, \uc1, \fcharset0). The argument is
// terminated by the first non-digit, non-minus character.
func parseControlWordWithValue(body []byte, i int) (name string, value int, hasValue bool, end int) {
	start := i
	for i < len(body) && isLetter(body[i]) {
		i++
	}
	name = string(body[start:i])
	if i < len(body) && (body[i] == '-' || isDigit(body[i])) {
		hasValue = true
		valStart := i
		if body[i] == '-' {
			i++
		}
		for i < len(body) && isDigit(body[i]) {
			i++
		}
		if v, err := strconv.Atoi(string(body[valStart:i])); err == nil {
			value = v
		}
	}
	if i < len(body) && body[i] == ' ' {
		i++
	}
	end = i
	return name, value, hasValue, end
}

// tryReadUnicodeEscape attempts to read another \uN escape
// immediately after a high surrogate. Returns the decoded rune, the
// new offset, and a success flag.
func tryReadUnicodeEscape(body []byte, i int, ucSkip int) (rune, int, bool) {
	// Consume any ANSI fallback bytes first.
	for ucSkip > 0 && i < len(body) {
		if body[i] == '\\' || body[i] == '{' || body[i] == '}' {
			break
		}
		i++
		ucSkip--
	}
	if i+1 >= len(body) || body[i] != '\\' || body[i+1] != 'u' {
		return 0, i, false
	}
	name, value, hasValue, end := parseControlWordWithValue(body, i+1)
	if name != "u" || !hasValue {
		return 0, i, false
	}
	code := value
	if code < 0 {
		code += 0x10000
	}
	return rune(code), end, true
}

func isLetter(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isDigit(c byte) bool {
	return c >= '0' && c <= '9'
}

package imapx

import (
	"encoding/base64"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// EncodeMailbox and DecodeMailbox implement RFC 3501 §5.1.3 Modified UTF-7
// for mailbox names at the protocol boundary: printable ASCII except '&'
// passes through, '&' becomes "&-", and every other rune run is shifted to
// modified BASE64 (the RFC 2152 alphabet with '/' replaced by ',') over
// UTF-16BE, wrapped in '&' and '-'. Raw UTF-8 must never reach the wire for
// mailbox arguments: go-imap serializes raw names to exactly this form, so
// hand-built commands and display paths must agree with it.
const mUTF7Alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+,"

var mUTF7Encoding = base64.NewEncoding(mUTF7Alphabet).WithPadding(base64.NoPadding)

func EncodeMailbox(value string) string {
	var out strings.Builder
	runes := make([]rune, 0, 8)
	flush := func() {
		if len(runes) == 0 {
			return
		}
		out.WriteString("&")
		out.WriteString(encodeRun(runes))
		out.WriteString("-")
		runes = runes[:0]
	}
	for _, r := range value {
		switch {
		case r >= 0x20 && r <= 0x7e && r != '&':
			flush()
			out.WriteRune(r)
		case r == '&':
			flush()
			out.WriteString("&-")
		default:
			runes = append(runes, r)
		}
	}
	flush()
	return out.String()
}

// DecodeMailbox is the display-side inverse of EncodeMailbox. Decoding is
// intentionally lenient — it accepts some non-canonical inputs, such as runs
// that decode to printable ASCII (canonical form requires those to be direct)
// or unpaired surrogates (which surface as U+FFFD) — because it feeds folder
// listing and display, not wire validation; protocol arguments must always be
// built with EncodeMailbox.
func DecodeMailbox(value string) (string, error) {
	var out strings.Builder
	for i := 0; i < len(value); {
		r, size := utf8.DecodeRuneInString(value[i:])
		if r == utf8.RuneError && size == 1 {
			return "", &mUTF7Error{at: i, reason: "invalid UTF-8"}
		}
		if r != '&' {
			out.WriteRune(r)
			i += size
			continue
		}
		end := strings.IndexByte(value[i:], '-')
		if end < 0 {
			return "", &mUTF7Error{at: i, reason: "unterminated shift sequence"}
		}
		body := value[i+1 : i+end]
		i += end + 1
		if body == "" {
			out.WriteByte('&')
			continue
		}
		runes, err := decodeRun(body)
		if err != nil {
			// decodeRun's own error already carries the "modified UTF-7: "
			// prefix (it is an *mUTF7Error); re-wrapping its Error() verbatim
			// would double the prefix. Keep only the inner reason and let the
			// outer error add position and prefix exactly once.
			reason := err.Error()
			if inner, ok := err.(*mUTF7Error); ok {
				reason = inner.reason
			}
			return "", &mUTF7Error{at: i, reason: reason}
		}
		for _, r := range runes {
			out.WriteRune(r)
		}
	}
	return out.String(), nil
}

func encodeRun(runes []rune) string {
	units := utf16.Encode(runes)
	be := make([]byte, 0, len(units)*2)
	for _, unit := range units {
		be = append(be, byte(unit>>8), byte(unit))
	}
	return strings.Map(func(r rune) rune {
		if r == '/' {
			return ','
		}
		return r
	}, mUTF7Encoding.EncodeToString(be))
}

func decodeRun(body string) ([]rune, error) {
	// Decode against the standard BASE64 alphabet after mapping mUTF-7's ','
	// back to '/' — mUTF7Alphabet itself has no '/', so it cannot decode.
	normalized := strings.Map(func(r rune) rune {
		if r == ',' {
			return '/'
		}
		return r
	}, body)
	be, err := base64.StdEncoding.WithPadding(base64.NoPadding).DecodeString(normalized)
	if err != nil || len(be) == 0 || len(be)%2 != 0 {
		return nil, errInvalidRun
	}
	units := make([]uint16, 0, len(be)/2)
	for i := 0; i+1 < len(be); i += 2 {
		units = append(units, uint16(be[i])<<8|uint16(be[i+1]))
	}
	return utf16.Decode(units), nil
}

type mUTF7Error struct {
	at     int
	reason string
}

var errInvalidRun = &mUTF7Error{reason: "invalid modified BASE64 run"}

func (e *mUTF7Error) Error() string { return "modified UTF-7: " + e.reason }

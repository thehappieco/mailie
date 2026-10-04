package mime

import (
	"net/http"
	"strings"
	"unicode/utf8"
)

// SniffBytes is how much of a file UploadContentType looks at.
const SniffBytes = 512

// UploadContentType is the Content-Type a file someone attached to a message
// they are sending goes out with.
//
// What the uploader's browser declared is an input like any other, and so is
// the file: neither is trusted alone. The type sniffed from the first bytes
// (the WHATWG algorithm, net/http's) decides when it can tell; the declared
// one only narrows a sniff that could not — application/octet-stream, text
// that is CSV or a calendar, a zip that is an office document, text that
// says it is something that runs. Then the same allowlist a download goes
// through: a type nobody listed, or one that runs when it is opened (HTML,
// SVG, XML, scripts), goes out as application/octet-stream. A recipient's
// mail client picks what opens an attachment by this header, and a file that
// says it is a picture should be one.
//
// Text goes out undecoded, so its charset says what the bytes are, never
// what they are hoped to be: charset=utf-8 when utf8Text says the whole file
// is valid UTF-8 (UTF8Validator, as it is spooled), UTF-16's when the file
// starts with its byte order mark, and none otherwise — a Latin-1 CSV
// labelled UTF-8 is mojibake in every client that previews it, where one
// with no charset is left to the client's guess.
func UploadContentType(declared string, head []byte, utf8Text bool) string {
	if len(head) > SniffBytes {
		head = head[:SniffBytes]
	}
	sniffedFull := http.DetectContentType(head)
	sniffed, params, _ := strings.Cut(sniffedFull, ";")
	sniffed = normalizeToken(sniffed)
	want, _, _ := strings.Cut(declared, ";")
	want = normalizeToken(want)

	chosen := sniffed
	switch {
	case !validMediaType(want) || want == sniffed:
	case sniffed == OctetStream:
		chosen = want
	case sniffed == "text/plain" && strings.HasPrefix(want, "text/"):
		chosen = want
	case sniffed == "text/plain" && !passive(want):
		// Text that says it is SVG, a script or a page is that, as far as
		// whoever opens it is concerned.
		chosen = OctetStream
	case sniffed == "application/zip" && (strings.HasPrefix(want, "application/vnd.openxmlformats-officedocument.") ||
		strings.HasPrefix(want, "application/vnd.oasis.opendocument.")):
		chosen = want
	}
	if !validMediaType(chosen) || !passive(chosen) {
		return OctetStream
	}
	if !strings.HasPrefix(chosen, "text/") {
		return chosen
	}
	switch charset := strings.ToLower(strings.TrimSpace(params)); {
	case charset == "charset=utf-16be" || charset == "charset=utf-16le":
		return chosen + "; " + charset
	case utf8Text:
		return chosen + "; charset=utf-8"
	}
	return chosen
}

// UTF8Validator is an io.Writer that reports whether everything written
// through it, taken together, is valid UTF-8. A character split between two
// writes counts as the one character it is; one cut off by the end does not.
// It never fails a write.
type UTF8Validator struct {
	// pending is the start of a character the last write ended in.
	pending []byte
	invalid bool
}

func (v *UTF8Validator) Write(p []byte) (int, error) {
	n := len(p)
	if v.invalid {
		return n, nil
	}
	// First the character the last write left unfinished, a byte at a time
	// until it is whole, or plainly not a character.
	for len(v.pending) > 0 && len(p) > 0 {
		v.pending = append(v.pending, p[0])
		p = p[1:]
		if !utf8.FullRune(v.pending) {
			continue
		}
		if r, size := utf8.DecodeRune(v.pending); r == utf8.RuneError && size <= 1 {
			v.invalid = true
			return n, nil
		}
		v.pending = v.pending[:0]
	}
	cut := len(p) - unfinished(p)
	if !utf8.Valid(p[:cut]) {
		v.invalid = true
		return n, nil
	}
	v.pending = append(v.pending, p[cut:]...)
	return n, nil
}

// Valid reports whether what was written is valid UTF-8, nothing written
// included.
func (v *UTF8Validator) Valid() bool { return !v.invalid && len(v.pending) == 0 }

// unfinished is how many bytes at the end of p begin a character that p
// does not finish.
func unfinished(p []byte) int {
	for i := 1; i < utf8.UTFMax && i <= len(p); i++ {
		if utf8.RuneStart(p[len(p)-i]) {
			if utf8.FullRune(p[len(p)-i:]) {
				return 0
			}
			return i
		}
	}
	return 0
}

// UploadFilename is the name an uploaded attachment goes out with: the
// uploader's, made safe (SafeFilename), or "attachment" with an extension
// for its type when nothing of it is left.
func UploadFilename(name, contentType string) string {
	if safe := SafeFilename(name); safe != "" {
		return safe
	}
	mt, _, _ := strings.Cut(contentType, ";")
	return "attachment" + extensions[normalizeToken(mt)]
}

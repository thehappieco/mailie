// Package mime turns the pieces of a message fetched from a mail server into
// something safe to hand to a person: text decoded to UTF-8 within a byte cap,
// attachment filenames that cannot name a path, and response content types a
// browser will not execute.
//
// It is lenient by design. A message that reached a mailbox is shown as well
// as it can be; a sloppy encoder, an unknown charset or a mislabelled part
// degrades the result and says so (Text.CharsetFallback), it never fails the
// request. Content never causes an error: only the reader's own failures and
// a caller's misuse (a non-positive cap, a section that does not exist) do.
//
// Nothing here stores, caches or logs content: every function works on what
// it is given and returns it.
package mime

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/emersion/go-message/charset"

	"github.com/thehappieco/mailie/internal/provider"
)

// ErrBadLimit is returned when a byte cap is not positive.
var ErrBadLimit = errors.New("mime: the byte limit must be positive")

// Text is a text part decoded to UTF-8.
type Text struct {
	// Text is valid UTF-8 with LF line endings. HTML stays HTML: nothing
	// here sanitises markup, and whoever renders it must treat it as unsafe.
	Text string
	// Truncated reports that the part had more than the cap allowed, and
	// Text is its beginning, cut on a character boundary.
	Truncated bool
	// CharsetFallback reports that the part's declared charset could not be
	// used — unknown, unsupported, or contradicted by the bytes — and Text
	// was decoded by a guess: UTF-8 when the bytes are valid UTF-8, otherwise
	// Windows-1252 (what browsers read "latin-1" as), with invalid sequences
	// in otherwise-UTF-8 text replaced by U+FFFD.
	CharsetFallback bool
}

// DecodeText decodes one fetched text part: r is the section exactly as the
// server returned it for BODY.PEEK[<path>] — still transfer-encoded, in its
// declared charset — and info is what BODYSTRUCTURE (or the index) says about
// it. At most maxBytes of UTF-8 are returned, and r is read only about as far
// as that needs (the decoders buffer a few kilobytes ahead), so a huge part
// costs no more than its first maxBytes.
func DecodeText(info provider.PartInfo, r io.Reader, maxBytes int64) (Text, error) {
	if maxBytes <= 0 {
		return Text{}, ErrBadLimit
	}
	decoded, _ := NewTransferDecoder(info.Encoding, r)
	label := normalizeToken(Param(info.Params, "charset"))

	var (
		out      []byte
		cut      bool
		fallback bool
		err      error
	)
	switch {
	case isUTF8Family(label) || isASCIIFamily(label) || label == "":
		out, cut, fallback, err = decodeSniffed(decoded, maxBytes, label, info.MIMEType)
	case unsupportedCharset(label):
		out, cut, err = readCapped(decoded, maxBytes)
		out, fallback = guessUTF8(out, cut), true
	default:
		out, cut, fallback, err = decodeDeclared(decoded, maxBytes, label)
	}
	if err != nil {
		return Text{}, err
	}

	if cut {
		// The cap may have split a character; that is the cap's doing, not
		// the sender's, so it is dropped rather than shown as U+FFFD.
		out = dropIncompleteTail(out)
	}
	out = bytes.ToValidUTF8(out, replacement)
	out, over := truncateUTF8(out, maxBytes)
	out = normalizeText(out)
	return Text{Text: string(out), Truncated: cut || over, CharsetFallback: fallback}, nil
}

// decodeDeclared converts from a charset go-message knows, streaming, so only
// the capped output is ever held.
func decodeDeclared(r io.Reader, maxBytes int64, label string) (out []byte, cut, fallback bool, err error) {
	src := &errRecorder{r: r}
	conv, convErr := charset.Reader(label, src)
	if convErr != nil {
		// Unknown to go-message: read the bytes as they are and guess.
		raw, rawCut, err := readCapped(r, maxBytes)
		if err != nil {
			return nil, false, false, err
		}
		return guessUTF8(raw, rawCut), rawCut, true, nil
	}
	out, cut, err = readCapped(conv, maxBytes)
	if err != nil {
		if src.err != nil {
			return nil, false, false, err
		}
		// The converter itself gave up. x/text decoders substitute U+FFFD
		// rather than fail, so this is a converter this code has not met:
		// keep what it produced, and say both that the charset was not
		// honoured and that this is not the whole part.
		return out, true, true, nil
	}
	return out, cut, false, nil
}

// decodeSniffed handles parts labelled UTF-8, US-ASCII or nothing at all:
// the bytes are UTF-8 unless they prove otherwise.
//
// US-ASCII and "no charset" holding valid 8-bit UTF-8 is not a fallback —
// that is what every mail client does with them, and the text is right. An
// HTML part with no MIME charset gets its own <meta charset> honoured before
// anything is guessed.
func decodeSniffed(r io.Reader, maxBytes int64, label, mimeType string) (out []byte, cut, fallback bool, err error) {
	raw, cut, err := readCapped(r, maxBytes)
	if err != nil {
		return nil, false, false, err
	}
	if validUTF8Prefix(raw, cut) {
		return raw, cut, false, nil
	}
	if label == "" && normalizeToken(mimeType) == "text/html" {
		if meta := metaCharset(raw); meta != "" && !isUTF8Family(meta) && !isASCIIFamily(meta) && !unsupportedCharset(meta) {
			if conv, err := charset.Reader(meta, bytes.NewReader(raw)); err == nil {
				if converted, err := io.ReadAll(conv); err == nil {
					return converted, cut, false, nil
				}
			}
		}
	}
	return guessUTF8(raw, cut), cut, true, nil
}

// guessUTF8 decodes bytes whose charset is unknown or wrong.
//
// Valid UTF-8 is taken as UTF-8. Text with valid multi-byte sequences and a
// few broken ones is damaged UTF-8: the broken bytes become U+FFFD. Text with
// no valid multi-byte sequence at all is an 8-bit single-byte charset, and
// Windows-1252 — a superset of ISO-8859-1's printable range, and what browsers
// read "latin-1" as — is the best single guess for the mail that has them.
func guessUTF8(raw []byte, cut bool) []byte {
	if validUTF8Prefix(raw, cut) {
		return raw
	}
	valid, invalid := countRunes(raw)
	if valid > 0 && valid >= invalid {
		return bytes.ToValidUTF8(raw, replacement)
	}
	conv, err := charset.Reader("windows-1252", bytes.NewReader(raw))
	if err == nil {
		if out, err := io.ReadAll(conv); err == nil {
			return out
		}
	}
	return bytes.ToValidUTF8(raw, replacement)
}

var replacement = []byte("\uFFFD")

// countRunes counts valid multi-byte UTF-8 sequences and invalid bytes.
func countRunes(b []byte) (validMulti, invalid int) {
	for len(b) > 0 {
		if b[0] < utf8.RuneSelf {
			b = b[1:]
			continue
		}
		r, size := utf8.DecodeRune(b)
		if r == utf8.RuneError && size == 1 {
			invalid++
		} else {
			validMulti++
		}
		b = b[size:]
	}
	return validMulti, invalid
}

// validUTF8Prefix reports whether b is valid UTF-8, forgiving an incomplete
// sequence at the end when b was cut there by the cap.
func validUTF8Prefix(b []byte, cut bool) bool {
	if utf8.Valid(b) {
		return true
	}
	if !cut {
		return false
	}
	for back := 1; back <= utf8.UTFMax-1 && back <= len(b); back++ {
		if utf8.RuneStart(b[len(b)-back]) {
			return !utf8.FullRune(b[len(b)-back:]) && utf8.Valid(b[:len(b)-back])
		}
	}
	return false
}

// readCapped reads at most maxBytes, reporting whether there was more. On an
// error it still returns what it read before it.
func readCapped(r io.Reader, maxBytes int64) ([]byte, bool, error) {
	b, err := io.ReadAll(io.LimitReader(r, maxBytes+1))
	if err != nil {
		return b, false, fmt.Errorf("mime: read part: %w", err)
	}
	if int64(len(b)) > maxBytes {
		return b[:maxBytes], true, nil
	}
	return b, false, nil
}

// dropIncompleteTail removes a character the cap cut in half.
func dropIncompleteTail(b []byte) []byte {
	for back := 1; back <= utf8.UTFMax-1 && back <= len(b); back++ {
		if utf8.RuneStart(b[len(b)-back]) {
			if !utf8.FullRune(b[len(b)-back:]) {
				return b[:len(b)-back]
			}
			return b
		}
	}
	return b
}

// truncateUTF8 cuts valid UTF-8 to at most maxBytes without splitting a
// character.
func truncateUTF8(b []byte, maxBytes int64) ([]byte, bool) {
	if int64(len(b)) <= maxBytes {
		return b, false
	}
	return dropIncompleteTail(b[:maxBytes]), true
}

// normalizeText makes the result predictable for every consumer: no
// byte-order mark, and LF line endings (CRLF and lone CR both become LF).
// It only ever shrinks its input, so the cap still holds afterwards.
func normalizeText(b []byte) []byte {
	b = bytes.TrimPrefix(b, []byte("\uFEFF"))
	if bytes.IndexByte(b, '\r') < 0 {
		return b
	}
	b = bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n"))
	return bytes.ReplaceAll(b, []byte("\r"), []byte("\n"))
}

// errRecorder remembers the underlying reader's error, so a failure of the
// converter can be told apart from a failure of the spool file under it.
type errRecorder struct {
	r   io.Reader
	err error
}

func (e *errRecorder) Read(p []byte) (int, error) {
	n, err := e.r.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		e.err = err
	}
	return n, err
}

func isUTF8Family(label string) bool {
	switch label {
	case "utf-8", "utf8", "unicode-1-1-utf-8", "x-unicode20utf8":
		return true
	}
	return false
}

func isASCIIFamily(label string) bool {
	switch label {
	case "us-ascii", "ascii", "ansi_x3.4-1968", "iso-646-us", "iso646-us", "us", "csascii":
		return true
	}
	return false
}

// unsupportedCharset lists labels go-message resolves, through the WHATWG
// index, to the "replacement" encoding: it turns the whole part into a
// single U+FFFD. Guessing does better.
func unsupportedCharset(label string) bool {
	switch label {
	case "csiso2022kr", "hz-gb-2312", "iso-2022-cn", "iso-2022-cn-ext", "iso-2022-kr", "replacement",
		"unknown-8bit", "x-unknown", "unknown":
		return true
	}
	return false
}

// metaCharset finds the charset an HTML document declares for itself, looking
// only where a browser would: the first kilobytes.
func metaCharset(html []byte) string {
	const sniffBytes = 4096
	if len(html) > sniffBytes {
		html = html[:sniffBytes]
	}
	m := metaCharsetPattern.FindSubmatch(html)
	if m == nil {
		return ""
	}
	return normalizeToken(string(m[1]))
}

var metaCharsetPattern = regexp.MustCompile(`(?i)<meta[^>]*?charset\s*=\s*["']?\s*([a-z0-9_.:-]+)`)

// Param reads a MIME parameter case-insensitively. BODYSTRUCTURE parameter
// names arrive in whatever case the sender wrote them.
func Param(params map[string]string, key string) string {
	if v, ok := params[key]; ok {
		return v
	}
	for k, v := range params {
		if strings.EqualFold(k, key) {
			return v
		}
	}
	return ""
}

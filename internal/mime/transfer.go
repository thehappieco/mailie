package mime

import (
	"bufio"
	"io"
	"strings"
)

// NewTransferDecoder undoes a part's Content-Transfer-Encoding.
//
// It never fails on the data: a message that reached a mailbox is shown as
// well as it can be, never refused because its encoder was sloppy. The only
// errors the returned reader reports are the underlying reader's own. known is
// false when the encoding is not one MIME defines (x-uuencode, a typo); the
// bytes then pass through untouched, which is the honest thing to hand back.
func NewTransferDecoder(encoding string, r io.Reader) (dec io.Reader, known bool) {
	switch normalizeToken(encoding) {
	case "base64":
		return &base64Reader{src: bufio.NewReader(r)}, true
	case "quoted-printable":
		return &qpReader{src: bufio.NewReader(r)}, true
	case "", "7bit", "8bit", "binary":
		return r, true
	default:
		return r, false
	}
}

// normalizeToken lowercases a header token and strips what sloppy writers
// leave around it: whitespace, quotes, a trailing semicolon.
func normalizeToken(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimRight(s, ";")
	s = strings.Trim(s, "\"' \t")
	return strings.ToLower(s)
}

// base64Reader decodes base64 without ever refusing input.
//
// encoding/base64 rejects a stray character, missing padding and a padded
// block followed by more data. All three occur in real mail: punctuation or
// 8-bit junk left in the body by a broken gateway, encoders that drop the
// final "=", and bodies built by concatenating separately encoded chunks.
// Here anything outside the alphabet is skipped, as RFC 2045 asks, "=" closes
// the current quantum, and a lone final sextet (6 bits, not a byte) is
// dropped.
type base64Reader struct {
	src     *bufio.Reader
	quantum [4]byte // sextets of the quantum being assembled
	have    int     // sextets in quantum
	out     [3]byte // decoded bytes not yet handed out
	outLen  int
	outPos  int
	err     error
}

var base64Values = func() [256]byte {
	var t [256]byte
	for i := range t {
		t[i] = 0xff
	}
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	for i := range len(alphabet) {
		t[alphabet[i]] = byte(i)
	}
	return t
}()

func (b *base64Reader) Read(p []byte) (int, error) {
	n := 0
	for n < len(p) {
		if b.outPos < b.outLen {
			c := copy(p[n:], b.out[b.outPos:b.outLen])
			b.outPos += c
			n += c
			continue
		}
		if b.err != nil {
			if n > 0 {
				return n, nil
			}
			return 0, b.err
		}
		c, err := b.src.ReadByte()
		if err != nil {
			b.flush()
			b.err = err
			continue
		}
		if c == '=' {
			b.flush()
			continue
		}
		v := base64Values[c]
		if v == 0xff {
			continue
		}
		b.quantum[b.have] = v
		b.have++
		if b.have == 4 {
			b.flush()
		}
	}
	return n, nil
}

// flush turns the sextets collected so far into bytes: four make three, three
// make two, two make one, and one is not enough bits for a byte.
func (b *base64Reader) flush() {
	q := b.quantum
	b.outPos, b.outLen = 0, 0
	switch b.have {
	case 4:
		b.out = [3]byte{q[0]<<2 | q[1]>>4, q[1]<<4 | q[2]>>2, q[2]<<6 | q[3]}
		b.outLen = 3
	case 3:
		b.out = [3]byte{q[0]<<2 | q[1]>>4, q[1]<<4 | q[2]>>2, 0}
		b.outLen = 2
	case 2:
		b.out = [3]byte{q[0]<<2 | q[1]>>4, 0, 0}
		b.outLen = 1
	}
	b.have = 0
}

// qpReader decodes quoted-printable without ever refusing input.
//
// mime/quotedprintable fails on a line longer than its 4 KiB buffer — which
// HTML generators produce all the time — on a bare control character, and on
// junk after a soft line break. Here "=XX" (either case) is a byte, "=" before
// a line break (after optional whitespace) joins lines, whitespace before a
// hard line break is dropped as RFC 2045 asks, and every other byte, including
// an "=" that starts none of those, is taken literally.
type qpReader struct {
	src *bufio.Reader
	out []byte // decoded, not yet handed out
	pos int
	// ws is a run of spaces and tabs whose fate depends on what follows it:
	// dropped before a line break or the end, kept before anything else.
	ws  []byte
	err error
}

// maxHeldWhitespace bounds the whitespace held back while deciding whether it
// trails a line; a longer run is kept. Being wrong about a four kilobyte run
// of spaces is not worth unbounded memory.
const maxHeldWhitespace = 1024

func (q *qpReader) Read(p []byte) (int, error) {
	for q.pos == len(q.out) {
		if q.err != nil {
			return 0, q.err
		}
		q.out, q.pos = q.out[:0], 0
		q.step()
	}
	n := copy(p, q.out[q.pos:])
	q.pos += n
	return n, nil
}

// step consumes input until something is decided: output appended, or the
// end of the input reached.
func (q *qpReader) step() {
	c, err := q.src.ReadByte()
	if err != nil {
		// Whitespace at the very end of the part is trailing whitespace too.
		q.ws = q.ws[:0]
		q.err = err
		return
	}
	switch c {
	case ' ', '\t':
		q.ws = append(q.ws, c)
		if len(q.ws) >= maxHeldWhitespace {
			q.keepWhitespace()
		}
	case '\n':
		q.ws = q.ws[:0]
		q.out = append(q.out, '\n')
	case '\r':
		if next, err := q.src.Peek(1); err == nil && next[0] == '\n' {
			q.discard(1)
			q.ws = q.ws[:0]
			q.out = append(q.out, '\r', '\n')
			return
		}
		q.keepWhitespace()
		q.out = append(q.out, '\r')
	case '=':
		q.keepWhitespace()
		q.equals()
	default:
		q.keepWhitespace()
		q.out = append(q.out, c)
	}
}

func (q *qpReader) keepWhitespace() {
	q.out = append(q.out, q.ws...)
	q.ws = q.ws[:0]
}

// equals decides what an "=" starts: an escaped byte, a soft line break, or
// nothing, in which case it is a literal "=".
func (q *qpReader) equals() {
	// Peek fails exactly when fewer bytes than asked for remain.
	if next, err := q.src.Peek(2); err == nil && isHex(next[0]) && isHex(next[1]) {
		q.discard(2)
		q.out = append(q.out, unhex(next[0])<<4|unhex(next[1]))
		return
	}
	// A soft line break: optional whitespace, then CRLF, LF or the end.
	for i := 0; i <= maxHeldWhitespace; i++ {
		b, err := q.src.Peek(i + 1)
		if len(b) <= i {
			if err != nil {
				q.discard(i) // "=" and whitespace up to the end of the part
				return
			}
			break
		}
		switch b[i] {
		case ' ', '\t':
			continue
		case '\n':
			q.discard(i + 1)
			return
		case '\r':
			if more, err := q.src.Peek(i + 2); err == nil && more[i+1] == '\n' {
				q.discard(i + 2)
				return
			}
		}
		break
	}
	q.out = append(q.out, '=')
}

// discard skips bytes a Peek has already shown to be there.
func (q *qpReader) discard(n int) {
	//nolint:errcheck // the bytes were just peeked, so Discard cannot fall short
	_, _ = q.src.Discard(n)
}

func isHex(c byte) bool {
	return ('0' <= c && c <= '9') || ('a' <= c && c <= 'f') || ('A' <= c && c <= 'F')
}

func unhex(c byte) byte {
	switch {
	case '0' <= c && c <= '9':
		return c - '0'
	case 'a' <= c && c <= 'f':
		return c - 'a' + 10
	default:
		return c - 'A' + 10
	}
}

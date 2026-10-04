package backup

import (
	"bufio"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
)

// The .mlbk format, version 1. Integers are big-endian.
//
//	magic         8 bytes   "MAILIEBK"
//	version       1 byte    1
//	chunk size    4 bytes   plaintext bytes per chunk: 65536, 4 KiB to 1 MiB accepted
//	nonce prefix  7 bytes   random, per backup
//	rest length   2 bytes   length of the three fields below, at most 8 KiB
//	key ARN       2-byte length, then the ARN of the KMS key that wrapped the data key
//	wrapped key   2-byte length, then the KMS CiphertextBlob of the data key
//	context       2-byte pair count, then for each pair a 2-byte length and the
//	              key, a 2-byte length and the value; keys strictly ascending
//
// The chunks follow the header and run to the end of the file. Every chunk
// but the last holds exactly chunk-size bytes of plaintext; the last holds 0
// to chunk-size. Each is sealed with AES-256-GCM under the backup's data key:
//
//	nonce = nonce prefix (7) || chunk index (4) || 0x01 for the last chunk, else 0x00
//	AAD   = the header, every byte of it as it appears in the file
//
// This is the STREAM construction (Hoang, Reyhanitabar, Rogaway and Vizár):
// the index in the nonce makes a reordered, dropped or repeated chunk fail
// to open, and the final flag makes a file cut at a chunk boundary fail too,
// because the chunk before the cut was sealed as "not last". The data key is
// fresh for every backup, so a chunk from another backup opens under nothing
// here; the header as AAD binds the chunks to the key ARN, the wrapped key,
// the context, the chunk size and the nonce prefix, so none of those can be
// changed without every chunk failing. The context is also bound to the
// wrapped key by KMS itself: unwrapping it with any other context fails.
const (
	magic          = "MAILIEBK"
	formatVersion  = 1
	fixedHeaderLen = len(magic) + 1 + 4 + noncePrefixLen + 2

	// DefaultChunkSize is the plaintext per chunk. Small enough that a
	// damaged byte is reported with its chunk, large enough that the 16-byte
	// tag per chunk costs nothing.
	DefaultChunkSize = 64 << 10
	minChunkSize     = 4 << 10
	maxChunkSize     = 1 << 20

	noncePrefixLen = 7
	maxHeaderRest  = 8 << 10
	tagLen         = 16
	// dataKeyLen is AES-256.
	dataKeyLen = 32
)

var (
	// ErrNotABackup is a file that does not start like a backup of this
	// format: another file, or a version this binary does not read.
	ErrNotABackup = errors.New("backup: not a Mailie database backup")
	// ErrCorrupt is a backup that does not authenticate: damaged, cut short,
	// with chunks moved, repeated or taken from another backup, or with its
	// header changed. Nothing it decrypted to may be used.
	ErrCorrupt = errors.New("backup: the backup is damaged or was tampered with")
)

// header is everything before the first chunk.
type header struct {
	chunkSize   uint32
	noncePrefix [noncePrefixLen]byte
	keyARN      string
	wrappedKey  []byte
	context     map[string]string
	// raw is the header exactly as it appears in the file: the associated
	// data of every chunk.
	raw []byte
}

// marshal encodes h and records the encoding in h.raw.
func (h *header) marshal() ([]byte, error) {
	if h.chunkSize < minChunkSize || h.chunkSize > maxChunkSize {
		return nil, fmt.Errorf("backup: chunk size %d is outside %d to %d", h.chunkSize, minChunkSize, maxChunkSize)
	}
	var rest []byte
	var err error
	if rest, err = appendField(rest, []byte(h.keyARN)); err != nil {
		return nil, fmt.Errorf("backup: key ARN: %w", err)
	}
	if rest, err = appendField(rest, h.wrappedKey); err != nil {
		return nil, fmt.Errorf("backup: wrapped key: %w", err)
	}
	keys := make([]string, 0, len(h.context))
	for k := range h.context {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	rest = binary.BigEndian.AppendUint16(rest, uint16(len(keys))) //nolint:gosec // G115: bounded by maxHeaderRest below
	for _, k := range keys {
		if k == "" {
			return nil, errors.New("backup: an encryption context key is empty")
		}
		if rest, err = appendField(rest, []byte(k)); err != nil {
			return nil, fmt.Errorf("backup: context key: %w", err)
		}
		if rest, err = appendField(rest, []byte(h.context[k])); err != nil {
			return nil, fmt.Errorf("backup: context value: %w", err)
		}
	}
	if len(rest) > maxHeaderRest {
		return nil, fmt.Errorf("backup: header of %d bytes is over %d", len(rest), maxHeaderRest)
	}

	out := make([]byte, 0, fixedHeaderLen+len(rest))
	out = append(out, magic...)
	out = append(out, formatVersion)
	out = binary.BigEndian.AppendUint32(out, h.chunkSize)
	out = append(out, h.noncePrefix[:]...)
	out = binary.BigEndian.AppendUint16(out, uint16(len(rest))) //nolint:gosec // G115: checked against maxHeaderRest
	out = append(out, rest...)
	h.raw = out
	return out, nil
}

func appendField(b, v []byte) ([]byte, error) {
	if len(v) > math.MaxUint16 {
		return nil, fmt.Errorf("%d bytes is too long", len(v))
	}
	b = binary.BigEndian.AppendUint16(b, uint16(len(v))) //nolint:gosec // G115: checked just above
	return append(b, v...), nil
}

// readHeader reads and parses the header, and nothing past it. It checks the
// shape only: whether the header is genuine is decided by KMS, which unwraps
// the data key only with this context, and by every chunk, which opens only
// with these bytes as associated data.
func readHeader(r io.Reader) (*header, error) {
	fixed := make([]byte, fixedHeaderLen)
	if _, err := io.ReadFull(r, fixed); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, fmt.Errorf("%w: shorter than a header", ErrNotABackup)
		}
		return nil, fmt.Errorf("backup: read header: %w", err)
	}
	if string(fixed[:len(magic)]) != magic {
		return nil, ErrNotABackup
	}
	p := fixed[len(magic):]
	if v := p[0]; v != formatVersion {
		return nil, fmt.Errorf("%w: format version %d, this binary reads %d", ErrNotABackup, v, formatVersion)
	}
	h := &header{chunkSize: binary.BigEndian.Uint32(p[1:5])}
	copy(h.noncePrefix[:], p[5:5+noncePrefixLen])
	restLen := int(binary.BigEndian.Uint16(p[5+noncePrefixLen:]))
	if h.chunkSize < minChunkSize || h.chunkSize > maxChunkSize {
		return nil, fmt.Errorf("%w: chunk size %d", ErrCorrupt, h.chunkSize)
	}
	if restLen > maxHeaderRest {
		return nil, fmt.Errorf("%w: header of %d bytes", ErrCorrupt, restLen)
	}
	rest := make([]byte, restLen)
	if _, err := io.ReadFull(r, rest); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, fmt.Errorf("%w: the header is cut short", ErrCorrupt)
		}
		return nil, fmt.Errorf("backup: read header: %w", err)
	}

	d := decoder{b: rest}
	h.keyARN = string(d.field())
	h.wrappedKey = d.field()
	n := d.uint16()
	h.context = make(map[string]string, n)
	prev := ""
	for i := 0; i < n && d.err == nil; i++ {
		k, v := string(d.field()), string(d.field())
		if d.err == nil && (k == "" || (i > 0 && k <= prev)) {
			// One encoding per header: a context written in another order,
			// or with a key twice, is not one this program wrote.
			return nil, fmt.Errorf("%w: the encryption context is not in canonical order", ErrCorrupt)
		}
		h.context[k], prev = v, k
	}
	if d.err != nil {
		return nil, fmt.Errorf("%w: %w", ErrCorrupt, d.err)
	}
	if len(d.b) != 0 {
		return nil, fmt.Errorf("%w: %d stray bytes in the header", ErrCorrupt, len(d.b))
	}
	if h.keyARN == "" || len(h.wrappedKey) == 0 {
		return nil, fmt.Errorf("%w: the header names no key", ErrCorrupt)
	}
	h.raw = append(fixed, rest...)
	return h, nil
}

// decoder reads length-prefixed fields and remembers the first overrun.
type decoder struct {
	b   []byte
	err error
}

func (d *decoder) uint16() int {
	if d.err != nil {
		return 0
	}
	if len(d.b) < 2 {
		d.err = errors.New("the header is cut short")
		return 0
	}
	v := int(binary.BigEndian.Uint16(d.b))
	d.b = d.b[2:]
	return v
}

func (d *decoder) field() []byte {
	n := d.uint16()
	if d.err != nil {
		return nil
	}
	if len(d.b) < n {
		d.err = errors.New("a header field runs past the header")
		return nil
	}
	v := d.b[:n:n]
	d.b = d.b[n:]
	return v
}

// newAEAD builds the chunk cipher. The caller zeroes key once this returns:
// the cipher keeps its own expanded copy, which Go gives no way to wipe and
// which lives only as long as the returned value.
func newAEAD(key []byte) (cipher.AEAD, error) {
	if len(key) != dataKeyLen {
		return nil, fmt.Errorf("backup: a data key of %d bytes, want %d", len(key), dataKeyLen)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("backup: data key: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("backup: data key: %w", err)
	}
	return aead, nil
}

// chunkNonce is the STREAM nonce for chunk i.
func chunkNonce(prefix [noncePrefixLen]byte, i uint32, last bool) []byte {
	n := make([]byte, 0, noncePrefixLen+5)
	n = append(n, prefix[:]...)
	n = binary.BigEndian.AppendUint32(n, i)
	if last {
		return append(n, 1)
	}
	return append(n, 0)
}

// sealStream writes h and then src, chunk by chunk, to dst. It returns the
// plaintext and ciphertext sizes.
func sealStream(ctx context.Context, dst io.Writer, src io.Reader, aead cipher.AEAD, h *header) (plain, sealed int64, err error) {
	if h.raw == nil {
		return 0, 0, errors.New("backup: header not marshalled")
	}
	if _, err := dst.Write(h.raw); err != nil {
		return 0, 0, fmt.Errorf("backup: write header: %w", err)
	}
	sealed = int64(len(h.raw))
	size := int(h.chunkSize)
	cur, next := make([]byte, size), make([]byte, size)
	out := make([]byte, 0, size+tagLen)

	n, err := io.ReadFull(src, cur)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return 0, 0, fmt.Errorf("backup: read snapshot: %w", err)
	}
	for i := uint32(0); ; i++ {
		if err := ctx.Err(); err != nil {
			return 0, 0, err
		}
		// A chunk is the last when the source ended inside it or right
		// after it: the second read decides which.
		last := n < size
		m := 0
		if !last {
			m, err = io.ReadFull(src, next)
			switch {
			case m == 0 && errors.Is(err, io.EOF):
				last = true
			case err != nil && !errors.Is(err, io.ErrUnexpectedEOF):
				return 0, 0, fmt.Errorf("backup: read snapshot: %w", err)
			}
		}
		if !last && i == math.MaxUint32 {
			return 0, 0, errors.New("backup: the snapshot has more chunks than the format can number")
		}
		out = aead.Seal(out[:0], chunkNonce(h.noncePrefix, i, last), cur[:n], h.raw)
		if _, err := dst.Write(out); err != nil {
			return 0, 0, fmt.Errorf("backup: write chunk: %w", err)
		}
		plain += int64(n)
		sealed += int64(len(out))
		if last {
			return plain, sealed, nil
		}
		cur, next, n = next, cur, m
	}
}

// openStream reads the chunks that follow h from src and writes their plaintext to
// dst, each only once it has authenticated. It fails unless the stream ends
// exactly after a chunk sealed as the last.
func openStream(ctx context.Context, dst io.Writer, src io.Reader, aead cipher.AEAD, h *header) (int64, error) {
	size := int(h.chunkSize) + tagLen
	in := bufio.NewReaderSize(src, size+1)
	buf := make([]byte, size)
	var plain int64
	for i := uint32(0); ; i++ {
		if err := ctx.Err(); err != nil {
			return plain, err
		}
		n, err := io.ReadFull(in, buf)
		last := false
		switch {
		case errors.Is(err, io.EOF):
			return plain, fmt.Errorf("%w: it ends before its final chunk", ErrCorrupt)
		case errors.Is(err, io.ErrUnexpectedEOF):
			last = true
		case err != nil:
			return plain, fmt.Errorf("backup: read chunk %d: %w", i, err)
		default:
			// A full chunk is the last when nothing follows it.
			if _, perr := in.Peek(1); errors.Is(perr, io.EOF) {
				last = true
			} else if perr != nil {
				return plain, fmt.Errorf("backup: read chunk %d: %w", i, perr)
			}
		}
		if n < tagLen {
			return plain, fmt.Errorf("%w: chunk %d is cut short", ErrCorrupt, i)
		}
		pt, err := aead.Open(buf[:0], chunkNonce(h.noncePrefix, i, last), buf[:n], h.raw)
		if err != nil {
			return plain, fmt.Errorf("%w: chunk %d does not authenticate", ErrCorrupt, i)
		}
		if _, err := dst.Write(pt); err != nil {
			return plain, fmt.Errorf("backup: write plaintext: %w", err)
		}
		plain += int64(len(pt))
		if last {
			return plain, nil
		}
		if i == math.MaxUint32 {
			return plain, fmt.Errorf("%w: more chunks than the format can number", ErrCorrupt)
		}
	}
}

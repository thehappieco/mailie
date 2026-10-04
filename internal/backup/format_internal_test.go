package backup

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"testing"
)

func testHeader(t *testing.T) *header {
	t.Helper()
	h := &header{
		chunkSize:  minChunkSize,
		keyARN:     "arn:aws:kms:us-east-2:111122223333:key/00000000-0000-4000-8000-000000000001",
		wrappedKey: []byte("wrapped data key"),
		context:    EncryptionContext("prod", "db/20261001T000000Z-00000000.mlbk"),
	}
	if _, err := rand.Read(h.noncePrefix[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := h.marshal(); err != nil {
		t.Fatal(err)
	}
	return h
}

func testAEADKey(t *testing.T) []byte {
	t.Helper()
	key := make([]byte, dataKeyLen)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	return key
}

func TestTheStreamRoundTripsAtEveryChunkBoundary(t *testing.T) {
	h := testHeader(t)
	aead, err := newAEAD(testAEADKey(t))
	if err != nil {
		t.Fatal(err)
	}
	cs := int(h.chunkSize)
	for _, size := range []int{0, 1, cs - 1, cs, cs + 1, 3 * cs, 3*cs + 7} {
		plain := make([]byte, size)
		if _, err := rand.Read(plain); err != nil {
			t.Fatal(err)
		}
		var sealed bytes.Buffer
		pn, sn, err := sealStream(context.Background(), &sealed, bytes.NewReader(plain), aead, h)
		if err != nil {
			t.Fatalf("%d bytes: seal: %v", size, err)
		}
		chunks := max(1, (size+cs-1)/cs)
		if size > 0 && size%cs == 0 {
			// The last chunk is a full one, not an empty one after it.
			chunks = size / cs
		}
		if want := int64(len(h.raw) + size + chunks*tagLen); sn != want || int64(sealed.Len()) != want || pn != int64(size) {
			t.Fatalf("%d bytes: sealed %d (wrote %d), plain %d; want %d sealed in %d chunks", size, sn, sealed.Len(), pn, want, chunks)
		}

		got, err := readHeader(&sealed)
		if err != nil {
			t.Fatalf("%d bytes: header: %v", size, err)
		}
		if !bytes.Equal(got.raw, h.raw) {
			t.Fatalf("%d bytes: the header read back differs from the one written", size)
		}
		var out bytes.Buffer
		if _, err := openStream(context.Background(), &out, &sealed, aead, got); err != nil {
			t.Fatalf("%d bytes: open: %v", size, err)
		}
		if !bytes.Equal(out.Bytes(), plain) {
			t.Fatalf("%d bytes: the plaintext did not round-trip", size)
		}
	}
}

func TestOnlyTheLastChunkIsSealedAsFinal(t *testing.T) {
	h := testHeader(t)
	aead, err := newAEAD(testAEADKey(t))
	if err != nil {
		t.Fatal(err)
	}
	cs := int(h.chunkSize)
	plain := make([]byte, 2*cs+10)
	var sealed bytes.Buffer
	if _, _, err := sealStream(context.Background(), &sealed, bytes.NewReader(plain), aead, h); err != nil {
		t.Fatal(err)
	}
	body := sealed.Bytes()[len(h.raw):]
	for i, chunk := range [][]byte{body[:cs+tagLen], body[cs+tagLen : 2*(cs+tagLen)], body[2*(cs+tagLen):]} {
		last := i == 2
		if _, err := aead.Open(nil, chunkNonce(h.noncePrefix, uint32(i), last), chunk, h.raw); err != nil { //nolint:gosec // G115: tiny index
			t.Errorf("chunk %d does not open as index %d, final=%t", i, i, last)
		}
		if _, err := aead.Open(nil, chunkNonce(h.noncePrefix, uint32(i), !last), chunk, h.raw); err == nil { //nolint:gosec // G115: tiny index
			t.Errorf("chunk %d also opens with the final flag flipped", i)
		}
	}
}

func TestTheNonceIsThePrefixTheIndexAndTheFinalFlag(t *testing.T) {
	prefix := [noncePrefixLen]byte{1, 2, 3, 4, 5, 6, 7}
	n := chunkNonce(prefix, 0x01020304, true)
	want := []byte{1, 2, 3, 4, 5, 6, 7, 1, 2, 3, 4, 1}
	if !bytes.Equal(n, want) {
		t.Fatalf("nonce %x, want %x", n, want)
	}
	if n := chunkNonce(prefix, 0x01020304, false); n[11] != 0 || len(n) != 12 {
		t.Fatalf("a chunk that is not the last has nonce %x", n)
	}
}

func TestAMalformedHeaderIsRefused(t *testing.T) {
	good := testHeader(t).raw
	restAt := fixedHeaderLen
	edit := func(f func(b []byte) []byte) []byte { return f(bytes.Clone(good)) }
	encode := func(h *header) []byte {
		raw, err := h.marshal()
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	// A rest of fields written by hand, for orders marshal never produces.
	withRest := func(rest []byte) []byte {
		b := bytes.Clone(good[:fixedHeaderLen])
		binary.BigEndian.PutUint16(b[fixedHeaderLen-2:], uint16(len(rest))) //nolint:gosec // G115: test sizes
		return append(b, rest...)
	}
	field := func(b []byte, s string) []byte {
		b = binary.BigEndian.AppendUint16(b, uint16(len(s))) //nolint:gosec // G115: test sizes
		return append(b, s...)
	}
	pairs := func(kv ...string) []byte {
		b := field(field(nil, "arn"), "blob")
		b = binary.BigEndian.AppendUint16(b, uint16(len(kv)/2)) //nolint:gosec // G115: test sizes
		for _, s := range kv {
			b = field(b, s)
		}
		return b
	}

	for _, c := range []struct {
		name string
		raw  []byte
		want error
	}{
		{"another file", []byte("SQLite format 3\x00 and more bytes"), ErrNotABackup},
		{"empty", nil, ErrNotABackup},
		{"another version", edit(func(b []byte) []byte { b[len(magic)] = 2; return b }), ErrNotABackup},
		{"chunks too small", edit(func(b []byte) []byte {
			binary.BigEndian.PutUint32(b[len(magic)+1:], minChunkSize-1)
			return b
		}), ErrCorrupt},
		{"chunks too large", edit(func(b []byte) []byte {
			binary.BigEndian.PutUint32(b[len(magic)+1:], maxChunkSize+1)
			return b
		}), ErrCorrupt},
		{"a header longer than allowed", edit(func(b []byte) []byte {
			binary.BigEndian.PutUint16(b[restAt-2:], maxHeaderRest+1)
			return b
		}), ErrCorrupt},
		{"cut inside the header", good[:len(good)-1], ErrCorrupt},
		{"a field running past the header", withRest(field(nil, "arn")[:2]), ErrCorrupt},
		{"stray bytes after the context", withRest(append(pairs(contextPurpose, Purpose, contextRef, "x"), 0)), ErrCorrupt},
		{"a context out of order", withRest(pairs(contextRef, "x", contextPurpose, Purpose)), ErrCorrupt},
		{"a context key twice", withRest(pairs(contextRef, "x", contextRef, "y")), ErrCorrupt},
		{"an empty context key", withRest(pairs("", "x")), ErrCorrupt},
		{"no key", encode(&header{chunkSize: minChunkSize, wrappedKey: []byte("blob")}), ErrCorrupt},
		{"no wrapped key", encode(&header{chunkSize: minChunkSize, keyARN: "arn"}), ErrCorrupt},
	} {
		if _, err := readHeader(bytes.NewReader(c.raw)); !errors.Is(err, c.want) {
			t.Errorf("%s: readHeader = %v, want %v", c.name, err, c.want)
		}
	}
	// And the good one still reads, so the cases above fail for their own
	// reason.
	if _, err := readHeader(bytes.NewReader(good)); err != nil {
		t.Fatalf("the unedited header: %v", err)
	}
}

func TestAHeaderIsEncodedOneWayOnly(t *testing.T) {
	// The same fields always give the same bytes, whatever order the map
	// iterates in: the bytes are the associated data of every chunk.
	a, b := testHeader(t), testHeader(t)
	b.noncePrefix = a.noncePrefix
	for range 20 {
		rawA, err := a.marshal()
		if err != nil {
			t.Fatal(err)
		}
		rawB, err := b.marshal()
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(rawA, rawB) {
			t.Fatal("two marshals of the same header differ")
		}
	}
}

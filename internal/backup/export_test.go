package backup

import (
	"bytes"
	"context"
	"fmt"
)

// What the external tests in this directory reach inside for.

// Snapshot is snapshot.
func Snapshot(ctx context.Context, live, out string) error { return snapshot(ctx, live, out) }

// Inspect is inspect.
func Inspect(ctx context.Context, path string) (Inspection, error) { return inspect(ctx, path) }

// SourceDSN is the DSN the snapshot opens the live database with.
func SourceDSN(path string) string { return sourceDSN(path) }

// ReadLive is readLive.
func ReadLive(ctx context.Context, read func() error) error { return readLive(ctx, read) }

// ReadAttempts is readAttempts.
const ReadAttempts = readAttempts

// SetBeforeReadRetry installs the hook that runs before readLive tries a read
// again, and returns what undoes it.
func SetBeforeReadRetry(f func(err error)) (undo func()) {
	prev := beforeReadRetry
	beforeReadRetry = f
	return func() { beforeReadRetry = prev }
}

// SetAfterSnapshot installs the hook that runs between the snapshot and its
// check, and returns what undoes it.
func SetAfterSnapshot(f func(path string)) (undo func()) {
	prev := afterSnapshot
	afterSnapshot = f
	return func() { afterSnapshot = prev }
}

// HeaderFields are the parts of a header a test may rewrite.
type HeaderFields struct {
	ChunkSize int
	KeyARN    string
	Wrapped   []byte
	Context   map[string]string
}

// SplitHeader returns the length of the header at the start of b, and its
// fields.
func SplitHeader(b []byte) (int, HeaderFields, error) {
	h, err := readHeader(bytes.NewReader(b))
	if err != nil {
		return 0, HeaderFields{}, err
	}
	return len(h.raw), HeaderFields{
		ChunkSize: int(h.chunkSize), KeyARN: h.keyARN, Wrapped: h.wrappedKey, Context: h.context,
	}, nil
}

// RewriteHeader re-encodes the header of the backup b with edit applied,
// keeping the nonce prefix and every chunk as they were.
func RewriteHeader(b []byte, edit func(*HeaderFields)) ([]byte, error) {
	h, err := readHeader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	f := HeaderFields{ChunkSize: int(h.chunkSize), KeyARN: h.keyARN, Wrapped: h.wrappedKey, Context: h.context}
	edit(&f)
	h2 := &header{
		chunkSize: uint32(f.ChunkSize), noncePrefix: h.noncePrefix, //nolint:gosec // G115: test input
		keyARN: f.KeyARN, wrappedKey: f.Wrapped, context: f.Context,
	}
	raw, err := h2.marshal()
	if err != nil {
		return nil, err
	}
	return append(raw, b[len(h.raw):]...), nil
}

// Seal builds a backup of plaintext under the data key key, with the header
// fields f, as Run would.
func Seal(plaintext, key []byte, f HeaderFields) ([]byte, error) {
	h := &header{
		chunkSize: uint32(f.ChunkSize), //nolint:gosec // G115: test input
		keyARN:    f.KeyARN, wrappedKey: f.Wrapped, context: f.Context,
	}
	if _, err := h.marshal(); err != nil {
		return nil, err
	}
	aead, err := newAEAD(key)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if _, _, err := sealStream(context.Background(), &out, bytes.NewReader(plaintext), aead, h); err != nil {
		return nil, fmt.Errorf("seal: %w", err)
	}
	return out.Bytes(), nil
}

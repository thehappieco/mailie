// Package secretstest has a second secrets.Sealer, for the tests that prove a
// caller works with any sealer rather than with the keyring alone: a
// composite that opens both kinds, a rewrap from one kind to the other, a
// credentials row whose key id is 0. It also has a kms.Wrapper (Wrapper), for
// the tests of the sealer under a key service (kmssealer), which then run
// with no key service and no build tag.
//
// It lives in its own package, as storetest does, so that nothing in the
// daemon can import it by accident.
package secretstest

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/thehappieco/mailie/internal/secrets"
)

// Sealer stands in for a sealer under a key service. Its envelopes start with
// secrets.MagicTHCSEAL, as such a sealer's do, then its name:
//
//	"THCSEAL" || "test:" || name || 0x00 || nonce(12) || ciphertext || tag(16)
//
// under an AES-256-GCM key made from the name, with the header and the
// binding as additional data. Nothing about it is secret: it is a test's.
// Like a call to a key service it honours its context, sealing and opening
// nothing under one that has ended, can be made to fail every opening as an
// unreachable one does (FailOpens), and it counts what it was asked to do.
type Sealer struct {
	name   string
	header []byte
	aead   cipher.AEAD

	seals    atomic.Int64
	opens    atomic.Int64
	failOpen atomic.Pointer[error]
}

var _ secrets.Sealer = (*Sealer)(nil)

// New builds a test sealer. Two with the same name open each other's
// envelopes; one with another name knows none of them.
func New(name string) *Sealer {
	if name == "" || strings.ContainsRune(name, 0) {
		panic("secretstest: a name, without NUL")
	}
	key := sha256.Sum256([]byte("secretstest/" + name))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		panic(err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		panic(err)
	}
	header := []byte(secrets.MagicTHCSEAL + "test:" + name + "\x00")
	return &Sealer{name: name, header: header, aead: aead}
}

// Seal seals plaintext for b.
func (s *Sealer) Seal(ctx context.Context, b secrets.Binding, plaintext []byte) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := b.Validate(); err != nil {
		return nil, err
	}
	s.seals.Add(1)
	out := make([]byte, len(s.header)+s.aead.NonceSize(), len(s.header)+s.aead.NonceSize()+len(plaintext)+s.aead.Overhead())
	copy(out, s.header)
	nonce := out[len(s.header):]
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return s.aead.Seal(out, nonce, plaintext, s.aad(b)), nil
}

// Open opens an envelope this sealer, or one of the same name, sealed for b.
func (s *Sealer) Open(ctx context.Context, b secrets.Binding, envelope []byte) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := b.Validate(); err != nil {
		return nil, err
	}
	if failed := s.failOpen.Load(); failed != nil {
		return nil, *failed
	}
	if !s.Knows(envelope) {
		return nil, fmt.Errorf("%w: not test sealer %s's", secrets.ErrUnknownKey, s.name)
	}
	s.opens.Add(1)
	rest := envelope[len(s.header):]
	if len(rest) < s.aead.NonceSize()+s.aead.Overhead() {
		return nil, secrets.ErrMalformed
	}
	plaintext, err := s.aead.Open(nil, rest[:s.aead.NonceSize()], rest[s.aead.NonceSize():], s.aad(b))
	if err != nil {
		return nil, secrets.ErrDecrypt
	}
	return plaintext, nil
}

// Knows reports whether envelope carries this sealer's header.
func (s *Sealer) Knows(envelope []byte) bool { return bytes.HasPrefix(envelope, s.header) }

// Current is Knows: the test sealer has one key.
func (s *Sealer) Current(envelope []byte) bool { return s.Knows(envelope) }

// Describe names the sealer.
func (s *Sealer) Describe() string { return "test sealer " + s.name }

// FailOpens makes every Open fail with err, which says nothing about the
// envelope: a key service the sealer cannot reach, or one that refuses to
// decrypt while it still generates keys. Sealing goes on working. A nil err
// lets Open open again.
func (s *Sealer) FailOpens(err error) {
	if err == nil {
		s.failOpen.Store(nil)
		return
	}
	s.failOpen.Store(&err)
}

// Seals is how many envelopes the sealer has sealed.
func (s *Sealer) Seals() int64 { return s.seals.Load() }

// Opens is how many envelopes the sealer has tried to open.
func (s *Sealer) Opens() int64 { return s.opens.Load() }

// aad is the header and the binding, its purpose and its ref ended by NUL.
func (s *Sealer) aad(b secrets.Binding) []byte {
	out := append([]byte(nil), s.header...)
	out = append(out, b.Purpose+"\x00"+b.Ref+"\x00"...)
	return out
}

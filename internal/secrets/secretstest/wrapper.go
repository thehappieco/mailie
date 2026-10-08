package secretstest

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"strings"
	"sync/atomic"

	"github.com/thehappieco/kit/kms"
)

// Wrapper stands in for a key service's kms.Wrapper, AWS KMS's above all,
// so the sealer over it runs in every test with no key service and no build
// tag. It writes AWS KMS's provider byte unless it is built with another
// (NewWrapperFor), and wraps each data key with AES-256-GCM under a key made
// from its name, with the encryption context as additional data:
//
//	nonce(12) || AES-256-GCM(key, data key, context) || tag(16)
//
// As KMS does, it unwraps only what a wrapper of the same name wrapped under
// the same context, and anything else is kms.ErrUnwrap. Like a call to KMS it
// honours its context, can be made to fail every unwrapping (FailDecrypts) or
// every new data key (FailGenerates) as a service that is not reached,
// throttles or refuses the call does, and counts what it was asked to do. Nothing about it is secret: it is a
// test's.
type Wrapper struct {
	provider byte
	aead     cipher.AEAD

	generated    atomic.Int64
	decrypted    atomic.Int64
	failDecrypt  atomic.Pointer[error]
	failGenerate atomic.Pointer[error]
}

var _ kms.Wrapper = (*Wrapper)(nil)

// NewWrapper builds a test wrapper with AWS KMS's provider byte. Two with the
// same name unwrap each other's keys, as two handles on one KMS key do; one
// with another name unwraps none of them, as another key does not.
func NewWrapper(name string) *Wrapper { return NewWrapperFor(name, kms.ProviderAWS) }

// NewWrapperFor builds a test wrapper that writes, and requires, provider.
func NewWrapperFor(name string, provider byte) *Wrapper {
	if name == "" || strings.ContainsRune(name, 0) {
		panic("secretstest: a name, without NUL")
	}
	key := sha256.Sum256([]byte("secretstest/wrapper/" + name))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		panic(err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		panic(err)
	}
	return &Wrapper{provider: provider, aead: aead}
}

// Provider is the byte the wrapper writes into the envelopes sealed with it.
func (w *Wrapper) Provider() byte { return w.provider }

// GenerateDataKey makes a fresh data key and wraps it for ec.
func (w *Wrapper) GenerateDataKey(ctx context.Context, ec kms.Context) (plaintext, wrapped []byte, err error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if err := ec.Validate(); err != nil {
		return nil, nil, err
	}
	if failed := w.failGenerate.Load(); failed != nil {
		return nil, nil, *failed
	}
	w.generated.Add(1)
	dek := make([]byte, kms.DataKeyLen)
	if _, err := rand.Read(dek); err != nil {
		return nil, nil, err
	}
	out := make([]byte, w.aead.NonceSize(), w.aead.NonceSize()+len(dek)+w.aead.Overhead())
	if _, err := rand.Read(out); err != nil {
		return nil, nil, err
	}
	return dek, w.aead.Seal(out, out, dek, aad(ec)), nil
}

// Decrypt unwraps a data key wrapped for ec by a wrapper of the same name.
func (w *Wrapper) Decrypt(ctx context.Context, wrapped []byte, ec kms.Context) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := ec.Validate(); err != nil {
		return nil, err
	}
	if failed := w.failDecrypt.Load(); failed != nil {
		return nil, *failed
	}
	w.decrypted.Add(1)
	if len(wrapped) < w.aead.NonceSize()+w.aead.Overhead() {
		return nil, kms.ErrUnwrap
	}
	n := w.aead.NonceSize()
	dek, err := w.aead.Open(nil, wrapped[:n], wrapped[n:], aad(ec))
	if err != nil {
		return nil, kms.ErrUnwrap
	}
	return dek, nil
}

// FailDecrypts makes every Decrypt fail with err, which says nothing about
// the envelope: a key service not reached, one that throttles, or one whose
// policy refuses to decrypt while it still generates keys. Generating keys
// goes on working. A nil err lets Decrypt unwrap again.
func (w *Wrapper) FailDecrypts(err error) {
	if err == nil {
		w.failDecrypt.Store(nil)
		return
	}
	w.failDecrypt.Store(&err)
}

// FailGenerates makes every GenerateDataKey fail with err, as a key service
// that is not reached, throttles, or whose key is disabled does: nothing can
// be sealed. Unwrapping goes on working. A nil err lets it make keys again.
func (w *Wrapper) FailGenerates(err error) {
	if err == nil {
		w.failGenerate.Store(nil)
		return
	}
	w.failGenerate.Store(&err)
}

// Generated is how many data keys the wrapper has made.
func (w *Wrapper) Generated() int64 { return w.generated.Load() }

// Decrypted is how many data keys the wrapper has tried to unwrap.
func (w *Wrapper) Decrypted() int64 { return w.decrypted.Load() }

// aad is the context's four fields, each ended by a newline, which the
// context's alphabet does not have.
func aad(ec kms.Context) []byte {
	return []byte("secretstest/wrapper\n" + ec.Service + "\n" + ec.Env + "\n" + ec.Purpose + "\n" + ec.Ref + "\n")
}

// Package backuptest has stand-ins for KMS and S3, for tests of backups and
// restores that never touch the network.
//
// It lives in its own package, as storetest does, so that nothing in the
// binary can import a fake by accident.
package backuptest

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"sync"

	"github.com/thehappieco/mailie/internal/backup"
)

// KeyARN is the key the fakes are set up with, in AWS's documentation
// account, which belongs to nobody.
const KeyARN = "arn:aws:kms:us-east-2:111122223333:key/00000000-0000-4000-8000-000000000001"

// Region is KeyARN's region.
const Region = "us-east-2"

// ErrRefused is what the fake KMS answers for anything real KMS would refuse.
var ErrRefused = errors.New("backuptest: KMS refused")

// KMS is a stand-in for one KMS key. It wraps data keys with AES-GCM under a
// key of its own, with the key ARN and the canonical encryption context as
// associated data, so a wrapped key opens only under the same key and with
// the same context — which is what KMS promises.
type KMS struct {
	keyARN string
	master cipher.AEAD

	mu sync.Mutex
	// handed is every plaintext data key handed out, by either call, kept so a
	// test can check the caller zeroed it.
	handed   [][]byte
	contexts []map[string]string
	decrypts int
	// Fail, when set, is returned by GenerateDataKey.
	Fail error
}

// NewKMS is a fake KMS holding the key keyARN.
func NewKMS(keyARN string) *KMS {
	var k [32]byte
	if _, err := rand.Read(k[:]); err != nil {
		panic(err)
	}
	block, err := aes.NewCipher(k[:])
	if err != nil {
		panic(err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		panic(err)
	}
	return &KMS{keyARN: keyARN, master: aead}
}

// GenerateDataKey implements backup.KMS.
func (k *KMS) GenerateDataKey(_ context.Context, keyARN string, encCtx map[string]string) (backup.DataKey, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.Fail != nil {
		return backup.DataKey{}, k.Fail
	}
	if keyARN != k.keyARN {
		return backup.DataKey{}, fmt.Errorf("%w: no key %s", ErrRefused, keyARN)
	}
	plain := make([]byte, 32)
	if _, err := rand.Read(plain); err != nil {
		return backup.DataKey{}, err
	}
	wrapped := k.wrap(plain, encCtx)
	k.handed = append(k.handed, plain)
	k.contexts = append(k.contexts, maps.Clone(encCtx))
	return backup.DataKey{KeyARN: k.keyARN, Plaintext: plain, Wrapped: wrapped}, nil
}

// Decrypt implements backup.KMS.
func (k *KMS) Decrypt(_ context.Context, keyARN string, wrapped []byte, encCtx map[string]string) ([]byte, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.decrypts++
	if keyARN != k.keyARN {
		return nil, fmt.Errorf("%w: IncorrectKeyException", ErrRefused)
	}
	n := k.master.NonceSize()
	if len(wrapped) < n {
		return nil, fmt.Errorf("%w: InvalidCiphertextException", ErrRefused)
	}
	plain, err := k.master.Open(nil, wrapped[:n], wrapped[n:], aad(k.keyARN, encCtx))
	if err != nil {
		return nil, fmt.Errorf("%w: InvalidCiphertextException", ErrRefused)
	}
	k.handed = append(k.handed, plain)
	return plain, nil
}

// Wrap wraps a data key the test chose under ctx, as GenerateDataKey would.
// Real KMS can do this with Encrypt, which a key policy denies; a test uses
// it to build a header that KMS accepts but the chunks do not.
func (k *KMS) Wrap(plain []byte, encCtx map[string]string) []byte {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.wrap(plain, encCtx)
}

func (k *KMS) wrap(plain []byte, encCtx map[string]string) []byte {
	nonce := make([]byte, k.master.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		panic(err)
	}
	return k.master.Seal(nonce, nonce, plain, aad(k.keyARN, encCtx))
}

// Handed returns every plaintext data key either call returned: the same
// slices, so a test sees whether the caller zeroed them.
func (k *KMS) Handed() [][]byte {
	k.mu.Lock()
	defer k.mu.Unlock()
	return slices.Clone(k.handed)
}

// Contexts returns the encryption context of every GenerateDataKey call.
func (k *KMS) Contexts() []map[string]string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return slices.Clone(k.contexts)
}

// Decrypts counts Decrypt calls.
func (k *KMS) Decrypts() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.decrypts
}

// aad is a canonical encoding of the key and the context.
func aad(keyARN string, encCtx map[string]string) []byte {
	keys := make([]string, 0, len(encCtx))
	for key := range encCtx {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	var b []byte
	put := func(s string) {
		b = binary.BigEndian.AppendUint32(b, uint32(len(s))) //nolint:gosec // G115: test strings
		b = append(b, s...)
	}
	put(keyARN)
	for _, key := range keys {
		put(key)
		put(encCtx[key])
	}
	return b
}

// Bucket is a stand-in for S3 with what the host is allowed: PutObject of a
// new key, verified against its SHA-256 and length, refused if the key exists
// (If-None-Match: *). GetObject is for whoever restores.
type Bucket struct {
	mu      sync.Mutex
	objects map[string][]byte
	puts    []backup.PutInput

	// BeforePut, when set, runs at the start of every PutObject with the
	// request as received; an error from it fails the upload.
	BeforePut func(ctx context.Context, in backup.PutInput) error
}

// NewBucket is an empty fake bucket.
func NewBucket() *Bucket { return &Bucket{objects: map[string][]byte{}} }

// ErrPreconditionFailed is S3's answer to If-None-Match: * on an existing key.
var ErrPreconditionFailed = errors.New("backuptest: PreconditionFailed")

// PutObject implements backup.Uploader.
func (b *Bucket) PutObject(ctx context.Context, in backup.PutInput) error {
	if b.BeforePut != nil {
		if err := b.BeforePut(ctx, in); err != nil {
			return err
		}
	}
	body, err := io.ReadAll(in.Body)
	if err != nil {
		return err
	}
	if int64(len(body)) != in.Size {
		return fmt.Errorf("backuptest: Content-Length %d, body %d bytes", in.Size, len(body))
	}
	if sum := sha256.Sum256(body); !bytes.Equal(sum[:], in.SHA256) {
		return errors.New("backuptest: BadDigest")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	name := in.Bucket + "/" + in.Key
	if _, ok := b.objects[name]; ok {
		return ErrPreconditionFailed
	}
	b.objects[name] = body
	in.Body = nil
	b.puts = append(b.puts, in)
	return nil
}

// GetObject implements backup.Downloader.
func (b *Bucket) GetObject(_ context.Context, bucket, key string) (io.ReadCloser, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	body, ok := b.objects[bucket+"/"+key]
	if !ok {
		return nil, errors.New("backuptest: NoSuchKey")
	}
	return io.NopCloser(bytes.NewReader(bytes.Clone(body))), nil
}

// Puts returns every accepted upload, without its body.
func (b *Bucket) Puts() []backup.PutInput {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.puts)
}

// Object returns a stored object's bytes, or nil.
func (b *Bucket) Object(bucket, key string) []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return bytes.Clone(b.objects[bucket+"/"+key])
}

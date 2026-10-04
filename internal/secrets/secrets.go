// Package secrets seals the credentials this server must be able to open again.
//
// Unlike an archive that only ever accepts content, a mail client has to
// authenticate to somebody else's IMAP and SMTP servers, so it needs the
// plaintext of a refresh token every time it reconnects. There is no design in
// which the key is absent; what there is instead is a key that lives in the
// environment, never on disk beside the database, and an envelope that says
// which key sealed it so the key can be replaced without a flag day.
//
// Envelope layout, stored as a BLOB:
//
//	version(1) || keyID(1) || nonce(12) || ciphertext || tag(16)
//
// The header is inside the additional data, not merely in front of it. If it
// were unauthenticated, an attacker with write access to the database could
// change the key id or the version byte and watch what the daemon did about
// it; authenticating it makes every such edit a decryption failure instead.
//
// The rest of the additional data binds the ciphertext to the row it belongs
// to: account id and field name. A refresh token lifted from one account's row
// and pasted into another's therefore does not decrypt, so a database editor
// cannot make account A authenticate with account B's token.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"sort"
)

const (
	// Version1 is the only envelope version. A second version would change
	// the cipher; the byte exists so that change does not require guessing.
	Version1 byte = 1

	headerLen = 2
	nonceLen  = 12
	tagLen    = 16
	// Overhead is what sealing adds to the plaintext length.
	Overhead = headerLen + nonceLen + tagLen

	// KeyLen is the AES-256 key size.
	KeyLen = 32

	// aadPrefix domain-separates this project's envelopes from anything else
	// that might ever be sealed with the same key. It is part of every stored
	// envelope: it never follows the module's name, and changing it makes every
	// stored credential undecryptable (TestTheEnvelopeLabelsNeverChange).
	aadPrefix = "mailserver/cred/v1/"
	// deriveInfoPrefix does the same for derived keys, under the same rule.
	deriveInfoPrefix = "mailserver/derive/v1/"
)

var (
	// ErrUnknownKey means the envelope was sealed with a key this process was
	// not given. During a rotation the previous keys must stay configured
	// until every row has been rewrapped.
	ErrUnknownKey = errors.New("secrets: unknown key id")
	// ErrMalformed means the blob is too short or carries an unknown version.
	ErrMalformed = errors.New("secrets: malformed envelope")
	// ErrDecrypt means authentication failed: wrong key, tampered ciphertext,
	// or an envelope moved to a different account or field.
	ErrDecrypt = errors.New("secrets: decryption failed")
)

// Keyring holds the active key plus any previous keys still needed to read
// rows that have not been rewrapped.
type Keyring struct {
	activeID uint8
	ciphers  map[uint8]cipher.AEAD
	// derivable is the active key's material, for DeriveKey.
	derivable []byte
}

// NewKeyring builds a keyring. keys maps key id to a 32-byte key and must
// contain activeID.
func NewKeyring(activeID uint8, keys map[uint8][]byte) (*Keyring, error) {
	if activeID == 0 {
		return nil, errors.New("secrets: key id 0 is reserved")
	}
	if len(keys[activeID]) == 0 {
		return nil, fmt.Errorf("secrets: no key material for the active key id %d", activeID)
	}
	kr := &Keyring{
		activeID: activeID, ciphers: make(map[uint8]cipher.AEAD, len(keys)),
		derivable: append([]byte(nil), keys[activeID]...),
	}
	for id, key := range keys {
		if id == 0 {
			return nil, errors.New("secrets: key id 0 is reserved")
		}
		if len(key) != KeyLen {
			return nil, fmt.Errorf("secrets: key %d is %d bytes, want %d", id, len(key), KeyLen)
		}
		block, err := aes.NewCipher(key)
		if err != nil {
			return nil, fmt.Errorf("secrets: key %d: %w", id, err)
		}
		aead, err := cipher.NewGCM(block)
		if err != nil {
			return nil, fmt.Errorf("secrets: key %d: %w", id, err)
		}
		kr.ciphers[id] = aead
	}
	return kr, nil
}

// DeriveKey is a key for purpose, derived from the active key with
// HKDF-SHA256: for a keyed hash that must not be recomputable from the
// database alone, which is exactly what the credential key is kept apart
// from. Purposes never share a key, and no derived key opens an envelope.
//
// It follows the active key: after a rotation it is another key, and what
// was made under the old one no longer matches.
func (k *Keyring) DeriveKey(purpose string) ([]byte, error) {
	if purpose == "" {
		return nil, errors.New("secrets: a derived key needs a purpose")
	}
	key, err := hkdf.Key(sha256.New, k.derivable, nil, deriveInfoPrefix+purpose, KeyLen)
	if err != nil {
		return nil, fmt.Errorf("secrets: derive a key: %w", err)
	}
	return key, nil
}

// ActiveKeyID is the id new envelopes are sealed under. Rows carrying a
// different id are what `rewrap-credentials` looks for.
func (k *Keyring) ActiveKeyID() uint8 { return k.activeID }

// KeyIDs lists the configured key ids, ascending, for startup logs.
func (k *Keyring) KeyIDs() []uint8 {
	out := make([]uint8, 0, len(k.ciphers))
	for id := range k.ciphers {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Seal encrypts plaintext for one field of one account under the active key.
func (k *Keyring) Seal(accountID, field string, plaintext []byte) ([]byte, error) {
	aead := k.ciphers[k.activeID]
	if aead == nil {
		return nil, ErrUnknownKey
	}

	out := make([]byte, headerLen+nonceLen, headerLen+nonceLen+len(plaintext)+tagLen)
	out[0] = Version1
	out[1] = k.activeID
	nonce := out[headerLen : headerLen+nonceLen]
	// A random nonce rather than a counter: a counter needs durable state
	// that survives a crash, and a nonce reused under the same key breaks GCM
	// completely. 96 random bits per key are ample for this volume.
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("secrets: read random: %w", err)
	}
	return aead.Seal(out, nonce, plaintext, aad(out[:headerLen], accountID, field)), nil
}

// Open decrypts an envelope. It fails if the envelope was sealed for a
// different account or field, under an unknown key, or has been altered.
func (k *Keyring) Open(accountID, field string, envelope []byte) ([]byte, error) {
	if len(envelope) < Overhead {
		return nil, ErrMalformed
	}
	if envelope[0] != Version1 {
		return nil, fmt.Errorf("%w: version %d", ErrMalformed, envelope[0])
	}
	aead := k.ciphers[envelope[1]]
	if aead == nil {
		return nil, fmt.Errorf("%w: %d", ErrUnknownKey, envelope[1])
	}
	nonce := envelope[headerLen : headerLen+nonceLen]
	plaintext, err := aead.Open(nil, nonce, envelope[headerLen+nonceLen:], aad(envelope[:headerLen], accountID, field))
	if err != nil {
		// The cause is deliberately not reported: wrong key, wrong account and
		// tampered ciphertext are indistinguishable to the caller, so nothing
		// here can be used to probe which one it was.
		return nil, ErrDecrypt
	}
	return plaintext, nil
}

// KeyID reports which key sealed an envelope, without opening it. The column
// beside the blob carries the same value so a rewrap can select rows in SQL;
// this is what verifies that column.
func KeyID(envelope []byte) (uint8, error) {
	if len(envelope) < headerLen {
		return 0, ErrMalformed
	}
	if envelope[0] != Version1 {
		return 0, fmt.Errorf("%w: version %d", ErrMalformed, envelope[0])
	}
	return envelope[1], nil
}

// NeedsRewrap reports whether an envelope is sealed under something other than
// the active key.
func (k *Keyring) NeedsRewrap(envelope []byte) bool {
	id, err := KeyID(envelope)
	return err != nil || id != k.activeID
}

// Rewrap opens an envelope with whichever key sealed it and re-seals it under
// the active key. The plaintext never leaves this function.
func (k *Keyring) Rewrap(accountID, field string, envelope []byte) ([]byte, error) {
	plaintext, err := k.Open(accountID, field, envelope)
	if err != nil {
		return nil, err
	}
	sealed, err := k.Seal(accountID, field, plaintext)
	// Best effort: Go cannot guarantee the copy is gone, but leaving the
	// buffer readable in the heap for the rest of the process is worse.
	for i := range plaintext {
		plaintext[i] = 0
	}
	return sealed, err
}

// aad binds an envelope to its header, its account and its field.
func aad(header []byte, accountID, field string) []byte {
	out := make([]byte, 0, len(header)+len(aadPrefix)+len(accountID)+1+len(field))
	out = append(out, header...)
	out = append(out, aadPrefix...)
	out = append(out, accountID...)
	out = append(out, '/')
	out = append(out, field...)
	return out
}

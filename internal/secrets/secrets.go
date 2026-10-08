// Package secrets seals the secrets this server must be able to open again:
// the credentials of every mailbox, and the send-hash root.
//
// Unlike an archive that only ever accepts content, a mail client has to
// authenticate to somebody else's IMAP and SMTP servers, so it needs the
// plaintext of a refresh token every time it reconnects. There is no design in
// which the key is absent; what there is instead is a key that lives outside
// the database, and an envelope that says what sealed it so the key can be
// replaced without a flag day.
//
// Whatever seals is a Sealer: it seals for a Binding (what the secret is, and
// what it belongs to) and opens only for the same one, says without opening
// an envelope whether it can open it and whether it is what it would write
// now, and takes a context, because a sealer may call a key service for every
// envelope. A Composite seals with one sealer and opens what any of several
// sealed, which is what lets the kind of key change without a flag day too.
// The Keyring, the key in the environment, is the one implementation here and
// the self-hosted default; an envelope under a key service's data key starts
// with MagicTHCSEAL instead of Version1.
//
// The keyring's envelope layout, stored as a BLOB:
//
//	version(1) || keyID(1) || nonce(12) || ciphertext || tag(16)
//
// The header is inside the additional data, not merely in front of it. If it
// were unauthenticated, an attacker with write access to the database could
// change the key id or the version byte and watch what the daemon did about
// it; authenticating it makes every such edit a decryption failure instead.
//
// The rest of the additional data binds the ciphertext to the row it belongs
// to: the binding's ref and its purpose's label, for a credential its account
// id and field name (keyringLabels). A refresh token lifted from one account's
// row and pasted into another's therefore does not decrypt, so a database
// editor cannot make account A authenticate with account B's token.
package secrets

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"sort"
)

const (
	// Version1 is the only keyring envelope version. A second version would
	// change the cipher; the byte exists so that change does not require
	// guessing.
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
	// ErrSealedElsewhere is ErrDecrypt from a sealer whose envelopes do not
	// name what they were sealed under (a key service's: neither the KMS key
	// nor the deployment, MAIL_ENV, is in the header), for an envelope of its
	// own kind and key provider that its key would not open for the binding.
	// Most likely the settings that sealed it have changed since, another
	// KMS key or another MAIL_ENV, and putting them back opens it; it may
	// also have been moved to another row or altered. A sealer that returns
	// it wraps ErrDecrypt with it. Unlike a key that is not given, nothing
	// can be added beside the sealer to open it, a rewrap cannot move it (it
	// reads as current), and only an operator who knows that key is lost for
	// good may have what it holds replaced.
	ErrSealedElsewhere = errors.New("secrets: sealed under another KMS key or MAIL_ENV than the configured " +
		"ones, or moved or altered")
	// ErrBinding is a binding no sealer seals or opens for (Binding.Validate):
	// without a purpose or a ref, which would bind an envelope to less than
	// the row it belongs to, or with one no key service would take.
	ErrBinding = errors.New("secrets: invalid binding")
)

// Keyring holds the active key plus any previous keys still needed to read
// rows that have not been rewrapped. It is a Sealer.
type Keyring struct {
	activeID uint8
	ciphers  map[uint8]cipher.AEAD
}

var _ Sealer = (*Keyring)(nil)

// NewKeyring builds a keyring. keys maps key id to a 32-byte key and must
// contain activeID. Key id 0 is reserved: the credentials table records it
// for an envelope no keyring key sealed (KeyID).
func NewKeyring(activeID uint8, keys map[uint8][]byte) (*Keyring, error) {
	if activeID == 0 {
		return nil, errors.New("secrets: key id 0 is reserved")
	}
	if len(keys[activeID]) == 0 {
		return nil, fmt.Errorf("secrets: no key material for the active key id %d", activeID)
	}
	kr := &Keyring{activeID: activeID, ciphers: make(map[uint8]cipher.AEAD, len(keys))}
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

// Describe names the active key by its id.
func (k *Keyring) Describe() string { return fmt.Sprintf("credential key %d", k.activeID) }

// Seal encrypts plaintext for b under the active key. The keyring calls
// nothing, so it has no use for the context.
func (k *Keyring) Seal(_ context.Context, b Binding, plaintext []byte) ([]byte, error) {
	if err := b.Validate(); err != nil {
		return nil, err
	}
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
	return aead.Seal(out, nonce, plaintext, aad(out[:headerLen], b)), nil
}

// Open decrypts an envelope. It fails if the envelope was sealed for a
// different binding (another account, another field), under an unknown key,
// or has been altered.
func (k *Keyring) Open(_ context.Context, b Binding, envelope []byte) ([]byte, error) {
	if err := b.Validate(); err != nil {
		return nil, err
	}
	if isTHCSEAL(envelope) {
		return nil, unknownEnvelope(envelope)
	}
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
	plaintext, err := aead.Open(nil, nonce, envelope[headerLen+nonceLen:], aad(envelope[:headerLen], b))
	if err != nil {
		// The cause is deliberately not reported: wrong key, wrong account and
		// tampered ciphertext are indistinguishable to the caller, so nothing
		// here can be used to probe which one it was.
		return nil, ErrDecrypt
	}
	return plaintext, nil
}

// Knows reports whether envelope is a v1 envelope under a key the keyring
// holds, the active one or a previous one.
func (k *Keyring) Knows(envelope []byte) bool {
	return len(envelope) >= headerLen && envelope[0] == Version1 && k.ciphers[envelope[1]] != nil
}

// Current reports whether envelope is a v1 envelope under the active key.
func (k *Keyring) Current(envelope []byte) bool {
	return len(envelope) >= headerLen && envelope[0] == Version1 && envelope[1] == k.activeID
}

// KeyID reports which keyring key sealed an envelope, without opening it: 0,
// which no keyring key may have, for a THCSEAL envelope, which no keyring key
// sealed. The column beside a credential carries the same value, so a person
// reading the table sees which key each row still needs.
func KeyID(envelope []byte) (uint8, error) {
	if isTHCSEAL(envelope) {
		return 0, nil
	}
	if len(envelope) < headerLen {
		return 0, ErrMalformed
	}
	if envelope[0] != Version1 {
		return 0, fmt.Errorf("%w: version %d", ErrMalformed, envelope[0])
	}
	return envelope[1], nil
}

// isTHCSEAL reports whether an envelope starts with MagicTHCSEAL.
func isTHCSEAL(envelope []byte) bool { return bytes.HasPrefix(envelope, []byte(MagicTHCSEAL)) }

// keyringLabels are what a keyring envelope's additional data says for a
// credential's purpose: the credentials row's field, as it did before
// purposes had names. Every stored credential was sealed with them, so they
// never change (TestTheEnvelopeLabelsNeverChange); a purpose not listed, the
// send-hash root's, is its own label.
//
//nolint:gosec // G101: field names, not credentials
var keyringLabels = map[string]string{
	PurposeOAuthToken: "oauth_token",
	PurposePassword:   "password",
}

// aad binds a keyring envelope to its header and its binding: the header,
// aadPrefix, the ref, '/' and the purpose's label. For a credential that is
// the account id and the field, byte for byte what every stored credential
// was sealed with.
func aad(header []byte, b Binding) []byte {
	label, ok := keyringLabels[b.Purpose]
	if !ok {
		label = b.Purpose
	}
	out := make([]byte, 0, len(header)+len(aadPrefix)+len(b.Ref)+1+len(label))
	out = append(out, header...)
	out = append(out, aadPrefix...)
	out = append(out, b.Ref...)
	out = append(out, '/')
	out = append(out, label...)
	return out
}

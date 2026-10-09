// Package keyscheme is Mailie's profile of the kit's key scheme: the labels,
// headers, magic, kinds and additional data with which a person's account key
// is wrapped, a mailbox's private key is granted, and the account key is kept
// in a browser. docs/key-scheme.md is the specification it implements and
// docs/key-scheme-threat-model.md what it defends against; every byte is
// pinned by the vectors in testdata/, which this package writes and opens and
// web/test/keyscheme.spec.ts opens with web/src/crypto/mailie.ts.
//
// It holds parameters and the checks around them, never a primitive of its
// own: Argon2id and the auth/wrap split are the kit's account package, the
// wrap envelope is account.Wrap, a grant is seal.SealDirect at seal.GrantRow,
// the cloud's wrap under the product key is platformwrap with the kit's
// Mailie profile, the password preparation and the recovery code are the
// platform profile's, and the restricted JSON AAD is platform.JCSArray.
//
// Who calls what: the browser makes every key and every wrap and grant (the
// console, through mailie.ts); the server only checks shapes and spellings
// (CheckAccountWrapShape, CheckGrantShape, CheckPublicKey, ValidSealID,
// ValidNamespace), normalises addresses as it stores them
// (NormaliseAddress) and derives the salts it hands out (DecoySalt). The Go
// functions that seal and open exist for the vectors, the tests and tools
// that run on a person's own machine.
package keyscheme

import (
	"crypto/ecdh"
	"crypto/subtle"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/thehappieco/kit/profiles/platform"
)

// KeyLen is the length of every key in the scheme: X25519 private and public
// keys, wrap keys and the product key.
const KeyLen = 32

// The scheme's own failures. Each is a sentinel errors.Is matches; the kit's
// errors (account.ErrWrongKey, seal.ErrAuthentication, ...) pass through as
// they are, and docs/key-scheme.md section 13 lists them all.
var (
	// ErrBinding is an input outside its spelling, refused before anything
	// is derived, sealed or opened: a seal id or namespace that is not a
	// lowercase UUIDv4, a wrap kind other than password and recovery, an
	// epoch outside 1 to 65535, a key that is not 32 bytes, an account key
	// whose public half is not the one bound, or a platform wrap's seal id
	// equal to the sub.
	ErrBinding = errors.New("keyscheme: the binding is outside its spelling")
	// ErrShape is what the server refuses of a value it stores and cannot
	// open: an account wrap, or a grant, of the wrong shape.
	ErrShape = errors.New("keyscheme: not the shape this column holds")
	// ErrPublicKey is a public key the server refuses to store: not 32
	// bytes, not the canonical encoding of an X25519 point, or of low order.
	ErrPublicKey = errors.New("keyscheme: not an X25519 public key the server accepts")
)

// ValidSealID reports whether id is a seal id in its one spelling: a version
// 4 UUID of the RFC 9562 variant, as 36 characters of lowercase hyphenated
// text. A seal id is the immutable UUID a person is known by inside every
// wrap and grant (users.seal_id); the server draws it, the address never
// takes part in it.
func ValidSealID(id string) bool { return validUUIDv4(id) }

// ValidNamespace reports whether ns is a mailbox namespace in its one
// spelling: a version 4 UUID of the RFC 9562 variant, as 36 characters of
// lowercase hyphenated text. The linker's browser draws it with the
// mailbox's key pair; the server stores it once and never changes it.
func ValidNamespace(ns string) bool { return validUUIDv4(ns) }

// NewSealID draws a seal id for a person: a random UUIDv4.
func NewSealID() string { return uuid.NewString() }

// validUUIDv4 checks the lowercase hyphenated spelling (the platform's rule
// for a sub, section 11.1 of the kit's SPEC), then the version and variant
// nibbles. uuid.Parse alone would accept upper case, braces and a urn:
// prefix, which are other spellings of the same 16 bytes.
func validUUIDv4(s string) bool {
	if !platform.ValidSub(s) {
		return false
	}
	return s[14] == '4' && (s[19] == '8' || s[19] == '9' || s[19] == 'a' || s[19] == 'b')
}

// parseUUIDv4 returns the 16 bytes of a seal id or a namespace.
func parseUUIDv4(s, what string) (uuid.UUID, error) {
	if !validUUIDv4(s) {
		return uuid.UUID{}, fmt.Errorf("%w: the %s is not a lowercase UUIDv4", ErrBinding, what)
	}
	return uuid.MustParse(s), nil
}

// PublicKey returns X25519(key, 9), the public half of a 32-byte private
// key. Any 32 bytes are a private key.
func PublicKey(key []byte) ([]byte, error) {
	if len(key) != KeyLen {
		return nil, fmt.Errorf("%w: a private key is %d bytes", ErrBinding, KeyLen)
	}
	priv, err := ecdh.X25519().NewPrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrBinding, err)
	}
	return priv.PublicKey().Bytes(), nil
}

// isPublicHalf reports, in constant time, whether pub is X25519(key, 9).
func isPublicHalf(key, pub []byte) bool {
	got, err := PublicKey(key)
	return err == nil && subtle.ConstantTimeCompare(got, pub) == 1
}

// CheckPublicKey is the server's check of a public key it is asked to store
// once and hand to others: a person's account public key (users.public_key)
// and a mailbox's (mailbox_keys.public_key). It is the platform's check of a
// product public key (the kit's SPEC section 11.4): exactly 32 bytes, the
// canonical encoding of the point, and not of low order, so that a key the
// server hands out has one spelling and a secret can be agreed with it.
func CheckPublicKey(pub []byte) error {
	if err := platform.CheckPublicKey(pub); err != nil {
		return fmt.Errorf("%w: %w", ErrPublicKey, err)
	}
	return nil
}

// Package keyscheme is Mailie's profile of the kit's key scheme: the labels,
// headers, magic, kinds and additional data with which a person's account key
// is wrapped, a mailbox's private key is granted, and the account key is kept
// in a browser. docs/key-scheme.md is the specification it implements and
// docs/key-scheme-threat-model.md what it defends against; every byte is
// pinned by the vectors in testdata/, which this package writes and opens and
// web/test/keyscheme.spec.ts opens with web/src/crypto/mailie.ts.
//
// The profile itself is the kit's profiles/mailie (its SPEC Appendix D),
// which the kit's v0.7.0 took from this specification, frozen by these
// vectors: the kit's vectors/mailie/key-scheme-v1 are testdata/, byte for
// byte (TestTheVectorsAreTheKitsFrozenOnes). This file gives it the names the
// core uses, as aliases, constants and forwarders, and holds no code of its
// own; server.go holds what stays the server's: how it normalises an address
// and derives the salt it hands out (a server's salts are outside the kit,
// its SPEC section 13), and drawing seal ids.
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
	"github.com/google/uuid"

	"github.com/thehappieco/kit/account"
	"github.com/thehappieco/kit/hpke"
	"github.com/thehappieco/kit/platformwrap"
	"github.com/thehappieco/kit/profiles/mailie"
	"github.com/thehappieco/kit/seal"
)

// KeyLen is the length of every key in the scheme: X25519 private and public
// keys, wrap keys and the product key.
const KeyLen = mailie.KeyLen

// The scheme's own failures, the kit's sentinels themselves, so errors.Is
// matches whatever the profile returns; the kit's other errors
// (account.ErrWrongKey, seal.ErrAuthentication, ...) pass through as they
// are, and docs/key-scheme.md section 13 lists them all.
var (
	// ErrBinding is an input outside its spelling, refused before anything
	// is derived, sealed or opened: a seal id or namespace that is not a
	// lowercase UUIDv4, a wrap kind other than password and recovery, an
	// epoch outside its range, a key that is not 32 bytes, or a platform
	// wrap's seal id equal to the sub.
	ErrBinding = mailie.ErrBinding
	// ErrShape is what the server refuses of a value it stores and cannot
	// open: an account wrap, or a grant, of the wrong shape.
	ErrShape = mailie.ErrShape
	// ErrPublicKey is a public key the server refuses to store: not 32
	// bytes, not the canonical encoding of an X25519 point, or of low order.
	ErrPublicKey = mailie.ErrPublicKey
)

// ---------------------------------------------------------------------------
// Identities and public keys (docs/key-scheme.md sections 3 and 4)
// ---------------------------------------------------------------------------

// ValidSealID reports whether id is a seal id in its one spelling: a version
// 4 UUID of the RFC 9562 variant, as 36 characters of lowercase hyphenated
// text. A seal id is the immutable UUID a person is known by inside every
// wrap and grant (users.seal_id); the server draws it (NewSealID), the
// address never takes part in it.
func ValidSealID(id string) bool { return mailie.ValidSealID(id) }

// ValidNamespace reports whether ns is a mailbox namespace in its one
// spelling, the same as a seal id's. The linker's browser draws it with the
// mailbox's key pair; the server stores it once and never changes it.
func ValidNamespace(ns string) bool { return mailie.ValidNamespace(ns) }

// PublicKey returns X25519(key, 9), the public half of a 32-byte private
// key. Any 32 bytes are a private key; another length is ErrBinding.
func PublicKey(key []byte) ([]byte, error) { return mailie.PublicKey(key) }

// CheckPublicKey is the server's check of a public key it is asked to store
// once and hand to others: a person's account public key (users.public_key)
// and a mailbox's (mailbox_keys.public_key). It is the platform's check of a
// product public key (the kit's SPEC section 11.4): exactly 32 bytes, the
// canonical encoding of the point, and not of low order. A refusal matches
// ErrPublicKey.
func CheckPublicKey(pub []byte) error { return mailie.CheckPublicKey(pub) }

// ---------------------------------------------------------------------------
// The account (docs/key-scheme.md section 5)
// ---------------------------------------------------------------------------

// The account scheme's labels and the account wrap's format. They name keys;
// they are not credentials.
const (
	// PasswordAuthLabel and PasswordWrapLabel are the HKDF infos of the two
	// branches of the Argon2id master key: the auth key, which is sent, and
	// the wrap key, which never leaves the browser.
	PasswordAuthLabel = mailie.PasswordAuthLabel
	PasswordWrapLabel = mailie.PasswordWrapLabel
	// RecoveryWrapLabel and RecoveryAuthLabel are the HKDF infos of the two
	// branches of a recovery code: its wrap key and its proof.
	RecoveryWrapLabel = mailie.RecoveryWrapLabel
	RecoveryAuthLabel = mailie.RecoveryAuthLabel
	// AccountWrapTag opens the additional data of an account wrap.
	AccountWrapTag = mailie.AccountWrapTag
	// AccountWrapVersion is the 1 in the additional data.
	AccountWrapVersion = mailie.AccountWrapVersion
	// AccountWrapHeader is the first byte of the password and recovery wraps
	// of an account key, 0x02. The platform wrap starts with 0x03 and a
	// grant with the seal magic 'M', so a blob in the wrong column fails at
	// its first byte.
	AccountWrapHeader = mailie.AccountWrapHeader
	// AccountWrapLen is a wrap's length, 61 bytes: the header, a 12-byte
	// nonce, the 32-byte account key and a 16-byte tag.
	AccountWrapLen = mailie.AccountWrapLen
	// SaltLen is the only salt length the profile derives with.
	SaltLen = mailie.SaltLen
	// PasswordPreparation names the preparation of a password: the platform
	// profile's, for a password being presented (no minimum length; a new
	// password's minimum is checked by platform.PrepareNewPassword).
	PasswordPreparation = mailie.PasswordPreparation
)

// WrapKind says which secret an account wrap is under. It is bound into the
// wrap's additional data.
type WrapKind = mailie.WrapKind

const (
	// WrapPassword is the wrap under the password's wrap key.
	WrapPassword = mailie.WrapPassword
	// WrapRecovery is the wrap under the recovery code's wrap key.
	WrapRecovery = mailie.WrapRecovery
)

// DefaultKDF is what new accounts derive with: Argon2id, 64 MiB, three
// passes, one lane, the floor of the platform's bounds.
var DefaultKDF = mailie.DefaultKDF

// Account is Mailie's profile for the kit's account package: its labels, the
// platform's preparation of a presented password, the platform's KDF bounds,
// base64url text without padding, the platform's canonical form of a
// recovery code, and the one-byte header 0x02 with no legacy form. Every
// call returns a fresh value.
func Account() account.Profile { return mailie.Account() }

// AccountWrapAAD is the additional data of an account wrap, a restricted JSON
// AAD (the kit's SPEC section 11.1):
//
//	JCS(["mailie/account-wrap", 1, kind, seal_id, base64url(account public key)])
//
// It binds the immutable seal id, never the address; the kind; and the
// account public key the server holds for the person.
func AccountWrapAAD(kind WrapKind, sealID string, accountPublicKey []byte) ([]byte, error) {
	return mailie.AccountWrapAAD(kind, sealID, accountPublicKey)
}

// SealAccountWrap wraps a 32-byte account key under a wrap key (the
// password's, or the recovery code's) for the person sealID names, 61 bytes
// under a fresh nonce, and opens what it made before it returns it. The keys
// are the caller's to clear.
func SealAccountWrap(kind WrapKind, wrapKey, accountKey []byte, sealID string) ([]byte, error) {
	return mailie.SealAccountWrap(kind, wrapKey, accountKey, sealID)
}

// OpenAccountWrap opens an account wrap with its wrap key and returns the
// account key, which the caller clears. The binding comes first
// (ErrBinding); a wrap shorter than 61 bytes is account.ErrTruncated; a wrap
// key that is not 32 bytes is account.ErrBadKey; a wrong key, another
// binding, a changed byte and a key that is not the private half of
// accountPublicKey are all account.ErrWrongKey.
func OpenAccountWrap(kind WrapKind, wrapKey, wrap []byte, sealID string, accountPublicKey []byte) ([]byte, error) {
	return mailie.OpenAccountWrap(kind, wrapKey, wrap, sealID, accountPublicKey)
}

// CheckAccountWrapShape is what the server checks of a password or recovery
// wrap it is sent, which it cannot open: 61 bytes starting with 0x02.
func CheckAccountWrapShape(wrap []byte) error { return mailie.CheckAccountWrapShape(wrap) }

// ---------------------------------------------------------------------------
// The envelope domain, the kinds and the grants (docs/key-scheme.md
// sections 9 and 10)
// ---------------------------------------------------------------------------

// SealLabel prefixes the additional data and the HPKE info of every envelope
// Mailie seals, "mlv1"; Wappie's is "wsv1".
const SealLabel = mailie.SealLabel

// SealMagic is bytes 0-1 of every envelope Mailie seals: "ML". Wappie's are
// "WS".
var SealMagic = mailie.SealMagic

// SealDomain is Mailie's envelope domain: magic "ML", label "mlv1".
func SealDomain() seal.Domain { return mailie.SealDomain() }

// Kind identifies what a sealed value is; its byte and its name are wire
// format. Phase 3 seals only KindMailboxGrant; the bytes named for phase 4
// are reserved, and a byte with no name is unassigned.
type Kind = mailie.Kind

const (
	KindHeaders       = mailie.KindHeaders       // reserved for phase 4: a message's indexed header fields
	KindSnippet       = mailie.KindSnippet       // reserved for phase 4: a message's preview text
	KindBody          = mailie.KindBody          // reserved for phase 4: a message's text
	KindAttachmentKey = mailie.KindAttachmentKey // reserved for phase 4: an attachment's key
	KindSearchIndex   = mailie.KindSearchIndex   // reserved for phase 4: a segment of the search index
	KindContentKey    = mailie.KindContentKey    // the core content key (seal.KindContentKey), phase 4
	KindMailboxGrant  = mailie.KindMailboxGrant  // the core grant (seal.KindGrant): a mailbox's key sealed to a person
	KindUserWrap      = mailie.KindUserWrap      // the core reserved byte (seal.KindUserWrap), never sealed
	KindFolderName    = mailie.KindFolderName    // reserved for phase 4: a folder's name
	KindDraft         = mailie.KindDraft         // reserved for phase 4: a draft written in the browser
)

// Mailbox key epochs, and a grant's length.
const (
	// MinEpoch is a mailbox key's first epoch; 0 is never used.
	MinEpoch = mailie.MinEpoch
	// MaxEpoch is the largest, the envelope header's u16be.
	MaxEpoch = mailie.MaxEpoch
	// GrantLen is a grant's length, 88 bytes.
	GrantLen = mailie.GrantLen
)

// GrantRow is the row a grant binds to, Row(namespace, namespace ‖ seal_id ‖
// u16be(epoch)), so a grant opens only for its mailbox, its person and its
// epoch.
func GrantRow(namespace, sealID string, epoch int) (uuid.UUID, error) {
	return mailie.GrantRow(namespace, sealID, epoch)
}

// GrantInfo is a grant's HPKE info: "mlv1/mailbox_grant/<namespace>/<epoch>".
func GrantInfo(namespace string, epoch int) ([]byte, error) {
	return mailie.GrantInfo(namespace, epoch)
}

// GrantAAD is a grant's additional data: "mlv1" ‖ 0x07 ‖ namespace (16) ‖
// GrantRow (16) ‖ the direct envelope's header at that epoch (8), 45 bytes.
func GrantAAD(namespace, sealID string, epoch int) ([]byte, error) {
	return mailie.GrantAAD(namespace, sealID, epoch)
}

// SealGrant seals a mailbox's 32-byte private key to a person's account
// public key: the kit's direct envelope with kind 0x07 at GrantRow, at the
// mailbox key's epoch, 88 bytes. A public key of low order is refused with
// an error matching seal.ErrInvalidKey. The mailbox key is the caller's to
// clear.
func SealGrant(recipientPublicKey []byte, namespace, sealID string, epoch int, mailboxKey []byte) ([]byte, error) {
	return mailie.SealGrant(recipientPublicKey, namespace, sealID, epoch, mailboxKey)
}

// OpenGrant opens a grant with the person's account private key and returns
// the mailbox's private key, which the caller clears. The binding comes first
// (ErrBinding), then the kit's header checks; every other failure is
// seal.ErrAuthentication, a grant that opens to anything but the private
// half of mailboxPublicKey included.
func OpenGrant(account hpke.PrivateKey, namespace, sealID string, epoch int, mailboxPublicKey, grant []byte) ([]byte, error) {
	return mailie.OpenGrant(account, namespace, sealID, epoch, mailboxPublicKey, grant)
}

// CheckGrantShape is what the server checks of a grant it is sent, which it
// cannot open: 88 bytes, Mailie's magic, version 1, suite 1, direct mode,
// the mailbox key's current epoch and a zero reserved byte.
func CheckGrantShape(grant []byte, epoch int) error { return mailie.CheckGrantShape(grant, epoch) }

// ---------------------------------------------------------------------------
// The wrap under the product key (docs/key-scheme.md section 6)
// ---------------------------------------------------------------------------

// PlatformWrap is the wrap of the account key under the product key sk_p
// that the platform's id. delivers to a server whose people sign in through
// it: the kit's platformwrap under the kit's Mailie profile, header 0x03, 61
// bytes, salt "mailie/platform-wrap/v1", label "mailie/platform-wrap".
func PlatformWrap() platformwrap.Profile { return mailie.PlatformWrap() }

// PlatformWrapBinding is what Mailie binds a platform wrap to: the person's
// seal id as the product's user id, id.'s sub, the product key id
// "mailie:<epoch>" the server pinned at the person's sign-in, and the account
// public key it holds (users.public_key). The user id is the seal id for
// every person, never the sub (docs/key-scheme.md section 6.1): a seal id
// equal to the sub is ErrBinding, whatever the sub's version.
func PlatformWrapBinding(sealID, sub string, productKeyEpoch int, accountPublicKey []byte) (platformwrap.Binding, error) {
	return mailie.PlatformWrapBinding(sealID, sub, productKeyEpoch, accountPublicKey)
}

// ---------------------------------------------------------------------------
// The browser vault (docs/key-scheme.md section 7)
// ---------------------------------------------------------------------------

// The browser vault: the kit's key at rest in the browser (its SPEC section
// 8) under Mailie's tag.
const (
	BrowserVaultTag     = mailie.BrowserVaultTag
	BrowserVaultVersion = mailie.BrowserVaultVersion
)

// BrowserVaultAAD is the additional data under which the browser keeps the
// account key at rest, the kit's JSON AAD, not the restricted one of the
// other bindings:
//
//	JCS(["mailie/browser-account-key", 1, seal_id, base64(account public key)])
//
// The browser computes it (web/src/crypto/mailie.ts); this is its reference
// for the vectors.
func BrowserVaultAAD(sealID string, accountPublicKey []byte) ([]byte, error) {
	return mailie.BrowserVaultAAD(sealID, accountPublicKey)
}

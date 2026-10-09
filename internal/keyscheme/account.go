package keyscheme

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/thehappieco/kit/account"
	"github.com/thehappieco/kit/profiles/platform"
)

// The account scheme's labels (docs/key-scheme.md section 5). They name keys;
// they are not credentials.
//
//nolint:gosec // G101: HKDF labels, not credentials
const (
	// PasswordAuthLabel and PasswordWrapLabel are the HKDF infos of the two
	// branches of the Argon2id master key: the auth key, which is sent, and
	// the wrap key, which never leaves the browser.
	PasswordAuthLabel = "mailie/v1/password/auth"
	PasswordWrapLabel = "mailie/v1/password/wrap"
	// RecoveryWrapLabel and RecoveryAuthLabel are the HKDF infos of the two
	// branches of a recovery code: its wrap key and its proof.
	RecoveryWrapLabel = "mailie/v1/recovery/wrap"
	RecoveryAuthLabel = "mailie/v1/recovery/auth"
	// AccountWrapTag opens the additional data of an account wrap.
	AccountWrapTag = "mailie/account-wrap"
	// AccountWrapVersion is the 1 in the additional data: the format's
	// version, the same for both kinds.
	AccountWrapVersion = 1
	// AccountWrapHeader is the first byte of the password and recovery wraps
	// of an account key. The cloud's wrap under the product key starts with
	// 0x03 (platformwrap.Header) and a grant with the seal magic 'M', so a
	// blob in the wrong column fails at its first byte.
	AccountWrapHeader = 0x02
	// AccountWrapLen is a wrap's length: the header, a 12-byte nonce, the
	// 32-byte account key and a 16-byte tag.
	AccountWrapLen = 1 + 12 + KeyLen + 16
	// SaltLabel keys the salt the server hands out for an address
	// (DecoySalt).
	SaltLabel = "mailie/v1/kdf-salt"
	// SaltLen is the only salt length the profile derives with.
	SaltLen = platform.SaltLen
	// PasswordPreparation names the preparation of a password: the platform
	// profile's, for a password being presented (no minimum length; a new
	// password's minimum is checked by platform.PrepareNewPassword).
	PasswordPreparation = platform.PasswordProfile
)

// WrapKind says which secret an account wrap is under. It is bound into the
// wrap's additional data.
type WrapKind string

const (
	// WrapPassword is the wrap under the password's wrap key.
	WrapPassword WrapKind = "password"
	// WrapRecovery is the wrap under the recovery code's wrap key.
	WrapRecovery WrapKind = "recovery"
)

func (k WrapKind) valid() bool { return k == WrapPassword || k == WrapRecovery }

// DefaultKDF is what new accounts derive with: Argon2id, 64 MiB, three
// passes, one lane, the floor of Bounds.
var DefaultKDF = account.KDFParams{Alg: platform.KDFAlg, M: platform.KDFMinM, T: platform.KDFMinT, P: platform.KDFMinP}

// Account is Mailie's account profile for the kit's account package: its
// labels, the platform's preparation of a presented password, the platform's
// KDF bounds (m from 64 to 256 MiB, t from 3 to 10, p from 1 to 4, m×t at
// most 1 GiB·pass, a salt of exactly 16 bytes), base64url text without
// padding, the platform's canonical form of a recovery code, and the
// one-byte header 0x02 with no legacy form. Every call returns a fresh
// value.
//
//	master = Argon2id(prepared password, salt (16), m, t, p, 32 bytes, version 0x13)
//	auth   = HKDF-SHA256(master, salt = ∅, info = "mailie/v1/password/auth", 32)
//	wrap   = HKDF-SHA256(master, salt = ∅, info = "mailie/v1/password/wrap", 32)
func Account() account.Profile {
	return account.Profile{
		AuthLabel:          PasswordAuthLabel,
		WrapLabel:          PasswordWrapLabel,
		RecoveryKeyLabel:   RecoveryWrapLabel,
		RecoveryProofLabel: RecoveryAuthLabel,
		WrapHeader:         []byte{AccountWrapHeader},
		LegacyV1:           false,
		Prepare:            platform.PreparePassword,
		Bounds:             platform.KDFBounds(),
		Encoding:           base64.RawURLEncoding,
		NormaliseRecovery:  platform.CanonicalRecoveryCode,
	}
}

// AccountWrapAAD is the additional data of an account wrap, a restricted JSON
// AAD (the kit's SPEC section 11.1):
//
//	JCS(["mailie/account-wrap", 1, kind, seal_id, base64url(account public key)])
//
// It binds the immutable seal id, never the address, so an address change
// needs no re-wrap; the kind, so a password wrap is never taken for the
// recovery wrap; and the account public key the server holds for the person,
// so a wrap opens only for the key the person's grants are sealed to.
func AccountWrapAAD(kind WrapKind, sealID string, accountPublicKey []byte) ([]byte, error) {
	if !kind.valid() {
		return nil, fmt.Errorf("%w: a wrap kind is password or recovery", ErrBinding)
	}
	if !ValidSealID(sealID) {
		return nil, fmt.Errorf("%w: the seal id is not a lowercase UUIDv4", ErrBinding)
	}
	if len(accountPublicKey) != KeyLen {
		return nil, fmt.Errorf("%w: an account public key is %d bytes", ErrBinding, KeyLen)
	}
	aad, err := platform.JCSArray(AccountWrapTag, AccountWrapVersion, string(kind), sealID, platform.EncodeB64(accountPublicKey))
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrBinding, err)
	}
	return aad, nil
}

// SealAccountWrap wraps a 32-byte account key under a wrap key (the
// password's, or the recovery code's) for the person sealID names:
//
//	0x02 ‖ nonce (12) ‖ AES-256-GCM(wrapKey, nonce, account key, AccountWrapAAD(...))     61 bytes
//
// with a fresh random nonce. It opens what it made and compares before it
// returns it (the self-test). The keys are the caller's to clear.
func SealAccountWrap(kind WrapKind, wrapKey, accountKey []byte, sealID string) ([]byte, error) {
	pub, err := PublicKey(accountKey)
	if err != nil {
		return nil, err
	}
	aad, err := AccountWrapAAD(kind, sealID, pub)
	if err != nil {
		return nil, err
	}
	wrap, err := account.Wrap(Account(), wrapKey, accountKey, aad)
	if err != nil {
		return nil, err
	}
	again, err := OpenAccountWrap(kind, wrapKey, wrap, sealID, pub)
	if err != nil {
		return nil, fmt.Errorf("keyscheme: the new wrap failed its self-test: %w", err)
	}
	clear(again)
	return wrap, nil
}

// OpenAccountWrap opens an account wrap with its wrap key and returns the
// account key, which the caller clears. In order: the binding (ErrBinding);
// the shape, a wrap shorter than 61 bytes being account.ErrTruncated and one
// longer, or not starting with 0x02, account.ErrWrongKey; the wrap key's
// length (account.ErrBadKey); the tag; and that the key it opened is the
// private half of accountPublicKey, compared in constant time. A wrong key,
// another binding, a changed byte and a key that is not the bound one are
// all account.ErrWrongKey.
func OpenAccountWrap(kind WrapKind, wrapKey, wrap []byte, sealID string, accountPublicKey []byte) ([]byte, error) {
	aad, err := AccountWrapAAD(kind, sealID, accountPublicKey)
	if err != nil {
		return nil, err
	}
	if len(wrap) < AccountWrapLen {
		return nil, account.ErrTruncated
	}
	if len(wrap) != AccountWrapLen || wrap[0] != AccountWrapHeader {
		return nil, account.ErrWrongKey
	}
	key, _, err := account.Unwrap(Account(), wrapKey, wrap, aad)
	if err != nil {
		return nil, err
	}
	if !isPublicHalf(key, accountPublicKey) {
		clear(key)
		return nil, account.ErrWrongKey
	}
	return key, nil
}

// CheckAccountWrapShape is what the server checks of a password or recovery
// wrap it is sent, which it cannot open: 61 bytes starting with 0x02.
func CheckAccountWrapShape(wrap []byte) error {
	if len(wrap) != AccountWrapLen || wrap[0] != AccountWrapHeader {
		return fmt.Errorf("%w: an account wrap is %d bytes starting with 0x%02x", ErrShape, AccountWrapLen, AccountWrapHeader)
	}
	return nil
}

// DecoySalt is the salt the server hands out for an address: the first 16
// bytes of
//
//	HMAC-SHA256(key, "mailie/v1/kdf-salt|" ‖ NormaliseAddress(address))
//
// under a 32-byte secret of the server's own. It is an account's target salt
// (docs/key-scheme.md section 5.3): stored at enrolment, and stored again,
// once the person's browser re-derives, after the account's address changes;
// and the salt a challenge answers for an address with no account, so that a
// challenge does not say which addresses have one, nor when one was made.
// Only this server uses it, so only its spelling here matters.
func DecoySalt(key []byte, address string) ([]byte, error) {
	if len(key) != KeyLen {
		return nil, fmt.Errorf("keyscheme: the salt key is %d bytes, not %d", len(key), KeyLen)
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(SaltLabel + "|" + NormaliseAddress(address)))
	return mac.Sum(nil)[:SaltLen], nil
}

// NormaliseAddress is an address as the server stores it (users.email, the
// output of auth.NormalizeEmail for every address it accepts): white space
// trimmed at both ends as Go's strings.TrimSpace does, and every letter
// lowered by Unicode's simple lowercase mapping, as strings.ToLower does (Ä
// is ä, a Kelvin sign is k, a capital I with a dot is i, a capital sigma is
// σ wherever it stands). The salt is derived from it, the challenge and the
// sign-in look an account up by it, and the browser keys its memory of the
// addresses that enrolled by it (web/src/crypto/mailie.ts,
// normaliseAddress, held to the same vectors). It validates nothing.
func NormaliseAddress(address string) string { return strings.ToLower(strings.TrimSpace(address)) }

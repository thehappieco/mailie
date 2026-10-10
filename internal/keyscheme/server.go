package keyscheme

import (
	"crypto/hmac"
	"crypto/sha256"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// What stays the server's: the kit's profile leaves to Mailie how its server
// normalises an address and derives the salt it hands out (a server's salts
// are outside the kit, its SPEC section 13) and the drawing of seal ids. The
// vectors pin the first two all the same (testdata/account-go.json, the ops
// mailie.normalise_address and mailie.decoy_salt), and the console's
// normaliseAddress (web/src/crypto/mailie.ts) is held to them.

// SaltLabel keys the salt the server hands out for an address (DecoySalt).
// It names a key; it is not a credential.
const SaltLabel = "mailie/v1/kdf-salt"

// NewSealID draws a seal id for a person: a random UUIDv4.
func NewSealID() string { return uuid.NewString() }

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

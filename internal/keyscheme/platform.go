package keyscheme

import (
	"encoding/base64"
	"fmt"

	"github.com/thehappieco/kit/jcs"
	"github.com/thehappieco/kit/platformwrap"
	"github.com/thehappieco/kit/profiles/mailie"
	"github.com/thehappieco/kit/profiles/platform"
)

// PlatformWrap is the wrap of the account key under the product key sk_p
// that the platform's id. delivers to a server whose people sign in through
// it (docs/key-scheme.md section 6): the kit's platformwrap under the kit's
// Mailie profile (its SPEC section 6.8 and Appendix D), header 0x03, 61
// bytes, salt "mailie/platform-wrap/v1", label "mailie/platform-wrap".
func PlatformWrap() platformwrap.Profile { return mailie.PlatformWrap() }

// PlatformWrapBinding is what Mailie binds a platform wrap to: the person's
// seal id as the product's user id, id.'s sub, the product key id
// "mailie:<epoch>" the server pinned at the person's sign-in, and the account
// public key it holds (users.public_key). The user id is the seal id for
// every person, never the sub, which departs from the kit's rule for an
// account created through id. (its SPEC section 6.8; docs/key-scheme.md
// section 6.1): a seal id equal to the sub is refused, whatever the sub's
// version. The kit refuses a sub or a product key id outside their spelling
// when the wrap is sealed or opened.
func PlatformWrapBinding(sealID, sub string, productKeyEpoch int, accountPublicKey []byte) (platformwrap.Binding, error) {
	if !ValidSealID(sealID) {
		return platformwrap.Binding{}, fmt.Errorf("%w: the seal id is not a lowercase UUIDv4", ErrBinding)
	}
	if sealID == sub {
		return platformwrap.Binding{}, fmt.Errorf("%w: the user id is the seal id, never the sub", ErrBinding)
	}
	if productKeyEpoch < 1 || productKeyEpoch > platform.MaxEpoch {
		return platformwrap.Binding{}, fmt.Errorf("%w: a product key epoch is from 1 to %d", ErrBinding, platform.MaxEpoch)
	}
	return platformwrap.Binding{
		UserID:           sealID,
		Sub:              sub,
		ProductKeyID:     platform.ProductKeyID(mailie.PlatformWrapProduct, productKeyEpoch),
		AccountPublicKey: accountPublicKey,
	}, nil
}

// The browser vault (docs/key-scheme.md section 7): the kit's key at rest in
// the browser (its SPEC section 8) under Mailie's tag.
const (
	BrowserVaultTag     = "mailie/browser-account-key"
	BrowserVaultVersion = 1
)

// BrowserVaultAAD is the additional data under which the browser keeps the
// account key at rest, the kit's JSON AAD (its SPEC sections 2 and 8), not
// the restricted one of the other bindings:
//
//	JCS(["mailie/browser-account-key", 1, seal_id, base64(account public key)])
//
// with standard base64, padded, as the kit's builder writes it; its '+', '/'
// and '=' are outside the restricted alphabet, so platform.JCSArray would
// refuse it. The browser
// computes it (web/src/crypto/mailie.ts); this is its reference for the
// vectors.
func BrowserVaultAAD(sealID string, accountPublicKey []byte) ([]byte, error) {
	if !ValidSealID(sealID) {
		return nil, fmt.Errorf("%w: the seal id is not a lowercase UUIDv4", ErrBinding)
	}
	if len(accountPublicKey) != KeyLen {
		return nil, fmt.Errorf("%w: an account public key is %d bytes", ErrBinding, KeyLen)
	}
	aad, err := jcs.Marshal([]any{BrowserVaultTag, BrowserVaultVersion, sealID, stdB64(accountPublicKey)})
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrBinding, err)
	}
	return aad, nil
}

// stdB64 is standard base64 with padding, the kit's text for the browser
// vault's public key.
func stdB64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

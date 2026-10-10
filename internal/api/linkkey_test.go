package api_test

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"testing"

	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/auth/authtest"
	"github.com/thehappieco/mailie/internal/keyscheme"
)

// linkAs is a link's body as the caller sends it: a person's browser adds the
// mailbox's first key, a fresh one (docs/key-scheme.md section 12.11); an API
// key's link carries none, since an operator mailbox has no key.
func linkAs(t *testing.T, token, body string) string {
	t.Helper()
	if auth.IsAPIKey(token) {
		return body
	}
	var fields map[string]any
	if err := json.Unmarshal([]byte(body), &fields); err != nil {
		t.Fatal(err)
	}
	fields["public_key"] = mailboxPublicKey(t)
	fields["namespace"] = keyscheme.NewSealID()
	fields["grant"] = grantAt(t, 1)
	return jsonOf(t, fields)
}

// mailboxPublicKey is the public half of a fresh X25519 key pair, base64url,
// as a browser sends a mailbox's.
func mailboxPublicKey(t *testing.T) string {
	t.Helper()
	key, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(key.PublicKey().Bytes())
}

// grantAt is 88 random bytes with a grant's header at epoch, base64url: what
// the server checks of a grant, sealing nothing.
func grantAt(t *testing.T, epoch int) string {
	t.Helper()
	g := make([]byte, keyscheme.GrantLen)
	if _, err := rand.Read(g); err != nil {
		t.Fatal(err)
	}
	copy(g, []byte{0x4d, 0x4c, 0x01, 0x01, 0x01})
	binary.BigEndian.PutUint16(g[5:7], uint16(epoch)) //nolint:gosec // G115: tests pass epochs that fit
	g[7] = 0
	return base64.RawURLEncoding.EncodeToString(g)
}

// sealedFor is a change of flags that gives userID read on a mailbox whose
// key is at epoch 1, with their grant and the account public key it was
// sealed to, theirs now, as the giver's browser sends it (docs/key-scheme.md
// section 12.13).
func sealedFor(t *testing.T, h *harness, userID, body string) string {
	t.Helper()
	var fields map[string]any
	if err := json.Unmarshal([]byte(body), &fields); err != nil {
		t.Fatal(err)
	}
	if fields["read"] == true {
		fields["grant"] = grantAt(t, 1)
		fields["public_key"] = accountKeyOf(t, h, userID)
	}
	return jsonOf(t, fields)
}

// accountKeyOf is a person's account public key as the server serves it now,
// base64url: what a browser seals their grants to, and names.
func accountKeyOf(t *testing.T, h *harness, userID string) string {
	t.Helper()
	return base64.RawURLEncoding.EncodeToString(authtest.AccountPublicKey(t, h.store, userID))
}
